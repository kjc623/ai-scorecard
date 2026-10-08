package loopback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ---- test upstream ----------------------------------------------------------------------

// stubUpstream is a relocated local inference server on an ephemeral loopback port. It is
// created explicitly (rather than with httptest) so a test can close it, watch the broker
// release, and bring it back on the same port: crash and recover.
type stubUpstream struct {
	mu       sync.Mutex
	ln       net.Listener
	port     int
	requests []recordedRequest
	srv      *http.Server
}

type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	u := &stubUpstream{}
	if err := u.listen(); err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	t.Cleanup(u.Close)
	return u
}

func (u *stubUpstream) listen() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.ln = ln
	u.port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	u.srv = srv
	u.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (u *stubUpstream) Port() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.port
}

func (u *stubUpstream) Close() {
	u.mu.Lock()
	srv := u.srv
	u.srv = nil
	u.ln = nil
	u.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Reopen restarts the server on the port it had before, which is what "the user restarts the
// server" means.
func (u *stubUpstream) Reopen(t *testing.T) {
	t.Helper()
	u.mu.Lock()
	port := u.port
	u.mu.Unlock()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("reopen upstream on %d: %v", port, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	u.mu.Lock()
	u.ln = ln
	u.srv = srv
	u.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

func (u *stubUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// countPath counts requests the *broker* made, which excludes the preflight's own request:
// the preflight is a real request to the upstream and is expected to appear here.
func (u *stubUpstream) countPath(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, r := range u.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

func (u *stubUpstream) lastBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return nil
	}
	return u.requests[len(u.requests)-1].Body
}

// freePort returns a port that was free a moment ago. It is policy data in the test, which is
// what lets the whole suite run on ephemeral ports instead of the vendor defaults.
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

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- fake pipeline ----------------------------------------------------------------------

type recordingPipeline struct {
	mu     sync.Mutex
	mode   protocol.CollectionMode
	obs    []core.Observation
	read   bool
	over   bool
	failOn bool
	done   int // Process calls that have returned
}

func (p *recordingPipeline) ResolveMode(core.ScopeQuery) core.Resolution {
	return core.Resolution{Mode: p.mode, PolicyVersion: "test"}
}

func (p *recordingPipeline) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	defer func() {
		p.mu.Lock()
		p.done++
		p.mu.Unlock()
	}()
	p.mu.Lock()
	p.obs = append(p.obs, obs)
	p.mu.Unlock()
	out := core.Outcome{Route: obs.Route, Mode: p.mode, Emitted: true, Reason: core.ReasonEmitted}
	if p.mode.ReadsContent() && obs.Content != nil {
		if _, err := obs.Content.Read(ctx); err != nil {
			return out, err
		}
		p.mu.Lock()
		p.read = true
		p.mu.Unlock()
	}
	if obs.OverCap {
		p.mu.Lock()
		p.over = true
		p.mu.Unlock()
	}
	return out, nil
}

func (p *recordingPipeline) observations() []core.Observation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Observation(nil), p.obs...)
}

// processed is the number of observations whose Process call has returned: the observation is
// recorded on entry, but the content read and the broker's counters follow it.
func (p *recordingPipeline) processed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *recordingPipeline) didRead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.read
}

func testConfig(upstreamPort, heldPort int, pipe Pipeline) Config {
	return Config{
		Ports: []policy.LoopbackPort{{
			ToolFingerprint: "local_inference",
			Port:            heldPort,
			UpstreamPort:    upstreamPort,
			PreflightPath:   "/health",
			Mode:            protocol.ModeM1,
		}},
		ProbeInterval:          25 * time.Millisecond,
		PreflightInterval:      200 * time.Millisecond,
		PreflightTimeout:       250 * time.Millisecond,
		MaxConsecutiveFailures: 3,
		CoolDown:               300 * time.Millisecond,
		BackoffBase:            20 * time.Millisecond,
		BackoffMax:             60 * time.Millisecond,
		Pipeline:               pipe,
	}
}

// TestBroker_6_2_NeverBoundWithoutServingUpstream is the invariant the whole provider exists
// for: with no reachable upstream the port is *not* bound, so a client gets connection-refused
// rather than a broker that accepts and fails.
func TestBrokerNeverBoundWithoutServingUpstream(t *testing.T) {
	deadUpstream := freePort(t) // nothing is listening there
	held := freePort(t)
	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(deadUpstream, held, pipe))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())

	waitFor(t, 2*time.Second, "the broker to give up binding", func() bool {
		return !portOpen(held)
	})
	if portOpen(held) {
		t.Fatal("the broker bound a port whose upstream is unreachable")
	}
	configured, heldN, reachable := b.Coverage()
	if configured != 1 || heldN != 0 || reachable != 0 {
		t.Fatalf("coverage = (%d,%d,%d), want (1,0,0)", configured, heldN, reachable)
	}
	h := b.Health()
	if h.State != protocol.StateDegraded {
		t.Fatalf("health state = %q, want degraded (not absent: the provider is running)", h.State)
	}
	if h.Detail != protocol.DetailUpstreamUnreachable {
		t.Fatalf("detail = %q, want %q", h.Detail, protocol.DetailUpstreamUnreachable)
	}
}

func TestBrokerBindsAfterPreflightAndBrokersRequests(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(upstream.Port(), held, pipe))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())

	waitFor(t, 3*time.Second, "the broker to hold the port", func() bool { return portOpen(held) })

	// The client believes it is talking to the local server; the broker forwards.
	body := `{"model":"local","messages":[{"role":"user","content":"Summarise the attached contract."}]}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", held), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST through the broker: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(got, []byte(`"ok":true`)) {
		t.Fatalf("response through the broker = %d %q", resp.StatusCode, got)
	}
	if n := upstream.countPath("/v1/chat/completions"); n != 1 {
		t.Fatalf("upstream saw %d brokered chat requests, want 1", n)
	}
	if string(upstream.lastBody()) != body {
		t.Fatalf("upstream saw body %q, want the client's bytes unchanged", upstream.lastBody())
	}

	waitFor(t, 2*time.Second, "the pipeline to finish processing the observation", func() bool {
		return pipe.processed() == 1 && b.Counters().Cumulative()[protocol.CounterEmitted] == 1
	})
	obs := pipe.observations()[0]
	if obs.ToolFingerprint != "local_inference" {
		t.Fatalf("tool fingerprint = %q", obs.ToolFingerprint)
	}
	if obs.SizeBytes != int64(len(body)) {
		t.Fatalf("size_bytes = %d, want %d", obs.SizeBytes, len(body))
	}
	if !pipe.didRead() {
		t.Fatal("content was forwarded but never handed to the pipeline at M1")
	}
	if obs.Decision == nil {
		t.Fatal("the broker emitted an observation with no policy decision; policy_decision is required for every prompt")
	}
	c := b.Counters().Cumulative()
	if c[protocol.CounterObserved] != 1 || c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want observed=1 emitted=1", c)
	}
	if h := b.Health(); h.State != protocol.StateHealthy || h.Detail != protocol.DetailNone {
		t.Fatalf("health = %q/%q, want healthy with no detail (port held and upstream reachable)", h.State, h.Detail)
	}
}

// At M0 the body is forwarded but never retained, so the pipeline gets no
// content reader at all.
func TestBrokerM0ForwardsWithoutRetainingTheBody(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	pipe := &recordingPipeline{mode: protocol.ModeM0}
	b := New(testConfig(upstream.Port(), held, pipe))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())
	waitFor(t, 3*time.Second, "the broker to hold the port", func() bool { return portOpen(held) })

	body := `{"messages":[{"role":"user","content":"secret payload"}]}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", held), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST through the broker: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	waitFor(t, 2*time.Second, "the observation", func() bool { return len(pipe.observations()) == 1 })
	obs := pipe.observations()[0]
	if obs.Content != nil {
		t.Fatal("the broker retained a content reader at M0")
	}
	if pipe.didRead() {
		t.Fatal("content was read at M0")
	}
	if obs.SizeBytes != int64(len(body)) {
		t.Fatalf("size_bytes = %d, want %d (size is available at M0)", obs.SizeBytes, len(body))
	}
	if string(upstream.lastBody()) != body {
		t.Fatal("the request body did not reach the upstream unchanged")
	}
}

// The upstream crashes, the broker releases, the user restarts the server, the broker
// re-binds. The invariant across the whole sequence is that the port is never bound while the
// upstream is down.
func TestBrokerUpstreamCrashReleasesAndRecoveryRebinds(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(upstream.Port(), held, pipe))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())
	waitFor(t, 3*time.Second, "the broker to hold the port", func() bool { return portOpen(held) })

	upstream.Close()
	waitFor(t, 5*time.Second, "the broker to release the port", func() bool { return !portOpen(held) })
	if h := b.Health(); h.State != protocol.StateDegraded {
		t.Fatalf("state after upstream crash = %q, want degraded", h.State)
	}
	if h := b.Health(); h.Detail != protocol.DetailUpstreamUnreachable && h.Detail != protocol.DetailCoolingDown {
		t.Fatalf("detail after upstream crash = %q, want upstream_unreachable or cooling_down", h.Detail)
	}

	upstream.Reopen(t)
	waitFor(t, 5*time.Second, "the broker to re-bind after recovery", func() bool { return portOpen(held) })
	configured, heldN, reachable := b.Coverage()
	if configured != 1 || heldN != 1 || reachable != 1 {
		t.Fatalf("coverage after recovery = (%d,%d,%d), want (1,1,1)", configured, heldN, reachable)
	}
}

// When a port conflict ends, the health row and the coverage row must agree about the same port:
// a broker that recovered, re-bound and served a request must not still say tampered, the only state
// that raises a security finding.
func TestBrokerRecoveredPortConflictIsNotTamperedAndCoverageAgrees(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)

	holder, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", held))
	if err != nil {
		t.Fatalf("hold the port: %v", err)
	}

	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(upstream.Port(), held, pipe))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())

	waitFor(t, 5*time.Second, "the broker to report the tampered port", func() bool {
		return b.Health().State == protocol.StateTampered
	})
	if b.PortConflicts() == 0 {
		t.Fatal("a conflict was reported as tampered but not counted")
	}

	// The conflict ends: the holder releases the port.
	if err := holder.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	waitFor(t, 10*time.Second, "the broker to recover and hold the port", func() bool {
		configured, heldN, reachable := b.Coverage()
		return configured == 1 && heldN == 1 && reachable == 1
	})

	h := b.Health()
	if h.State == protocol.StateTampered {
		t.Fatalf("after recovery the health row still says tampered (detail=%q) while coverage says held=1 reachable=1: a false security finding on a working port", h.Detail)
	}
	if h.Detail == protocol.DetailPortHeldByOther {
		t.Fatalf("after recovery the health detail still names the ended conflict: %q", h.Detail)
	}
	if h.State != protocol.StateHealthy {
		t.Fatalf("health after recovery = %s/%s, want healthy", h.State, h.Detail)
	}
	// The history survives the recovery, on the coverage row rather than in the state.
	if b.PortConflicts() != 1 {
		t.Fatalf("conflict count = %d, want the ended conflict to remain observable as history", b.PortConflicts())
	}
	// And the port is genuinely serving: a request through it reaches the upstream.
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", held), "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST through the recovered broker: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if upstream.countPath("/v1/chat/completions") != 1 {
		t.Fatal("the recovered broker did not forward the request")
	}
	if state := b.Health().State; state == protocol.StateTampered {
		t.Fatalf("serving a request changed nothing: health is %s again", state)
	}
}

// If something is listening that is not the expected upstream, the broker does not
// bind, does not kill the holder, and reports tampered with detail=port_held_by_other.
func TestBrokerPortHeldByOtherIsTamperedAndNeverFoughtFor(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)

	// The user's own server, back on its default port.
	holder, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", held))
	if err != nil {
		t.Fatalf("hold the port: %v", err)
	}
	defer holder.Close()
	holderAccepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := holder.Accept()
			if err != nil {
				return
			}
			holderAccepted <- struct{}{}
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi"))
			_ = c.Close()
		}
	}()

	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(upstream.Port(), held, pipe))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())

	waitFor(t, 3*time.Second, "the broker to report the tampered port", func() bool {
		return b.Health().State == protocol.StateTampered
	})
	h := b.Health()
	if h.Detail != protocol.DetailPortHeldByOther {
		t.Fatalf("conflict detail = %q, want %q", h.Detail, protocol.DetailPortHeldByOther)
	}
	_, heldN, _ := b.Coverage()
	if heldN != 0 {
		t.Fatalf("broker claims %d held ports while another process holds it", heldN)
	}

	// The holder is untouched: still listening, still serving.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", held), time.Second)
	if err != nil {
		t.Fatalf("the broker killed the holder's listener: %v", err)
	}
	_ = conn.Close()
	select {
	case <-holderAccepted:
	case <-time.After(time.Second):
		t.Fatal("the holder's listener stopped accepting")
	}
}

func TestBrokerStopReleasesThePortAndIsIdempotent(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	b := New(testConfig(upstream.Port(), held, &recordingPipeline{mode: protocol.ModeM1}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "holding", func() bool { return portOpen(held) })

	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if portOpen(held) {
		t.Fatal("the port is still bound after Stop")
	}
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop is not idempotent: %v", err)
	}
	if h := b.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after Stop = %q, want absent", h.State)
	}

	// The port is genuinely free: another process can take it.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", held))
	if err != nil {
		t.Fatalf("the port was not released for reuse: %v", err)
	}
	_ = ln.Close()
}

// The release happens before anything else on shutdown, and it happens while the
// provider is still running.
func TestBrokerReleaseIsSeparateFromStop(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	b := New(testConfig(upstream.Port(), held, &recordingPipeline{mode: protocol.ModeM1}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "holding", func() bool { return portOpen(held) })

	if err := b.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if portOpen(held) {
		t.Fatal("Release left the port bound")
	}
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop after Release: %v", err)
	}
}

// Repeated failure past a threshold stops trying for a long cool-down, so a
// broken configuration is not an endless bind/release loop against the user's machine.
func TestBrokerCoolDownAfterRepeatedFailures(t *testing.T) {
	dead := freePort(t)
	held := freePort(t)
	cfg := testConfig(dead, held, &recordingPipeline{mode: protocol.ModeM1})
	cfg.MaxConsecutiveFailures = 2
	cfg.CoolDown = 5 * time.Second
	cfg.BackoffBase = 10 * time.Millisecond
	b := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())

	waitFor(t, 3*time.Second, "the cool-down", func() bool {
		return b.Health().Detail == protocol.DetailCoolingDown
	})
	if h := b.Health(); h.State != protocol.StateDegraded {
		t.Fatalf("state during cool-down = %q, want degraded", h.State)
	}
	if portOpen(held) {
		t.Fatal("the broker bound the port while cooling down")
	}
}

// A non-submission (a GET, or a POST with no body) is counted as skipped_not_generative: the
// predicate is running, and the counter proves it without claiming it is right.
func TestBrokerSkipsNonGenerativeRequestsAndCountsThem(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	pipe := &recordingPipeline{mode: protocol.ModeM1}
	b := New(testConfig(upstream.Port(), held, pipe))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(context.Background())
	waitFor(t, 3*time.Second, "holding", func() bool { return portOpen(held) })

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", held))
	if err != nil {
		t.Fatalf("GET through the broker: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if n := upstream.countPath("/v1/models"); n != 1 {
		t.Fatal("a non-generative request was not forwarded")
	}
	waitFor(t, time.Second, "the skip counter", func() bool {
		return b.Counters().Cumulative()[protocol.CounterSkippedNotGenerative] == 1
	})
	if len(pipe.observations()) != 0 {
		t.Fatal("a GET produced an observation")
	}
}

// The JSON extractor takes the last user-role message, or a top-level prompt.
func TestJSONExtractorLastUserTurn(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"text","text":"second"}]}]}`)
	text, atts, err := JSONExtractor{}.Extract(body, "application/json")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if text != "second" {
		t.Fatalf("text = %q, want the last user turn", text)
	}
	if len(atts) != 0 {
		t.Fatalf("attachments = %v, want none", atts)
	}
	if _, _, err := (JSONExtractor{}).Extract([]byte(`{"nope":true}`), "application/json"); err == nil {
		t.Fatal("a body with no identifiable user-authored segment must not be guessed at")
	}
}

// The canonicalisation seam is exercised here too: the broker's extractor feeds the same
// dedup.ContentDigest the pipeline uses, so the two cannot drift.
func TestBrokerExtractionFeedsCanonicalDigest(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	text, atts, err := JSONExtractor{}.Extract(body, "application/json")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	d1 := dedup.ContentDigest(text, atts)
	d2 := dedup.ContentDigest("hello world", nil)
	if d1 != d2 {
		t.Fatalf("digest through the extractor = %s, want %s", d1, d2)
	}
}

func TestBrokerHealthNeverHealthyWhenStopped(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	b := New(testConfig(upstream.Port(), held, &recordingPipeline{mode: protocol.ModeM1}))
	if h := b.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health before Start = %q, want absent", h.State)
	}
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "holding", func() bool { return portOpen(held) })
	_ = b.Stop(ctx)
	if h := b.Health(); h.State == protocol.StateHealthy {
		t.Fatal("Health reported healthy after Stop")
	}
}

var _ = json.Marshal // keep encoding/json imported for the body literals above
