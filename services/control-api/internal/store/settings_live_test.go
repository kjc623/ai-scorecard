package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// TestSettingsAgainstPostgres drives the Settings write path as sac_control against the database,
// asserting the combinations the database refuses: a requested mode above the ceiling, a search
// tier the ceiling cannot back, and a per-tool override wider than the requested mode.
func TestSettingsAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus") // ceiling m1
	now := time.Now().UTC().Truncate(time.Microsecond)
	audit := store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
		Action: "test", ObjectType: "tenant", ObjectID: tenant, OccurredAt: now}

	m0, m1, m2 := "m0", "m1", "m2"

	// A requested mode above the ceiling is refused by the CHECK.
	if err := st.SetCollectionMode(ctx, tenant, &m2, audit); !errors.Is(err, store.ErrCollectionExceedsCeiling) {
		t.Fatalf("collection mode above ceiling: %v", err)
	}

	// A scope override wider than the ceiling is refused even while following the ceiling.
	if err := st.SetScopeOverride(ctx, tenant, "claude_code", &m2, audit); !errors.Is(err, store.ErrScopeOverrideTooWide) {
		t.Fatalf("override above the ceiling: %v", err)
	}

	// full_text needs an M3 ceiling; attachment_names need above M0.
	if err := st.SetContentSearch(ctx, tenant, "full_text", audit); !errors.Is(err, store.ErrSearchTierRequiresCeiling) {
		t.Fatalf("full_text on an M1 ceiling: %v", err)
	}
	if err := st.SetContentSearch(ctx, tenant, "attachment_names", audit); err != nil {
		t.Fatalf("attachment_names on an M1 ceiling: %v", err)
	}
	if err := st.SetContentSearch(ctx, tenant, "disabled", audit); err != nil {
		t.Fatalf("disabled: %v", err)
	}

	// The requested mode is set to the ceiling, then a wider override is refused.
	if err := st.SetCollectionMode(ctx, tenant, &m1, audit); err != nil {
		t.Fatalf("collection mode m1: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "claude_code", &m2, audit); !errors.Is(err, store.ErrScopeOverrideTooWide) {
		t.Fatalf("override wider than the requested mode: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "claude_code", &m0, audit); err != nil {
		t.Fatalf("narrower override: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "claude_code", nil, audit); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "tls_b6681b043244c43f", &m0, audit); !errors.Is(err, store.ErrUnknownTool) {
		t.Fatalf("override on a fingerprint rather than a tool: %v", err)
	}

	// Retention writes the policy matrix and reads back the one value.
	if err := st.SetRetention(ctx, tenant, "event", 120, audit); err != nil {
		t.Fatalf("event retention: %v", err)
	}
	if err := st.SetRetention(ctx, tenant, "content", 45, audit); err != nil {
		t.Fatalf("content retention: %v", err)
	}
	if err := st.SetRetention(ctx, tenant, "other", 1, audit); !errors.Is(err, store.ErrRetentionOutOfRange) {
		t.Fatalf("unknown applies_to: %v", err)
	}

	// A sanction decision is attributed and read back.
	if err := st.SetToolSanction(ctx, tenant, "claude_code", "unsanctioned", audit); err != nil {
		t.Fatalf("tool sanction: %v", err)
	}

	s, err := st.Settings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if s.CeilingMode != "m1" || s.CollectionMode != "m1" || s.ContentSearch != "disabled" {
		t.Fatalf("settings = %+v", s)
	}
	if len(s.ScopeOverrides) != 0 {
		t.Fatalf("overrides not cleared: %+v", s.ScopeOverrides)
	}
	if s.EventRetentionDays == nil || *s.EventRetentionDays != 120 || s.ContentRetentionDays == nil || *s.ContentRetentionDays != 45 {
		t.Fatalf("retention = %v / %v", s.EventRetentionDays, s.ContentRetentionDays)
	}
	if s.RetentionDefaults.EventDays != 90 || s.RetentionDefaults.ContentDays != 30 {
		t.Fatalf("defaults = %+v", s.RetentionDefaults)
	}
	found := false
	for _, tool := range s.Tools {
		if tool.ToolKey == "claude_code" && tool.SanctionedState == "unsanctioned" && slices.Contains(tool.Fingerprints, "tls_b6681b043244c43f") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sanctioned tool not in %+v", s.Tools)
	}
}

// TestEndpointSettingsAgainstPostgres: a tenant without rows reads the defaults through both the
// Settings page and the policy inputs; the writes are read back and each writes its audit row.
func TestEndpointSettingsAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	now := time.Now().UTC().Truncate(time.Microsecond)
	audit := func(action string) store.AuditEntry {
		return store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
			Action: action, ObjectType: "tenant", ObjectID: tenant, OccurredAt: now}
	}

	defaults := store.EndpointSettings{
		Collectors: store.EndpointCollectors{Inventory: true, Processes: true, Flows: true, OTel: true, Hooks: true},
		Tools: map[string]store.EndpointTool{
			"claude_code": {OTel: true, Hooks: true},
			"codex":       {OTel: true, Hooks: true},
			"copilot":     {OTel: true},
			"cursor":      {Hooks: true},
			"ollama":      {},
		},
	}
	s, err := st.Settings(ctx, tenant)
	if err != nil || !reflect.DeepEqual(s.Endpoint, defaults) {
		t.Fatalf("Settings endpoint = %+v, %v; want the defaults", s.Endpoint, err)
	}
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil || !reflect.DeepEqual(in.Endpoint, defaults) {
		t.Fatalf("PolicyInputs endpoint = %+v, %v; want the defaults", in.Endpoint, err)
	}

	collectors := store.EndpointCollectors{Inventory: false, Processes: true, Flows: false, OTel: true, Hooks: false, HooksManagedOnly: true}
	if err := st.SetEndpointCollectors(ctx, tenant, collectors, audit("tenant.endpoint_collectors.set")); err != nil {
		t.Fatal(err)
	}
	collectors.Flows = true // the second write updates the row the first inserted
	if err := st.SetEndpointCollectors(ctx, tenant, collectors, audit("tenant.endpoint_collectors.set")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEndpointTool(ctx, tenant, "cursor", store.EndpointTool{}, audit("tenant.endpoint_tool.set")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEndpointTool(ctx, tenant, "ollama", store.EndpointTool{Loopback: true}, audit("tenant.endpoint_tool.set")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEndpointTool(ctx, tenant, "lm_studio", store.EndpointTool{}, audit("x")); !errors.Is(err, store.ErrUnknownEndpointTool) {
		t.Fatalf("unknown tool key: %v", err)
	}
	if err := st.SetEndpointCollectors(ctx, pgtest.UUID(t), collectors, audit("x")); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}

	want := store.EndpointSettings{Collectors: collectors, Tools: map[string]store.EndpointTool{}}
	for k, v := range defaults.Tools {
		want.Tools[k] = v
	}
	want.Tools["cursor"] = store.EndpointTool{}
	want.Tools["ollama"] = store.EndpointTool{Loopback: true}
	in, err = st.PolicyInputs(ctx, tenant)
	if err != nil || !reflect.DeepEqual(in.Endpoint, want) {
		t.Fatalf("PolicyInputs endpoint = %+v, %v; want %+v", in.Endpoint, err, want)
	}

	// The audit rows carry the old and the new values.
	rows, err := owner.QueryContext(ctx, `SELECT action, detail->'previous', detail->'new' FROM ops.audit
		WHERE tenant_id = $1::uuid ORDER BY audit_seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, previous, next string
		if err := rows.Scan(&action, &previous, &next); err != nil {
			t.Fatal(err)
		}
		got = append(got, action+" "+previous+" "+next)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantAudits := []string{
		`tenant.endpoint_collectors.set {"otel": true, "flows": true, "hooks": true, "inventory": true, "processes": true, "hooks_managed_only": false} {"otel": true, "flows": false, "hooks": false, "inventory": false, "processes": true, "hooks_managed_only": true}`,
		`tenant.endpoint_collectors.set {"otel": true, "flows": false, "hooks": false, "inventory": false, "processes": true, "hooks_managed_only": true} {"otel": true, "flows": true, "hooks": false, "inventory": false, "processes": true, "hooks_managed_only": true}`,
		`tenant.endpoint_tool.set {"otel": false, "hooks": true} {"otel": false, "hooks": false}`,
		`tenant.endpoint_tool.set {"loopback": false} {"loopback": true}`,
	}
	if !reflect.DeepEqual(got, wantAudits) {
		t.Fatalf("audits =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantAudits, "\n"))
	}
}

// TestTLSInspectionAgainstPostgres: a new tenant has TLS inspection off in both the Settings page
// and the policy inputs; turning it on and off is read back, and each write is audited with the old
// and the new value.
func TestTLSInspectionAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	audit := store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
		Action: "tenant.tls_inspection.set", ObjectType: "tenant", ObjectID: tenant, OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}

	read := func() (bool, bool) {
		t.Helper()
		s, err := st.Settings(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		in, err := st.PolicyInputs(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		return s.TLSInspection, in.Tenant.TLSInspection
	}
	if page, policy := read(); page || policy {
		t.Fatalf("a new tenant reads TLS inspection %t (Settings) and %t (policy), want off", page, policy)
	}
	if err := st.SetTLSInspection(ctx, tenant, true, audit); err != nil {
		t.Fatal(err)
	}
	if page, policy := read(); !page || !policy {
		t.Fatalf("after turning it on: %t (Settings) and %t (policy), want on", page, policy)
	}
	if err := st.SetTLSInspection(ctx, tenant, false, audit); err != nil {
		t.Fatal(err)
	}
	if page, policy := read(); page || policy {
		t.Fatalf("after turning it off: %t (Settings) and %t (policy), want off", page, policy)
	}
	if err := st.SetTLSInspection(ctx, pgtest.UUID(t), true, audit); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}

	rows, err := owner.QueryContext(ctx, `SELECT detail->>'previous', detail->>'new' FROM ops.audit
		WHERE tenant_id = $1::uuid AND action = 'tenant.tls_inspection.set' ORDER BY audit_seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var previous, next string
		if err := rows.Scan(&previous, &next); err != nil {
			t.Fatal(err)
		}
		got = append(got, previous+">"+next)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"false>true", "true>false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audits = %v, want %v", got, want)
	}
}
