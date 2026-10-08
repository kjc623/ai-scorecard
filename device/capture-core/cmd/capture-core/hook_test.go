package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// testHookInput is the test adapter's input for one prompt to claude_code.
func testHookInput(t *testing.T, prompt string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"tool": "claude_code", "session_id": "session-1", "prompt_text": prompt})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// allowOutput is what the test adapter prints when the hook fails open.
func allowOutput(t *testing.T) string {
	t.Helper()
	a, _ := hooks.Lookup(hooks.TestTool)
	out, code := a.Allow("prompt")
	if code != 0 {
		t.Fatalf("the test adapter's allow exits %d", code)
	}
	return string(out)
}

// runTestHook runs hook mode with the test adapter and returns its output, exit code and how long it
// took.
func runTestHook(t *testing.T, stdin io.Reader, dial func(context.Context) (net.Conn, error)) (string, int, time.Duration) {
	t.Helper()
	var stdout bytes.Buffer
	start := time.Now()
	code := runHook(start, []string{hooks.TestTool, "prompt"}, stdin, &stdout, dial)
	return stdout.String(), code, time.Since(start)
}

// fakeHookService listens on a private endpoint and answers each connection's first frame with
// answer; nil reads the frame and never answers.
func fakeHookService(t *testing.T, answer func(frame []byte) []byte) func(context.Context) (net.Conn, error) {
	t.Helper()
	addr := testNativeAddr()
	if !strings.HasPrefix(addr, `\\.\pipe\`) {
		if err := os.MkdirAll(filepath.Dir(addr), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(addr)) })
	}
	ln, err := localipc.Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				frame, err := localipc.ReadFrame(conn)
				if err != nil {
					return
				}
				if answer == nil {
					<-stop
					return
				}
				_ = localipc.WriteFrame(conn, answer(frame))
			}()
		}
	}()
	return func(ctx context.Context) (net.Conn, error) { return dialOwnAccount(ctx, addr) }
}

func failDial(t *testing.T, dialled *atomic.Int32) func(context.Context) (net.Conn, error) {
	t.Helper()
	return func(context.Context) (net.Conn, error) {
		dialled.Add(1)
		return nil, errors.New("not dialled in this test")
	}
}

// With no service listening the hook prints the allow output at once.
func TestHookFailsOpenWithoutAService(t *testing.T) {
	addr := testNativeAddr()
	dial := func(ctx context.Context) (net.Conn, error) { return dialOwnAccount(ctx, addr) }
	out, code, took := runTestHook(t, strings.NewReader(testHookInput(t, "hello")), dial)
	if out != allowOutput(t) || code != 0 {
		t.Fatalf("hook printed %q and exited %d, want the allow output and 0", out, code)
	}
	if took >= hookDeadline {
		t.Fatalf("a missing service took %v to fail open", took)
	}
}

// A service that does not answer, and a tool that never closes stdin, leave the hook at its 400 ms
// deadline, which it does not overrun.
func TestHookFailsOpenAtItsDeadline(t *testing.T) {
	stalled, stallWriter := io.Pipe()
	t.Cleanup(func() { _ = stallWriter.Close() })
	for name, tc := range map[string]struct {
		stdin io.Reader
		dial  func(context.Context) (net.Conn, error)
	}{
		"slow service":   {strings.NewReader(testHookInput(t, "hello")), fakeHookService(t, nil)},
		"unending stdin": {stalled, fakeHookService(t, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			out, code, took := runTestHook(t, tc.stdin, tc.dial)
			if out != allowOutput(t) || code != 0 {
				t.Fatalf("hook printed %q and exited %d, want the allow output and 0", out, code)
			}
			if took < hookDeadline || took > hookDeadline+150*time.Millisecond {
				t.Fatalf("the hook gave up after %v, want its %v deadline", took, hookDeadline)
			}
		})
	}
}

// Input the adapter cannot read fails open without dialling the service.
func TestHookFailsOpenOnMalformedInput(t *testing.T) {
	for name, stdin := range map[string]string{
		"not json":        `{`,
		"empty":           ``,
		"unknown field":   `{"tool":"claude_code","session_id":"s","prompt_text":"x","extra":1}`,
		"not a tool key":  `{"tool":"Claude Code","session_id":"s","prompt_text":"x"}`,
		"no session":      `{"tool":"claude_code","prompt_text":"x"}`,
		"over the 1 MiB":  `{"tool":"claude_code","session_id":"s","prompt_text":"` + strings.Repeat("a", maxHookStdin) + `"}`,
		"prompt not text": `{"tool":"claude_code","session_id":"s","prompt_text":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			var dialled atomic.Int32
			out, code, _ := runTestHook(t, strings.NewReader(stdin), failDial(t, &dialled))
			if out != allowOutput(t) || code != 0 {
				t.Fatalf("hook printed %q and exited %d, want the allow output and 0", out, code)
			}
			if dialled.Load() != 0 {
				t.Fatal("the hook dialled the service with input it could not read")
			}
		})
	}
}

// A refusal, or any answer that is not a valid hook_decision, fails open.
func TestHookFailsOpenOnARefusal(t *testing.T) {
	refused, err := json.Marshal(protocol.NativeMessage{Type: protocol.TypeRefusal, Version: protocol.Version,
		Body: json.RawMessage(`{"reason":"malformed","message":"no"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for name, answer := range map[string][]byte{
		"refusal":        refused,
		"not json":       []byte(`{`),
		"wrong type":     []byte(`{"type":"ack","version":1,"body":{}}`),
		"wrong version":  []byte(`{"type":"hook_decision","version":9,"body":{"action":"block","rule_id":"r"}}`),
		"unknown action": []byte(`{"type":"hook_decision","version":1,"body":{"action":"redact","rule_id":"r"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			dial := fakeHookService(t, func([]byte) []byte { return answer })
			out, code, _ := runTestHook(t, strings.NewReader(testHookInput(t, "hello")), dial)
			if out != allowOutput(t) || code != 0 {
				t.Fatalf("hook printed %q and exited %d, want the allow output and 0", out, code)
			}
		})
	}
}

// A panic on the way to the service fails open.
func TestHookFailsOpenOnAPanic(t *testing.T) {
	dial := func(context.Context) (net.Conn, error) { panic("dial") }
	out, code, _ := runTestHook(t, strings.NewReader(testHookInput(t, "hello")), dial)
	if out != allowOutput(t) || code != 0 {
		t.Fatalf("hook printed %q and exited %d, want the allow output and 0", out, code)
	}
}

// A tool without an adapter, or a malformed command line, prints nothing and exits 0.
func TestHookWithoutAnAdapterPrintsNothing(t *testing.T) {
	var dialled atomic.Int32
	for _, args := range [][]string{{"no_such_tool", "prompt"}, {hooks.TestTool}, {}} {
		var stdout bytes.Buffer
		if code := runHook(time.Now(), args, strings.NewReader("{}"), &stdout, failDial(t, &dialled)); code != 0 || stdout.Len() != 0 {
			t.Fatalf("hook %q printed %q and exited %d", args, stdout.String(), code)
		}
	}
	if dialled.Load() != 0 {
		t.Fatal("a hook without an adapter dialled the service")
	}
}

// A prompt over 256 KiB is sent as its length only.
func TestHookSendsAnOverCapPromptAsItsLength(t *testing.T) {
	sent := make(chan protocol.HookEvaluate, 1)
	dial := fakeHookService(t, func(frame []byte) []byte {
		var msg protocol.NativeMessage
		var ev protocol.HookEvaluate
		_ = json.Unmarshal(frame, &msg)
		_ = json.Unmarshal(msg.Body, &ev)
		sent <- ev
		body, _ := json.Marshal(protocol.HookDecision{Action: protocol.HookAllow, RuleID: "policy.default"})
		out, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeHookDecision, Version: protocol.Version, Body: body})
		return out
	})
	prompt := strings.Repeat("a", protocol.MaxHookPromptBytes+1)
	out, _, _ := runTestHook(t, strings.NewReader(testHookInput(t, prompt)), dial)
	if !strings.Contains(out, `"rule_id":"policy.default"`) {
		t.Fatalf("hook printed %q, want the service's decision", out)
	}
	ev := <-sent
	if !ev.OverCap || ev.PromptText != "" || ev.PromptBytes != int64(len(prompt)) {
		t.Fatalf("sent over_cap=%v, %d text bytes, prompt_bytes=%d; want its length only", ev.OverCap, len(ev.PromptText), ev.PromptBytes)
	}
}

// hookBundle switches the hooks on for claude_code, off for cursor, and blocks claude_code outright,
// which needs no classification.
func hookBundle(t *testing.T, priv ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := policy.Sign("policy-key-1", priv, &policy.Bundle{
		Version:       "5",
		EffectiveAt:   time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		TenantDefault: protocol.ModeM0,
		Endpoint: policy.EndpointPolicy{
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{"claude_code": {Hooks: true}, "cursor": {Hooks: false}},
		},
		Rules: []policy.Rule{{
			RuleID: "block_claude_code", Action: policy.RuleBlock,
			Match:   policy.RuleMatch{Tools: []string{"app:claude_code"}},
			Message: "Claude Code is not approved here.", Link: "https://intranet.example/ai",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A hook reaches the running service over the real endpoint: the service decides from the
// bundle's rules, the hook prints the decision, and the prompt is delivered on route tool.hook
// with the decision recorded as blocked. A tool whose hooks are off is allowed and not recorded.
func TestHookIsDecidedAndRecordedByTheService(t *testing.T) {
	trustTestServer(t)
	_, cloud := enrolledServiceWith(t, func(priv ed25519.PrivateKey) []byte { return hookBundle(t, priv) })

	out, code, _ := runTestHook(t, strings.NewReader(testHookInput(t, "hello")), dialNative)
	var d protocol.HookDecision
	if err := json.Unmarshal([]byte(out), &d); err != nil || code != 0 {
		t.Fatalf("hook printed %q and exited %d", out, code)
	}
	if d != (protocol.HookDecision{Action: protocol.HookBlock, Message: "Claude Code is not approved here.", Link: "https://intranet.example/ai", RuleID: "block_claude_code"}) {
		t.Fatalf("decision = %+v", d)
	}

	cursor := `{"tool":"cursor","session_id":"session-2","prompt_text":"hello"}`
	out, _, _ = runTestHook(t, strings.NewReader(cursor), dialNative)
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Action != protocol.HookAllow || d.RuleID != "" {
		t.Fatalf("a tool whose hooks are off printed %q", out)
	}

	var env struct {
		Source          string             `json:"source"`
		ToolFingerprint string             `json:"tool_fingerprint"`
		Mode            string             `json:"collection_mode"`
		SizeBytes       int64              `json:"size_bytes"`
		Decision        *protocol.Decision `json:"policy_decision"`
	}
	deadline := time.Now().Add(10 * time.Second)
	var hookEvents []json.RawMessage
	for time.Now().Before(deadline) {
		hookEvents = hookEvents[:0]
		for _, e := range cloud.receivedEvents() {
			if strings.Contains(string(e), `"source":"tool.hook"`) {
				hookEvents = append(hookEvents, e)
			}
		}
		if len(hookEvents) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(hookEvents) != 1 {
		t.Fatalf("the edge received %d tool.hook events, want 1", len(hookEvents))
	}
	if err := json.Unmarshal(hookEvents[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.ToolFingerprint != "app:claude_code" || env.Mode != "m0" || env.SizeBytes != 5 ||
		env.Decision == nil || env.Decision.Action != protocol.ActionBlocked || env.Decision.RuleID != "block_claude_code" {
		t.Fatalf("delivered envelope = %s", hookEvents[0])
	}
}
