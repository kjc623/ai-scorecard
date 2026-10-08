package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// sharedCases holds the evaluator's cases. The extension's evaluator reads the same file
// (device/extension/test/enforce.test.mjs), so both implementations pass the same cases.
const sharedCases = "../../integration/testdata/enforce/cases.json"

type evaluateCase struct {
	Name   string          `json:"name"`
	Bundle json.RawMessage `json:"bundle"`
	Input  struct {
		Route           protocol.Route `json:"route"`
		ToolFingerprint string         `json:"tool_fingerprint"`
		Labels          []string       `json:"labels"`
		LabelsKnown     bool           `json:"labels_known"`
	} `json:"input"`
	Want struct {
		RuleID  string            `json:"rule_id"`
		Action  policy.RuleAction `json:"action"`
		Message string            `json:"message"`
		Link    string            `json:"link"`
	} `json:"want"`
}

func loadEvaluateCases(t *testing.T) []evaluateCase {
	t.Helper()
	raw, err := os.ReadFile(sharedCases)
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var file struct {
		Cases []evaluateCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode the shared cases: %v", err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("the shared case file holds no cases")
	}
	return file.Cases
}

func TestEvaluate(t *testing.T) {
	for _, tc := range loadEvaluateCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			// Decoded strictly, so a case can only use the bundle's own field names.
			var b policy.Bundle
			dec := json.NewDecoder(bytes.NewReader(tc.Bundle))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&b); err != nil {
				t.Fatalf("the case's bundle is not a policy bundle: %v", err)
			}
			in := Input{Route: tc.Input.Route, ToolFingerprint: tc.Input.ToolFingerprint, Labels: tc.Input.Labels, LabelsKnown: tc.Input.LabelsKnown}
			want := Decision{RuleID: tc.Want.RuleID, Action: tc.Want.Action, Message: tc.Want.Message, Link: tc.Want.Link}
			if got := Evaluate(&b, in); got != want {
				t.Fatalf("decision = %+v, want %+v", got, want)
			}
		})
	}
}

func TestEvaluateWithoutABundle(t *testing.T) {
	if got := Evaluate(nil, Input{Route: protocol.RouteProxyTLS}); got != (Decision{RuleID: DefaultRuleID, Action: policy.RuleAllow}) {
		t.Fatalf("decision = %+v", got)
	}
}

func TestRecordedAction(t *testing.T) {
	for _, tc := range []struct {
		action     policy.RuleAction
		canEnforce bool
		want       string
	}{
		{policy.RuleBlock, true, protocol.ActionBlocked},
		{policy.RuleWarn, true, protocol.ActionWarned},
		{policy.RuleAllow, true, protocol.ActionLogged},
		{policy.RuleBlock, false, protocol.ActionLogged},
		{policy.RuleWarn, false, protocol.ActionLogged},
		{policy.RuleAllow, false, protocol.ActionLogged},
	} {
		got := RecordedAction(Decision{RuleID: "r", Action: tc.action, Message: "m"}, tc.canEnforce)
		if got != (protocol.Decision{RuleID: "r", Action: tc.want, DecidedLocally: true}) {
			t.Errorf("%s canEnforce=%v recorded %+v, want %s under rule r", tc.action, tc.canEnforce, got, tc.want)
		}
	}
}

func TestHookReadsTheBundleInForceAtEachDecision(t *testing.T) {
	var inForce *policy.Bundle
	hook := Hook(func() *policy.Bundle { return inForce }, protocol.RouteProxyTLS, "chatgpt.com", false)

	if got := hook([]string{"credential"}, true); got.RuleID != DefaultRuleID || got.Action != protocol.ActionLogged || !got.DecidedLocally {
		t.Fatalf("no bundle recorded %+v", got)
	}
	inForce = &policy.Bundle{Rules: []policy.Rule{{RuleID: "block_credentials", Action: policy.RuleBlock, Match: policy.RuleMatch{Labels: []string{"credential"}}, Message: "msg block_credentials"}}}
	if got := hook([]string{"credential"}, true); got != (protocol.Decision{RuleID: "block_credentials", Action: protocol.ActionLogged, DecidedLocally: true}) {
		t.Fatalf("after the swap recorded %+v", got)
	}
	if got := hook([]string{"credential"}, false); got.RuleID != DefaultRuleID {
		t.Fatalf("unknown labels recorded %+v", got)
	}
	if got := Hook(nil, protocol.RouteProxyTLS, "t", false)(nil, false); got.RuleID != DefaultRuleID {
		t.Fatalf("a hook without bundles recorded %+v", got)
	}
}
