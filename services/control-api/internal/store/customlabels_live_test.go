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

// TestCustomLabelsAgainstPostgres drives the custom labels as sac_control: a new tenant has none;
// a list is read back by display name by the Settings route and the policy read; a rule may then
// name a label; a list leaving out a label a rule names is refused and leaves the list in force; a
// rename keeps the label; each replacement is audited with the previous and the new list.
func TestCustomLabelsAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	audit := store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
		Action: "tenant.custom_labels.set", ObjectType: "tenant", ObjectID: tenant, OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}

	read := func() []store.CustomLabel {
		t.Helper()
		s, err := st.Settings(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		in, err := st.PolicyInputs(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.CustomLabels, in.CustomLabels) {
			t.Fatalf("the Settings read %+v and the policy read %+v differ", s.CustomLabels, in.CustomLabels)
		}
		return s.CustomLabels
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("a new tenant has custom labels %+v", got)
	}

	falcon := store.CustomLabel{Label: "custom_project_falcon", DisplayName: "Project Falcon", Words: []string{"falcon"},
		CustomerNames: []string{"Acme Corp"}, FileExtensions: []string{}, FileNames: []string{"falcon-*.xlsx"}}
	cad := store.CustomLabel{Label: "custom_cad", DisplayName: "CAD drawings", Words: []string{}, CustomerNames: []string{},
		FileExtensions: []string{"dwg"}, FileNames: []string{}}
	if err := st.ReplaceCustomLabels(ctx, tenant, []store.CustomLabel{falcon, cad}, audit); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), []store.CustomLabel{cad, falcon}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the first replacement:\n got %+v\nwant %+v", got, want)
	}

	rule := []store.EnforcementRule{{RuleID: "block_falcon", Action: "block", Message: "Not for AI tools.",
		Match: store.RuleMatch{Labels: []string{"custom_project_falcon", "credential"}}}}
	rulesAudit := audit
	rulesAudit.Action = "tenant.enforcement_rules.set"
	if err := st.ReplaceEnforcementRules(ctx, tenant, rule, rulesAudit); err != nil {
		t.Fatalf("a rule naming a custom label: %v", err)
	}
	other := pgtest.Tenant(t, owner, "eastus")
	if err := st.ReplaceEnforcementRules(ctx, other, rule, rulesAudit); !errors.Is(err, store.ErrUnknownRuleLabel) {
		t.Fatalf("a rule naming another tenant's custom label: %v", err)
	}

	// Leaving out the label the rule names refuses the whole list; a rename and a label dropped
	// that no rule names are fine.
	if err := st.ReplaceCustomLabels(ctx, tenant, []store.CustomLabel{cad}, audit); !errors.Is(err, store.ErrCustomLabelInUse) {
		t.Fatalf("leaving out a label a rule names: %v", err)
	}
	if got, want := read(), []store.CustomLabel{cad, falcon}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a refused list changed the labels: %+v", got)
	}
	renamed := falcon
	renamed.DisplayName, renamed.Words = "Falcon", []string{"falcon", "project falcon"}
	if err := st.ReplaceCustomLabels(ctx, tenant, []store.CustomLabel{renamed}, audit); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), []store.CustomLabel{renamed}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the rename:\n got %+v\nwant %+v", got, want)
	}
	if err := st.ReplaceCustomLabels(ctx, pgtest.UUID(t), nil, audit); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}

	rows, err := owner.QueryContext(ctx, `SELECT detail->'previous', detail->'new' FROM ops.audit
		WHERE tenant_id = $1::uuid AND action = 'tenant.custom_labels.set' ORDER BY audit_seq`, tenant)
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
	detail := func(labels ...store.CustomLabel) string {
		b, _ := json.Marshal(store.CustomLabelsDetail(labels))
		return canonicalJSON(t, b)
	}
	// The first audit's new list is in the order sent; a previous list is in the order read.
	want := [][2]string{{"[]", detail(falcon, cad)}, {detail(cad, falcon), detail(renamed)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audits =\n%v\nwant\n%v", got, want)
	}
}
