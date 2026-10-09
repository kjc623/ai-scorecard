package tlsproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	ruleMessage = "Remove the credential and try again."
	ruleLink    = "https://intranet.example/ai"
)

// fakeHelper stands in for the user-session helper: every connection's process runs in session 7.
type fakeHelper struct {
	mu       sync.Mutex
	err      error
	sessions []uint32
	shown    []protocol.Notify
}

func (h *fakeHelper) session(conn net.Conn) (uint32, error) {
	if conn == nil {
		return 0, errors.New("no connection")
	}
	return 7, nil
}

func (h *fakeHelper) notify(session uint32, n protocol.Notify) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = append(h.sessions, session)
	h.shown = append(h.shown, n)
	return h.err
}

func (h *fakeHelper) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

func (h *fakeHelper) notifications() ([]uint32, []protocol.Notify) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uint32(nil), h.sessions...), append([]protocol.Notify(nil), h.shown...)
}

// enforcing is a started proxy in front of an upstream that counts the requests it receives.
type enforcing struct {
	p        *Provider
	pipe     *fakePipeline
	helper   *fakeHelper
	upstream *atomic.Int32
	target   string

	mu     sync.Mutex
	bundle *policy.Bundle
}

func startEnforcing(t *testing.T, mode protocol.CollectionMode, labels []string, rules ...policy.Rule) *enforcing {
	t.Helper()
	e := &enforcing{
		pipe:     &fakePipeline{mode: mode, labels: labels, extract: true},
		helper:   &fakeHelper{},
		upstream: &atomic.Int32{},
	}
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.upstream.Add(1)
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	port := up.Listener.Addr().(*net.TCPAddr).Port
	e.target = fmt.Sprintf("127.0.0.1:%d", port)
	e.bundle = bundleIntercepting(port)
	e.bundle.Rules = rules
	e.p = newProviderForTest(t, Config{
		Listen:        "127.0.0.1:0",
		Bundles:       e.currentBundle,
		Pipeline:      e.pipe,
		UpstreamRoots: upstreamPool(up),
		CanaryHost:    "127.0.0.1",
		CanaryPort:    port,
		BodyCap:       1 << 20,
		Session:       e.helper.session,
		Notify:        e.helper.notify,
	})
	if err := e.p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return e
}

func (e *enforcing) currentBundle() *policy.Bundle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bundle
}

func (e *enforcing) setBundle(b *policy.Bundle) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bundle = b
}

// post sends one request through the proxy to the upstream, with the API's host in the Host header
// so its parser is chosen, and returns the response the client sees.
func (e *enforcing) post(t *testing.T, host, path, headers, body string) (*http.Response, string) {
	t.Helper()
	return e.send(t, e.dial(t), host, path, headers, body)
}

// dial opens an intercepted connection through the proxy.
func (e *enforcing) dial(t *testing.T) *tls.Conn {
	t.Helper()
	conn, _ := dialThroughProxy(t, e.p.ListenAddr(), e.target, &tls.Config{ServerName: "127.0.0.1", RootCAs: e.p.CA().Pool()})
	return conn
}

// send sends one request over an intercepted connection and closes it.
func (e *enforcing) send(t *testing.T, conn *tls.Conn, host, path, headers, body string) (*http.Response, string) {
	t.Helper()
	defer conn.Close()
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n%s\r\n%s", path, host, headers, body)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(got)
}

func contentLength(body string) string { return fmt.Sprintf("Content-Length: %d\r\n", len(body)) }

func blockCredentials() policy.Rule {
	return policy.Rule{RuleID: "block_credentials", Action: policy.RuleBlock, Message: ruleMessage, Link: ruleLink,
		Match: policy.RuleMatch{Labels: []string{"credential"}}}
}

func decided(rule, action string) protocol.Decision {
	return protocol.Decision{RuleID: rule, Action: action, DecidedLocally: true}
}

// waitForNotification waits for the helper to have been asked to show want in session 7.
func waitForNotification(t *testing.T, h *fakeHelper, want protocol.Notify) {
	t.Helper()
	waitFor(t, 2*time.Second, "the notification", func() bool {
		_, shown := h.notifications()
		return len(shown) > 0
	})
	sessions, shown := h.notifications()
	if len(shown) != 1 || shown[0] != want || sessions[0] != 7 {
		t.Fatalf("notifications = %+v in sessions %v, want %+v in session 7", shown, sessions, want)
	}
}

// A block rule over the body's labels is answered with 403 in the API's own error shape, the
// request never reaches the upstream, the event records blocked, and the person whose process
// sent it is notified.
func TestTLSBlockRuleAnswersInTheAPIShapeAndForwardsNothing(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	cases := []struct {
		api, host, path, body, want string
	}{
		{
			"anthropic", "api.anthropic.com", "/v1/messages",
			`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"my key is ` + secret + `"}]}`,
			`{"type":"error","error":{"type":"permission_error","message":"` + ruleMessage + ` ` + ruleLink + `"}}`,
		},
		{
			"openai", "api.openai.com", "/v1/chat/completions",
			`{"model":"gpt-4.1","messages":[{"role":"user","content":"my key is ` + secret + `"}]}`,
			`{"error":{"message":"` + ruleMessage + ` ` + ruleLink + `","type":"policy_violation","param":null,"code":null}}`,
		},
		{
			"gemini", "generativelanguage.googleapis.com", "/v1beta/models/gemini-flash-latest:generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"my key is ` + secret + `"}]}]}`,
			`{"error":{"code":403,"message":"` + ruleMessage + ` ` + ruleLink + `","status":"PERMISSION_DENIED"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.api, func(t *testing.T) {
			e := startEnforcing(t, protocol.ModeM1, []string{"credential"}, blockCredentials())
			resp, got := e.post(t, c.host, c.path, contentLength(c.body), c.body)
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %q, want 403 application/json", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if got != c.want {
				t.Fatalf("body = %s\nwant   %s", got, c.want)
			}
			if n := e.upstream.Load(); n != 0 {
				t.Fatalf("the upstream received %d requests, want none", n)
			}
			waitFor(t, 2*time.Second, "the recorded decision", func() bool { return len(e.pipe.recorded()) == 1 })
			if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("block_credentials", protocol.ActionBlocked) {
				t.Fatalf("recorded decisions = %+v, want blocked", d)
			}
			waitForNotification(t, e.helper, protocol.Notify{Title: "Shadow AI Capture", Body: ruleMessage, Link: ruleLink})
		})
	}
}

// At M0 nothing is read, so no label rule can match, but a rule on the route still blocks: the
// body is neither held nor forwarded, and its size, here of unknown length, is still recorded.
func TestTLSBlockAtM0ReadsNothingAndForwardsNothing(t *testing.T) {
	route := policy.Rule{RuleID: "block_proxy", Action: policy.RuleBlock, Message: ruleMessage,
		Match: policy.RuleMatch{Routes: []protocol.Route{protocol.RouteProxyTLS}}}
	e := startEnforcing(t, protocol.ModeM0, nil, route)
	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	chunked := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	resp, got := e.post(t, "api.anthropic.com", "/v1/messages", "Transfer-Encoding: chunked\r\n", chunked)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if want := `{"type":"error","error":{"type":"permission_error","message":"` + ruleMessage + `"}}`; got != want {
		t.Fatalf("body = %s", got)
	}
	if n := e.upstream.Load(); n != 0 {
		t.Fatalf("the upstream received %d requests, want none", n)
	}
	// The answer goes out before the discarded body's size is known, so the observation is awaited.
	waitFor(t, 2*time.Second, "the observation", func() bool { return len(e.pipe.observations()) == 1 })
	obs := e.pipe.observations()
	if len(obs) != 1 || obs[0].Content != nil || obs[0].SizeBytes != int64(len(body)) {
		t.Fatalf("observations = %+v, want one without content and of size %d", obs, len(body))
	}
	if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("block_proxy", protocol.ActionBlocked) {
		t.Fatalf("recorded decisions = %+v, want blocked", d)
	}
	waitForNotification(t, e.helper, protocol.Notify{Title: "Shadow AI Capture", Body: ruleMessage})
}

// A warning rule carries the request, records warned, and notifies once it is forwarded.
func TestTLSWarnRuleForwardsThenNotifies(t *testing.T) {
	warn := policy.Rule{RuleID: "warn_credentials", Action: policy.RuleWarn, Message: ruleMessage, Link: ruleLink,
		Match: policy.RuleMatch{Labels: []string{"credential"}}}
	e := startEnforcing(t, protocol.ModeM1, []string{"credential"}, warn)
	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"my key is AKIAIOSFODNN7EXAMPLE"}]}`
	resp, got := e.post(t, "api.openai.com", "/v1/chat/completions", contentLength(body), body)
	if resp.StatusCode != http.StatusOK || got != `{"ok":true}` {
		t.Fatalf("response = %d %q, want the upstream's", resp.StatusCode, got)
	}
	if n := e.upstream.Load(); n != 1 {
		t.Fatalf("the upstream received %d requests, want 1", n)
	}
	if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("warn_credentials", protocol.ActionWarned) {
		t.Fatalf("recorded decisions = %+v, want warned", d)
	}
	waitForNotification(t, e.helper, protocol.Notify{Title: "Shadow AI Capture", Body: ruleMessage, Link: ruleLink})
}

// A destination only the generic parser reads has no error shape its client displays, so a block
// is recorded as logged, the request is carried, and nobody is notified.
func TestTLSGenericDestinationRecordsABlockAsLogged(t *testing.T) {
	e := startEnforcing(t, protocol.ModeM1, []string{"credential"}, blockCredentials())
	body := `{"messages":[{"role":"user","content":"my key is AKIAIOSFODNN7EXAMPLE"}]}`
	resp, _ := e.post(t, "llm.example.com", "/v1/chat/completions", contentLength(body), body)
	if resp.StatusCode != http.StatusOK || e.upstream.Load() != 1 {
		t.Fatalf("response = %d with %d upstream requests, want the request carried", resp.StatusCode, e.upstream.Load())
	}
	if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("block_credentials", protocol.ActionLogged) {
		t.Fatalf("recorded decisions = %+v, want logged", d)
	}
	time.Sleep(50 * time.Millisecond)
	if _, shown := e.helper.notifications(); len(shown) != 0 {
		t.Fatalf("notified %+v for a request the proxy did not act on", shown)
	}
}

// A kill switch on proxy.tls that comes into force on a connection already intercepted disables
// enforcement for the request still to be decided on it: a block is recorded as logged and the
// request carried. (Every later connection is tunnelled blind.)
func TestTLSKillSwitchRecordsEveryRuleAsLogged(t *testing.T) {
	e := startEnforcing(t, protocol.ModeM1, []string{"credential"}, blockCredentials())
	conn := e.dial(t)
	killed := *e.currentBundle()
	killed.KillSwitches = []policy.KillSwitch{{Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable, ReasonCode: "fleet_regression_1234"}}
	e.setBundle(&killed)

	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"my key is AKIAIOSFODNN7EXAMPLE"}]}`
	resp, _ := e.send(t, conn, "api.anthropic.com", "/v1/messages", contentLength(body), body)
	if resp.StatusCode != http.StatusOK || e.upstream.Load() != 1 {
		t.Fatalf("response = %d with %d upstream requests, want the request carried", resp.StatusCode, e.upstream.Load())
	}
	if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("block_credentials", protocol.ActionLogged) {
		t.Fatalf("recorded decisions = %+v, want logged", d)
	}
	time.Sleep(50 * time.Millisecond)
	if _, shown := e.helper.notifications(); len(shown) != 0 {
		t.Fatalf("notified %+v under a kill switch", shown)
	}
}

// A notification that cannot be shown changes nothing about the block; it is counted as an error.
func TestTLSNotifyFailureKeepsTheBlock(t *testing.T) {
	e := startEnforcing(t, protocol.ModeM1, []string{"credential"}, blockCredentials())
	e.helper.fail(errors.New("no helper in the session"))
	errorsNow := func() uint64 { return e.p.Counters().Cumulative()[protocol.CounterErrors] }
	// The startup probe's connection sends no request, which counts one error of its own.
	waitFor(t, 2*time.Second, "the probe's error", func() bool { return errorsNow() == 1 })

	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"my key is AKIAIOSFODNN7EXAMPLE"}]}`
	resp, _ := e.post(t, "api.anthropic.com", "/v1/messages", contentLength(body), body)
	if resp.StatusCode != http.StatusForbidden || e.upstream.Load() != 0 {
		t.Fatalf("response = %d with %d upstream requests, want the block", resp.StatusCode, e.upstream.Load())
	}
	if d := e.pipe.recorded(); len(d) != 1 || d[0] != decided("block_credentials", protocol.ActionBlocked) {
		t.Fatalf("recorded decisions = %+v, want blocked", d)
	}
	waitFor(t, 2*time.Second, "the failed notification to be counted", func() bool { return errorsNow() == 2 })
}
