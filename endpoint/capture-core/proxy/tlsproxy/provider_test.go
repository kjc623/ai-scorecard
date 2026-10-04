package tlsproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ---- fakes ---------------------------------------------------------------------------------

type recProxy struct {
	mu         sync.Mutex
	steps      []string
	addr       string
	effective  bool
	openAtRest map[string]bool
	openCheck  func() bool
}

func (s *recProxy) PointAt(_ context.Context, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = addr
	s.effective = true
	s.steps = append(s.steps, "systemproxy.PointAt:"+addr)
	return nil
}

func (s *recProxy) Restore(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "systemproxy.Restore")
	if s.openCheck != nil {
		s.openAtRest["listener_open"] = s.openCheck()
	}
	s.effective = false
	return nil
}

func (s *recProxy) Effective(context.Context) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr, s.effective
}

func (s *recProxy) sequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steps...)
}

type fakeTrust struct{ installed []byte }

func (t *fakeTrust) Remove(context.Context) error { return nil }
func (t *fakeTrust) Install(_ context.Context, der []byte) error {
	t.installed = append([]byte(nil), der...)
	return nil
}

type fakePipeline struct {
	mode       protocol.CollectionMode
	procErr    error
	degraded   bool
	reason     string
	mu         sync.Mutex
	observed   []core.Observation
	readBodies [][]byte
}

func (p *fakePipeline) ResolveMode(core.ScopeQuery) core.Resolution {
	return core.Resolution{Mode: p.mode, PolicyVersion: "test"}
}

func (p *fakePipeline) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	out := core.Outcome{Route: obs.Route, Mode: p.mode, Emitted: true, Reason: core.ReasonEmitted}
	p.mu.Lock()
	p.observed = append(p.observed, obs)
	p.mu.Unlock()
	if p.procErr != nil {
		return out, p.procErr
	}
	if obs.Content != nil && p.mode.ReadsContent() {
		b, err := obs.Content.Read(ctx)
		if err != nil {
			return out, err
		}
		p.mu.Lock()
		p.readBodies = append(p.readBodies, b)
		p.mu.Unlock()
	}
	if p.degraded {
		out.Degraded = true
		out.Reason = p.reason
	}
	return out, nil
}

func (p *fakePipeline) observations() []core.Observation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Observation(nil), p.observed...)
}

// ---- helpers -------------------------------------------------------------------------------

func bundleIntercepting(port int) *policy.Bundle {
	return &policy.Bundle{
		Version:       "1",
		EffectiveAt:   time.Unix(1_700_000_000, 0),
		TenantDefault: protocol.ModeM3,
		ToolModes:     map[string]protocol.CollectionMode{},
		Interception: policy.Interception{
			SeedHosts: []string{"127.0.0.1"},
			Ports:     []int{port},
		},
		Classifier: policy.ClassifierRelease{ReleaseID: "rel-1", State: policy.ReleaseEnforcing},
	}
}

func upstreamPool(s *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return pool
}

// dialThroughProxy performs the CONNECT handshake and returns a TLS-over-tunnel connection.
func dialThroughProxy(t *testing.T, proxyAddr, target string, cfg *tls.Config) (*tls.Conn, *http.Response) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	conn := tls.Client(&bufferedConn{Reader: br, Conn: raw}, cfg)
	hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		t.Fatalf("tunnel TLS handshake: %v", err)
	}
	return conn, resp
}

type bufferedConn struct {
	*bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

func postThroughTunnel(t *testing.T, conn *tls.Conn, host, body string) (*http.Response, string) {
	t.Helper()
	req := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		host, len(body), body)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(got)
}

func newProviderForTest(t *testing.T, cfg Config) *Provider {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = testLogger{t}
	}
	p := New(cfg)
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- §5.3 interception ----------------------------------------------------------------------

// §5.3: an eligible destination is terminated with a minted leaf, the request is read, and the
// response streams back. The client believes it is talking to the real host.
func TestTLS_5_3_InterceptsEligibleDestination(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"Summarise the attached contract."}]}`
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("upstream body = %q, want %q", got, body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	sysProxy := &recProxy{openAtRest: map[string]bool{}}
	pipe := &fakePipeline{mode: protocol.ModeM1}
	bundle := bundleIntercepting(upPort)
	p := newProviderForTest(t, Config{
		Listen:        "127.0.0.1:0",
		Bundles:       func() *policy.Bundle { return bundle },
		Pipeline:      pipe,
		SystemProxy:   sysProxy,
		UpstreamRoots: upstreamPool(upstream),
		CanaryHost:    "127.0.0.1",
		CanaryPort:    upPort,
		BodyCap:       1 << 20,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.Health().State != protocol.StateHealthy {
		t.Fatalf("§5.6 health = %s/%s, want healthy after the end-to-end probe", p.Health().State, p.Health().Detail)
	}

	ca := p.CA()
	if ca == nil {
		t.Fatal("no device CA was minted")
	}
	target := fmt.Sprintf("127.0.0.1:%d", upPort)
	conn, _ := dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    ca.Pool(), // the in-process root pool, never the OS store
	})
	defer conn.Close()
	if got := conn.ConnectionState().PeerCertificates[0].Issuer.CommonName; !strings.Contains(got, "Device CA") {
		t.Fatalf("client saw issuer %q, want a leaf minted by the device CA", got)
	}
	resp, got := postThroughTunnel(t, conn, "127.0.0.1", body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("tunnelled response = %d %q", resp.StatusCode, got)
	}
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 1 })
	obs := pipe.observations()[0]
	if obs.Route != protocol.RouteProxyTLS {
		t.Fatalf("route = %s", obs.Route)
	}
	if len(pipe.readBodies) != 1 || string(pipe.readBodies[0]) != body {
		t.Fatalf("the pipeline did not receive the body: %v", pipe.readBodies)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterObserved] != 1 || c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want observed=1 emitted=1", c)
	}
}

// §5.1: a destination in none of the three sets is blind-tunnelled. The proof is that the client
// sees the *upstream's* certificate: our leaf would fail verification against the upstream pool.
func TestTLS_5_1_BlindTunnelsUnlistedDestination(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	// The interception set names a different host, so 127.0.0.1 is not eligible.
	bundle := bundleIntercepting(upPort)
	bundle.Interception.SeedHosts = []string{"api.example.invalid"}
	p := newProviderForTest(t, Config{
		Listen:        "127.0.0.1:0",
		Bundles:       func() *policy.Bundle { return bundle },
		Pipeline:      &fakePipeline{mode: protocol.ModeM1},
		UpstreamRoots: upstreamPool(upstream),
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	target := fmt.Sprintf("127.0.0.1:%d", upPort)
	conn, _ := dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    upstreamPool(upstream), // trust the real upstream, not our CA
	})
	defer conn.Close()
	resp, got := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"direct":true`) {
		t.Fatalf("blind tunnel response = %d %q", resp.StatusCode, got)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterBlindTunnelled] != 1 {
		t.Fatalf("§5.1 blind_tunnelled = %d, want 1", c[protocol.CounterBlindTunnelled])
	}
}

// §13.3 rule 5 through this provider's eyes: with no bundle in force nothing is intercepted, so
// no content is read. M0 is a reduction in capability, never an increase.
func TestTLS_13_3_NoBundleMeansNothingIsDecrypted(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	p := newProviderForTest(t, Config{
		Listen:        "127.0.0.1:0",
		Bundles:       func() *policy.Bundle { return nil },
		Pipeline:      &fakePipeline{mode: protocol.ModeM3},
		UpstreamRoots: upstreamPool(upstream),
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    upstreamPool(upstream),
	})
	defer conn.Close()
	if _, got := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"secret"}]}`); !strings.Contains(got, `"direct":true`) {
		t.Fatalf("response = %q", got)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterBlindTunnelled] != 1 {
		t.Fatalf("blind_tunnelled = %d, want 1 (nothing may be decrypted without a verified bundle)", c[protocol.CounterBlindTunnelled])
	}
}

// ---- §5.4 fail-open ---------------------------------------------------------------------

// §5.4's table, trigger by trigger. The acceptance property is that the client's exchange
// completes in every row except the one where the upstream itself failed.
func TestTLS_5_4_FailOpenTable(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}]}`

	t.Run("classifier unavailable carries the request and reports degraded", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()
		upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
		pipe := &fakePipeline{mode: protocol.ModeM1, degraded: true, reason: core.ReasonClassifierDegraded}
		sysProxy := &recProxy{openAtRest: map[string]bool{}}
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
			Pipeline: pipe, SystemProxy: sysProxy, UpstreamRoots: upstreamPool(upstream),
			CanaryHost: "127.0.0.1", CanaryPort: upPort,
		})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		resp, _ := postThroughTunnel(t, conn, "127.0.0.1", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("§5.4: the client's request was not carried: %d", resp.StatusCode)
		}
		waitFor(t, 2*time.Second, "the degradation to be recorded", func() bool {
			return p.Health().Detail == protocol.DetailClassifierUnavailable
		})
		if h := p.Health(); h.State != protocol.StateDegraded {
			t.Fatalf("§5.4: health = %s, want degraded", h.State)
		}
	})

	t.Run("spool unwritable carries the request and counts dropped", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()
		upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
		pipe := &fakePipeline{mode: protocol.ModeM1, procErr: errors.New("spool full")}
		sysProxy := &recProxy{openAtRest: map[string]bool{}}
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
			Pipeline: pipe, SystemProxy: sysProxy, UpstreamRoots: upstreamPool(upstream),
			CanaryHost: "127.0.0.1", CanaryPort: upPort,
		})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		resp, _ := postThroughTunnel(t, conn, "127.0.0.1", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("§5.4: a spool failure blocked the client: %d", resp.StatusCode)
		}
		waitFor(t, 2*time.Second, "the dropped count", func() bool {
			return p.Counters().Cumulative()[protocol.CounterDropped] == 1
		})
		if d := p.Health().Detail; d != protocol.DetailSpoolUnwritable {
			t.Fatalf("§5.4: detail = %q, want %q", d, protocol.DetailSpoolUnwritable)
		}
	})

	t.Run("upstream connect failure returns the connection error, never a substituted response", func(t *testing.T) {
		dead := freePort(t) // nothing listening
		live := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer live.Close()
		livePort := live.Listener.Addr().(*net.TCPAddr).Port

		bundle := bundleIntercepting(dead)
		bundle.Interception.Ports = []int{dead, livePort} // the canary needs to be interceptable
		sysProxy := &recProxy{openAtRest: map[string]bool{}}
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundle },
			Pipeline: &fakePipeline{mode: protocol.ModeM1}, SystemProxy: sysProxy,
			UpstreamRoots: upstreamPool(live), CanaryHost: "127.0.0.1", CanaryPort: livePort,
		})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", dead), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		// The request is accepted and then the exchange fails: no synthesized 502, no substituted
		// body. The client sees exactly the failure the upstream produced.
		if _, err := conn.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 2\r\n\r\n{}")); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err == nil {
			t.Fatal("§5.4: a response was substituted for an upstream connect failure")
		}
		waitFor(t, 2*time.Second, "the upstream failure detail", func() bool {
			return p.Health().Detail == protocol.DetailUpstreamFailure
		})
	})
}

// §5.3: an over-cap body is not read into memory, is sized, and is reported as degraded — never
// as a silent "clean". The bytes still reach the upstream unchanged.
func TestTLS_5_3_OverCapBodyIsForwardedButNotHeld(t *testing.T) {
	body := strings.Repeat("A", 4096)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if len(got) != len(body) {
			t.Errorf("upstream saw %d bytes, want %d", len(got), len(body))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	pipe := &fakePipeline{mode: protocol.ModeM1}
	sysProxy := &recProxy{openAtRest: map[string]bool{}}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: pipe, SystemProxy: sysProxy, UpstreamRoots: upstreamPool(upstream),
		CanaryHost: "127.0.0.1", CanaryPort: upPort, BodyCap: 128,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
	defer conn.Close()
	// Send it as a raw payload rather than JSON so the extractor cannot help; over-cap is decided
	// before classification.
	req := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 1 })
	obs := pipe.observations()[0]
	if !obs.OverCap {
		t.Fatalf("§5.3: over-cap body was not flagged (size=%d cap=128)", obs.SizeBytes)
	}
	if obs.SizeBytes != int64(len(body)) {
		t.Fatalf("§5.3: size_bytes = %d, want the full observed size %d", obs.SizeBytes, len(body))
	}
	// §5.3 allows a digest of the first N bytes, so the pipeline may see the bounded prefix — but
	// never the whole body: the point is that an over-cap payload is not held in memory.
	for _, held := range pipe.readBodies {
		if len(held) > 128 {
			t.Fatalf("§5.3: an over-cap body was read into memory: %d bytes (cap 128)", len(held))
		}
	}
}

// ---- §5.5 kill switch and the pinned-client ladder ------------------------------------------

// §5.5's ordering is the dangerous half: enforcement and interception stop FIRST, then the system
// proxy is restored, then the provider reports absent with detail=killed. Traffic must never be
// left pointed at a proxy that has stopped intercepting.
func TestTLS_5_5_KillSwitchStopsEnforcementBeforeRestoringTheProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	var p *Provider
	sysProxy := &recProxy{openAtRest: map[string]bool{}}
	sysProxy.openCheck = func() bool {
		// True would mean the listener is still intercepting when the system proxy is restored.
		return p.ListenAddr() != ""
	}
	p = newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, SystemProxy: sysProxy,
		UpstreamRoots: upstreamPool(upstream), CanaryHost: "127.0.0.1", CanaryPort: upPort,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.ListenAddr() == "" {
		t.Fatal("precondition: the proxy is not listening")
	}

	ks := policy.Bundle{
		Version:       "2",
		EffectiveAt:   time.Unix(1_700_000_100, 0),
		TenantDefault: protocol.ModeM0,
		KillSwitches: []policy.KillSwitch{{
			Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable,
			EffectiveAt: time.Unix(1_700_000_100, 0), ReasonCode: "fleet_regression_1234",
		}},
		Classifier: policy.ClassifierRelease{ReleaseID: "rel-1", State: policy.ReleaseEnforcing},
	}
	if err := p.ApplyPolicy(ks); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}

	want := []string{
		"stop_interception:kill_switch", // 1 stop enforcement and interception first
		"systemproxy.Restore",           // 2 then restore the system proxy
		"health:absent:killed",          // 3 then report absent detail=killed
	}
	got := p.Sequence()
	if strings.Join(got, " > ") != strings.Join(want, " > ") {
		t.Fatalf("§5.5 ordering\ngot:  %v\nwant: %v", got, want)
	}
	if open, recorded := sysProxy.openAtRest["listener_open"]; recorded && open {
		t.Fatal("§5.5: the system proxy was restored while interception was still listening — the exact ordering that breaks egress")
	}
	h := p.Health()
	if h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("§5.5 health after the kill switch = %s/%s, want absent/killed", h.State, h.Detail)
	}
	// The kill switch removes the risky capability, not the product: capture-core stays installed
	// and its other providers keep running. This provider is done, and says so.
	if p.ListenAddr() != "" {
		t.Fatal("§5.5: the proxy is still listening after the kill switch")
	}
}

// §5.5's first row: a client that fails the minted-leaf handshake is excluded per process and
// destination, and tunnelled blind from then on — never left broken to preserve collection.
func TestTLS_5_5_PinnedClientExclusionLadder(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	sysProxy := &recProxy{openAtRest: map[string]bool{}}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, SystemProxy: sysProxy,
		UpstreamRoots: upstreamPool(upstream), CanaryHost: "127.0.0.1", CanaryPort: upPort,
		Process: func(net.Conn) string { return "pinned-client" },
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	target := fmt.Sprintf("127.0.0.1:%d", upPort)

	// First connection: the client pins a certificate and aborts the handshake.
	raw, err := net.DialTimeout("tcp", p.ListenAddr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	if _, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}); err != nil {
		t.Fatalf("CONNECT: %v", err)
	}
	_, _ = raw.Write([]byte("not a TLS ClientHello at all\r\n\r\n"))
	_ = raw.Close()
	waitFor(t, 3*time.Second, "the exclusion", func() bool { return p.ExcludedCount() == 1 })

	// Second connection: the same process is now tunnelled blind, so the *upstream's* certificate
	// is what the client sees.
	conn, _ := dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    upstreamPool(upstream),
	})
	defer conn.Close()
	if _, got := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`); !strings.Contains(got, `"direct":true`) {
		t.Fatalf("excluded client did not reach the upstream directly: %q", got)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterNotCooperative] < 2 {
		t.Fatalf("not_cooperative = %d, want at least 2 (the failed handshake and the excluded tunnel)", c[protocol.CounterNotCooperative])
	}
}

// §5.6: healthy requires the end-to-end probe. Without one the provider reports tls_probe_failed
// rather than claiming health on the strength of a successful bind.
func TestTLS_5_6_HealthNeverHealthyWithoutTheProbe(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	sysProxy := &recProxy{openAtRest: map[string]bool{}}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, SystemProxy: sysProxy,
		UpstreamRoots: upstreamPool(upstream), // no canary configured
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := p.Health()
	if h.State == protocol.StateHealthy {
		t.Fatal("§5.6: healthy without a successful end-to-end probe through a minted leaf")
	}
	if h.State != protocol.StateDegraded || h.Detail != protocol.DetailTLSProbeFailed {
		t.Fatalf("§5.6 health = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailTLSProbeFailed)
	}
}

// §5.2/A3: the device CA is per device, its private key never leaves the process, and only the
// sealed form is handed to platform key protection.
func TestTLS_5_2_CAIsPerDeviceAndOnlyTheSealedFormLeaves(t *testing.T) {
	sealer := &fakeSealer{key: []byte("0123456789abcdef0123456789abcdef")}
	ca, err := NewCA("device-1", sealer, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if len(ca.Sealed()) == 0 {
		t.Fatal("no sealed key was produced; the CA key must not be left in the clear")
	}
	if sealer.sealedCalls != 1 {
		t.Fatalf("sealed %d times, want 1", sealer.sealedCalls)
	}
	if !strings.Contains(ca.Info().Subject, "device-1") {
		t.Fatalf("CA subject = %q, want the device id in it (per-device, not per-fleet)", ca.Info().Subject)
	}
	leaf, err := ca.Leaf("api.example.invalid", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if parsed.NotAfter.Sub(parsed.NotBefore) > 24*time.Hour {
		t.Fatalf("leaf lifetime = %v, want short-lived", parsed.NotAfter.Sub(parsed.NotBefore))
	}
	if err := parsed.VerifyHostname("api.example.invalid"); err != nil {
		t.Fatalf("leaf does not cover its hostname: %v", err)
	}
	if _, err := ca.Leaf("api.example.invalid", time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("second Leaf: %v", err)
	}
	if len(ca.leaves) != 1 {
		t.Fatalf("leaf cache holds %d entries, want 1", len(ca.leaves))
	}
}

type fakeSealer struct {
	key         []byte
	sealedCalls int
}

func (f *fakeSealer) Seal(plain []byte) ([]byte, error) {
	f.sealedCalls++
	out := make([]byte, len(plain))
	for i := range plain {
		out[i] = plain[i] ^ f.key[i%len(f.key)]
	}
	return out, nil
}

func (f *fakeSealer) Open(sealed []byte) ([]byte, error) {
	out := make([]byte, len(sealed))
	for i := range sealed {
		out[i] = sealed[i] ^ f.key[i%len(f.key)]
	}
	return out, nil
}

// ---- §5.2 trust-store installation -----------------------------------------------------------

// fakeVerifyingTrust records an install and then answers the store read-back. A store that does
// not confirm the certificate is the silent-wrong-store case §5.2 warns about, so it must surface
// as a named degraded cause rather than a healthy row.
type fakeVerifyingTrust struct {
	fakeTrust
	ok  bool
	err error
}

func (t *fakeVerifyingTrust) Verify(context.Context, []byte) (bool, error) { return t.ok, t.err }

// fakeFailingTrust fails the install itself.
type fakeFailingTrust struct{}

func (fakeFailingTrust) Remove(context.Context) error { return nil }
func (fakeFailingTrust) Install(context.Context, []byte) error {
	return fmt.Errorf("store is read-only")
}

// §5.2: the end-to-end probe uses the in-process pool, so it can succeed even when the OS store
// was never touched. The store verification is what makes that silent failure visible in health.
func TestTLS_5_2_TrustStoreFailureIsNamedInHealth(t *testing.T) {
	canaryPort := freePort(t)
	bundle := bundleIntercepting(canaryPort)

	p := newProviderForTest(t, Config{
		Listen:     "127.0.0.1:0",
		Bundles:    func() *policy.Bundle { return bundle },
		Pipeline:   &fakePipeline{mode: protocol.ModeM1},
		TrustRoot:  &fakeVerifyingTrust{ok: false},
		CanaryHost: "127.0.0.1",
		CanaryPort: canaryPort,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailTrustVerifyFailed {
		t.Fatalf("health = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailTrustVerifyFailed)
	}

	p2 := newProviderForTest(t, Config{
		Listen:     "127.0.0.1:0",
		Bundles:    func() *policy.Bundle { return bundle },
		Pipeline:   &fakePipeline{mode: protocol.ModeM1},
		TrustRoot:  fakeFailingTrust{},
		CanaryHost: "127.0.0.1",
		CanaryPort: canaryPort,
	})
	if err := p2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := p2.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailTrustInstallFailed {
		t.Fatalf("health = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailTrustInstallFailed)
	}
}
