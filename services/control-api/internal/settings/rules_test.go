package settings_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

const rulesPath = "/admin/v1/settings/rules"

// The three rules a test tenant starts with, in the bundle's spelling.
const threeRules = `{"rules":[
  {"rule_id":"block_credentials","action":"block","match":{"labels":["credential"],"tools":[],"categories":[],"sanction":[],"routes":[]},
   "message":"Remove the credential and try again.","link":"https://intranet.example/ai"},
  {"rule_id":"warn.unsanctioned-agents","action":"warn","match":{"categories":["coding_agent"],"sanction":["unsanctioned"]},
   "message":"Use the approved coding agent."},
  {"rule_id":"allow_hooks","action":"allow","match":{"tools":["app:claude_code"],"routes":["tool.hook","proxy.tls"]},"message":""}
]}`

func (r *rig) rules(t *testing.T) string {
	t.Helper()
	rec := r.do(t, "admin", "GET", rulesPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET rules: %d %s", rec.Code, rec.Body)
	}
	return canonical(t, rec.Body.String())
}

// withUsers is a {"rules":[...]} document as the GET reads it when no rule names people.
func withUsers(doc string) string {
	return strings.TrimSuffix(doc, "}") + `,"users":[]}`
}

func canonical(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("%v: %s", err, doc)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRulesReplaceTheOrderedList: a tenant without rules reads an empty list; a PUT replaces the
// whole list and the GET returns it in the order sent, every match list present; a second PUT that
// reorders and drops a rule is what the GET returns next; [] removes every rule. Each write is one
// audit row with the previous and the new list.
func TestRulesReplaceTheOrderedList(t *testing.T) {
	r := newRig(t)
	if got := r.rules(t); got != `{"rules":[],"users":[]}` {
		t.Fatalf("a tenant without rules reads %s", got)
	}

	if rec := r.do(t, "admin", "PUT", rulesPath, threeRules); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	first := canonical(t, `{"rules":[
	  {"rule_id":"block_credentials","action":"block","match":{"labels":["credential"],"tools":[],"categories":[],"sanction":[],"routes":[]},
	   "message":"Remove the credential and try again.","link":"https://intranet.example/ai","users":[],"enabled":true},
	  {"rule_id":"warn.unsanctioned-agents","action":"warn","match":{"labels":[],"tools":[],"categories":["coding_agent"],"sanction":["unsanctioned"],"routes":[]},
	   "message":"Use the approved coding agent.","users":[],"enabled":true},
	  {"rule_id":"allow_hooks","action":"allow","match":{"labels":[],"tools":["app:claude_code"],"categories":[],"sanction":[],"routes":["tool.hook","proxy.tls"]},"message":"","users":[],"enabled":true}]}`)
	if got := r.rules(t); got != withUsers(first) {
		t.Fatalf("after the first PUT:\n got %s\nwant %s", got, withUsers(first))
	}

	reordered := `{"rules":[
	  {"rule_id":"allow_hooks","action":"allow","match":{"tools":["app:claude_code"],"routes":["tool.hook","proxy.tls"]},"message":""},
	  {"rule_id":"block_credentials","action":"warn","match":{"labels":["credential","payment_card"]},"message":"Check before you send."}]}`
	if rec := r.do(t, "admin", "PUT", rulesPath, reordered); rec.Code != http.StatusNoContent {
		t.Fatalf("reorder: %d %s", rec.Code, rec.Body)
	}
	second := canonical(t, `{"rules":[
	  {"rule_id":"allow_hooks","action":"allow","match":{"labels":[],"tools":["app:claude_code"],"categories":[],"sanction":[],"routes":["tool.hook","proxy.tls"]},"message":"","users":[],"enabled":true},
	  {"rule_id":"block_credentials","action":"warn","match":{"labels":["credential","payment_card"],"tools":[],"categories":[],"sanction":[],"routes":[]},"message":"Check before you send.","users":[],"enabled":true}]}`)
	if got := r.rules(t); got != withUsers(second) {
		t.Fatalf("after the reorder:\n got %s\nwant %s", got, withUsers(second))
	}

	if rec := r.do(t, "admin", "PUT", rulesPath, `{"rules":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if got := r.rules(t); got != `{"rules":[],"users":[]}` {
		t.Fatalf("after clearing: %s", got)
	}

	audits := r.store.Audits()
	if len(audits) != 3 {
		t.Fatalf("audits = %+v, want one per accepted write", audits)
	}
	list := func(doc string) string { return canonical(t, doc[strings.Index(doc, "["):len(doc)-1]) }
	for i, want := range []struct{ previous, next string }{
		{"[]", list(first)}, {list(first), list(second)}, {list(second), "[]"},
	} {
		a := audits[i]
		if a.Action != "tenant.enforcement_rules.set" || a.ActorID != admin.Actor || a.ObjectType != "tenant" || a.ObjectID != tenantA || a.Detail["subject"] != admin.Subject {
			t.Fatalf("audit %d = %+v", i, a)
		}
		previous, _ := json.Marshal(a.Detail["previous"])
		next, _ := json.Marshal(a.Detail["new"])
		if canonical(t, string(previous)) != want.previous || canonical(t, string(next)) != want.next {
			t.Fatalf("audit %d:\n previous %s\n      new %s\nwant\n previous %s\n      new %s", i, previous, next, want.previous, want.next)
		}
	}
}

// TestRulesRefuseEveryBadValue: each refused value is a 400 that names the rule and the field, and
// leaves the list in force and the audit trail unchanged.
func TestRulesRefuseEveryBadValue(t *testing.T) {
	r := newRig(t)
	if rec := r.do(t, "admin", "PUT", rulesPath, threeRules); rec.Code != http.StatusNoContent {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	before := r.rules(t)

	rule := func(fields string) string {
		return `{"rules":[{"rule_id":"ok_rule","action":"allow","match":{}},` + fields + `]}`
	}
	var many []string
	for i := 0; i <= 100; i++ {
		many = append(many, fmt.Sprintf(`{"rule_id":"r%d","action":"allow","match":{}}`, i))
	}
	cases := []struct {
		name, body, code, field string
	}{
		{"no list", `{}`, apierr.CodeInvalidRequest, ""},
		{"a null list", `{"rules":null}`, apierr.CodeInvalidRequest, ""},
		{"101 rules", `{"rules":[` + strings.Join(many, ",") + `]}`, apierr.CodeInvalidRequest, ""},
		{"an empty rule_id", rule(`{"rule_id":"","action":"allow"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"an upper-case rule_id", rule(`{"rule_id":"Block","action":"allow"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"a rule_id starting with a digit", rule(`{"rule_id":"9block","action":"allow"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"a rule_id with a space", rule(`{"rule_id":"block it","action":"allow"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"a rule_id of 129 characters", rule(`{"rule_id":"a` + strings.Repeat("b", 128) + `","action":"allow"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"a duplicate rule_id", rule(`{"rule_id":"ok_rule","action":"block","message":"x"}`), apierr.CodeInvalidRequest, "rule_id"},
		{"the action redact", rule(`{"rule_id":"r","action":"redact"}`), apierr.CodeInvalidRequest, "action"},
		{"no action", rule(`{"rule_id":"r"}`), apierr.CodeInvalidRequest, "action"},
		{"a message of 281 characters", rule(`{"rule_id":"r","action":"warn","message":"` + strings.Repeat("é", 281) + `"}`), apierr.CodeInvalidRequest, "message"},
		{"an http link", rule(`{"rule_id":"r","action":"warn","message":"x","link":"http://intranet.example/ai"}`), apierr.CodeInvalidRequest, "link"},
		{"a link with no host", rule(`{"rule_id":"r","action":"warn","message":"x","link":"https://"}`), apierr.CodeInvalidRequest, "link"},
		{"an upper-case scheme", rule(`{"rule_id":"r","action":"warn","message":"x","link":"HTTPS://intranet.example"}`), apierr.CodeInvalidRequest, "link"},
		{"a relative link", rule(`{"rule_id":"r","action":"warn","message":"x","link":"intranet.example/ai"}`), apierr.CodeInvalidRequest, "link"},
		{"a redirect with no link", rule(`{"rule_id":"r","action":"redirect","message":"x","match":{"tools":["app:chatgpt"]}}`), apierr.CodeInvalidRequest, "link"},
		{"an unknown route", rule(`{"rule_id":"r","action":"block","message":"x","match":{"routes":["tool.mcp"]}}`), apierr.CodeInvalidRequest, "match.routes"},
		{"an unknown sanction", rule(`{"rule_id":"r","action":"block","message":"x","match":{"sanction":["unknown"]}}`), apierr.CodeInvalidRequest, "match.sanction"},
		{"an empty tool", rule(`{"rule_id":"r","action":"block","message":"x","match":{"tools":[" "]}}`), apierr.CodeInvalidRequest, "match.tools"},
		{"a label outside the data classes", rule(`{"rule_id":"r","action":"block","message":"x","match":{"labels":["credential","secrets"]}}`), apierr.CodeInvalidRequest, ""},
		{"an unknown match list", rule(`{"rule_id":"r","action":"block","message":"x","match":{"apps":["cursor"]}}`), apierr.CodeSchemaViolation, ""},
		{"a match list that is not a list", rule(`{"rule_id":"r","action":"block","message":"x","match":{"labels":"credential"}}`), apierr.CodeSchemaViolation, ""},
		{"an unknown rule field", rule(`{"rule_id":"r","action":"block","message":"x","priority":1}`), apierr.CodeSchemaViolation, ""},
		{"a user that is an address", rule(`{"rule_id":"r","action":"allow","users":["user:ana@example.com"]}`), apierr.CodeInvalidRequest, "users"},
		{"a group that is not an id", rule(`{"rule_id":"r","action":"allow","users":["group:finance"]}`), apierr.CodeInvalidRequest, "users"},
		{"an unknown kind", rule(`{"rule_id":"r","action":"allow","users":["role:admin"]}`), apierr.CodeInvalidRequest, "users"},
		{"an empty department", rule(`{"rule_id":"r","action":"allow","users":["department:"]}`), apierr.CodeInvalidRequest, "users"},
		{"a department of 1025 characters", rule(`{"rule_id":"r","action":"allow","users":["department:` + strings.Repeat("d", 1025) + `"]}`), apierr.CodeInvalidRequest, "users"},
		{"a value twice", rule(`{"rule_id":"r","action":"allow","users":["department:Finance","department:Finance"]}`), apierr.CodeInvalidRequest, "users"},
		{"users that is not a list", rule(`{"rule_id":"r","action":"allow","users":"department:Finance"}`), apierr.CodeSchemaViolation, ""},
		{"enabled that is not a boolean", rule(`{"rule_id":"r","action":"allow","enabled":"no"}`), apierr.CodeSchemaViolation, ""},
	}
	for _, c := range cases {
		rec := r.do(t, "admin", "PUT", rulesPath, c.body)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, rec.Code, rec.Body, c.code)
			continue
		}
		if c.field != "" {
			var e struct {
				Error struct {
					Detail struct {
						Index int    `json:"index"`
						Field string `json:"field"`
					} `json:"detail"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if e.Error.Detail.Index != 1 || e.Error.Detail.Field != c.field {
				t.Errorf("%s: names rule %d field %q, want rule 1 field %q: %s", c.name, e.Error.Detail.Index, e.Error.Detail.Field, c.field, rec.Body)
			}
		}
	}
	if got := r.rules(t); got != before {
		t.Fatalf("a refused write changed the rules:\n got %s\nwant %s", got, before)
	}
	if n := len(r.store.Audits()); n != 1 {
		t.Fatalf("audits = %d, want the seed's 1", n)
	}

	// The boundaries are accepted: a message of 280 characters, an allow with no message, a link
	// with a path and query, a redirect with its link, and the most rules a list may hold.
	if rec := r.do(t, "admin", "PUT", rulesPath, rule(`{"rule_id":"r","action":"warn","message":"`+strings.Repeat("é", 280)+`","link":"https://intranet.example/ai?x=1"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("boundaries: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "PUT", rulesPath, rule(`{"rule_id":"r","action":"redirect","message":"Use Copilot.","link":"https://copilot.example/","match":{"tools":["app:chatgpt"]}}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("a redirect: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "PUT", rulesPath, `{"rules":[`+strings.Join(many[:100], ",")+`]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("100 rules: %d %s", rec.Code, rec.Body)
	}
}

// TestGetCarriesTheDataClasses: the Settings read names the labels a rule may use.
func TestGetCarriesTheDataClasses(t *testing.T) {
	r := newRig(t)
	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		DataClasses []string `json:"data_classes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"credential", "customer_pii", "government_id", "health", "legal_commercial", "payment_card", "source_code"}
	if !reflect.DeepEqual(got.DataClasses, want) {
		t.Fatalf("data_classes = %v, want %v", got.DataClasses, want)
	}
}

// TestGetCarriesTheAppCategories: the Settings read names the categories the app catalog's apps
// fall in, once each and sorted, and an empty list when the catalog is empty.
func TestGetCarriesTheAppCategories(t *testing.T) {
	r := newRig(t)
	read := func() string {
		t.Helper()
		rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var got struct {
			AppCategories json.RawMessage `json:"app_categories"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return string(got.AppCategories)
	}
	if got := read(); got != `[]` {
		t.Fatalf("app_categories without a catalog = %s, want []", got)
	}
	r.store.SetCatalog(
		store.CatalogApp{AppKey: "ollama", Category: "local_runtime"},
		store.CatalogApp{AppKey: "cursor", Category: "ide"},
		store.CatalogApp{AppKey: "vscode", Category: "ide"},
	)
	if got := read(); got != `["ide","local_runtime"]` {
		t.Fatalf("app_categories = %s", got)
	}
}

// TestRulesNameWhoTheyApplyTo: a rule keeps the people, groups, departments, units and teams it
// names and whether it is enabled; the GET names each value once, with how many people it reaches,
// and a group that no longer exists is read as not found.
func TestRulesNameWhoTheyApplyTo(t *testing.T) {
	r := newRig(t)
	const group = "group:0b6f3c2e-1d4a-4c8b-9e7f-112233445566"
	const gone = "team:0b6f3c2e-1d4a-4c8b-9e7f-112233445599"
	const person = "user:u_0123456789abcdef0123456789abcdef"
	r.store.SetRuleUser(tenantA, group, "Finance", 2, "u_11111111111111111111111111111111", "u_22222222222222222222222222222222")
	r.store.SetRuleUser(tenantA, person, "Ana Ruiz", 1, "u_0123456789abcdef0123456789abcdef")
	body := `{"rules":[
	  {"rule_id":"allow_legal","action":"allow","users":["` + group + `","department:Legal"],"enabled":false},
	  {"rule_id":"block_people","action":"block","message":"No.","users":["` + person + `","` + gone + `","` + group + `"]}]}`
	if rec := r.do(t, "admin", "PUT", rulesPath, body); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	empty := `{"labels":[],"tools":[],"categories":[],"sanction":[],"routes":[]}`
	want := canonical(t, `{"rules":[
	  {"rule_id":"allow_legal","action":"allow","match":`+empty+`,"message":"","users":["`+group+`","department:Legal"],"enabled":false},
	  {"rule_id":"block_people","action":"block","match":`+empty+`,"message":"No.","users":["`+person+`","`+gone+`","`+group+`"],"enabled":true}],
	 "users":[
	  {"value":"department:Legal","name":"Legal","members":0,"found":true},
	  {"value":"`+group+`","name":"Finance","members":2,"found":true},
	  {"value":"`+gone+`","name":"","members":0,"found":false},
	  {"value":"`+person+`","name":"Ana Ruiz","members":1,"found":true}]}`)
	if got := r.rules(t); got != want {
		t.Fatalf("GET:\n got %s\nwant %s", got, want)
	}
	audits := r.store.Audits()
	next, _ := json.Marshal(audits[len(audits)-1].Detail["new"])
	if !strings.Contains(string(next), `"enabled":false`) || !strings.Contains(string(next), `"users":["`+person) {
		t.Fatalf("the audit does not carry who the rules apply to or that one is disabled: %s", next)
	}
}
