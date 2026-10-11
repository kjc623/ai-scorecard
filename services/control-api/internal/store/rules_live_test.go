package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// TestEnforcementRulesAgainstPostgres drives the rules as sac_control: a new tenant has none; a
// replacement is read back in its order by the Settings route and the policy read; a list naming a
// label outside ref.data_class is refused and leaves the list in force; each replacement is audited
// with the previous and the new list. The policy read also carries the sanctioned tools.
func TestEnforcementRulesAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	audit := store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
		Action: "tenant.enforcement_rules.set", ObjectType: "tenant", ObjectID: tenant, OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}

	read := func() []store.EnforcementRule {
		t.Helper()
		rules, err := st.EnforcementRules(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		in, err := st.PolicyInputs(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rules, in.Rules) {
			t.Fatalf("the Settings read %+v and the policy read %+v differ", rules, in.Rules)
		}
		return rules
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("a new tenant has rules %+v", got)
	}
	s, err := st.Settings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"credential", "customer_pii", "government_id", "health", "legal_commercial", "payment_card", "source_code"}; !reflect.DeepEqual(s.DataClasses, want) {
		t.Fatalf("data classes = %v, want %v", s.DataClasses, want)
	}

	empty := store.RuleMatch{Labels: []string{}, Tools: []string{}, Categories: []string{}, Sanction: []string{}, Routes: []string{}}
	first := []store.EnforcementRule{
		{RuleID: "block_credentials", Action: "block", Match: store.RuleMatch{Labels: []string{"credential"}, Tools: []string{},
			Categories: []string{}, Sanction: []string{}, Routes: []string{"proxy.tls"}},
			Message: "Remove the credential and try again.", Link: "https://intranet.example/ai", Users: []string{}},
		{RuleID: "warn.unsanctioned", Action: "warn", Match: store.RuleMatch{Labels: []string{}, Tools: []string{"cursor"},
			Categories: []string{"coding_agent"}, Sanction: []string{"unsanctioned"}, Routes: []string{}},
			Message: "Use the approved coding agent.", Users: []string{}},
		{RuleID: "allow_rest", Action: "allow", Match: empty, Users: []string{}},
	}
	if err := st.ReplaceEnforcementRules(ctx, tenant, first, audit); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, first) {
		t.Fatalf("after the first replacement:\n got %+v\nwant %+v", got, first)
	}

	// The same ids in another order, one dropped: the positions are reassigned in one transaction.
	second := []store.EnforcementRule{first[2], first[0]}
	if err := st.ReplaceEnforcementRules(ctx, tenant, second, audit); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, second) {
		t.Fatalf("after the reorder:\n got %+v\nwant %+v", got, second)
	}

	// A label outside ref.data_class refuses the whole list; the one in force stays.
	bad := append([]store.EnforcementRule{}, second...)
	bad = append(bad, store.EnforcementRule{RuleID: "secrets", Action: "block", Message: "x",
		Match: store.RuleMatch{Labels: []string{"credential", "secrets"}}})
	if err := st.ReplaceEnforcementRules(ctx, tenant, bad, audit); !errors.Is(err, store.ErrUnknownRuleLabel) {
		t.Fatalf("unknown label: %v", err)
	}
	if got := read(); !reflect.DeepEqual(got, second) {
		t.Fatalf("a refused list changed the rules: %+v", got)
	}
	if err := st.ReplaceEnforcementRules(ctx, pgtest.UUID(t), second, audit); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := st.EnforcementRules(ctx, pgtest.UUID(t)); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant read: %v", err)
	}

	// The audit rows carry the previous and the new list, in order.
	rows, err := owner.QueryContext(ctx, `SELECT detail->'previous', detail->'new' FROM ops.audit
		WHERE tenant_id = $1::uuid AND action = 'tenant.enforcement_rules.set' ORDER BY audit_seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var previous, next []byte
		if err := rows.Scan(&previous, &next); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]string{canonicalJSON(t, previous), canonicalJSON(t, next)})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	detail := func(rules []store.EnforcementRule) string {
		b, _ := json.Marshal(store.RulesDetail(rules))
		return canonicalJSON(t, b)
	}
	want := [][2]string{{"[]", detail(first)}, {detail(first), detail(second)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audits =\n%v\nwant\n%v", got, want)
	}

	// The policy read carries every catalogue fingerprint of the sanctioned tools, sorted, and only
	// those, and the catalogue's fingerprints by tool for the rules to expand to.
	for key, state := range map[string]string{"cursor": "sanctioned", "claude_code": "sanctioned", "windsurf": "unsanctioned"} {
		if err := st.SetToolSanction(ctx, tenant, key, state, store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser,
			ActorID: "admin@example.com", Action: "tool.sanction", ObjectType: "tool", ObjectID: key, OccurredAt: audit.OccurredAt}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetToolSanction(ctx, tenant, "tls_b6681b043244c43f", "sanctioned", audit); !errors.Is(err, store.ErrUnknownTool) {
		t.Fatalf("a fingerprint is not a tool: %v", err)
	}
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	wantSanctioned := []string{"app:claude_code", "app:cursor", "tls_14a9086e088f0d82", "tls_31299ef7601928f7", "tls_5a5a41ed0bf50d9d",
		"tls_69f6ab029df3c019", "tls_9230903190dcf0dc", "tls_b6681b043244c43f", "tls_cb53d2b2add3d450"}
	if !reflect.DeepEqual(in.SanctionedTools, wantSanctioned) {
		t.Fatalf("sanctioned tools = %v, want %v", in.SanctionedTools, wantSanctioned)
	}
	if got := in.ToolFingerprints["cursor"]; !reflect.DeepEqual(got, []string{"app:cursor", "tls_14a9086e088f0d82", "tls_9230903190dcf0dc"}) {
		t.Fatalf("cursor fingerprints = %v", got)
	}
}

func canonicalJSON(t *testing.T, doc []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("%v: %s", err, doc)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRuleUsersAgainstPostgres drives who rules apply to as sac_control: a person, a provisioned
// group (its active members only), a department, a unit and the units below it, and a team reach
// their members' canonical refs and the aliases of those refs; a disabled rule and a group since
// removed reach nobody; the console's read names each value with how many people it reaches.
func TestRuleUsersAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	audit := store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
		Action: "tenant.enforcement_rules.set", ObjectType: "tenant", ObjectID: tenant, OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}
	ref := func(n byte) string { return "u_" + strings.Repeat(string("0123456789abcdef"[n]), 32) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ana, bo, cy, di := ref(1), ref(2), ref(3), ref(4)
	group, gone, team := pgtest.UUID(t), pgtest.UUID(t), pgtest.UUID(t)
	user := func(n byte, r string, active bool) string {
		id := pgtest.UUID(t)
		exec(`INSERT INTO ops.scim_user (tenant_id, scim_id, user_name_hash, resource_enc, user_ref, active)
		      VALUES ($1::uuid, $2::uuid, decode(repeat($3, 32), 'hex'), '\x00', $4, $5)`, tenant, id, fmt.Sprintf("%02x", n), r, active)
		return id
	}
	anaID, boID := user(1, ana, true), user(2, bo, false)
	exec(`INSERT INTO ops.scim_group (tenant_id, scim_id, display_name) VALUES ($1::uuid, $2::uuid, 'Finance')`, tenant, group)
	exec(`INSERT INTO ops.scim_group_member (tenant_id, group_id, user_id) VALUES ($1::uuid, $2::uuid, $3::uuid), ($1::uuid, $2::uuid, $4::uuid)`, tenant, group, anaID, boID)
	exec(`INSERT INTO ops.user_ref_alias (tenant_id, alias_ref, user_ref) VALUES ($1::uuid, $2, $3)`, tenant, ref(9), ana)
	exec(`INSERT INTO ops.user_dim (tenant_id, user_ref, department, org_unit, display_name) VALUES
	      ($1::uuid, $2, 'Legal', 'OU=Paris,OU=Legal,DC=contoso,DC=com', 'Cy Young'),
	      ($1::uuid, $3, 'Sales', 'OU=Legal,DC=contoso,DC=com', NULL)`, tenant, cy, di)
	exec(`INSERT INTO ops.team (tenant_id, team_id, name, created_by) VALUES ($1::uuid, $2::uuid, 'Pilot', 'x')`, tenant, team)
	exec(`INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by) VALUES ($1::uuid, $2::uuid, $3, 'x')`, tenant, team, di)

	rules := []store.EnforcementRule{
		{RuleID: "allow_finance", Action: "allow", Users: []string{"group:" + group}},
		{RuleID: "allow_legal", Action: "allow", Users: []string{"department:legal"}},
		{RuleID: "allow_unit", Action: "allow", Users: []string{"org_unit:OU=Legal,DC=contoso,DC=com"}},
		{RuleID: "allow_pilot", Action: "allow", Users: []string{"team:" + team, "user:" + cy}},
		{RuleID: "allow_off", Action: "allow", Users: []string{"user:" + cy}, Disabled: true},
		{RuleID: "block_gone", Action: "block", Message: "x", Users: []string{"group:" + gone}},
		{RuleID: "block_rest", Action: "block", Message: "x"},
	}
	if err := st.ReplaceEnforcementRules(ctx, tenant, rules, audit); err != nil {
		t.Fatal(err)
	}
	read, err := st.EnforcementRules(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !read[4].Disabled || read[0].Disabled || !reflect.DeepEqual(read[3].Users, rules[3].Users) || !reflect.DeepEqual(read[6].Users, []string{}) {
		t.Fatalf("rules read back as %+v", read)
	}
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"allow_finance": {ana, ref(9)},
		"allow_legal":   {cy},
		"allow_unit":    {cy, di},
		"allow_pilot":   {cy, di},
	}
	if !reflect.DeepEqual(in.RuleUserRefs, want) {
		t.Fatalf("rule user refs = %v, want %v", in.RuleUserRefs, want)
	}
	users, err := st.RuleUsers(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	byValue := map[string]store.RuleUser{}
	for _, u := range users {
		byValue[u.Value] = u
	}
	for value, w := range map[string]store.RuleUser{
		"group:" + group:                      {Name: "Finance", Members: 1, Found: true},
		"group:" + gone:                       {Name: "", Members: 0, Found: false},
		"department:legal":                    {Name: "legal", Members: 1, Found: true},
		"org_unit:OU=Legal,DC=contoso,DC=com": {Name: "OU=Legal,DC=contoso,DC=com", Members: 2, Found: true},
		"team:" + team:                        {Name: "Pilot", Members: 1, Found: true},
		"user:" + cy:                          {Name: "Cy Young", Members: 1, Found: true},
	} {
		w.Value = value
		if byValue[value] != w {
			t.Errorf("%s = %+v, want %+v", value, byValue[value], w)
		}
	}
	if len(users) != 6 {
		t.Fatalf("rule users = %+v, want each value once", users)
	}
}
