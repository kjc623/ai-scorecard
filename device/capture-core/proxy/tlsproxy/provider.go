// Package tlsproxy is proxy.tls, the local HTTPS proxy that terminates TLS for the AI
// destinations the signed policy names and tunnels everything else untouched. CLI runtimes reach
// it through the environment cli.shim writes.
//
// It fails open: the user's traffic is never blocked, degraded or delayed by the provider's
// inability to do its job. Every branch prefers carrying the request over reporting an error,
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
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
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

	// Bundles returns the bundle in force. nil means none, which resolves to M0 and, for this
	// provider, means it stops reading content; it never widens.
	Bundles func() *policy.Bundle

	Pipeline Pipeline
	Agent    core.ScopeQuery

	// TrustRoot installs and removes the device CA. Removal is as reliable as installation.
	TrustRoot core.TrustRoot

	// CA is the device CA the proxy mints leaves from: the per-device root the service keeps
	// (OpenDeviceCA), which is the root the trust store holds. It outlives a Stop, so a policy
	// toggle installs and removes the same root over the same key. nil mints a CA for this run.
	CA *CA

	// CanaryHost/CanaryPort is the end-to-end probe destination. Healthy requires a successful
	// handshake through a minted leaf against it; without one the provider reports
	// tls_probe_failed rather than claiming health on the strength of a successful file write.
	CanaryHost string
	CanaryPort int

	BodyCap int64

	// UpstreamRoots is the trust the proxy uses when it connects to the real destination. nil
	// means the platform trust store, which is the production case; a test supplies an
	// in-process pool rather than touching the OS store.
	UpstreamRoots *x509.CertPool

	// Process names the client behind a connection, for the per-process exclusion of clients
	// that pin certificates and of tools covered by a native collector. It is a seam because process
	// attribution is platform-specific.
	Process func(conn net.Conn) string

	// AppByExe names the catalog app whose executable has this lower-case image base name. nil
	// matches nothing, so no connection is excluded as a native collector's.
	AppByExe func(base string) (appKey string, ok bool)

	// Person names the owner of the process behind a connection, whom an intercepted request is
	// attributed to. nil leaves every request to the pipeline's identity (the console user); an
	// error does so for that request and is counted.
	Person func(conn net.Conn) (core.Person, error)

	// PinnedReprobeInterval bounds an exclusion: a client update can change its behaviour, so an
	// excluded destination is re-probed at this cadence and never silently omitted for good.
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
	return c
}

// Provider is `proxy.tls`.
type Provider struct {
	cfg Config

	mu           sync.Mutex
	ln           net.Listener
	ca           *CA
	startedAt    time.Time
	started      bool
	stopped      bool
	killed       bool
	enforcing    bool
	probeOK      bool
	lastSuccess  time.Time
	counters     *core.CounterSet
	exclusions   map[string]time.Time // process|host -> excluded until
	acceptDone   chan struct{}
	wg           sync.WaitGroup
	sequence     []string // side effects, for the ordering assertions in tests
	lastDegraded protocol.Detail
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return protocol.CollectorEgressProxy }

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

// ListenAddr is the address the proxy listens on, empty before it binds.
func (p *Provider) ListenAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// buildCA produces the device CA: the configured one when supplied, otherwise a CA minted for this
// run.
func (p *Provider) buildCA(deviceID string) (*CA, error) {
	if p.cfg.CA != nil {
		return p.cfg.CA, nil
	}
	return NewCA(deviceID, p.cfg.Clock())
}

// Enabled implements core.Toggled: the proxy runs only while the tenant's TLS inspection is on.
func (p *Provider) Enabled(b *policy.Bundle) bool { return b != nil && b.Interception.Enabled }

// Start loads the device CA, binds the proxy, installs the CA certificate when a trust root is
// configured, then runs the end-to-end probe. It never fails because interception is
// unavailable: it reports degraded and the user's traffic stays direct. A Start after a Stop
// brings the proxy and the root back.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started && !p.stopped {
		p.mu.Unlock()
		return nil
	}
	// A kill switch in the bundle already in force at startup suppresses the provider before it
	// binds or installs a root. The supervisor applies the bundle before any provider starts, so
	// without this check the switch would be recorded and then ignored.
	if p.killSwitchActive() {
		p.started = true
		p.stopped = false
		p.killed = true
		p.mu.Unlock()
		p.step("start:suppressed_by_kill_switch")
		return nil
	}
	p.killed = false
	p.mu.Unlock()

	deviceID := p.cfg.Agent.DeviceID
	ca, err := p.buildCA(deviceID)
	if err != nil {
		// Without a CA there is no interception. That is a degraded provider, not a failed
		// startup: the user's traffic must not be pointed at a proxy that cannot serve.
		p.mu.Lock()
		p.started = true
		p.stopped = false
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
		// Installation is verified by reading the store back, never by the write returning nil:
		// a write to the wrong store fails silently.
		if err := p.installTrustRoot(ctx, ca); err != nil {
			p.cfg.Log.Printf("tlsproxy: could not install the device CA: %v", err)
		}
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop()
	}()

	// The end-to-end probe, a real handshake against a destination whose leaf the proxy minted,
	// is the only thing that may produce healthy. It reads the listen address and the CA pool,
	// both of which take the mutex, so it runs outside the lock.
	ok := p.probe(ctx)
	p.mu.Lock()
	p.started = true
	p.stopped = false
	p.probeOK = ok
	p.mu.Unlock()
	if !ok {
		p.cfg.Log.Printf("tlsproxy: end-to-end probe failed; reporting degraded with %s", protocol.DetailTLSProbeFailed)
	}
	return nil
}

// killSwitchActive reports whether the bundle in force carries a kill switch for proxy.tls that is
// in effect now. A future-dated switch does not fire early: EffectiveAt is when the fleet operator
// asked enforcement to stop, and the bundle carries it so a poll can be scheduled rather than
// acted on immediately.
func (p *Provider) killSwitchActive() bool {
	b := p.bundle()
	if b == nil {
		return false
	}
	ks, ok := b.KillSwitchFor(protocol.RouteProxyTLS)
	if !ok || ks.Mode != policy.KillDisable {
		return false
	}
	if !ks.EffectiveAt.IsZero() && ks.EffectiveAt.After(p.cfg.Clock()) {
		return false
	}
	return true
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
		// A write that did not happen is a named degraded cause, never a silent health claim.
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

// Stop implements core.Provider: interception stops, the listener closes, then the device CA is
// removed from the trust store, so a device whose proxy is not running trusts no root of its own.
// The removal runs also when the proxy never started, which clears a root an earlier run left. It
// is idempotent.
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

	p.stopInterception("stop")
	if ln != nil {
		_ = ln.Close()
	}
	if p.cfg.TrustRoot != nil {
		if err := p.cfg.TrustRoot.Remove(ctx); err != nil {
			p.cfg.Log.Printf("tlsproxy: could not remove the trusted root: %v", err)
		}
		p.step("trustroot.Remove")
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
// The kill switch is the one policy change that stops interception. Interception stops first,
// then the trusted root is removed, then the provider reports absent with detail killed.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	ks, ok := b.KillSwitchFor(protocol.RouteProxyTLS)
	if !ok || ks.Mode != policy.KillDisable {
		return nil
	}
	if !ks.EffectiveAt.IsZero() && ks.EffectiveAt.After(p.cfg.Clock()) {
		return nil // future-dated: in force at EffectiveAt, not before
	}
	p.mu.Lock()
	already := p.killed
	p.killed = true
	p.mu.Unlock()
	if already {
		return nil
	}
	p.stopInterception("kill_switch")
	// The kill switch is exactly when a device must stop trusting the interception authority.
	// A benign remove error is logged and not escalated.
	if p.cfg.TrustRoot != nil {
		if err := p.cfg.TrustRoot.Remove(context.Background()); err != nil {
			p.cfg.Log.Printf("tlsproxy: kill switch could not remove the trusted root: %v", err)
		}
		p.step("trustroot.Remove")
	}
	p.step("health:absent:" + string(protocol.DetailKilled))
	return nil
}

// Health implements core.Provider. Healthy requires the proxy to be bound and a successful
// end-to-end probe through a minted leaf.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	ln := p.ln
	started, stopped, killed := p.started, p.stopped, p.killed
	probeOK := p.probeOK
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

	switch {
	case !probeOK:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailTLSProbeFailed, p.startedAt, lastOK)
	case lastDegraded != protocol.DetailNone:
		// A capability failed on a recent request (classifier, spool, upstream): health does not
		// claim healthy while a named capability is not working.
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

// handle is the CONNECT path. The decision tree is the whole provider: eligible
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

	// The exclusion ladder: a destination excluded for this process is blind-tunnelled, and
	// the exclusion is re-probed at a slow cadence rather than being permanent.
	if p.excluded(process, host) {
		p.counters.Add(protocol.CounterNotCooperative)
		p.blindTunnel(client, br, req.Host, process, protocol.DetailClientPinned)
		return
	}

	if bundle == nil || !bundle.Intercepts(host, port) {
		// A destination in none of the three sets is blind-tunnelled, and the device
		// records only host, bytes, duration and owning process.
		p.blindTunnel(client, br, req.Host, process, protocol.DetailNone)
		return
	}

	// A tool whose own telemetry or hooks report its prompts is not decrypted as well, so one prompt
	// is not recorded twice.
	if p.nativelyCovered(process, bundle) {
		p.blindTunnel(client, br, req.Host, process, protocol.DetailNone)
		return
	}

	p.intercept(client, br, req, host, port, process, p.person(client), bundle)
}

// nativelyCovered reports whether the client process is a catalog app that the bundle covers with
// an enabled native collector.
func (p *Provider) nativelyCovered(process string, bundle *policy.Bundle) bool {
	if p.cfg.AppByExe == nil {
		return false
	}
	app, ok := p.cfg.AppByExe(strings.ToLower(process))
	return ok && toolconfig.NativelyCovered(bundle, app)
}

// person is the owner of the process behind an intercepted connection, or nil to attribute the
// request to the pipeline's identity. It is resolved while the client is still connected, because
// a closed connection's row no longer names its process.
func (p *Provider) person(conn net.Conn) *core.Person {
	if p.cfg.Person == nil {
		return nil
	}
	who, err := p.cfg.Person(conn)
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("tlsproxy: the client at %s could not be attributed; attributing to the console user: %v", conn.RemoteAddr(), err)
		return nil
	}
	return &who
}

func (p *Provider) bundle() *policy.Bundle {
	if p.cfg.Bundles == nil {
		return nil
	}
	return p.cfg.Bundles()
}

// mode resolves the effective mode before anything is read and never returns an empty
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
func (p *Provider) intercept(client net.Conn, br *bufio.Reader, req *http.Request, host string, port int, process string, person *core.Person, bundle *policy.Bundle) {
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
		NextProtos:   []string{"http/1.1"}, // The proxy does not pretend to support HTTP/3
	})
	hctx, cancel := context.WithTimeout(context.Background(), p.cfg.HandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		// A TLS alert during the minted-leaf handshake, repeated, means the
		// client pins a certificate or validates against a bundled CA list. Exclude the
		// destination for that process and tunnel it blind from then on — never leave the client
		// broken to preserve collection.
		p.exclude(process, host)
		p.counters.Add(protocol.CounterNotCooperative)
		p.cfg.Log.Printf("tlsproxy: handshake with %s failed for %s; excluding %s for that process: %v", host, process, host, err)
		return
	}
	defer tlsConn.Close()

	p.serveIntercepted(tlsConn, host, port, process, person, bundle)
}

func (p *Provider) serveIntercepted(client *tls.Conn, host string, port int, process string, person *core.Person, bundle *policy.Bundle) {
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
		// The shape predicate said no. Counted, never guessed at.
		p.counters.Add(protocol.CounterSkippedNotGenerative)
	}

	res := p.mode(host)

	// The mode is applied before content is read.
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
		// Return the connection error the client would have seen anyway; never
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
	// Responses stream through unbuffered: responses are not captured.
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
	p.observe(req, counted.n, buf, res, person)
}

// observe hands the request to the pipeline. Failures here (classifier, spool) are reported and
// never stop the client's exchange, which has already completed.
func (p *Provider) observe(req *http.Request, counted int64, buf *bodyBuffer, res core.Resolution, person *core.Person) {
	size := counted
	if req.ContentLength > 0 {
		size = req.ContentLength
	}
	var content core.ContentReader
	if buf != nil {
		content = buf
	}
	tool := toolFingerprint(req.Host, req.URL.Path)
	// The exchange has completed by now, so whatever a rule asks for is recorded as logged.
	obs := core.Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: tool,
		Population:      p.cfg.Agent.Population,
		MediaType:       req.Header.Get("Content-Type"),
		OccurredAt:      p.cfg.Clock(),
		SizeBytes:       size,
		Enforce:         enforce.Hook(p.cfg.Bundles, protocol.RouteProxyTLS, tool, false),
		Content:         content,
		OverCap:         buf != nil && buf.overCap(),
		Extract:         JSONExtractor{},
		Person:          person,
	}
	out, err := p.cfg.Pipeline.Process(context.Background(), obs)
	switch {
	case out.Reason == core.ReasonIdentityUnresolved:
		// Fail-closed for identity: the request is carried, the envelope is not minted, and the
		// coverage row says so with the named detail rather than claiming healthy.
		p.counters.Add(protocol.CounterErrors)
		p.setDetail(protocol.DetailIdentityUnresolved)
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

// blindTunnel accepts the CONNECT, establishes a byte tunnel, and decrypts nothing.
func (p *Provider) blindTunnel(client net.Conn, br *bufio.Reader, hostport, process string, detail protocol.Detail) {
	upstream, err := net.DialTimeout("tcp", hostport, p.cfg.DialTimeout)
	if err != nil {
		// The client sees the connection error it would have seen anyway.
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
