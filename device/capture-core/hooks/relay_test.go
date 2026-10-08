package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// stubClassifier labels any content holding an AWS access key id as credential, after delay.
type stubClassifier struct {
	calls atomic.Int32
	delay time.Duration
}

func (c *stubClassifier) Classify(ctx context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.calls.Add(1)
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return protocol.ClassifyResponse{}, ctx.Err()
	}
	resp := protocol.ClassifyResponse{Labels: []protocol.Label{}, ClassifierVersion: "stub-1", Confidence: protocol.ConfidenceHigh}
	if strings.Contains(string(req.Content), "AKIA") {
		resp.Labels = append(resp.Labels, protocol.Label{Class: "credential", Score: 0.9, RuleID: "AWS_ACCESS_KEY_ID"})
	}
	return resp, nil
}

func TestRelayIsTheHookRelayCollector(t *testing.T) {
	r := hooks.New(hooks.Config{Pipeline: newPipeline(t, &memSink{}, nil)})
	if r.Name() != protocol.CollectorHookRelay || !r.Name().Valid() {
		t.Fatalf("collector %q", r.Name())
	}
	if r.Enabled(nil) || r.Enabled(&policy.Bundle{}) || !r.Enabled(testBundle(protocol.ModeM0)) {
		t.Fatal("the relay is not switched by endpoint.hooks.enabled")
	}
	if h := r.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("a stopped relay is %s", h.State)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := r.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("a started relay is %s", h.State)
	}
	_ = r.Stop(context.Background())
	if h := r.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("a stopped relay is %s", h.State)
	}
}

// A block rule on credential blocks the AWS-key-shaped prompt, which the real classifier labels,
// and the prompt is recorded on tool.hook as blocked. A clean prompt is allowed and recorded as
// logged.
func TestBlockOnCredentialBlocksAnAWSKeyAndRecordsBlocked(t *testing.T) {
	classifier := startClassifier(t, buildClassifier(t, t.TempDir()))
	sink := &memSink{}
	pipe := newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials))
	r := newRelay(t, pipe, classifier)

	answer, served := ask(t, r, evaluateFrame(t, "claude_code", "deploy with key "+awsKey+" please"))
	if d := decision(t, answer); d != (protocol.HookDecision{Action: protocol.HookBlock, Message: blockCredentials.Message, Link: blockCredentials.Link, RuleID: "block_credentials"}) {
		t.Fatalf("decision = %+v", d)
	}
	waitServed(t, served)
	answer, served = ask(t, r, evaluateFrame(t, "claude_code", "summarise the meeting notes"))
	if d := decision(t, answer); d.Action != protocol.HookAllow || d.RuleID != "policy.default" {
		t.Fatalf("a clean prompt was answered %+v", d)
	}
	waitServed(t, served)

	entries := sink.all()
	if len(entries) != 2 {
		t.Fatalf("spooled %d records, want 2", len(entries))
	}
	blocked := decodeEnvelope(t, entries[0])
	if blocked.Source != string(protocol.RouteToolHook) || blocked.ToolFingerprint != "app:claude_code" || blocked.Mode != "m1" || blocked.UserRef != testUserRef ||
		blocked.Decision == nil || blocked.Decision.Action != protocol.ActionBlocked || blocked.Decision.RuleID != "block_credentials" ||
		!slices.ContainsFunc(blocked.Labels, func(l protocol.Label) bool { return l.Class == "credential" }) {
		t.Fatalf("blocked record = %s", entries[0].Payload)
	}
	if strings.Contains(string(entries[0].Payload), awsKey) {
		t.Fatal("the record carries the prompt text")
	}
	logged := decodeEnvelope(t, entries[1])
	if logged.Decision == nil || logged.Decision.Action != protocol.ActionLogged || logged.Decision.RuleID != "policy.default" {
		t.Fatalf("allowed record = %s", entries[1].Payload)
	}
}

// At m0 nothing is classified and the prompt is never read, so a label rule cannot match; a rule
// on the tool still does.
func TestM0ClassifiesNothingAndLabelRulesDoNotMatch(t *testing.T) {
	warnTool := policy.Rule{RuleID: "warn_claude_code", Action: policy.RuleWarn, Match: policy.RuleMatch{Tools: []string{"app:claude_code"}}, Message: "Careful."}
	classifier := &stubClassifier{}
	sink := &memSink{}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM0, blockCredentials, warnTool)), classifier)

	answer, served := ask(t, r, evaluateFrame(t, "claude_code", "key "+awsKey))
	if d := decision(t, answer); d.Action != protocol.HookWarn || d.RuleID != "warn_claude_code" || d.Message != "Careful." {
		t.Fatalf("decision = %+v", d)
	}
	waitServed(t, served)
	if classifier.calls.Load() != 0 {
		t.Fatal("the prompt was classified at m0")
	}
	env := decodeEnvelope(t, sink.all()[0])
	if env.Mode != "m0" || len(env.Labels) != 0 || env.SizeBytes != int64(len("key "+awsKey)) ||
		env.Decision == nil || env.Decision.Action != protocol.ActionWarned || env.Decision.RuleID != "warn_claude_code" {
		t.Fatalf("record = %s", sink.all()[0].Payload)
	}
}

// A stopped relay, a bundle with the hooks off and a tool whose hooks are off are answered allow
// at once, and nothing is recorded.
func TestDisabledHooksAreAllowedAndNotRecorded(t *testing.T) {
	block := policy.Rule{RuleID: "block_all", Action: policy.RuleBlock}
	off := testBundle(protocol.ModeM1, block)
	off.Endpoint.Hooks.Enabled = false
	for name, tc := range map[string]struct {
		bundle  *policy.Bundle
		tool    string
		stopped bool
	}{
		"relay stopped":   {testBundle(protocol.ModeM1, block), "claude_code", true},
		"hooks off":       {off, "claude_code", false},
		"tool hooks off":  {testBundle(protocol.ModeM1, block), "cursor", false},
		"tool not listed": {testBundle(protocol.ModeM1, block), "codex", false},
		"no bundle":       {nil, "claude_code", false},
	} {
		t.Run(name, func(t *testing.T) {
			classifier := &stubClassifier{}
			sink := &memSink{}
			r := newRelay(t, newPipeline(t, sink, tc.bundle), classifier)
			if tc.stopped {
				_ = r.Stop(context.Background())
			}
			answer, served := ask(t, r, evaluateFrame(t, tc.tool, "hello"))
			if d := decision(t, answer); d != (protocol.HookDecision{Action: protocol.HookAllow}) {
				t.Fatalf("decision = %+v", d)
			}
			waitServed(t, served)
			if len(sink.all()) != 0 || classifier.calls.Load() != 0 {
				t.Fatalf("recorded %d, classified %d", len(sink.all()), classifier.calls.Load())
			}
		})
	}
}

// The answer is written while the record is still waiting for the spool.
func TestTheAnswerDoesNotWaitForTheSpool(t *testing.T) {
	sink := &memSink{gate: make(chan struct{})}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), &stubClassifier{})
	answer, served := ask(t, r, evaluateFrame(t, "claude_code", "key "+awsKey))
	if d := decision(t, answer); d.Action != protocol.HookBlock {
		t.Fatalf("decision = %+v", d)
	}
	select {
	case <-served:
		t.Fatal("the record was spooled before the gate opened")
	default:
	}
	close(sink.gate)
	waitServed(t, served)
	if len(sink.all()) != 1 {
		t.Fatal("the prompt was not recorded once the spool accepted it")
	}
}

// A record the spool refuses is counted dropped; the answer is the rule's all the same.
func TestASpoolFailureCountsDroppedAndKeepsTheAnswer(t *testing.T) {
	sink := &memSink{err: errors.New("spool full")}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), &stubClassifier{})
	answer, served := ask(t, r, evaluateFrame(t, "claude_code", "key "+awsKey))
	if d := decision(t, answer); d.Action != protocol.HookBlock || d.RuleID != "block_credentials" {
		t.Fatalf("decision = %+v", d)
	}
	waitServed(t, served)
	c := r.Health().Counters
	if c[protocol.CounterDropped] != 1 || c[protocol.CounterEmitted] != 0 || c[protocol.CounterObserved] != 1 {
		t.Fatalf("counters = %v, want one observed and dropped", c)
	}
}

// A classification that does not finish within the budget leaves the labels unknown: label rules do
// not match and the hook is answered in time.
func TestASlowClassifierLeavesTheLabelsUnknown(t *testing.T) {
	classifier := &stubClassifier{delay: time.Second}
	r := newRelay(t, newPipeline(t, &memSink{}, testBundle(protocol.ModeM1, blockCredentials)), classifier)
	start := time.Now()
	answer, _ := ask(t, r, evaluateFrame(t, "claude_code", "key "+awsKey))
	if took := time.Since(start); took > hooks.ClassifyBudget+200*time.Millisecond {
		t.Fatalf("the answer took %v", took)
	}
	if d := decision(t, answer); d.Action != protocol.HookAllow || d.RuleID != "policy.default" {
		t.Fatalf("decision = %+v", d)
	}
}

// A prompt sent as its length only is not classified, and is recorded with that size.
func TestAnOverCapPromptIsNotClassified(t *testing.T) {
	classifier := &stubClassifier{}
	sink := &memSink{}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), classifier)
	prompt := strings.Repeat("a", protocol.MaxHookPromptBytes) + awsKey
	answer, served := ask(t, r, evaluateFrame(t, "claude_code", prompt))
	if d := decision(t, answer); d.Action != protocol.HookAllow {
		t.Fatalf("decision = %+v", d)
	}
	waitServed(t, served)
	if classifier.calls.Load() != 0 {
		t.Fatal("an over-cap prompt was classified for the decision")
	}
	if env := decodeEnvelope(t, sink.all()[0]); env.SizeBytes != int64(len(prompt)) {
		t.Fatalf("recorded size %d, want %d", env.SizeBytes, len(prompt))
	}
}

// A frame the relay cannot read is refused, counted and not recorded.
func TestAMalformedFrameIsRefused(t *testing.T) {
	sink := &memSink{}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), &stubClassifier{})
	good := evaluateFrame(t, "claude_code", "hello")
	wrongVersion := good
	wrongVersion.Version = 9
	notJSON := good
	notJSON.Body = json.RawMessage(`"x"`)
	invalid := good
	invalid.Body = json.RawMessage(`{"tool":"claude_code","event":"prompt","session_id":"s","prompt_text":"hello","prompt_bytes":99}`)
	for name, tc := range map[string]struct {
		frame protocol.NativeMessage
		want  protocol.RefusalReason
	}{
		"wrong version": {wrongVersion, protocol.RefusalVersionMismatch},
		"not a body":    {notJSON, protocol.RefusalMalformed},
		"invalid":       {invalid, protocol.RefusalMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			answer, served := ask(t, r, tc.frame)
			waitServed(t, served)
			var ref protocol.Refusal
			if answer.Type != protocol.TypeRefusal || json.Unmarshal(answer.Body, &ref) != nil || ref.Reason != tc.want {
				t.Fatalf("answer = %s %s, want a %s refusal", answer.Type, answer.Body, tc.want)
			}
		})
	}
	if len(sink.all()) != 0 || r.Health().Counters[protocol.CounterErrors] != 3 {
		t.Fatalf("recorded %d, errors %d", len(sink.all()), r.Health().Counters[protocol.CounterErrors])
	}
}
