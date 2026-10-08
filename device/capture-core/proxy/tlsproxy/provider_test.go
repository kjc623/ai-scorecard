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

type fakeTrust struct {
	installed []byte
	removes   int
}

func (t *fakeTrust) Remove(context.Context) error {
	t.removes++
	return nil
}
func (t *fakeTrust) Install(_ context.Context, der []byte) error {
	t.installed = append([]byte(nil), der...)
	return nil
}

type fakePipeline struct {
	mode       protocol.CollectionMode
	procErr    error
	degraded   bool
	reason     string
	extract    bool // run the observation's extractor over the body, as the real pipeline does
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
		if p.extract {
			if _, _, err := obs.Extract.Extract(b, obs.MediaType); err != nil {
				out.Degraded = true
				out.Reason = core.ReasonExtractionDegraded
			}
		}
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
	return connectThrough(t, raw, target, cfg)
}

// connectThrough performs the CONNECT handshake over a connection already dialled to the proxy.
func connectThrough(t *testing.T, raw net.Conn, target string, cfg *tls.Config) (*tls.Conn, *http.Response) {
	t.Helper()
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

// ---- interception ----------------------------------------------------------------------

// An eligible destination is terminated with a minted leaf, the request is read, and the
// response streams back. The client believes it is talking to the real host.
func TestTLSInterceptsEligibleDestination(t *testing.T) {
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

	pipe := &fakePipeline{mode: protocol.ModeM1}
	bundle := bundleIntercepting(upPort)
	p := newProviderForTest(t, Config{
		Listen:        "127.0.0.1:0",
		Bundles:       func() *policy.Bundle { return bundle },
		Pipeline:      pipe,
		UpstreamRoots: upstreamPool(upstream),
		CanaryHost:    "127.0.0.1",
		CanaryPort:    upPort,
		BodyCap:       1 << 20,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.Health().State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s, want healthy after the end-to-end probe", p.Health().State, p.Health().Detail)
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
	// The proxy cannot stop a prompt, so a matching block rule is recorded as logged under its id.
	bundle.Rules = []policy.Rule{{RuleID: "block_credentials", Action: policy.RuleBlock, Match: policy.RuleMatch{Labels: []string{"credential"}}}}
	if got := obs.Enforce([]string{"credential"}, true); got != (protocol.Decision{RuleID: "block_credentials", Action: protocol.ActionLogged, DecidedLocally: true}) {
		t.Fatalf("recorded decision = %+v", got)
	}
	if len(pipe.readBodies) != 1 || string(pipe.readBodies[0]) != body {
		t.Fatalf("the pipeline did not receive the body: %v", pipe.readBodies)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterObserved] != 1 || c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want observed=1 emitted=1", c)
	}
}

// A destination in none of the three sets is blind-tunnelled. The proof is that the client
// sees the *upstream's* certificate: our leaf would fail verification against the upstream pool.
func TestTLSBlindTunnelsUnlistedDestination(t *testing.T) {
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
		t.Fatalf("blind_tunnelled = %d, want 1", c[protocol.CounterBlindTunnelled])
	}
}

// With no bundle in force nothing is intercepted, so
// no content is read. M0 is a reduction in capability, never an increase.
func TestTLSNoBundleMeansNothingIsDecrypted(t *testing.T) {
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

// ---- fail-open ---------------------------------------------------------------------

// The fail-open table, trigger by trigger. The client's exchange
// completes in every row except the one where the upstream itself failed.
func TestTLSFailOpenTable(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}]}`

	t.Run("classifier unavailable carries the request and reports degraded", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()
		upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
		pipe := &fakePipeline{mode: protocol.ModeM1, degraded: true, reason: core.ReasonClassifierDegraded}
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
			Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
			CanaryHost: "127.0.0.1", CanaryPort: upPort,
		})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		resp, _ := postThroughTunnel(t, conn, "127.0.0.1", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the client's request was not carried: %d", resp.StatusCode)
		}
		waitFor(t, 2*time.Second, "the degradation to be recorded", func() bool {
			return p.Health().Detail == protocol.DetailClassifierUnavailable
		})
		if h := p.Health(); h.State != protocol.StateDegraded {
			t.Fatalf("health = %s, want degraded", h.State)
		}
	})

	t.Run("spool unwritable carries the request and counts dropped", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()
		upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
		pipe := &fakePipeline{mode: protocol.ModeM1, procErr: errors.New("spool full")}
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
			Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
			CanaryHost: "127.0.0.1", CanaryPort: upPort,
		})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		resp, _ := postThroughTunnel(t, conn, "127.0.0.1", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a spool failure blocked the client: %d", resp.StatusCode)
		}
		waitFor(t, 2*time.Second, "the dropped count", func() bool {
			return p.Counters().Cumulative()[protocol.CounterDropped] == 1
		})
		if d := p.Health().Detail; d != protocol.DetailSpoolUnwritable {
			t.Fatalf("detail = %q, want %q", d, protocol.DetailSpoolUnwritable)
		}
	})

	t.Run("upstream connect failure returns the connection error, never a substituted response", func(t *testing.T) {
		dead := freePort(t) // nothing listening
		live := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer live.Close()
		livePort := live.Listener.Addr().(*net.TCPAddr).Port

		bundle := bundleIntercepting(dead)
		bundle.Interception.Ports = []int{dead, livePort} // the canary needs to be interceptable
		p := newProviderForTest(t, Config{
			Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundle },
			Pipeline:      &fakePipeline{mode: protocol.ModeM1},
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
			t.Fatal("a response was substituted for an upstream connect failure")
		}
		waitFor(t, 2*time.Second, "the upstream failure detail", func() bool {
			return p.Health().Detail == protocol.DetailUpstreamFailure
		})
	})
}

// An over-cap body is not read into memory, is sized, and is reported as degraded — never
// as a silent "clean". The bytes still reach the upstream unchanged.
func TestTLSOverCapBodyIsForwardedButNotHeld(t *testing.T) {
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
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
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
		t.Fatalf("over-cap body was not flagged (size=%d cap=128)", obs.SizeBytes)
	}
	if obs.SizeBytes != int64(len(body)) {
		t.Fatalf("size_bytes = %d, want the full observed size %d", obs.SizeBytes, len(body))
	}
	// A digest of the first N bytes is allowed, so the pipeline may see the bounded prefix, but
	// never the whole body: the point is that an over-cap payload is not held in memory.
	for _, held := range pipe.readBodies {
		if len(held) > 128 {
			t.Fatalf("an over-cap body was read into memory: %d bytes (cap 128)", len(held))
		}
	}
}

// A body its destination's parser does not recognise is a format change: the provider row says
// content_unprocessable until a body is read again.
func TestTLSUnknownBodyShapeIsContentUnprocessable(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	pipe := &fakePipeline{mode: protocol.ModeM1, extract: true}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
		CanaryHost: "127.0.0.1", CanaryPort: upPort, BodyCap: 1 << 20,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	post := func(body string) {
		t.Helper()
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		defer conn.Close()
		// The Host header names the API, which chooses its parser; the tunnel goes to the stub.
		n := len(pipe.observations())
		postThroughTunnel(t, conn, "api.openai.com", body)
		waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == n+1 })
	}

	post(`{"model":"gpt-4.1","msgs":[{"role":"user","content":"hello"}]}`)
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailContentUnprocessable {
		t.Fatalf("health = %s/%s, want degraded/content_unprocessable", h.State, h.Detail)
	}
	post(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s, want healthy once a body is read again", h.State, h.Detail)
	}
}

// ---- kill switch and the pinned-client ladder ------------------------------------------

// The kill switch stops interception first, then removes the trusted root, then reports absent
// with detail killed.
func TestKillSwitchStopsInterceptionThenRemovesTheRoot(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	trustRoot := &fakeTrust{}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, TrustRoot: trustRoot,
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
	}
	if err := p.ApplyPolicy(ks); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}

	want := []string{"stop_interception:kill_switch", "trustroot.Remove", "health:absent:killed"}
	if got := p.Sequence(); strings.Join(got, " > ") != strings.Join(want, " > ") {
		t.Fatalf("kill switch ordering\ngot:  %v\nwant: %v", got, want)
	}
	if trustRoot.removes != 1 {
		t.Fatalf("the kill switch called TrustRoot.Remove %d times, want 1", trustRoot.removes)
	}
	h := p.Health()
	if h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("health after the kill switch = %s/%s, want absent/killed", h.State, h.Detail)
	}
	if p.ListenAddr() != "" {
		t.Fatal("the proxy is still listening after the kill switch")
	}
}

// A client that fails the minted-leaf handshake is excluded per process and
// destination, and tunnelled blind from then on — never left broken to preserve collection.
func TestTLSPinnedClientExclusionLadder(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline:      &fakePipeline{mode: protocol.ModeM1},
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

// A tool covered by an enabled native collector is blind-tunnelled even to a destination the bundle
// intercepts, so its prompts are not recorded twice; any other client of that destination is
// decrypted, and so is the tool once its native collector is switched off.
func TestTLSNativelyCoveredToolIsBlindTunnelled(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
	target := fmt.Sprintf("127.0.0.1:%d", upPort)

	var mu sync.Mutex
	process := "Claude.exe"
	setProcess := func(name string) {
		mu.Lock()
		process = name
		mu.Unlock()
	}
	var looked []string
	bundle := bundleIntercepting(upPort)
	bundle.Endpoint = policy.EndpointPolicy{
		OTel:  policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
		Tools: map[string]policy.EndpointTool{"claude_code": {OTel: true}},
	}
	var bmu sync.Mutex
	pipe := &fakePipeline{mode: protocol.ModeM1}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0",
		Bundles: func() *policy.Bundle {
			bmu.Lock()
			defer bmu.Unlock()
			return bundle
		},
		Pipeline:      pipe,
		UpstreamRoots: upstreamPool(upstream), CanaryHost: "127.0.0.1", CanaryPort: upPort,
		Process: func(net.Conn) string {
			mu.Lock()
			defer mu.Unlock()
			return process
		},
		AppByExe: func(base string) (string, bool) {
			mu.Lock()
			looked = append(looked, base)
			mu.Unlock()
			if base == "claude.exe" {
				return "claude_code", true
			}
			return "", false
		},
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tunnelled := func() uint64 { return p.Counters().Cumulative()[protocol.CounterBlindTunnelled] }
	before := tunnelled()

	// Claude Code, with its OTel export on: the client sees the upstream's own certificate.
	conn, _ := dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{ServerName: "127.0.0.1", RootCAs: upstreamPool(upstream)})
	if _, got := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("response = %q", got)
	}
	conn.Close()
	if got := tunnelled() - before; got != 1 {
		t.Fatalf("blind_tunnelled rose by %d, want 1", got)
	}
	if len(pipe.observations()) != 0 {
		t.Fatal("a natively covered tool's request was decrypted and observed")
	}
	mu.Lock()
	if len(looked) == 0 || looked[len(looked)-1] != "claude.exe" {
		t.Fatalf("looked up %v, want the lower-case image name", looked)
	}
	mu.Unlock()

	// Another process to the same destination is decrypted.
	setProcess("python.exe")
	conn, _ = dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
	postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`)
	conn.Close()
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 1 })

	// Claude Code with its OTel export switched off is decrypted too.
	setProcess("claude.exe")
	bmu.Lock()
	bundle.Endpoint.Tools["claude_code"] = policy.EndpointTool{OTel: false}
	bmu.Unlock()
	conn, _ = dialThroughProxy(t, p.ListenAddr(), target, &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
	postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`)
	conn.Close()
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 2 })
	if got := tunnelled() - before; got != 1 {
		t.Fatalf("blind_tunnelled rose by %d, want 1", got)
	}
}

// fakeProcessTable stands in for the operating system's connection owners: each client connection,
// by its local address, belongs to a process run by a person.
type fakeProcessTable struct {
	mu     sync.Mutex
	owners map[string]core.Person
	calls  int
}

func (f *fakeProcessTable) dial(t *testing.T, proxyAddr string, owner core.Person) net.Conn {
	t.Helper()
	raw, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	f.mu.Lock()
	f.owners[raw.LocalAddr().String()] = owner
	f.mu.Unlock()
	return raw
}

// person answers for the server's end of a connection: the client is its remote address.
func (f *fakeProcessTable) person(conn net.Conn) (core.Person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	who, ok := f.owners[conn.RemoteAddr().String()]
	if !ok {
		return core.Person{}, fmt.Errorf("no process owns %s", conn.RemoteAddr())
	}
	return who, nil
}

// Each intercepted request is attributed to the user running the process that dialled the proxy,
// not to one user for the whole device.
func TestTLSAttributesEachRequestToTheDiallingProcessOwner(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	procs := &fakeProcessTable{owners: map[string]core.Person{}}
	pipe := &fakePipeline{mode: protocol.ModeM1}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
		Person: procs.person,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	target := fmt.Sprintf("127.0.0.1:%d", upPort)
	second := core.Person{UserRef: "u_second", SubjectName: "second@contoso.example"}
	console := core.Person{UserRef: "u_console", SubjectName: "console@contoso.example"}
	for _, who := range []core.Person{second, console} {
		conn, _ := connectThrough(t, procs.dial(t, p.ListenAddr(), who), target, &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		if resp, _ := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("the request was not carried: %d", resp.StatusCode)
		}
		_ = conn.Close()
	}
	waitFor(t, 2*time.Second, "both observations", func() bool { return len(pipe.observations()) == 2 })
	for i, want := range []core.Person{second, console} {
		got := pipe.observations()[i].Person
		if got == nil || *got != want {
			t.Fatalf("observation %d person = %+v, want the dialling process's owner %+v", i, got, want)
		}
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterErrors] != 0 {
		t.Fatalf("errors = %d, want 0 when every client is attributed", c[protocol.CounterErrors])
	}
}

// When the dialling process cannot be named, the request is still carried and observed, attributed
// to the pipeline's identity (the console user), and the failure is counted once.
func TestTLSUnattributedClientFallsBackToTheConsoleUser(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	procs := &fakeProcessTable{owners: map[string]core.Person{}}
	pipe := &fakePipeline{mode: protocol.ModeM1}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
		Person: procs.person,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
	defer conn.Close()
	if resp, _ := postThroughTunnel(t, conn, "127.0.0.1", `{"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("an attribution failure blocked the client: %d", resp.StatusCode)
	}
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 1 })
	if got := pipe.observations()[0].Person; got != nil {
		t.Fatalf("person = %+v, want nil so the pipeline attributes to the console user", got)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterErrors] != 1 || c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want errors=1 for the one failed attribution and emitted=1", c)
	}
	procs.mu.Lock()
	calls := procs.calls
	procs.mu.Unlock()
	if calls != 1 {
		t.Fatalf("attribution ran %d times for one request, want once", calls)
	}
}

// Healthy requires the end-to-end probe. Without one the provider reports tls_probe_failed
// rather than claiming health on the strength of a successful bind.
func TestTLSHealthNeverHealthyWithoutTheProbe(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline:      &fakePipeline{mode: protocol.ModeM1},
		UpstreamRoots: upstreamPool(upstream), // no canary configured
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := p.Health()
	if h.State == protocol.StateHealthy {
		t.Fatal("healthy without a successful end-to-end probe through a minted leaf")
	}
	if h.State != protocol.StateDegraded || h.Detail != protocol.DetailTLSProbeFailed {
		t.Fatalf("health = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailTLSProbeFailed)
	}
}

// The device CA is per device, and its leaves are short-lived, cover their hostname and are cached
// per host.
func TestCAIsPerDeviceAndMintsShortLivedLeaves(t *testing.T) {
	ca, err := NewCA("device-1", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("NewCA: %v", err)
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

// ---- trust-store installation -----------------------------------------------------------

// fakeVerifyingTrust records an install and then answers the store read-back. A store that does
// not confirm the certificate is the silent wrong-store case, so it must surface
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

// The end-to-end probe uses the in-process pool, so it can succeed even when the OS store
// was never touched. The store verification is what makes that silent failure visible in health.
func TestTLSTrustStoreFailureIsNamedInHealth(t *testing.T) {
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

// A kill switch in the bundle already in force when the provider starts must suppress it
// before it binds or installs a root. The supervisor applies the bundle before any provider
// starts, so a switch that is only honored in ApplyPolicy would be silently defeated at startup.
func TestTLSKillSwitchInInitialBundleSuppressesStart(t *testing.T) {
	canaryPort := freePort(t)
	b := bundleIntercepting(canaryPort)
	ksAt := time.Unix(1_600_000_000, 0)
	b.KillSwitches = []policy.KillSwitch{{
		Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable,
		EffectiveAt: ksAt, ReasonCode: "fleet_regression",
	}}
	trust := &fakeTrust{}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return b },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, TrustRoot: trust,
		CanaryHost: "127.0.0.1", CanaryPort: canaryPort,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.ListenAddr() != "" {
		t.Fatal("the proxy bound despite a kill switch in the initial bundle")
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("health = %s/%s, want absent/killed", h.State, h.Detail)
	}
	if len(trust.installed) != 0 {
		t.Fatal("the trust root was installed despite the kill switch")
	}
}

// EffectiveAt is when the operator asked enforcement to stop; a future-dated switch must not
// fire early.
func TestTLSFutureKillSwitchDoesNotFireEarly(t *testing.T) {
	b := bundleIntercepting(443)
	b.KillSwitches = []policy.KillSwitch{{
		Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable,
		EffectiveAt: time.Now().Add(time.Hour), ReasonCode: "scheduled",
	}}
	p := New(Config{Bundles: func() *policy.Bundle { return b }, Clock: time.Now})
	if p.killSwitchActive() {
		t.Fatal("a future-dated kill switch fired early")
	}
}

// The proxy is a policy toggle: it is enabled only by a bundle whose interception.enabled is true.
func TestTLSProxyIsEnabledByTLSInspection(t *testing.T) {
	var p core.Provider = New(Config{})
	toggled, ok := p.(core.Toggled)
	if !ok {
		t.Fatal("proxy.tls is not a policy toggle")
	}
	on := bundleIntercepting(443)
	on.Interception.Enabled = true
	if toggled.Enabled(nil) || toggled.Enabled(bundleIntercepting(443)) || !toggled.Enabled(on) {
		t.Fatal("proxy.tls is not enabled exactly by interception.enabled")
	}
}

// A Stop removes the root it installed and closes the listener; a Start after it binds and installs
// again, and the proxy is healthy, as a policy toggle off and on needs.
func TestTLSStopRemovesTheRootAndAStartBringsItBack(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port
	trustRoot := &fakeTrust{}
	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundleIntercepting(upPort) },
		Pipeline: &fakePipeline{mode: protocol.ModeM1}, TrustRoot: trustRoot,
		UpstreamRoots: upstreamPool(upstream), CanaryHost: "127.0.0.1", CanaryPort: upPort,
	})
	ctx := context.Background()

	for round := 1; round <= 2; round++ {
		if err := p.Start(ctx); err != nil {
			t.Fatalf("round %d Start: %v", round, err)
		}
		if p.ListenAddr() == "" || len(trustRoot.installed) == 0 {
			t.Fatalf("round %d: listening %q, root installed %t", round, p.ListenAddr(), len(trustRoot.installed) > 0)
		}
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("round %d health = %s/%s, want healthy", round, h.State, h.Detail)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("round %d Stop: %v", round, err)
		}
		if p.ListenAddr() != "" {
			t.Fatalf("round %d: still listening after Stop", round)
		}
		if trustRoot.removes != round {
			t.Fatalf("round %d: TrustRoot.Remove called %d times, want %d", round, trustRoot.removes, round)
		}
		if h := p.Health(); h.State != protocol.StateAbsent {
			t.Fatalf("round %d health after Stop = %s, want absent", round, h.State)
		}
	}
}

// A proxy stopped without ever starting still removes the root, so a root an earlier run left is
// not kept trusted while inspection is off.
func TestTLSStopWithoutStartRemovesTheRoot(t *testing.T) {
	trustRoot := &fakeTrust{}
	p := New(Config{TrustRoot: trustRoot})
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if trustRoot.removes != 1 || len(trustRoot.installed) != 0 {
		t.Fatalf("removes %d, installed %d bytes; want one removal and nothing installed", trustRoot.removes, len(trustRoot.installed))
	}
}
