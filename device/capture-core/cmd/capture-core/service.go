package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/classifierlink"
	"github.com/shadow-ai-capture/device/capture-core/cli"
	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/loopback"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/capture-core/trust"
	"github.com/shadow-ai-capture/device/capture-core/winproxy"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// Operating parameters. They are product constants rather than configuration: the signed policy
// bundle carries everything a tenant tunes.
const (
	drainDeadline     = 30 * time.Second // bounded spool drain at shutdown
	drainInterval     = time.Second
	backoffBase       = time.Second
	backoffCap        = 5 * time.Minute
	classifierBudget  = 2 * time.Second
	startupBound      = 30 * time.Second // enrolment and the first policy fetch at startup
	defaultBodyCap    = 4 << 20
	defaultTLSListen  = "127.0.0.1:0"
	spoolMaxBytes     = 25 << 20
	spoolMaxEntries   = 25000
	serviceStopBudget = drainDeadline + 15*time.Second
)

// facilities are the operating-system side effects the service performs beyond its own state
// directory: the trust store, the machine environment the CLI shim writes, the native messaging
// endpoint, the users' proxy settings the desktop-app PAC writes, and the process lookups that
// attribute a proxied connection. Tests replace them so nothing touches the machine.
type facilities struct {
	trustStore  func(logf func(string, ...any)) trustStore
	shimRunner  cli.Runner
	shimDir     string // empty: the platform default (cli.DefaultManagedDir)
	shimProfile string // empty: the platform default profile
	nativeAddr  string
	// connOwner names the process at the client end of a loopback connection the proxy accepted.
	// nil where the platform cannot; proxy observations are then attributed to the console user.
	connOwner func(conn net.Conn) (hostinfo.Process, error)
	// desktopPAC builds the desktop-app PAC over the platform's user settings. nil where the
	// platform has none; desktop apps there keep their own proxy behaviour.
	desktopPAC func(cfg winproxy.Config) *winproxy.Server
}

// trustStore installs, verifies and removes the per-device CA in the platform trust store.
type trustStore interface {
	Install(ctx context.Context, certDER []byte) error
	Verify(ctx context.Context, certDER []byte) (bool, error)
	Remove(ctx context.Context) error
}

var platform = facilities{
	trustStore: func(logf func(string, ...any)) trustStore {
		return trust.New(trust.Config{OS: trust.HostOS(), Logf: logf})
	},
	shimRunner: trust.ExecRunner{},
	nativeAddr: nativeEndpoint,
	connOwner:  loopbackAttribution(),
	desktopPAC: desktopPAC(),
}

// desktopPAC is the desktop-app PAC on Windows, where desktop apps read the per-user Internet
// Settings; nothing elsewhere.
func desktopPAC() func(winproxy.Config) *winproxy.Server {
	if runtime.GOOS != "windows" {
		return nil
	}
	return winproxy.New
}

// loopbackAttribution is how the platform names a loopback connection's client: the TCP owner table
// on Windows, nothing elsewhere.
func loopbackAttribution() func(net.Conn) (hostinfo.Process, error) {
	if runtime.GOOS != "windows" {
		return nil
	}
	return loopbackClient
}

// loopbackClient is the process that dialled a loopback connection the service accepted.
func loopbackClient(conn net.Conn) (hostinfo.Process, error) {
	local, lok := conn.LocalAddr().(*net.TCPAddr)
	remote, rok := conn.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok {
		return hostinfo.Process{}, fmt.Errorf("a %s connection has no TCP owner", conn.LocalAddr().Network())
	}
	pid, err := hostinfo.OwnerOfLocalTCP(local.AddrPort(), remote.AddrPort())
	if err != nil {
		return hostinfo.Process{}, err
	}
	return hostinfo.ProcessInfo(pid)
}

// runService runs the agent in the foreground until SIGINT or SIGTERM.
func runService(cfg Config, log *slog.Logger) error {
	ctx, cancel := signalContext()
	defer cancel()
	return runServiceContext(ctx, cfg, log, nil)
}

// runServiceContext builds and runs the agent until ctx ends, then shuts it down. onStarted, when
// non-nil, runs once the agent is up: the Windows service reports RUNNING only then.
func runServiceContext(ctx context.Context, cfg Config, log *slog.Logger, onStarted func()) error {
	svc, err := newService(ctx, cfg, log)
	if err != nil {
		return err
	}
	if err := svc.Start(ctx); err != nil {
		return err
	}
	if onStarted != nil {
		onStarted()
	}
	log.Info("capture-core running", "version", version)
	<-ctx.Done()
	log.Info("stopping")
	stopCtx, cancel := context.WithTimeout(context.Background(), serviceStopBudget)
	defer cancel()
	return svc.Stop(stopCtx)
}

// service is the wired agent: one graph, built once, started and stopped by the supervisor in its
// declared order. Policy resolution, verification and the mode gate live in the packages it uses.
type service struct {
	cfg  Config
	log  *slog.Logger
	logf core.Logger
	dir  state.Dir

	reg    *core.Registry
	sup    *core.Supervisor
	store  *policy.Store
	result policy.Result

	spool   *spoolHolder
	pipe    *core.Pipeline
	content *contentstore.Store
	host    *classifierHostController
	broker  *loopback.Broker
	drainer *drain.Drainer
	trust   trustStore
	health  *healthChannel
	native  *nativeServer

	// tlsProv is proxy.tls, and pac the Windows desktop-app PAC that points at it (nil where the
	// platform has none). Both, and the CLI shim, are policy toggles that run only while the
	// tenant's TLS inspection is on. The PAC fails open by returning the previous route when the
	// proxy drops out.
	tlsProv *tlsproxy.Provider
	pac     *winproxy.Server

	policyMu   sync.Mutex
	policySync *policySync
	policyNext time.Duration

	// people is the console user observations are attributed to.
	people  *people
	managed protocol.ManagedState

	// idMu guards the issued identity and the device identity setting, which enrolment, the
	// health response and the console-user watcher each update on their own goroutine.
	idMu         sync.Mutex
	issued       *credential.Credential
	identityMode protocol.DeviceIdentity

	startupDeadline time.Time

	bgStop chan struct{}
	bgWG   sync.WaitGroup
}

// newService builds the graph without starting any provider. Every constructor can fail, and a
// failure means nothing is running.
func newService(ctx context.Context, cfg Config, log *slog.Logger) (*service, error) {
	dir, err := state.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	logf := slogLogger{log}
	s := &service{
		cfg: cfg, log: log, logf: logf, dir: dir,
		reg:          core.NewRegistry(time.Now, logf),
		people:       newPeople(userSources()),
		identityMode: protocol.DeviceIdentity(cfg.DeviceIdentity),
		bgStop:       make(chan struct{}),
	}

	spoolKey, err := dir.Key(state.SpoolKeyFile)
	if err != nil {
		return nil, err
	}
	contentKey, err := dir.Key(state.ContentKeyFile)
	if err != nil {
		return nil, err
	}

	// Policy first: it decides what the providers may do and is in force before any starts.
	if cfg.PolicyKey != "" {
		if s.store, s.result, err = openFetchedPolicy(cfg, dir, log); err != nil {
			return nil, err
		}
	} else {
		log.Warn("no policy key configured: the device runs at M0 (metadata only) and fetches no policy")
		s.result = policy.Result{Outcome: policy.OutcomeFellToM0}
	}

	// The spool is opened by the supervisor; the pipeline writes through a sink that refuses until
	// then, so a provider that starts early fails loudly instead of dropping silently.
	s.spool = &spoolHolder{dir: dir.Path(state.SpoolDir), key: spoolKey, log: log}
	pipe, err := core.NewPipeline(&lazySink{spool: s.spool}, time.Now, nil)
	if err != nil {
		return nil, err
	}
	pipe.Bundles = s.currentBundle
	pipe.ClassifyBudget = classifierBudget
	pipe.Log = log
	if s.content, err = contentstore.Open(dir.Path(state.ContentDir), contentKey, time.Now); err != nil {
		return nil, err
	}
	pipe.Content = s.content
	s.pipe = pipe

	if err := s.buildDrainer(spoolKey); err != nil {
		return nil, err
	}

	// A first start has no bundle yet, and the providers take their listen address, shim settings
	// and body cap from it when they are built. Enrolling and fetching now, within the startup
	// bound, builds them under the tenant's policy instead of under M0 until the next restart.
	if s.policySync != nil && s.currentBundle() == nil {
		bounded, cancel := s.startupBound(ctx)
		if s.drainer.EnsureEnrolled(bounded) {
			s.policyNext = s.policySync.once(bounded)
		} else {
			log.Warn("not enrolled yet; the device starts at M0 and fetches its policy once enrolled")
		}
		cancel()
	}

	if cfg.ClassifierRelease != "" {
		s.host = &classifierHostController{cfg: cfg, log: log, onReady: func(c *classifierlink.Client) { pipe.Classifier = c }}
	}

	if err := s.buildProviders(); err != nil {
		return nil, err
	}

	sup, err := core.NewSupervisor(s.reg, logf, time.Now)
	if err != nil {
		return nil, err
	}
	sup.Policy = &policyLoader{svc: s}
	sup.Spool = s.spool
	sup.Identity = identityResolver{s}
	if s.host != nil {
		sup.ClassifierHost = s.host
	}
	if s.broker != nil {
		sup.Loopback = s.broker
	}
	// The per-device root is removed at every stop and installed again at start, so an uninstall
	// (which stops the service) never leaves a trusted root behind.
	sup.TrustRoot = s.trust
	sup.RemoveTrustRoot = true
	sup.DrainDeadline = drainDeadline
	s.sup = sup

	s.health = newHealthChannel(s)
	s.native = newNativeServer(s, platform.nativeAddr)
	return s, nil
}

// buildDrainer builds the device-to-cloud drain over the credential sealed under the spool key.
func (s *service) buildDrainer(spoolKey []byte) error {
	creds, err := credential.Open(s.dir.Path(state.CredentialFile), spoolKey)
	if err != nil {
		return err
	}
	// What the operating system says about the device: the hardware seed for the idempotency key,
	// whether an Intune enrolment makes it managed, and the attestation re-read at each enrolment.
	facts := collectHostFacts()
	for _, n := range facts.Notes {
		s.log.Info("attestation: " + n)
	}
	s.managed = protocol.ManagedStateUnknown
	if facts.Managed() {
		s.managed = protocol.ManagedStateManaged
	}
	seed := facts.HardwareSeed()
	if seed == "" {
		if seed, err = installSeed(s.dir); err != nil {
			return err
		}
	}
	d, err := drain.New(drain.Config{
		Endpoint:      s.cfg.DeviceEndpoint,
		DeploymentKey: s.cfg.DeploymentKey,
		CAFile:        s.cfg.CAFile,
		TenantID:      s.cfg.TenantID,
		HardwareSeed:  seed,
		Attestation:   func() *protocol.DeviceAttestation { return collectHostFacts().AttestationOrNil() },
		AgentVersion:  version,
		Hostname:      s.clearHostname(),
		HostnameHash:  s.hostnameHash(),
		ManagedState:  string(s.managed),
		BackoffBase:   backoffBase,
		BackoffCap:    backoffCap,
		DrainInterval: drainInterval,
		Expire:        s.spool.Expire,
		OnEnrolled:    s.adoptCredential,
		Content:       s.content,
	}, s.spool.store, creds, slogLogger{s.log}, time.Now)
	if err != nil {
		return err
	}
	s.drainer = d
	s.spool.drain = d.Drain
	if s.store != nil {
		s.policySync = newPolicySync(s.store, policyCache{dir: s.dir.Path(state.PolicyDir)}, d, s.policyFetched, s.log)
	}
	return nil
}

// buildProviders builds proxy.tls, the loopback broker and the CLI shim over the per-device CA, and
// the OTLP receiver.
func (s *service) buildProviders() error {
	b := s.currentBundle()
	label := s.resolvedHostname()
	if c := s.issuedCredential(); c != nil {
		label = c.DeviceID
	}
	caDir := s.dir.Path(state.DeviceCADir)
	caCert, caKey, created, err := ensureDeviceCA(caDir, label, time.Now())
	if err != nil {
		return fmt.Errorf("per-device CA in %s: %w", caDir, err)
	}
	s.log.Info("per-device CA ready", "created", created)

	trustRoot := platform.trustStore(func(f string, a ...any) { s.log.Warn(fmt.Sprintf(f, a...)) })
	s.trust = trustRoot

	canary := ""
	listen := defaultTLSListen
	if b != nil {
		canary = b.Interception.ProxyCanary
		if strings.TrimSpace(b.Interception.ProxyListen) != "" {
			listen = b.Interception.ProxyListen
		}
	}
	tlsCfg := tlsproxy.Config{
		Listen:     listen,
		Bundles:    s.currentBundle,
		Pipeline:   s.pipe,
		Log:        s.logf,
		Clock:      time.Now,
		CanaryHost: canaryHost(canary),
		CanaryPort: canaryPort(canary),
		BodyCap:    bodyCapFrom(b),
		CACertPEM:  caCert,
		CAKeyPEM:   caKey,
		TrustRoot:  trustRoot,
	}
	if owner := platform.connOwner; owner != nil {
		tlsCfg.Process = func(conn net.Conn) string { return clientProcessName(owner, conn) }
		tlsCfg.Person = func(conn net.Conn) (core.Person, error) { return s.clientPerson(owner, conn) }
	}
	tlsProv := tlsproxy.New(tlsCfg)
	if err := s.reg.Add(tlsProv); err != nil {
		return err
	}
	s.tlsProv = tlsProv

	s.broker = loopback.New(loopback.Config{
		Ports:    portsFrom(b),
		Pipeline: s.pipe,
		Log:      s.logf,
		Clock:    time.Now,
		BodyCap:  bodyCapFrom(b),
	})
	if err := s.reg.Add(s.broker); err != nil {
		return err
	}

	shim := cli.Config{
		ManagedDir:  platform.shimDir,
		ProfilePath: platform.shimProfile,
		ProxyAddr:   shimProxyAddr(b),
		RootCAPEM:   caCert,
		Runner:      platform.shimRunner,
		Log:         s.logf,
		Clock:       time.Now,
	}
	if b != nil {
		shim.NoProxy = b.CLIShim.NoProxy
		shim.Runtimes = b.CLIShim.Runtimes
		shim.NodeRequire = b.CLIShim.NodeRequire
	}
	if err := s.reg.Add(cli.New(shim)); err != nil {
		return err
	}

<<<<<<< HEAD
	return s.buildPAC()
=======
	// The listen addresses arrive with the bundle that switches the receiver on.
	otel, err := otlp.New(otlp.Config{TokenPath: s.dir.Path(otlp.TokenFile), Log: s.logf, Clock: time.Now})
	if err != nil {
		return err
	}
	if err := s.reg.Add(otel); err != nil {
		return err
	}

	s.buildPAC(b)
	return nil
>>>>>>> refs/heads/refactor-endpoint/23-otlp-receiver
}

// buildPAC registers the desktop-app PAC where the platform has one. It reads its listen address
// from the bundle in force each time it starts.
func (s *service) buildPAC() error {
	if platform.desktopPAC == nil {
		return nil
	}
	s.pac = platform.desktopPAC(winproxy.Config{
		Bundles:   s.currentBundle,
		ProxyAddr: func() string { return s.tlsProv.ListenAddr() },
		Log:       s.logf,
	})
	return s.reg.Add(s.pac)
}

func (s *service) currentBundle() *policy.Bundle {
	if s.store == nil {
		return nil
	}
	return s.store.InForce()
}

// Start runs the supervisor's startup sequence, then the background loops and the native
// messaging endpoint. A startup failure leaves nothing started.
func (s *service) Start(ctx context.Context) error {
	if err := s.sup.Startup(ctx); err != nil {
		s.log.Error("startup refused; stopping anything already started", "error", err)
		_ = s.sup.Shutdown(context.Background())
		return err
	}
	if err := s.drainer.Start(ctx); err != nil {
		s.log.Warn("drainer not started; observations stay spooled", "error", err)
	}
	if s.policySync != nil {
		first := s.policyNext
		if first <= 0 {
			first = policyRetryFloor
		}
		s.policySync.Start(ctx, first)
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.watchPeople(ctx, s.bgStop)
	}()
	if err := s.native.Start(); err != nil {
		// The browser relay is one collection path; the others keep running without it.
		s.log.Error("native messaging endpoint unavailable; the browser extension cannot reach the agent", "endpoint", s.native.addr, "error", err)
	}
	s.health.Start(ctx)
	s.log.Info("capture-core started", "order", s.sup.Order())
	return nil
}

// Stop ends the background loops, runs the shutdown sequence and releases the spool.
func (s *service) Stop(ctx context.Context) error {
	s.native.Stop()
	s.health.Stop()
	select {
	case <-s.bgStop:
	default:
		close(s.bgStop)
	}
	s.bgWG.Wait()
	if s.policySync != nil {
		s.policySync.Stop()
	}
	// Restore every user's previous proxy settings before the proxy stops enforcing, so no desktop
	// app is left pointing at a PAC whose proxy is about to go away. No bundle arrives after the
	// policy loop has stopped, so nothing starts it again.
	if s.pac != nil {
		s.reg.StopCollector(ctx, protocol.CollectorDesktopProxy)
	}
	// The background loop ends before the bounded shutdown drain, so that drain is the only
	// thing sending.
	_ = s.drainer.Stop(ctx)
	err := s.sup.Shutdown(ctx)
	closeErr := s.spool.Close()
	s.log.Info("capture-core stopped", "order", s.sup.Order())
	if err != nil {
		return err
	}
	return closeErr
}

// identityResolver installs the envelope identity before any provider starts: the stored
// credential's, or one from a bounded enrolment. Until then the pipeline refuses to mint, and the
// background drain keeps trying to enrol.
type identityResolver struct{ s *service }

func (r identityResolver) Resolve(ctx context.Context) error {
	s := r.s
	s.people.refresh()
	bounded, cancel := s.startupBound(ctx)
	defer cancel()
	if !s.drainer.EnsureEnrolled(bounded) {
		return errors.New("the device is not enrolled yet")
	}
	s.publishIdentity()
	if s.policySync != nil && s.policyNext <= 0 {
		s.policyNext = s.policySync.once(bounded)
	}
	return nil
}

// startupBound bounds enrolment and the first policy fetch at startup by one deadline, so an
// first start without a network waits once rather than once per step.
func (s *service) startupBound(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.startupDeadline.IsZero() {
		s.startupDeadline = time.Now().Add(startupBound)
	}
	return context.WithDeadline(ctx, s.startupDeadline)
}

// adoptCredential records an issued credential: its identity is stamped on every envelope from
// now on, its device identity setting gates the clear names, and its user-reference key derives
// each person's user_ref.
func (s *service) adoptCredential(c *credential.Credential) {
	s.adoptDeviceIdentity(c.DeviceIdentity)
	if _, err := s.people.setKey(c.UserRefKey); err != nil {
		s.log.Warn("the issued user_ref_key is unusable; observations stay unattributed", "error", err)
	}
	s.idMu.Lock()
	s.issued = c
	s.idMu.Unlock()
	s.publishIdentity()
}

func (s *service) issuedCredential() *credential.Credential {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.issued
}

// openFetchedPolicy builds the store a fetched bundle is verified into and puts the cached bundle
// in force when one verifies, so a restart enforces it before the network answers.
func openFetchedPolicy(cfg Config, dir state.Dir, log *slog.Logger) (*policy.Store, policy.Result, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(cfg.PolicyKey))
	if err != nil {
		return nil, policy.Result{}, fmt.Errorf("decoding --policy-key: %w", err)
	}
	verifier, err := policy.NewVerifier(cfg.PolicyKeyID, pub)
	if err != nil {
		return nil, policy.Result{}, err
	}
	store, err := policy.NewStore(verifier)
	if err != nil {
		return nil, policy.Result{}, err
	}
	cache := policyCache{dir: dir.Path(state.PolicyDir)}
	raw, _, err := cache.load()
	if err != nil {
		log.Info("no policy bundle cached yet; M0 (metadata only) until the first verified fetch")
		return store, policy.Result{Outcome: policy.OutcomeFellToM0}, nil
	}
	res := store.Apply(raw)
	if res.Err != nil {
		log.Warn("the cached policy bundle does not verify; M0 until the next verified fetch", "cause", res.Cause, "error", res.Err)
	}
	return store, res, nil
}

func (s *service) setPolicyResult(res policy.Result) {
	s.policyMu.Lock()
	s.result = res
	s.policyMu.Unlock()
}

func (s *service) policyResult() policy.Result {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	return s.result
}

// policyFetched records a fetch result and applies a newly accepted bundle to the providers as a
// diff, never a restart.
func (s *service) policyFetched(res policy.Result) {
	s.setPolicyResult(res)
	if res.Err != nil || res.Outcome != policy.OutcomeAccepted {
		return
	}
	if b := s.currentBundle(); b != nil {
		s.applyBundle(*b)
	}
}

func (s *service) applyBundle(b policy.Bundle) {
	for _, applied := range s.reg.ApplyPolicy(b) {
		if applied.Err != nil {
			s.log.Warn("provider could not apply the bundle", "provider", applied.Collector, "error", applied.Err)
		}
	}
}

// policyLoader is the supervisor's first step: the bundle in force (the verified cache) is applied
// to the providers before any of them starts.
type policyLoader struct{ svc *service }

func (p *policyLoader) Load(context.Context) error {
	if b := p.svc.currentBundle(); b != nil {
		p.svc.applyBundle(*b)
	}
	return nil
}

// spoolHolder opens the spool when the supervisor asks and stays its single owner.
type spoolHolder struct {
	dir string
	key []byte
	log *slog.Logger

	mu    sync.Mutex
	sp    *capturespool.Spool
	drain func(ctx context.Context, deadline time.Time) (drain.Result, error)
}

func (h *spoolHolder) Open(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp != nil {
		return nil
	}
	// An unreadable segment is quarantined and reported as data loss with a count, rather than
	// stopping collection or starting clean silently.
	sp, err := capturespool.Open(capturespool.Config{
		Dir:       h.dir,
		Key:       h.key,
		Bounds:    capturespool.Bounds{MaxBytes: spoolMaxBytes, MaxEntries: spoolMaxEntries},
		OnCorrupt: capturespool.CorruptQuarantine,
	})
	if err != nil {
		return err
	}
	if rec := sp.Extended().Recovery; rec.TornBytes > 0 || rec.InFlightResetToPending > 0 || len(rec.Quarantined) > 0 {
		h.log.Warn("spool recovery reported loss", "torn_bytes", rec.TornBytes, "in_flight_reset", rec.InFlightResetToPending, "quarantined", rec.Quarantined)
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

// store returns the open spool for the drainer.
func (h *spoolHolder) store() (protocol.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return nil, errors.New("spool is not open")
	}
	return h.sp, nil
}

// Expire drops records past their retention deadline (counted, never silent).
func (h *spoolHolder) Expire(now time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sp == nil {
		return 0, errors.New("spool is not open")
	}
	return h.sp.Expire(now)
}

// Drain is the bounded shutdown drain through the device-to-cloud path.
func (h *spoolHolder) Drain(ctx context.Context, deadline time.Time) (core.DrainResult, error) {
	h.mu.Lock()
	d := h.drain
	h.mu.Unlock()
	if d == nil {
		return core.DrainResult{}, nil
	}
	res, err := d(ctx, deadline)
	return core.DrainResult{Delivered: res.Delivered, Dropped: res.Rejected, Err: err}, err
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

// lazySink is the pipeline's view of the spool. It refuses writes before Open, which makes "the
// spool opens before any provider starts" observable rather than conventional.
type lazySink struct{ spool *spoolHolder }

func (l *lazySink) Append(e protocol.Entry) (protocol.Entry, error) {
	l.spool.mu.Lock()
	sp := l.spool.sp
	l.spool.mu.Unlock()
	if sp == nil {
		return protocol.Entry{}, errors.New("spool is not open")
	}
	return sp.Append(e)
}

func (l *lazySink) Stats() protocol.SpoolStats { return l.spool.Stats() }

// classifierHostController runs the classifier host beside this binary as a child on stdio. An
// unavailable host degrades classification to rules-only and never fails a submission.
type classifierHostController struct {
	cfg     Config
	log     *slog.Logger
	client  *classifierlink.Client
	onReady func(*classifierlink.Client)
}

func (c *classifierHostController) Start(ctx context.Context) error {
	exe := classifierHostExe()
	c.client = classifierlink.New(exe, []string{
		"serve", "--release", c.cfg.ClassifierRelease, "--pubkey", c.cfg.ClassifierPubkey, "--transport", "stdio",
	}, os.Stderr, "capture-core/"+version, classifierBudget)
	c.onReady(c.client)
	if err := c.client.Connect(ctx); err != nil {
		c.log.Warn("classifier host unavailable; classification degrades to rules-only", "error", err)
		return nil
	}
	c.log.Info("classifier host connected", "version", c.client.ClassifierVersion())
	return nil
}

func (c *classifierHostController) Stop(context.Context) error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// status is what the health snapshot reports about the classifier.
func (c *classifierHostController) status() (connected bool, version string, detail protocol.Detail) {
	if c == nil || c.client == nil {
		return false, core.RulesOnlyVersion, protocol.DetailClassifierUnavailable
	}
	if degraded, reason := c.client.Degraded(); degraded {
		return false, c.client.ClassifierVersion(), reason
	}
	return true, c.client.ClassifierVersion(), protocol.DetailNone
}

// classifierHostExe is the classifier host the installer lays down beside this binary.
func classifierHostExe() string {
	name := "classifier-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(self), name)
}

// installSeed is the hardware seed of last resort, for a machine that states no hardware
// identity at all: a random id kept in the state directory, stable for the life of the install.
func installSeed(dir state.Dir) (string, error) {
	path := dir.Path("install-id")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return "install:" + strings.TrimSpace(string(b)), nil
	}
	key, err := state.ReadOrCreateKey(dir.Path("install.key"))
	if err != nil {
		return "", err
	}
	id := hex.EncodeToString(key[:16])
	if err := state.WriteFile(path, []byte(id)); err != nil {
		return "", err
	}
	return "install:" + id, nil
}

// slogLogger adapts slog to the Printf logger the providers use.
type slogLogger struct{ log *slog.Logger }

func (s slogLogger) Printf(format string, args ...any) { s.log.Info(fmt.Sprintf(format, args...)) }

func canaryHost(s string) string {
	host, _, ok := splitHostPort(s)
	if !ok {
		return ""
	}
	return host
}

func canaryPort(s string) int {
	_, port, ok := splitHostPort(s)
	if !ok {
		return 0
	}
	return port
}

func splitHostPort(s string) (string, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, ":")
	if s == "" || i < 0 {
		return "", 0, false
	}
	port := 0
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

func portsFrom(b *policy.Bundle) []policy.LoopbackPort {
	if b == nil {
		return nil
	}
	return b.Loopback.Ports
}

func bodyCapFrom(b *policy.Bundle) int64 {
	if b == nil || b.Interception.BodyCapBytes <= 0 {
		return defaultBodyCap
	}
	return b.Interception.BodyCapBytes
}

// shimProxyAddr is the address the CLI shim exports: the bundle's cli_shim.proxy_addr, else its
// interception.proxy_listen. Empty means the address is not known yet, which the shim reports as
// an incomplete environment rather than guessing.
func shimProxyAddr(b *policy.Bundle) string {
	if b == nil {
		return ""
	}
	if strings.TrimSpace(b.CLIShim.ProxyAddr) != "" {
		return b.CLIShim.ProxyAddr
	}
	return b.Interception.ProxyListen
}
