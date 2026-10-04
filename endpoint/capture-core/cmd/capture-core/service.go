package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/canon"
	"github.com/shadow-ai-capture/device/capture-core/classifierlink"
	"github.com/shadow-ai-capture/device/capture-core/cli"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/detect"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/loopback"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/trust"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// service is the wired agent: one graph, built once, started by the supervisor in §3.5's literal
// order. It holds no policy of its own — resolution, verification and the mode gate live in
// capture-core's packages, and every provider keeps its own state machine.
type service struct {
	cfg    Config
	log    *slog.Logger
	logf   core.Logger
	reg    *core.Registry
	sup    *core.Supervisor
	store  *policy.Store
	result policy.Result

	spool   *spoolHolder
	sink    *lazySink
	pipe    *core.Pipeline
	host    *classifierHostController
	broker  *loopback.Broker
	tlsProv *tlsproxy.Provider
	detect  *detect.Provider
	drainer *drain.Drainer

	// trustMgr is the platform trust store seam. It is non-nil only when --trust-install is
	// set; the same instance is handed to proxy.tls (to install) and to the supervisor (to
	// remove), so the cert removed is the cert installed.
	trustMgr *trust.Manager

	health *healthChannel

	startedAt time.Time
	runErr    chan error
}

// newService builds the graph without starting anything. Every constructor here can fail, and a
// failure means nothing is running: the binary refuses to start half-way (§4.1's transactional
// Start, applied to the composition).
func newService(ctx context.Context, cfg Config, log *slog.Logger) (*service, error) {
	logf := slogLogger{log}

	s := &service{cfg: cfg, log: log, logf: logf, reg: core.NewRegistry(time.Now, logf), startedAt: time.Now()}

	// Policy first: it decides what the providers are allowed to do, and it must be in force before
	// any provider starts (§3.5 step 1).
	store, result, err := loadPolicy(cfg, policy.ArtefactResolverFunc(func(ref policy.ArtefactRef) error {
		if _, err := os.Stat(ref.Path); err != nil {
			return fmt.Errorf("artefact %s (%s): %w", ref.Name, ref.Path, err)
		}
		return nil
	}))
	if err != nil {
		// A missing or unreadable bundle is not fatal: with no previous bundle the agent runs at
		// M0, which reads no content. It is reported, never silently widened.
		log.Warn("policy bundle unavailable; running at M0", "path", cfg.BundlePath, "error", err)
		store, result = nil, policy.Result{Outcome: policy.OutcomeFellToM0, Cause: policy.CauseSchemaInvalid, Err: err}
	}
	s.store = store
	s.result = result
	if store != nil && store.InForce() != nil {
		log.Info("policy in force", "version", store.InForce().Version, "outcome", result.Outcome)
	} else {
		log.Warn("no policy in force: every observation resolves to M0 (metadata only)")
	}

	// The spool is opened by the supervisor (step 2). The pipeline is constructed now with a sink
	// that refuses to write until the spool is actually open, so a provider that somehow starts
	// early fails loudly instead of dropping silently (C22).
	boundsBytes, boundsRows := spoolBounds(cfg.SpoolBoundsProfile)
	s.spool = &spoolHolder{cfg: cfg, bounds: capturespool.Bounds{MaxBytes: boundsBytes, MaxEntries: boundsRows}}
	s.sink = &lazySink{spool: s.spool}

	retention, err := time.ParseDuration(cfg.Retention)
	if err != nil {
		return nil, err
	}
	pipe, err := core.NewPipeline(s.sink, time.Now, newEventID)
	if err != nil {
		return nil, err
	}
	// Identity is only the flags for a local/offline run (no --device-endpoint): nothing is ever
	// sent to a server, so the flags are the identity. With a drain configured the identity is left
	// unresolved and installed by the supervisor's resolve_device_identity step (before any provider
	// starts), from the sealed credential or a bounded synchronous enrolment. A placeholder flag is
	// never used for a drain run.
	if strings.TrimSpace(cfg.DeviceEndpoint) == "" {
		pipe.SetIdentity(cfg.identity())
	}
	pipe.Bundles = s.currentBundle
	pipe.Retention = retention
	// C3 comes from device/canon, generated from Node's ICU and checked against it. With no
	// normaliser the pipeline degrades and refuses to claim a Tier-T key; here there is one, so
	// canonical digests are real.
	pipe.Normalizer = canon.Normalizer{}
	s.pipe = pipe

	// The classifier host (§3.4). An unconfigured or unavailable host degrades to rules-only with
	// confidence: degraded, and never fails the submission.
	s.host = &classifierHostController{cfg: cfg, log: log}
	s.host.onReady = func(c *classifierlink.Client) { pipe.Classifier = c }

	// The device CA and the platform trust store. A pinned CA pair (or the bundle's public root)
	// keeps the trusted root stable across restarts; with neither, proxy.tls mints an ephemeral
	// one, which is the pre-existing behaviour. --trust-install is the only thing that writes the
	// OS store (§5.2: the wrong store fails silently, so installing is opt-in and the end-to-end
	// probe is what reports health, never the write returning nil).
	caCertPEM, caKeyPEM, err := deviceCAPEM(cfg, s.currentBundle())
	if err != nil {
		return nil, err
	}
	// The signed bundle carries only the public root by design; the private half is delivered as
	// --ca-key. A certificate with no key is not an interceptor CA — it cannot mint leaves — so the
	// features that point clients at the proxy or install the root (--cli-shim, --trust-install)
	// need the full pair, or they would advertise a proxy that cannot serve those clients.
	caPair := len(caCertPEM) > 0 && len(caKeyPEM) > 0
	if (cfg.TrustInstall || cfg.CLIShim) && !caPair {
		return nil, errors.New("--trust-install and --cli-shim need the full per-device CA key pair: set --ca-key plus --ca-cert (or a bundle carrying interception.root_ca_pem); a bundle root without its key cannot mint leaves")
	}
	if cfg.TrustInstall {
		s.trustMgr = trust.New(trust.Config{
			OS:    trust.HostOS(),
			Store: trustStoreName(cfg.TrustStore),
			Logf:  func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		})
	} else if cfg.TrustRemoveOnStop {
		log.Warn("--trust-remove-on-stop is set but --trust-install is not; there is no installed root to remove")
	}

	// Providers. Each one owns its own coverage row and its own failure mode.
	if cfg.EnableTLS {
		canary := tlsCanary(cfg, s.currentBundle())
		tlsCfg := tlsproxy.Config{
			Listen:     tlsListen(cfg, s.currentBundle()),
			Bundles:    s.currentBundle,
			Pipeline:   pipe,
			Decide:     defaultDecision,
			Agent:      cfg.scopeQuery(""),
			Log:        logf,
			Clock:      time.Now,
			CanaryHost: canaryHost(canary),
			CanaryPort: canaryPort(canary),
			BodyCap:    bodyCapFrom(s.currentBundle()),
		}
		// Pass the pair only when it is complete; with no pair the provider mints an ephemeral CA
		// (the pre-existing behaviour). Passing a cert without a key would make it refuse to start.
		if caPair {
			tlsCfg.CACertPEM = caCertPEM
			tlsCfg.CAKeyPEM = caKeyPEM
		} else if len(caCertPEM) > 0 {
			log.Warn("a bundle root CA is configured but no --ca-key is present; proxy.tls will mint an ephemeral CA that no client trusts (set --ca-key to pin the device CA)")
		}
		if s.trustMgr != nil {
			tlsCfg.TrustRoot = s.trustMgr
		}
		tlsProv := tlsproxy.New(tlsCfg)
		s.tlsProv = tlsProv
		if err := s.reg.Add(tlsProv); err != nil {
			return nil, err
		}
	}
	if cfg.EnableLoopback {
		broker := loopback.New(loopback.Config{
			Ports:    portsFrom(s.currentBundle()),
			Pipeline: pipe,
			Agent:    loopback.AgentInfo{Population: cfg.Population, UserRef: cfg.UserRef},
			Decide:   defaultDecision,
			Log:      logf,
			Clock:    time.Now,
			BodyCap:  bodyCapFrom(s.currentBundle()),
		})
		s.broker = broker
		if err := s.reg.Add(broker); err != nil {
			return nil, err
		}
	}
	if cfg.EnableProcDetect {
		enum := newProcessEnumerator(log)
		if enum == nil {
			log.Warn("proc.detect requested but no process enumerator is available on this host; the route is a named gap")
		} else {
			dp := detect.New(detect.Config{
				Enumerator: enum,
				Bundles:    s.currentBundle,
				Pipeline:   pipe,
				Agent:      cfg.scopeQuery(""),
				Log:        logf,
				Clock:      time.Now,
			})
			// Only the local/offline run stamps the flags; a drain run leaves proc.detect without an
			// identity until resolve_device_identity installs the issued (or unresolved) one.
			if strings.TrimSpace(cfg.DeviceEndpoint) == "" {
				dp.SetIdentity(detect.Identity{TenantID: cfg.TenantID, DeviceID: cfg.DeviceID})
			}
			s.detect = dp
			if err := s.reg.Add(dp); err != nil {
				return nil, err
			}
		}
	}

	// cli.shim (step 4 of §3.5): files and environment only, no ports. It needs the device root
	// CA from the signed bundle (or --ca-cert) and the proxy address the bundle pins, because it
	// runs before proxy.tls binds. Without a root it still starts and reports degraded — a shim
	// with no CA to install is a named coverage gap, never a silent healthy row.
	if cfg.CLIShim {
		if len(caCertPEM) == 0 {
			log.Warn("cli.shim is enabled but no device root CA is available (set --ca-cert or a bundle with interception.root_ca_pem); the shim will report degraded")
		}
		shimCfg := cli.Config{
			ManagedDir: cfg.ShimDir,
			ProxyAddr:  shimProxyAddr(cfg, s.currentBundle()),
			RootCAPEM:  caCertPEM,
			Log:        logf,
			Clock:      time.Now,
		}
		if b := s.currentBundle(); b != nil {
			shimCfg.NoProxy = b.CLIShim.NoProxy
			shimCfg.Runtimes = b.CLIShim.Runtimes
			shimCfg.NodeRequire = b.CLIShim.NodeRequire
			if strings.TrimSpace(shimCfg.ManagedDir) == "" {
				shimCfg.ManagedDir = b.CLIShim.ManagedDir
			}
		}
		if err := s.reg.Add(cli.New(shimCfg)); err != nil {
			return nil, err
		}
	}

	// The supervisor drives §3.5. It owns the sequence; the registry owns the bookkeeping.
	sup, err := core.NewSupervisor(s.reg, logf, time.Now)
	if err != nil {
		return nil, err
	}
	sup.Policy = &policyLoader{store: store, path: cfg.BundlePath, reg: s.reg, result: &s.result, log: log}
	sup.Spool = s.spool
	sup.Identity = identityResolver{s}
	sup.ClassifierHost = s.host
	// Assign only a real provider: a typed-nil interface is not nil, and calling Release on it
	// would panic the shutdown path (E14 is the path that must never fail).
	if s.broker != nil {
		sup.Loopback = s.broker
	}
	sup.DrainDeadline = cfg.DrainDeadline
	// No system proxy on this host: it is a platform facility behind an interface, and the
	// binary deliberately does not write it. The trust store IS wired when --trust-install is
	// set, so the same manager that installed the root removes it.
	sup.SystemProxy = nil
	if s.trustMgr != nil {
		sup.TrustRoot = s.trustMgr
	}
	sup.RemoveTrustRoot = cfg.TrustRemoveOnStop
	s.sup = sup

	s.health = newHealthChannel(cfg, log, s)
	return s, nil
}

func (s *service) currentBundle() *policy.Bundle {
	if s.store == nil {
		return nil
	}
	return s.store.InForce()
}

// Start runs the supervisor's startup column and then the health channel. It is the only way the
// agent reaches "running", and every error path here leaves nothing started.
func (s *service) Start(ctx context.Context) error {
	if s.cfg.DryRun {
		s.log.Info("dry run: nothing started")
		return nil
	}
	if err := s.sup.Startup(ctx); err != nil {
		// The supervisor refuses to continue when the spool cannot open, because a provider with
		// nowhere to write must not start (§3.5 step 2). Anything already started is stopped here.
		s.log.Error("startup refused; stopping anything already started", "error", err)
		_ = s.sup.Shutdown(context.Background())
		return err
	}
	if err := s.startDrainer(ctx); err != nil {
		// Not fatal: a drainer that cannot start leaves observations spooled, which is the honest
		// degraded state (reported in the health snapshot), never a silent drop.
		s.log.Warn("drainer not started; observations stay spooled", "error", err)
	}
	s.health.Start(ctx)
	s.log.Info("capture-core started", "order", s.sup.Order())
	return nil
}

// identityResolver is the core.IdentityResolver the supervisor runs between open_spool and the
// first provider, so the envelope identity is in place before anything can mint.
type identityResolver struct{ s *service }

// enrolStartupTimeout bounds the synchronous enrolment a drain-configured device performs before
// providers start. One enrol attempt fits comfortably; a longer outage is retried by the
// background drain loop rather than blocking startup.
const enrolStartupTimeout = 30 * time.Second

func (r identityResolver) Resolve(ctx context.Context) error { return r.s.resolveIdentity(ctx) }

// resolveIdentity installs the envelope identity before providers start:
//   - no --device-endpoint: the flags are the identity (local/offline, nothing is ever sent);
//   - a sealed credential exists: load it and adopt its server-minted tenant_id/device_id (the
//     flags are validated: a disagreement is logged and the credential wins);
//   - no credential: bounded synchronous enrolment; on failure the identity stays unresolved and
//     the pipeline refuses to mint (fail-closed) while the background drain retries.
func (s *service) resolveIdentity(ctx context.Context) error {
	if strings.TrimSpace(s.cfg.DeviceEndpoint) == "" {
		id := s.cfg.identity()
		s.pipe.SetIdentity(id)
		if s.detect != nil {
			s.detect.SetIdentity(detect.Identity{TenantID: id.TenantID, DeviceID: id.DeviceID})
		}
		return nil
	}
	if err := s.ensureDrainer(); err != nil {
		s.pipe.SetIdentityUnresolved()
		if s.detect != nil {
			s.detect.SetIdentity(detect.Identity{})
		}
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, enrolStartupTimeout)
	defer cancel()
	if !s.drainer.EnsureEnrolled(bounded) {
		s.pipe.SetIdentityUnresolved()
		if s.detect != nil {
			s.detect.SetIdentity(detect.Identity{})
		}
		return errors.New("drain: bounded synchronous enrolment did not produce an identity")
	}
	// OnEnrolled already installed the identity (setCredential -> adoptIssuedIdentity -> OnEnrolled).
	return nil
}

// ensureDrainer builds the device-to-cloud drainer (credential store + drain.New) and wires it into
// the service and the spool. It is idempotent and runs before providers start, so the startup
// identity resolution can load or enrol without starting the background loop. The spool is already
// open at this point, so the credential's key provider can read the spool key the drain shares with
// the spool.
func (s *service) ensureDrainer() error {
	if strings.TrimSpace(s.cfg.DeviceEndpoint) == "" || s.drainer != nil {
		return nil
	}
	keys, err := capturespool.NewFileKeyProvider(s.cfg.SpoolKey, s.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("drain: key provider: %w", err)
	}
	creds, err := credential.Open(s.cfg.CredentialFile, keys)
	if err != nil {
		return fmt.Errorf("drain: credential store: %w", err)
	}
	d, err := drain.New(drain.Config{
		Endpoint:       s.cfg.DeviceEndpoint,
		AuthMode:       protocol.AuthMode(s.cfg.AuthMode),
		EnrolmentToken: s.cfg.EnrolmentToken,
		CAFile:         s.cfg.CAFile,
		TenantID:       s.cfg.TenantID,
		DeviceID:       s.cfg.DeviceID,
		MDMID:          s.cfg.MDMID,
		AgentVersion:   version,
		BackoffBase:    s.cfg.BackoffBase,
		BackoffCap:     s.cfg.BackoffCap,
		DrainInterval:  s.cfg.DrainInterval,
		Expire:         s.spool.Expire,
		// Adopt the server-minted identity once the issued credential is in hand (loaded, first
		// enrol, or re-enrol), so envelopes carry the tenant_id/device_id the write path
		// authenticates. UserRef is not issued by the server, so it stays the flag value.
		OnEnrolled: func(c *credential.Credential) {
			s.pipe.SetIdentity(core.Identity{TenantID: c.TenantID, DeviceID: c.DeviceID, UserRef: s.cfg.UserRef})
			if s.detect != nil {
				s.detect.SetIdentity(detect.Identity{TenantID: c.TenantID, DeviceID: c.DeviceID})
			}
		},
	}, s.spool.store, creds, slogLogger{s.log}, time.Now)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	s.drainer = d
	s.log.Info("drain configured", "endpoint", s.cfg.DeviceEndpoint, "auth_mode", s.cfg.AuthMode, "credential_loaded", d.Status().Enrolled)
	s.spool.mu.Lock()
	s.spool.drain = d
	s.spool.mu.Unlock()
	return nil
}

// startDrainer starts the drainer's background loop. ensureDrainer must have run first (it does,
// from the supervisor's resolve_device_identity step).
func (s *service) startDrainer(ctx context.Context) error {
	if strings.TrimSpace(s.cfg.DeviceEndpoint) == "" {
		return nil
	}
	if err := s.ensureDrainer(); err != nil {
		return err
	}
	return s.drainer.Start(ctx)
}

// Stop runs the shutdown column and releases the spool. The supervisor releases the loopback port
// before anything else (§3.5 step 2, E14).
func (s *service) Stop(ctx context.Context) error {
	s.health.Stop()
	if s.drainer != nil {
		// End the background loop before the supervisor's bounded shutdown drain, so the final
		// drain is the only thing sending.
		_ = s.drainer.Stop(ctx)
	}
	err := s.sup.Shutdown(ctx)
	closeErr := s.spool.Close()
	s.log.Info("capture-core stopped", "order", s.sup.Order())
	if err != nil {
		return err
	}
	return closeErr
}

// ---------------------------------------------------------------------------------------------
// Spool: the real capture-spool, behind the supervisor's controller interface.

// spoolHolder opens the spool when the supervisor asks and stays the single owner of it.
type spoolHolder struct {
	cfg    Config
	bounds capturespool.Bounds

	mu sync.Mutex
	sp *capturespool.Spool

	// drain is the device-to-cloud drainer, wired after the service starts. It is nil when the
	// drain is disabled (no --device-endpoint), in which case Drain reports what is still spooled.
	drain *drain.Drainer
}

func (h *spoolHolder) Open(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp != nil {
		return nil
	}
	if err := os.MkdirAll(h.cfg.SpoolDir, 0o700); err != nil {
		return fmt.Errorf("creating spool directory: %w", err)
	}
	keys, err := capturespool.NewFileKeyProvider(h.cfg.SpoolKey, h.cfg.SpoolDir)
	if err != nil {
		return err
	}
	// CorruptQuarantine: §12.2 requires an unreadable spool to be reported as data loss with a
	// count rather than stopping collection, and never to start clean silently.
	sp, err := capturespool.Open(capturespool.Config{
		Dir:       h.cfg.SpoolDir,
		Keys:      keys,
		Bounds:    h.bounds,
		OnCorrupt: capturespool.CorruptQuarantine,
	})
	if err != nil {
		return err
	}
	if rec := sp.Extended().Recovery; reportsLoss(rec) {
		fmt.Fprintf(os.Stderr, "capture-core: spool recovery reported loss: %+v\n", rec)
	}
	h.sp = sp
	return nil
}

func (h *spoolHolder) Stats() protocol.SpoolStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return protocol.SpoolStats{}
	}
	return h.sp.Stats()
}

// store returns the spool as protocol.Store, for the drainer. It is resolved lazily because the
// spool is opened by the supervisor, not by the drainer's construction.
func (h *spoolHolder) store() (protocol.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return nil, errors.New("spool is not open")
	}
	return h.sp, nil
}

// Expire applies device-side retention through the concrete spool, which is not part of
// protocol.Store but is what drops records past their retention deadline (counted, never silent).
func (h *spoolHolder) Expire(now time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return 0, errors.New("spool is not open")
	}
	return h.sp.Expire(now)
}

// Drain is §3.5 step 3: bounded, and every record it cannot deliver is counted rather than
// silently discarded. When a drainer is wired it drives the real device-to-cloud path; otherwise
// the drain reports what is still pending and stops at the deadline.
func (h *spoolHolder) Drain(ctx context.Context, deadline time.Time) (core.DrainResult, error) {
	h.mu.Lock()
	d := h.drain
	sp := h.sp
	h.mu.Unlock()

	if d != nil {
		res, err := d.Drain(ctx, deadline)
		return core.DrainResult{Delivered: res.Delivered, Dropped: res.Rejected, Err: err}, nil
	}

	res := core.DrainResult{}
	if sp == nil {
		return res, errors.New("spool is not open")
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		entries, err := sp.Peek(64)
		if err != nil {
			return res, err
		}
		if len(entries) == 0 {
			return res, nil
		}
		res.Delivered += 0 // nothing leaves the device until the ingest client exists; see README
		return res, nil
	}
	return res, nil
}

func (h *spoolHolder) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return nil
	}
	err := h.sp.Close()
	h.sp = nil
	return err
}

// lazySink is the pipeline's view of the spool. It refuses writes before Open, which is what makes
// "the spool opens before any provider starts" observable rather than conventional.
type lazySink struct {
	spool *spoolHolder
}

func (l *lazySink) Append(e protocol.Entry) (protocol.Entry, error) {
	l.spool.mu.Lock()
	sp := l.spool.sp
	l.spool.mu.Unlock()
	if sp == nil {
		return protocol.Entry{}, errors.New("spool is not open; a provider must not start before the spool does (§3.5 step 2)")
	}
	return sp.Append(e)
}

func (l *lazySink) Stats() protocol.SpoolStats { return l.spool.Stats() }

func reportsLoss(rec capturespool.Recovery) bool {
	return rec.TornBytes > 0 || rec.InFlightResetToPending > 0 || len(rec.Quarantined) > 0
}

// ---------------------------------------------------------------------------------------------
// Policy: the supervisor's loader applies the bundle to the registry as a diff, never a restart.

type policyLoader struct {
	store  *policy.Store
	path   string
	reg    *core.Registry
	result *policy.Result
	log    *slog.Logger
}

func (p *policyLoader) Load(ctx context.Context) error {
	if p.store == nil || p.path == "" {
		return nil // no bundle configured: M0, already reported
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return err
	}
	res := p.store.Apply(raw)
	*p.result = res
	if res.Err != nil {
		// §13.3: the previous bundle stays in force (or M0 with none). The error is reported and
		// the agent keeps collecting what it is permitted to collect.
		return res.Err
	}
	for _, applied := range p.reg.ApplyPolicy(*p.store.InForce()) {
		if applied.Err != nil {
			p.log.Warn("provider could not apply the bundle", "provider", applied.Route, "error", applied.Err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Classifier host controller: the supervisor starts it at step 5; a failure degrades to rules-only.

type classifierHostController struct {
	cfg     Config
	log     *slog.Logger
	client  *classifierlink.Client
	onReady func(*classifierlink.Client)
}

func (c *classifierHostController) Start(ctx context.Context) error {
	addr, err := classifierAddress(c.cfg.ClassifierAddress)
	if err != nil {
		c.log.Warn("no classifier host configured; classification degrades to rules-only", "reason", err)
		return nil
	}
	client, err := classifierlink.New(classifierlink.Address{Network: addr.Network, Path: addr.Path}, "capture-core/"+version, c.cfg.ClassifierBudget)
	if err != nil {
		return err
	}
	c.client = client
	if c.onReady != nil {
		c.onReady(client)
	}
	if err := client.Connect(ctx); err != nil {
		// Not fatal: §3.4 says a version mismatch or an unreachable host is `degraded` with
		// confidence: degraded, never a failed submission.
		c.log.Warn("classifier host unavailable; classification degrades to rules-only", "error", err)
		return nil
	}
	c.log.Info("classifier host connected", "version", client.ClassifierVersion())
	return nil
}

func (c *classifierHostController) Stop(ctx context.Context) error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// classifierStatus is what the health snapshot reports about the link.
func (c *classifierHostController) status() (connected bool, version string, detail protocol.Detail) {
	if c.client == nil {
		return false, "rules-only", protocol.DetailClassifierUnavailable
	}
	degraded, reason := c.client.Degraded()
	if degraded {
		return false, c.client.ClassifierVersion(), reason
	}
	return true, c.client.ClassifierVersion(), protocol.DetailNone
}

// ---------------------------------------------------------------------------------------------
// Small adapters.

// slogLogger adapts slog to core.Logger (Printf), which the providers use for their own messages.
type slogLogger struct{ log *slog.Logger }

func (s slogLogger) Printf(format string, args ...any) { s.log.Info(fmt.Sprintf(format, args...)) }

// defaultDecision is what the binary supplies where a policy decision is required on the envelope.
// The rules engine lives server-side and in the classifier release; until a device-side rules
// bundle is applied, the honest default is `logged, decided locally`, never a fabricated block.
func defaultDecision(tool string) *protocol.Decision {
	return &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
}

func canaryHost(s string) string {
	host, _, err := splitHostPortLoose(s)
	if err != nil {
		return ""
	}
	return host
}

func canaryPort(s string) int {
	_, port, err := splitHostPortLoose(s)
	if err != nil {
		return 0
	}
	return port
}

func splitHostPortLoose(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, errNoClassifierAddress
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0, fmt.Errorf("missing port in %q", s)
	}
	port := 0
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil {
		return "", 0, err
	}
	return strings.Trim(s[:i], "[]"), port, nil
}

func portsFrom(b *policy.Bundle) []policy.LoopbackPort {
	if b == nil {
		return nil
	}
	return b.Loopback.Ports
}

func bodyCapFrom(b *policy.Bundle) int64 {
	if b == nil || b.Interception.BodyCapBytes <= 0 {
		return 4 << 20
	}
	return b.Interception.BodyCapBytes
}

// deviceCAPEM resolves the per-device CA pair: --ca-cert/--ca-key when set, otherwise the
// bundle's public root. A key with no certificate is an error (proxy.tls refuses a partial pair
// too); no certificate and no key means "mint an ephemeral CA", which is the pre-existing path.
func deviceCAPEM(cfg Config, b *policy.Bundle) (certPEM, keyPEM []byte, err error) {
	if cfg.CACertFile != "" {
		certPEM, err = os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading --ca-cert: %w", err)
		}
	} else if b != nil && strings.TrimSpace(b.Interception.RootCAPEM) != "" {
		certPEM = []byte(b.Interception.RootCAPEM)
	}
	if cfg.CAKeyFile != "" {
		keyPEM, err = os.ReadFile(cfg.CAKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading --ca-key: %w", err)
		}
	}
	if len(keyPEM) > 0 && len(certPEM) == 0 {
		return nil, nil, errors.New("--ca-key is set but no CA certificate is available; set --ca-cert or use a bundle carrying interception.root_ca_pem")
	}
	return certPEM, keyPEM, nil
}

// tlsListen is the flag when it was set away from the default; otherwise the bundle's
// interception.proxy_listen, so the signed bundle can pin the port the enrolment profile routes
// CLI clients to.
func tlsListen(cfg Config, b *policy.Bundle) string {
	if cfg.TLSListen != defaultTLSListen {
		return cfg.TLSListen
	}
	if b != nil && strings.TrimSpace(b.Interception.ProxyListen) != "" {
		return b.Interception.ProxyListen
	}
	return cfg.TLSListen
}

// tlsCanary is the flag when set, otherwise the bundle's interception.proxy_canary. Empty means
// the probe cannot run and proxy.tls reports degraded rather than healthy (§5.6).
func tlsCanary(cfg Config, b *policy.Bundle) string {
	if strings.TrimSpace(cfg.TLSCanary) != "" {
		return cfg.TLSCanary
	}
	if b != nil {
		return b.Interception.ProxyCanary
	}
	return ""
}

// trustStoreName maps the configuration vocabulary onto the trust package's Windows store name.
func trustStoreName(s string) string {
	if s == "enterprise" {
		return "enterprise"
	}
	return "Root"
}

// shimProxyAddr is the address cli.shim exports: the bundle's cli_shim.proxy_addr, else the
// bundle's interception.proxy_listen, else the flag when it was set. Empty means the address is
// not known yet, which the shim reports as an incomplete environment rather than guessing.
func shimProxyAddr(cfg Config, b *policy.Bundle) string {
	if b != nil && strings.TrimSpace(b.CLIShim.ProxyAddr) != "" {
		return b.CLIShim.ProxyAddr
	}
	if b != nil && strings.TrimSpace(b.Interception.ProxyListen) != "" {
		return b.Interception.ProxyListen
	}
	if cfg.TLSListen != defaultTLSListen {
		return cfg.TLSListen
	}
	return ""
}

func newEventID() string {
	var b [16]byte
	if _, err := readRand(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// bytesReader is the ContentReader for bytes that arrived over a channel (native messaging, a
// test). The pipeline is handed a reader, never the bytes: a pipeline given bytes has already read
// them, and §11.2's ordering cannot be enforced after the fact.
type bytesReader struct{ body []byte }

func (r bytesReader) Read(context.Context) ([]byte, error) { return r.body, nil }

// jsonReader is used by the native host for a body that arrived as a JSON document.
func compactJSON(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// ensureDir makes a directory that must exist before a file is written beside it.
func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

var _ = io.Discard
