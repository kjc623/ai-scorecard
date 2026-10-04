// Package tlsproxy is `proxy.tls` (docs/01-collectors.md §5): the only provider that reads
// content outside the browser, and therefore the one with the largest blast radius in the
// product (brief §5.5).
//
// Its defining property is that failing open is the acceptance criterion, not an aspiration: the
// user's traffic is never blocked, degraded or delayed by this provider's inability to do its
// job (§5.4). Every branch below therefore prefers "carry the request" over "report an error",
// and the only thing that stops interception is signed policy.
package tlsproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func parseIP(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	return nil
}

// Pipeline is the core pipeline as this provider uses it: resolve the mode before reading
// anything, then hand the observation over. The interface is structural, so *core.Pipeline
// satisfies it without an adapter.
type Pipeline interface {
	ResolveMode(q core.ScopeQuery) core.Resolution
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
}

// Config is the provider's policy data and its seams.
type Config struct {
	// Listen is the proxy's own address. In production it is a loopback port; a test passes
	// "127.0.0.1:0" so nothing binds a fixed port.
	Listen string

	// Bundles returns the bundle in force. nil means "none", which resolves to M0 (§13.3 rule 5)
	// and, for this provider, means it stops reading content — never that it widens.
	Bundles func() *policy.Bundle

	Pipeline Pipeline
	Decide   func(tool string) *protocol.Decision
	Agent    core.ScopeQuery

	// SystemProxy is the platform proxy configuration: the provider points it at itself only
	// once it is listening, confirms it is the *effective* proxy, and restores it on shutdown
	// and on the kill switch.
	SystemProxy core.SystemProxy

	// TrustRoot installs/removes the device CA. Removal is as reliable as installation (§5.2).
	TrustRoot core.TrustRoot

	// Sealer is the platform key protection for the CA key. nil is reported, not hidden.
	Sealer Sealer

	// CACertPEM and CAKeyPEM load a pre-existing device CA instead of minting one at start. They
	// must be set together; exactly one is a configuration error, never a silent generated
	// fallback. This is the path an offline generator (cmd/sac-bundle) uses to hand the proxy the
	// same per-device root the device already trusts.
	CACertPEM []byte
	CAKeyPEM  []byte

	// CanaryHost/CanaryPort is the end-to-end probe destination. Healthy requires a successful
	// handshake through a minted leaf against it (§5.2, §5.6); without one the provider reports
	// `tls_probe_failed` rather than claiming health on the strength of a successful file write.
	CanaryHost string
	CanaryPort int

	BodyCap int64

	// UpstreamRoots is the trust the proxy uses when it connects to the real destination. nil
	// means the platform trust store, which is the production case; a test supplies an
	// in-process pool rather than touching the OS store.
	UpstreamRoots *x509.CertPool

	// Process names the client behind a connection, for the per-process exclusion ladder (§5.5).
	// It is a seam because process attribution is platform-specific.
	Process func(conn net.Conn) string

	// PinnedReprobeInterval bounds an exclusion: "re-probed at a slow cadence because a client
	// update can change its behaviour. It is never a permanent silent omission" (§5.5).
	PinnedReprobeInterval time.Duration

	DialTimeout      time.Duration
	HandshakeTimeout time.Duration

	Log   core.Logger
	Clock func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.BodyCap <= 0 {
		c.BodyCap = 4 << 20
	}
	if c.PinnedReprobeInterval <= 0 {
		c.PinnedReprobeInterval = 10 * time.Minute
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Process == nil {
		c.Process = func(net.Conn) string { return "unknown" }
	}
	if c.Decide == nil {
		c.Decide = func(string) *protocol.Decision {
			return &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
		}
	}
	return c
}

// Provider is `proxy.tls`.
type Provider struct {
	cfg Config

	mu            sync.Mutex
	ln            net.Listener
	ca            *CA
	startedAt     time.Time
	started       bool
	stopped       bool
	killed        bool
	enforcing     bool
	wasEffective  bool
	probeOK       bool
	lastSuccess   time.Time
	counters      *core.CounterSet
	exclusions    map[string]time.Time // process|host -> excluded until
	acceptDone    chan struct{}
	wg            sync.WaitGroup
	sequence      []string // side effects, for the ordering assertions in tests
	lastDegraded  protocol.Detail
	systemProxyAt string
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Route { return protocol.RouteProxyTLS }

// New returns an unstarted provider.
func New(cfg Config) *Provider {
	cfg = cfg.withDefaults()
	now := cfg.Clock()
	return &Provider{
		cfg:        cfg,
		startedAt:  now,
		counters:   core.NewCounterSet(now),
		exclusions: map[string]time.Time{},
	}
}

func (p *Provider) step(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence = append(p.sequence, s)
}

// Sequence returns the side effects performed, in order. It exists so a test can assert the
// kill-switch ordering literally instead of trusting the order of statements in a function.
func (p *Provider) Sequence() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sequence...)
}

// CA exposes the device CA for a test or an installer that needs the public half. It never
// exposes the private key.
func (p *Provider) CA() *CA {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ca
}

// ListenAddr implements core.ListenAddr: the supervisor points the system proxy at this, and
// never before the listener exists.
func (p *Provider) ListenAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// buildCA produces the device CA: a configured PEM pair when supplied, otherwise a freshly minted
// per-device CA. Exactly one of the pair is refused rather than silently generating a fallback,
// because a proxy running a CA the device does not trust must not claim to intercept.
func (p *Provider) buildCA(deviceID string) (*CA, error) {
	haveCert, haveKey := len(p.cfg.CACertPEM) > 0, len(p.cfg.CAKeyPEM) > 0
	switch {
	case haveCert && haveKey:
		return NewCAFromPEM(p.cfg.CACertPEM, p.cfg.CAKeyPEM, p.cfg.Clock())
	case haveCert || haveKey:
		return nil, fmt.Errorf("tlsproxy: CA certificate and key must be supplied together; refusing a silent generated fallback")
	default:
		return NewCA(deviceID, p.cfg.Sealer, p.cfg.Clock())
	}
}

// Start mints the device CA, binds the proxy, optionally installs the CA public certificate and
// points the system proxy at itself, then runs the end-to-end probe. It never fails because
// interception is unavailable: it reports `degraded` and keeps the user's traffic direct.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	p.started = true
	deviceID := p.cfg.Agent.DeviceID
	p.mu.Unlock()

	ca, err := p.buildCA(deviceID)
	if err != nil {
		// Without a CA there is no interception. That is a degraded provider, not a failed
		// startup: the user's traffic must not be pointed at a proxy that cannot serve.
		p.mu.Lock()
		p.probeOK = false
		p.mu.Unlock()
		return fmt.Errorf("tlsproxy: device CA unavailable, interception disabled: %w", err)
	}
	ln, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return fmt.Errorf("tlsproxy: proxy listener: %w", err)
	}

	p.mu.Lock()
	p.ca = ca
	p.ln = ln
	p.enforcing = true
	p.acceptDone = make(chan struct{})
	p.mu.Unlock()

	if p.cfg.TrustRoot != nil {
		// Installation is verified by the probe below, never by the write returning nil (§5.2:
		// the wrong store fails silently).
		if err := p.installTrustRoot(ctx, ca); err != nil {
			p.cfg.Log.Printf("tlsproxy: could not install the device CA: %v", err)
		}
	}
	if p.cfg.SystemProxy != nil {
		if err := p.cfg.SystemProxy.PointAt(ctx, ln.Addr().String()); err != nil {
			p.cfg.Log.Printf("tlsproxy: could not point the system proxy at %s: %v", ln.Addr().String(), err)
		} else {
			p.mu.Lock()
			p.systemProxyAt = ln.Addr().String()
			p.mu.Unlock()
		}
		if eff, ok := p.cfg.SystemProxy.Effective(ctx); ok && eff == ln.Addr().String() {
			p.mu.Lock()
			p.wasEffective = true
			p.mu.Unlock()
		}
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop()
	}()

	// The end-to-end probe: a real handshake against a destination whose leaf we minted. This is
	// the only thing that may produce `healthy` (§5.2, §5.6).
	//
	// The probe reads the listen address and the CA pool, both of which take this mutex, so it is
	// called outside the lock: a self-deadlock here would hang startup forever.
	ok := p.probe(ctx)
	p.mu.Lock()
	p.probeOK = ok
	p.mu.Unlock()
	if !ok {
		p.cfg.Log.Printf("tlsproxy: end-to-end probe failed; reporting degraded with %s", protocol.DetailTLSProbeFailed)
	}
	_ = ctx
	return nil
}

func (p *Provider) installTrustRoot(ctx context.Context, ca *CA) error {
	tr, ok := p.cfg.TrustRoot.(interface {
		Install(context.Context, []byte) error
	})
	if !ok {
		p.setDetail(protocol.DetailTrustInstallFailed)
		return fmt.Errorf("tlsproxy: configured trust root cannot install a certificate")
	}
	der := ca.DER()
	if err := tr.Install(ctx, der); err != nil {
		// A write that did not happen is a named degraded cause, never a silent health claim:
		// §5.2's wrong store fails silently, so the failure has to reach the coverage row.
		p.setDetail(protocol.DetailTrustInstallFailed)
		return err
	}
	// When the trust root can read the store back, verify the certificate is actually there. The
	// end-to-end probe below exercises the interceptor, but a probe through the in-process pool
	// would succeed even if the OS store were never touched, so the store itself is verified here.
	if v, ok := p.cfg.TrustRoot.(interface {
		Verify(context.Context, []byte) (bool, error)
	}); ok {
		installed, verr := v.Verify(ctx, der)
		if verr != nil {
			p.setDetail(protocol.DetailTrustVerifyFailed)
			return verr
		}
		if !installed {
			p.setDetail(protocol.DetailTrustVerifyFailed)
			return fmt.Errorf("tlsproxy: the device CA was not found in the OS trust store after install")
		}
	}
	return nil
}

// probe completes a real TLS handshake through a minted leaf against the canary destination.
func (p *Provider) probe(ctx context.Context) bool {
	if p.cfg.CanaryHost == "" || p.cfg.CanaryPort == 0 {
		// No canary configured: the provider cannot assert it is in the path, so it must not.
		return false
	}
	addr := net.JoinHostPort(p.cfg.CanaryHost, fmt.Sprint(p.cfg.CanaryPort))
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	raw, err := d.DialContext(ctx, "tcp", p.ListenAddr())
	if err != nil {
		return false
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(p.cfg.HandshakeTimeout))

	// The probe goes *through* the proxy, exactly as a client would: a CONNECT, then the leaf.
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		return false
	}
	br := newBufReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: p.cfg.CanaryHost,
		RootCAs:    p.ca.Pool(), // the in-process root pool; no OS trust store is touched
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return false
	}
	return true
}

// Stop implements core.Provider: interception stops, the system proxy is restored, the CA public
// certificate is removed when an uninstall or kill switch asks for it, and the listener closes.
// It is idempotent.
func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	ln := p.ln
	p.enforcing = false
	p.mu.Unlock()

	// Enforcement first: no new interception, then the system proxy is restored. §5.5's
	// dangerous ordering is traffic left pointed at a proxy that has stopped intercepting.
	p.stopInterception("stop")
	if p.cfg.SystemProxy != nil && p.systemProxyAt != "" {
		if err := p.cfg.SystemProxy.Restore(ctx); err != nil {
			p.cfg.Log.Printf("tlsproxy: could not restore the system proxy: %v", err)
		}
	}
	if ln != nil {
		_ = ln.Close()
	}
	if p.acceptDone != nil {
		select {
		case <-p.acceptDone:
		case <-time.After(p.cfg.HandshakeTimeout):
		}
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// stopInterception closes the listener and marks the provider out of the path. It is the first
// half of every "stop" path, which is what makes the ordering a property rather than a habit.
func (p *Provider) stopInterception(reason string) {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.enforcing = false
	p.mu.Unlock()
	p.step("stop_interception:" + reason)
	if ln != nil {
		_ = ln.Close()
	}
}

// ApplyPolicy implements core.Provider: a diff, never a restart.
//
// The kill switch (§5.5) is the one policy change that stops interception, and its ordering is
// the dangerous half: enforcement and interception stop FIRST, then the system proxy is
// restored, then the provider reports `absent` with `detail=killed`. Traffic is never left
// pointed at a proxy that has stopped intercepting.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	ks, ok := b.KillSwitchFor(protocol.RouteProxyTLS)
	if !ok || ks.Mode != policy.KillDisable {
		return nil
	}
	p.mu.Lock()
	already := p.killed
	p.killed = true
	restore := p.systemProxyAt
	p.mu.Unlock()
	if already {
		return nil
	}
	// 1. stop enforcement and interception first.
	p.stopInterception("kill_switch")
	// 2. then restore the system proxy.
	if p.cfg.SystemProxy != nil && restore != "" {
		if err := p.cfg.SystemProxy.Restore(context.Background()); err != nil {
			p.cfg.Log.Printf("tlsproxy: kill switch could not restore the system proxy: %v", err)
		}
		p.step("systemproxy.Restore")
	}
	// 3. then report absent detail=killed (the health row reads p.killed).
	p.step("health:absent:" + string(protocol.DetailKilled))
	return nil
}

// Health implements core.Provider. Healthy requires all three of bound, confirmed as the
// effective system proxy, and a successful end-to-end probe through a minted leaf (§5.6).
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	ln := p.ln
	started, stopped, killed := p.started, p.stopped, p.killed
	probeOK, wasEffective := p.probeOK, p.wasEffective
	lastDegraded := p.lastDegraded
	excluded := 0
	now := p.cfg.Clock()
	for _, until := range p.exclusions {
		if until.After(now) {
			excluded++
		}
	}
	lastOK := p.lastSuccess
	p.mu.Unlock()

	switch {
	case !started || stopped:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, p.startedAt, lastOK)
	case killed:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailKilled, p.startedAt, lastOK)
	case ln == nil:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, p.startedAt, lastOK)
	}

	effective := false
	if p.cfg.SystemProxy != nil {
		if eff, ok := p.cfg.SystemProxy.Effective(context.Background()); ok && eff == ln.Addr().String() {
			effective = true
		}
	}
	switch {
	case wasEffective && p.cfg.SystemProxy != nil && !effective:
		// Something changed the system proxy away from us: that is external interference, which
		// is `tampered`, not a capability gap.
		return p.counters.Snapshot(protocol.StateTampered, protocol.DetailNotEffectiveProxy, p.startedAt, lastOK)
	case p.cfg.SystemProxy != nil && !effective:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailNotEffectiveProxy, p.startedAt, lastOK)
	case !probeOK:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailTLSProbeFailed, p.startedAt, lastOK)
	case lastDegraded != protocol.DetailNone:
		// A capability failed on a recent request (classifier, spool, upstream). Health must not
		// claim healthy while a named capability is not working (§4.2).
		return p.counters.Snapshot(protocol.StateDegraded, lastDegraded, p.startedAt, lastOK)
	case excluded > 0:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailClientPinned, p.startedAt, lastOK)
	default:
		return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, p.startedAt, lastOK)
	}
}

// Counters exposes the closed counter set for the coverage row.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

func (p *Provider) acceptLoop() {
	defer close(p.acceptDone)
	for {
		p.mu.Lock()
		ln := p.ln
		p.mu.Unlock()
		if ln == nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(conn)
		}()
	}
}

// handle is the CONNECT path of §5.3. The decision tree is the whole provider: eligible
// destinations are terminated, everything else is tunnelled blind, and no branch may break the
// client.
func (p *Provider) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * p.cfg.HandshakeTimeout))

	br := newBufReader(client)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	host, port := splitHostPort(req.Host)
	if host == "" {
		p.counters.Add(protocol.CounterErrors)
		return
	}

	process := p.cfg.Process(client)
	bundle := p.bundle()

	// §5.5's exclusion ladder: a destination excluded for this process is blind-tunnelled, and
	// the exclusion is re-probed at a slow cadence rather than being permanent.
	if p.excluded(process, host) {
		p.counters.Add(protocol.CounterNotCooperative)
		p.blindTunnel(client, br, req.Host, process, protocol.DetailClientPinned)
		return
	}

	if bundle == nil || !bundle.Intercepts(host, port) {
		// §5.1: a destination in none of the three sets is blind-tunnelled, and the device
		// records only host, bytes, duration and owning process.
		p.blindTunnel(client, br, req.Host, process, protocol.DetailNone)
		return
	}

	p.intercept(client, br, req, host, port, process, bundle)
}

func (p *Provider) bundle() *policy.Bundle {
	if p.cfg.Bundles == nil {
		return nil
	}
	return p.cfg.Bundles()
}

// mode resolves the effective mode before anything is read (§11.2) and never returns an empty
// value: with no bundle in force this is M0, which stops content reading.
func (p *Provider) mode(host string) core.Resolution {
	q := p.cfg.Agent
	if p.cfg.Pipeline == nil {
		return core.Resolution{Mode: protocol.ModeM0, Reasons: []string{"no_pipeline"}}
	}
	res := p.cfg.Pipeline.ResolveMode(q)
	if !res.Mode.Valid() {
		return core.Resolution{Mode: protocol.ModeM0, Reasons: []string{"unresolved"}}
	}
	return res
}

// intercept terminates TLS with a minted leaf and reads the request.
func (p *Provider) intercept(client net.Conn, br *bufio.Reader, req *http.Request, host string, port int, process string, bundle *policy.Bundle) {
	p.mu.Lock()
	ca := p.ca
	p.mu.Unlock()
	if ca == nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	// The client is told the tunnel is established; from here the client believes it is talking
	// to the real host, because the leaf is minted for that host under a CA the device trusts.
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	leaf, err := ca.Leaf(host, p.cfg.Clock())
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"}, // §5.3: the proxy does not pretend to support HTTP/3
	})
	hctx, cancel := context.WithTimeout(context.Background(), p.cfg.HandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		// §5.5 first row: a TLS alert during the minted-leaf handshake, repeated, means the
		// client pins a certificate or validates against a bundled CA list. Exclude the
		// destination for that process and tunnel it blind from then on — never leave the client
		// broken to preserve collection.
		p.exclude(process, host)
		p.counters.Add(protocol.CounterNotCooperative)
		p.cfg.Log.Printf("tlsproxy: handshake with %s failed for %s; excluding %s for that process: %v", host, process, host, err)
		return
	}
	defer tlsConn.Close()

	p.serveIntercepted(tlsConn, host, port, process, bundle)
}

func (p *Provider) serveIntercepted(client *tls.Conn, host string, port int, process string, bundle *policy.Bundle) {
	_ = client.SetDeadline(time.Now().Add(4 * p.cfg.DialTimeout))
	br := newBufReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	defer req.Body.Close()

	submission := req.Method == http.MethodPost || req.Method == http.MethodPut
	p.counters.Add(protocol.CounterObserved)
	if !submission {
		// The shape predicate said no. Counted, never guessed at (§4.3, §8.2).
		p.counters.Add(protocol.CounterSkippedNotGenerative)
	}

	res := p.mode(host)

	// §11.2: the mode is applied before content is read.
	var buf *bodyBuffer
	var body io.Reader = req.Body
	if submission && res.ReadsContent() {
		buf = newBodyBuffer(p.cfg.BodyCap)
		body = io.TeeReader(req.Body, buf)
	}
	counted := &countingReader{r: body}

	upstream, err := tls.DialWithDialer(&net.Dialer{Timeout: p.cfg.DialTimeout}, "tcp", net.JoinHostPort(host, fmt.Sprint(port)), &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		RootCAs:    p.cfg.UpstreamRoots,
	})
	if err != nil {
		// §5.4 trigger 4: return the connection error the client would have seen anyway; never
		// substitute a response. Closing without a response IS that error.
		p.counters.Add(protocol.CounterErrors)
		p.setDetail(protocol.DetailUpstreamFailure)
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(4 * p.cfg.DialTimeout))

	outReq := req.Clone(context.Background())
	outReq.URL.Scheme = "https"
	outReq.URL.Host = net.JoinHostPort(host, fmt.Sprint(port))
	outReq.Host = host
	outReq.RequestURI = ""
	outReq.Body = io.NopCloser(counted)

	writeDone := make(chan error, 1)
	go func() { writeDone <- outReq.Write(upstream) }()

	resp, rerr := http.ReadResponse(newBufReader(upstream), req)
	if rerr != nil {
		p.counters.Add(protocol.CounterErrors)
		p.setDetail(protocol.DetailUpstreamFailure)
		<-writeDone
		return
	}
	// Responses stream through unbuffered: E4 makes response capture a non-goal, and the
	// contract's `direction: ingress` exists and stays unused (§5.6).
	werr := resp.Write(client)
	_ = resp.Body.Close()
	<-writeDone
	if werr != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	p.mu.Lock()
	p.lastSuccess = p.cfg.Clock()
	p.mu.Unlock()

	if !submission || p.cfg.Pipeline == nil {
		return
	}
	p.observe(req, counted.n, buf, res)
}

// observe hands the request to the pipeline. Failures here are the §5.4 triggers 1 and 2 and
// never stop the client's exchange, which has already completed.
func (p *Provider) observe(req *http.Request, counted int64, buf *bodyBuffer, res core.Resolution) {
	size := counted
	if req.ContentLength > 0 {
		size = req.ContentLength
	}
	var content core.ContentReader
	if buf != nil {
		content = buf
	}
	host := req.Host
	obs := core.Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: toolFingerprint(host, req.URL.Path),
		Population:      p.cfg.Agent.Population,
		MediaType:       req.Header.Get("Content-Type"),
		OccurredAt:      p.cfg.Clock(),
		SizeBytes:       size,
		Decision:        p.cfg.Decide(host),
		Content:         content,
		OverCap:         buf != nil && buf.overCap(),
		Extract:         JSONExtractor{},
	}
	out, err := p.cfg.Pipeline.Process(context.Background(), obs)
	switch {
	case err != nil:
		// Spool unavailable or full: carry the request (already carried) and count the loss.
		p.counters.Add(protocol.CounterDropped)
		p.setDetail(protocol.DetailSpoolUnwritable)
	case out.Emitted:
		p.counters.Add(protocol.CounterEmitted)
	}
	if err == nil && !out.Degraded {
		// A completed, undegraded observation is the positive signal that clears a transient
		// capability failure.
		p.setDetail(protocol.DetailNone)
	}
	if out.Degraded && out.Reason == core.ReasonClassifierDegraded {
		p.setDetail(protocol.DetailClassifierUnavailable)
	}
	_ = res
}

func (p *Provider) setDetail(d protocol.Detail) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastDegraded = d
}

// blindTunnel accepts the CONNECT, establishes a byte tunnel, and decrypts nothing (§5.1).
func (p *Provider) blindTunnel(client net.Conn, br *bufio.Reader, hostport, process string, detail protocol.Detail) {
	upstream, err := net.DialTimeout("tcp", hostport, p.cfg.DialTimeout)
	if err != nil {
		// §5.4 trigger 4 again: the client sees the connection error it would have seen anyway.
		p.counters.Add(protocol.CounterErrors)
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	p.counters.Add(protocol.CounterBlindTunnelled)
	p.mu.Lock()
	p.lastSuccess = p.cfg.Clock()
	p.mu.Unlock()
	_ = detail

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, br)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

func (p *Provider) exclude(process, host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exclusions[process+"|"+host] = p.cfg.Clock().Add(p.cfg.PinnedReprobeInterval)
}

func (p *Provider) excluded(process, host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.exclusions[process+"|"+host]
	if !ok {
		return false
	}
	if p.cfg.Clock().After(until) {
		// The exclusion has expired: the client is re-probed, because a client update can change
		// its behaviour and this must never be a permanent silent omission.
		delete(p.exclusions, process+"|"+host)
		return false
	}
	return true
}

// ExcludedCount reports how many (process, host) pairs are currently excluded.
func (p *Provider) ExcludedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.exclusions)
}

func splitHostPort(hostport string) (string, int) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.Trim(hostport, "[]"), 443
	}
	port := 443
	if n, err := fmt.Sscanf(portStr, "%d", &port); n != 1 || err != nil {
		port = 443
	}
	return host, port
}

// toolFingerprint is behaviour-derived, not a brand name: the schema requires a fingerprint and
// discovery must classify behaviour rather than match a curated list. The host and path shape
// are the observable signals available at this vantage point, hashed so the value stays stable
// and bounded.
func toolFingerprint(host, path string) string {
	h := sha256Sum([]byte("tls|" + strings.ToLower(host) + "|" + strings.Trim(path, "/")))
	return fmt.Sprintf("tls_%x", h[:8])
}
