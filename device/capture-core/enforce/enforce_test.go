package enforce

import (
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

func rule(id string, action policy.RuleAction, m policy.RuleMatch) policy.Rule {
	return policy.Rule{RuleID: id, Action: action, Match: m, Message: "msg " + id, Link: "https://intranet.example/" + id}
}

func TestEvaluate(t *testing.T) {
	sanctioned := []string{"app:claude_code"}
	tls := Input{Route: protocol.RouteProxyTLS, ToolFingerprint: "chatgpt.com", Labels: []string{"credential"}, LabelsKnown: true}

	for name, tc := range map[string]struct {
		rules []policy.Rule
		in    Input
		want  string
	}{
		"no rules":                   {nil, tls, DefaultRuleID},
		"empty lists match anything": {[]policy.Rule{rule("any", policy.RuleWarn, policy.RuleMatch{})}, tls, "any"},
		"empty lists match nil lists": {
			[]policy.Rule{rule("any", policy.RuleWarn, policy.RuleMatch{Labels: []string{}, Tools: []string{}, Categories: []string{}, Sanction: []string{}, Routes: []protocol.Route{}})},
			Input{}, "any"},

		"label matches":           {[]policy.Rule{rule("cred", policy.RuleBlock, policy.RuleMatch{Labels: []string{"payment_card", "credential"}})}, tls, "cred"},
		"label does not match":    {[]policy.Rule{rule("pci", policy.RuleBlock, policy.RuleMatch{Labels: []string{"payment_card"}})}, tls, DefaultRuleID},
		"no labels found":         {[]policy.Rule{rule("cred", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}})}, Input{Route: protocol.RouteProxyTLS, LabelsKnown: true}, DefaultRuleID},
		"labels unknown":          {[]policy.Rule{rule("cred", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}})}, Input{Route: protocol.RouteProxyTLS, Labels: []string{"credential"}}, DefaultRuleID},
		"m0 still matches a tool": {[]policy.Rule{rule("tool", policy.RuleBlock, policy.RuleMatch{Tools: []string{"chatgpt.com"}})}, Input{Route: protocol.RouteProxyTLS, ToolFingerprint: "chatgpt.com"}, "tool"},
		"m0 label and tool rule":  {[]policy.Rule{rule("both", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}, Tools: []string{"chatgpt.com"}})}, Input{Route: protocol.RouteProxyTLS, ToolFingerprint: "chatgpt.com"}, DefaultRuleID},

		"tool matches":                             {[]policy.Rule{rule("tool", policy.RuleWarn, policy.RuleMatch{Tools: []string{"x", "chatgpt.com"}})}, tls, "tool"},
		"tool does not match":                      {[]policy.Rule{rule("tool", policy.RuleWarn, policy.RuleMatch{Tools: []string{"x"}})}, tls, DefaultRuleID},
		"categories never match without a catalog": {[]policy.Rule{rule("cat", policy.RuleBlock, policy.RuleMatch{Categories: []string{"chat"}})}, tls, DefaultRuleID},

		"unsanctioned matches":                           {[]policy.Rule{rule("shadow", policy.RuleWarn, policy.RuleMatch{Sanction: []string{Unsanctioned}})}, tls, "shadow"},
		"sanctioned does not match an unsanctioned tool": {[]policy.Rule{rule("ok", policy.RuleAllow, policy.RuleMatch{Sanction: []string{Sanctioned}})}, tls, DefaultRuleID},
		"sanctioned matches":                             {[]policy.Rule{rule("ok", policy.RuleAllow, policy.RuleMatch{Sanction: []string{Sanctioned}})}, Input{Route: protocol.RouteToolHook, ToolFingerprint: "app:claude_code"}, "ok"},
		"either sanction state":                          {[]policy.Rule{rule("all", policy.RuleWarn, policy.RuleMatch{Sanction: []string{Sanctioned, Unsanctioned}})}, tls, "all"},

		"route matches":        {[]policy.Rule{rule("route", policy.RuleWarn, policy.RuleMatch{Routes: []protocol.Route{protocol.RouteExtDOM, protocol.RouteProxyTLS}})}, tls, "route"},
		"route does not match": {[]policy.Rule{rule("route", policy.RuleWarn, policy.RuleMatch{Routes: []protocol.Route{protocol.RouteToolHook}})}, tls, DefaultRuleID},

		"every list must match": {[]policy.Rule{rule("and", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}, Tools: []string{"chatgpt.com"}, Sanction: []string{Unsanctioned}, Routes: []protocol.Route{protocol.RouteToolHook}})}, tls, DefaultRuleID},
		"all lists matching":    {[]policy.Rule{rule("and", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}, Tools: []string{"chatgpt.com"}, Sanction: []string{Unsanctioned}, Routes: []protocol.Route{protocol.RouteProxyTLS}})}, tls, "and"},

		"first match wins": {[]policy.Rule{
			rule("miss", policy.RuleBlock, policy.RuleMatch{Tools: []string{"x"}}),
			rule("first", policy.RuleAllow, policy.RuleMatch{Labels: []string{"credential"}}),
			rule("second", policy.RuleBlock, policy.RuleMatch{}),
		}, tls, "first"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &policy.Bundle{Rules: tc.rules, SanctionedTools: sanctioned}
			got := Evaluate(b, tc.in)
			if got.RuleID != tc.want {
				t.Fatalf("rule = %q, want %q", got.RuleID, tc.want)
			}
			if tc.want == DefaultRuleID {
				if got != (Decision{RuleID: DefaultRuleID, Action: policy.RuleAllow}) {
					t.Fatalf("default decision = %+v", got)
				}
				return
			}
			for _, r := range tc.rules {
				if r.RuleID == tc.want && (got.Action != r.Action || got.Message != r.Message || got.Link != r.Link) {
					t.Fatalf("decision = %+v, want the action, message and link of %+v", got, r)
				}
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
	inForce = &policy.Bundle{Rules: []policy.Rule{rule("block_credentials", policy.RuleBlock, policy.RuleMatch{Labels: []string{"credential"}})}}
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
