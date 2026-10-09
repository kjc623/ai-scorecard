package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
			Message: "Remove the credential and try again.", Link: "https://intranet.example/ai"},
		{RuleID: "warn.unsanctioned", Action: "warn", Match: store.RuleMatch{Labels: []string{}, Tools: []string{"app:cursor"},
			Categories: []string{"coding_agent"}, Sanction: []string{"unsanctioned"}, Routes: []string{}},
			Message: "Use the approved coding agent."},
		{RuleID: "allow_rest", Action: "allow", Match: empty},
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

	// The policy read carries the sanctioned tools, sorted, and only those.
	for fp, state := range map[string]string{"app:cursor": "sanctioned", "app:claude_code": "sanctioned", "app:windsurf": "unsanctioned"} {
		if err := st.SetToolSanction(ctx, tenant, fp, state, store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser,
			ActorID: "admin@example.com", Action: "tool.sanction", ObjectType: "tool", ObjectID: fp, OccurredAt: audit.OccurredAt}); err != nil {
			t.Fatal(err)
		}
	}
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"app:claude_code", "app:cursor"}; !reflect.DeepEqual(in.SanctionedTools, want) {
		t.Fatalf("sanctioned tools = %v, want %v", in.SanctionedTools, want)
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
