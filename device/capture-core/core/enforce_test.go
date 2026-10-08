package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// decided is an enforcement hook that records d whatever the labels.
func decided(d protocol.Decision) func([]string, bool) protocol.Decision {
	return func([]string, bool) protocol.Decision { return d }
}

// enforceCall is what one call of the enforcement hook saw.
type enforceCall struct {
	labels []string
	known  bool
}

// recordingHook records each call and answers with the rule id "seen".
func recordingHook(calls *[]enforceCall) func([]string, bool) protocol.Decision {
	return func(labels []string, known bool) protocol.Decision {
		*calls = append(*calls, enforceCall{labels: labels, known: known})
		return protocol.Decision{RuleID: "seen", Action: protocol.ActionLogged, DecidedLocally: true}
	}
}

func recordedDecision(t *testing.T, sink *recordingSink) protocol.Decision {
	t.Helper()
	var env struct {
		PolicyDecision *protocol.Decision `json:"policy_decision"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.PolicyDecision == nil {
		t.Fatalf("the prompt envelope carries no policy_decision: %s", sink.last().Payload)
	}
	return *env.PolicyDecision
}

func promptObservation(body string, hook func([]string, bool) protocol.Decision) Observation {
	return Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(body)),
		Content:         &countingReader{body: []byte(body)},
		Extract: ExtractorFunc(func(b []byte, _ string) (string, []dedup.Attachment, error) {
			return string(b), nil, nil
		}),
		Enforce: hook,
	}
}

// The hook runs after classification and sees each label class once.
func TestPipelineEnforcesOverTheClassifiersLabels(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels: []protocol.Label{
			{Class: "credential", Score: 0.99, RuleID: "AWS_ACCESS_KEY_ID"},
			{Class: "credential", Score: 0.9, RuleID: "GENERIC_SECRET"},
			{Class: "source_code", Score: 0.7},
		},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}
	var calls []enforceCall
	if _, err := p.Process(context.Background(), promptObservation("key AKIA...", recordingHook(&calls))); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("hook called %d times, want once", len(calls))
	}
	if !calls[0].known || !slices.Equal(calls[0].labels, []string{"credential", "source_code"}) {
		t.Fatalf("hook saw %+v, want the known classes credential and source_code", calls[0])
	}
	if got := recordedDecision(t, sink); got.RuleID != "seen" {
		t.Fatalf("recorded %+v, want the hook's decision", got)
	}
}

// At M0 nothing is classified, so the hook is told the labels are unknown, and the prompt still
// records a decision.
func TestPipelineEnforcesAtM0WithUnknownLabels(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	var calls []enforceCall
	obs := promptObservation("anything", recordingHook(&calls))
	obs.Content = &tripwireReader{t: t}
	if _, err := p.Process(context.Background(), obs); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(calls) != 1 || calls[0].known || len(calls[0].labels) != 0 {
		t.Fatalf("hook saw %+v, want one call with no labels and known false", calls)
	}
	if got := recordedDecision(t, sink); got.RuleID != "seen" {
		t.Fatalf("recorded %+v", got)
	}
}

// A classification that did not complete leaves the labels unknown.
func TestPipelineEnforcesWithUnknownLabelsWhenClassificationDegrades(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{err: errors.New("classifier unavailable")}
	var calls []enforceCall
	if _, err := p.Process(context.Background(), promptObservation("text", recordingHook(&calls))); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(calls) != 1 || calls[0].known {
		t.Fatalf("hook saw %+v, want one call with known false", calls)
	}
}

// With the device's evaluator as the hook, the decision comes from the bundle in force when it
// runs, so a swapped bundle applies to the next prompt.
func TestPipelineEnforcesWithTheBundleInForce(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, nil)
	b := m1Bundle()
	p.Bundles = func() *policy.Bundle { return b }
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels: []protocol.Label{{Class: "credential", Score: 0.99}}, ClassifierVersion: "rel-1", Confidence: protocol.ConfidenceHigh,
	}}
	hook := enforce.Hook(p.Bundles, protocol.RouteProxyTLS, "tool", false)

	if _, err := p.Process(context.Background(), promptObservation("one", hook)); err != nil {
		t.Fatal(err)
	}
	if got := recordedDecision(t, sink); got.RuleID != "policy.default" {
		t.Fatalf("before the swap recorded %+v", got)
	}
	swapped := m1Bundle()
	swapped.Rules = []policy.Rule{{RuleID: "block_credentials", Action: policy.RuleBlock, Match: policy.RuleMatch{Labels: []string{"credential"}}}}
	b = swapped
	if _, err := p.Process(context.Background(), promptObservation("two", hook)); err != nil {
		t.Fatal(err)
	}
	if got := recordedDecision(t, sink); got != (protocol.Decision{RuleID: "block_credentials", Action: protocol.ActionLogged, DecidedLocally: true}) {
		t.Fatalf("after the swap recorded %+v", got)
	}
}
