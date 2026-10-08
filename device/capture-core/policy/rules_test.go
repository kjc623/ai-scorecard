package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// rulesSection is a rules list in control-api's spelling: every field and every match list.
const rulesSection = `[
  {"rule_id": "block_credentials", "action": "block",
   "match": {"labels": ["credential"], "tools": ["app:cursor"], "categories": ["ide"], "sanction": ["unsanctioned"], "routes": ["proxy.tls", "tool.hook"]},
   "message": "Remove the credential and try again.", "link": "https://intranet.example/ai"},
  {"rule_id": "warn.unsanctioned-1", "action": "warn",
   "match": {"labels": [], "tools": [], "categories": [], "sanction": ["unsanctioned"], "routes": []},
   "message": "Use the approved coding agent."},
  {"rule_id": "allow_rest", "action": "allow",
   "match": {"labels": [], "tools": [], "categories": [], "sanction": [], "routes": []},
   "message": ""}
]`

// signWithMembers signs testBundle with each named member replaced by its raw JSON, so the payload
// carries exactly the names under test.
func signWithMembers(t *testing.T, priv ed25519.PrivateKey, version string, members map[string]string) []byte {
	t.Helper()
	raw, err := Sign("policy-key-1", priv, testBundle(version))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(env["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	for name, v := range members {
		payload[name] = json.RawMessage(v)
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env["payload"] = newPayload
	env["signature"], _ = json.Marshal(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, newPayload)))
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRulesAndSanctionedToolsDecode(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b, err := v.Open(signWithMembers(t, priv, "42", map[string]string{
		"rules": rulesSection, "sanctioned_tools": `["app:claude_code", "app:cursor"]`,
	}), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []Rule{
		{RuleID: "block_credentials", Action: RuleBlock,
			Match: RuleMatch{Labels: []string{"credential"}, Tools: []string{"app:cursor"}, Categories: []string{"ide"},
				Sanction: []string{"unsanctioned"}, Routes: []protocol.Route{protocol.RouteProxyTLS, protocol.RouteToolHook}},
			Message: "Remove the credential and try again.", Link: "https://intranet.example/ai"},
		{RuleID: "warn.unsanctioned-1", Action: RuleWarn,
			Match:   RuleMatch{Labels: []string{}, Tools: []string{}, Categories: []string{}, Sanction: []string{"unsanctioned"}, Routes: []protocol.Route{}},
			Message: "Use the approved coding agent."},
		{RuleID: "allow_rest", Action: RuleAllow,
			Match: RuleMatch{Labels: []string{}, Tools: []string{}, Categories: []string{}, Sanction: []string{}, Routes: []protocol.Route{}}},
	}
	if !reflect.DeepEqual(b.Rules, want) {
		t.Fatalf("rules = %+v\nwant    %+v", b.Rules, want)
	}
	if !reflect.DeepEqual(b.SanctionedTools, []string{"app:claude_code", "app:cursor"}) {
		t.Fatalf("sanctioned_tools = %v", b.SanctionedTools)
	}

	// A misspelt name inside a rule is an unknown field, so the whole bundle is refused.
	for _, misspelt := range []string{
		strings.Replace(rulesSection, `"rule_id": "allow_rest"`, `"id": "allow_rest"`, 1),
		strings.Replace(rulesSection, `"sanction": ["unsanctioned"], "routes": []`, `"sanctioned": ["unsanctioned"], "routes": []`, 1),
	} {
		if _, err := v.Open(signWithMembers(t, priv, "43", map[string]string{"rules": misspelt}), nil); CauseOf(err) != CauseSchemaInvalid {
			t.Fatalf("misspelt rule field cause = %q, want %q (err=%v)", CauseOf(err), CauseSchemaInvalid, err)
		}
	}

	// A bundle without either has no rules and nothing sanctioned.
	none := testBundle("44")
	if err := none.Validate(); err != nil || none.Rules != nil || none.SanctionedTools != nil {
		t.Fatalf("a bundle without rules = %+v %v, %v", none.Rules, none.SanctionedTools, err)
	}
}

func TestRulesValidation(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	refused := map[string][2]string{
		"unknown action":         {`"action": "warn"`, `"action": "redact"`},
		"empty action":           {`"action": "allow"`, `"action": ""`},
		"duplicate rule_id":      {`"rule_id": "allow_rest"`, `"rule_id": "block_credentials"`},
		"upper-case rule_id":     {`"rule_id": "allow_rest"`, `"rule_id": "Allow_rest"`},
		"rule_id with a digit":   {`"rule_id": "allow_rest"`, `"rule_id": "1allow"`},
		"empty rule_id":          {`"rule_id": "allow_rest"`, `"rule_id": ""`},
		"rule_id over 128":       {`"rule_id": "allow_rest"`, `"rule_id": "a` + strings.Repeat("b", 128) + `"`},
		"message over 280":       {`"message": ""`, `"message": "` + strings.Repeat("é", 281) + `"`},
		"http link":              {`"https://intranet.example/ai"`, `"http://intranet.example/ai"`},
		"link without a host":    {`"https://intranet.example/ai"`, `"https://"`},
		"upper-case link scheme": {`"https://intranet.example/ai"`, `"HTTPS://intranet.example/ai"`},
		"relative link":          {`"https://intranet.example/ai"`, `"/ai"`},
		"unknown route":          {`"routes": ["proxy.tls", "tool.hook"]`, `"routes": ["proxy.tls", "tool.mcp"]`},
	}
	for name, r := range refused {
		section := strings.Replace(rulesSection, r[0], r[1], 1)
		if section == rulesSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		_, err := v.Open(signWithMembers(t, priv, "50", map[string]string{"rules": section}), nil)
		if CauseOf(err) != CauseSchemaInvalid {
			t.Errorf("%s: cause = %q, want %q (err=%v)", name, CauseOf(err), CauseSchemaInvalid, err)
		}
		// An error never carries a rule's message or link text.
		if err != nil && (strings.Contains(err.Error(), "Remove the credential") || strings.Contains(err.Error(), "intranet.example")) {
			t.Errorf("%s: the error quotes a rule's text: %v", name, err)
		}
	}

	accepted := map[string][2]string{
		"message of 280":    {`"message": ""`, `"message": "` + strings.Repeat("é", 280) + `"`},
		"no link":           {`, "link": "https://intranet.example/ai"`, ``},
		"empty link":        {`"https://intranet.example/ai"`, `""`},
		"link with a query": {`"https://intranet.example/ai"`, `"https://intranet.example:8443/ai?x=1#top"`},
		"no match lists": {`"match": {"labels": [], "tools": [], "categories": [], "sanction": [], "routes": []},
   "message": ""`, `"match": {}, "message": ""`},
		"rule_id of 128": {`"rule_id": "allow_rest"`, `"rule_id": "a` + strings.Repeat("b", 127) + `"`},
		"every route":    {`"routes": ["proxy.tls", "tool.hook"]`, `"routes": ["ext.web_request", "ext.page_context", "ext.dom", "proxy.tls", "proxy.loopback", "proc.detect", "cli.shim", "tool.hook", "tool.otel", "inv.scan", "net.flow"]`},
	}
	for name, r := range accepted {
		section := strings.Replace(rulesSection, r[0], r[1], 1)
		if section == rulesSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		if _, err := v.Open(signWithMembers(t, priv, "60", map[string]string{"rules": section}), nil); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	if _, err := v.Open(signWithMembers(t, priv, "61", map[string]string{"rules": `[]`, "sanctioned_tools": `[]`}), nil); err != nil {
		t.Errorf("empty lists: refused: %v", err)
	}
}
