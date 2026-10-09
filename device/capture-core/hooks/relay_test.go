package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
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
	classifier, _ := startClassifier(t, buildClassifier(t, t.TempDir()))
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

// Under a classifier host slower than the relay's budget, each hook of a burst is answered from the
// bundle with the labels unknown, and the host keeps running: a classification that runs out of
// budget does not end it.
func TestASlowClassifierDegradesABurstWithoutARestart(t *testing.T) {
	classifier, host := startSlowClassifier(t)
	sink := &memSink{}
	r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), classifier)

	const burst = 10
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			client, server := net.Pipe()
			defer client.Close()
			go func() {
				defer server.Close()
				r.Serve(server, hostinfo.User{Account: "hook-user"}, evaluateFrame(t, "claude_code", "deploy with key "+awsKey))
			}()
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			payload, err := localipc.ReadFrame(client)
			if err != nil {
				t.Errorf("hook %d: no answer: %v", i, err)
				return
			}
			var answer protocol.NativeMessage
			var d protocol.HookDecision
			if json.Unmarshal(payload, &answer) != nil || answer.Type != protocol.TypeHookDecision || json.Unmarshal(answer.Body, &d) != nil {
				t.Errorf("hook %d: the relay answered %s", i, payload)
				return
			}
			if d.Action != protocol.HookAllow || d.RuleID != "policy.default" {
				t.Errorf("hook %d: decision = %+v, want the default allow of unknown labels", i, d)
			}
		})
	}
	wg.Wait()
	// Stopping the relay waits for the answered prompts to be recorded.
	_ = r.Stop(context.Background())
	if n := len(sink.all()); n != burst {
		t.Errorf("recorded %d prompts, want %d", n, burst)
	}

	// A restart waits a second after the child exits; wait past it.
	time.Sleep(1500 * time.Millisecond)
	if n := host.Restarts(); n != 0 {
		t.Fatalf("classifier-host was restarted %d times", n)
	}
	if h := host.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("classifier-host is %s/%s", h.State, h.Detail)
	}
	resp, err := classifier.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte(awsKey), Mode: protocol.ModeM1, BudgetMS: 5000,
	})
	if err != nil || resp.Confidence != protocol.ConfidenceHigh || resp.ClassifierVersion != "slow-1" {
		t.Fatalf("after the burst the host answered %+v, %v; want its own answer", resp, err)
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

// The record carries the labels the decision was made with, without classifying the prompt again.
// A prompt whose classification ran out of the decision's budget is classified for the record.
func TestTheRecordTakesTheDecisionsClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		calls int32
		want  protocol.HookAction
	}{
		{"classified in time", 0, 1, protocol.HookBlock},
		{"out of the decision's budget", 4 * hooks.ClassifyBudget, 2, protocol.HookAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classifier := &stubClassifier{delay: tc.delay}
			sink := &memSink{}
			r := newRelay(t, newPipeline(t, sink, testBundle(protocol.ModeM1, blockCredentials)), classifier)
			answer, served := ask(t, r, evaluateFrame(t, "claude_code", "deploy with key "+awsKey))
			if d := decision(t, answer); d.Action != tc.want {
				t.Fatalf("decision = %+v, want %s", d, tc.want)
			}
			waitServed(t, served)
			if n := classifier.calls.Load(); n != tc.calls {
				t.Errorf("the prompt was classified %d times, want %d", n, tc.calls)
			}
			env := decodeEnvelope(t, sink.all()[0])
			if !slices.ContainsFunc(env.Labels, func(l protocol.Label) bool { return l.Class == "credential" }) {
				t.Fatalf("the record carries labels %+v, want credential", env.Labels)
			}
		})
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

// LastServed names when a tool's hook last reached the relay with a frame it could read; a refused
// frame and another tool's hook leave it as it was.
func TestLastServedFollowsTheTool(t *testing.T) {
	r := newRelay(t, newPipeline(t, &memSink{}, testBundle(protocol.ModeM1)), &stubClassifier{})
	bad := evaluateFrame(t, "cursor", "hello")
	bad.Version = 9
	_, served := ask(t, r, bad)
	waitServed(t, served)
	if got := r.LastServed("cursor"); !got.IsZero() {
		t.Fatalf("LastServed after a refused frame = %v", got)
	}
	before := time.Now()
	_, served = ask(t, r, evaluateFrame(t, "claude_code", "hello"))
	waitServed(t, served)
	if got := r.LastServed("claude_code"); got.Before(before) || got.After(time.Now()) {
		t.Fatalf("LastServed after a hook = %v, want the time it arrived", got)
	}
	if got := r.LastServed("cursor"); !got.IsZero() {
		t.Fatalf("LastServed of a tool whose hook did not run = %v", got)
	}
}

// promptsProbe records what reaches the relay's Prompts.
type promptsProbe struct {
	mu   sync.Mutex
	seen []core.Observation
}

func (p *promptsProbe) Process(_ context.Context, obs core.Observation) (core.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, obs)
	return core.Outcome{Route: obs.Route}, nil
}

// A prompt the user submits goes to Prompts, the merge with the tool's other path; a tool call's
// check goes to the pipeline.
func TestSubmittedPromptsGoToPromptsAndToolCallsToThePipeline(t *testing.T) {
	sink := &memSink{}
	pipe := newPipeline(t, sink, testBundle(protocol.ModeM0))
	probe := &promptsProbe{}
	r := hooks.New(hooks.Config{Pipeline: pipe, Prompts: probe, Bundles: pipe.Bundles})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	const prompt = "summarise the meeting notes"
	_, served := ask(t, r, evaluateFrame(t, "claude_code", prompt))
	waitServed(t, served)

	const toolInput = `{"command":"ls"}`
	ev := protocol.HookEvaluate{Tool: "claude_code", Event: "PreToolUse", SessionID: "session-1", ToolName: "Bash", PromptText: toolInput}
	ev.CapPrompt()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	_, served = ask(t, r, protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "2", Body: body})
	waitServed(t, served)

	probe.mu.Lock()
	seen := probe.seen
	probe.mu.Unlock()
	if len(seen) != 1 || seen[0].Route != protocol.RouteToolHook || seen[0].ClientID != "session-1" || seen[0].SizeBytes != int64(len(prompt)) {
		t.Fatalf("Prompts received %+v, want the submitted prompt only", seen)
	}
	if got := sink.all(); len(got) != 1 || decodeEnvelope(t, got[0]).SizeBytes != int64(len(toolInput)) {
		t.Fatalf("the pipeline spooled %d records, want the tool call's only", len(got))
	}
}

// monitorOnly is an adapter for a tool that runs the hook but goes ahead whatever it answers.
type monitorOnly struct{}

func (monitorOnly) Parse(string, []byte) (protocol.HookEvaluate, error) {
	return protocol.HookEvaluate{}, errors.New("not used")
}
func (monitorOnly) Render(string, protocol.HookDecision) ([]byte, int) { return nil, 0 }
func (monitorOnly) Allow(string) ([]byte, int)                         { return nil, 0 }
func (monitorOnly) CanEnforce(string) bool                             { return false }

// A block from an event the tool cannot enforce is still the hook's answer, and the prompt is
// recorded as logged with the rule that matched, since the tool went ahead.
func TestABlockTheToolCannotEnforceIsRecordedLogged(t *testing.T) {
	hooks.SetAdapter(t, "codex", monitorOnly{})
	block := policy.Rule{RuleID: "block_codex", Action: policy.RuleBlock, Match: policy.RuleMatch{Tools: []string{"app:codex"}}, Message: "Not here."}
	b := testBundle(protocol.ModeM1, block)
	b.Endpoint.Tools["codex"] = policy.EndpointTool{Hooks: true}
	sink := &memSink{}
	r := newRelay(t, newPipeline(t, sink, b), &stubClassifier{})

	answer, served := ask(t, r, evaluateFrame(t, "codex", "hello"))
	if d := decision(t, answer); d != (protocol.HookDecision{Action: protocol.HookBlock, Message: "Not here.", RuleID: "block_codex"}) {
		t.Fatalf("decision = %+v", d)
	}
	waitServed(t, served)
	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("spooled %d records, want 1", len(entries))
	}
	env := decodeEnvelope(t, entries[0])
	if env.ToolFingerprint != "app:codex" || env.Decision == nil || env.Decision.Action != protocol.ActionLogged || env.Decision.RuleID != "block_codex" {
		t.Fatalf("record = %s", entries[0].Payload)
	}
}
