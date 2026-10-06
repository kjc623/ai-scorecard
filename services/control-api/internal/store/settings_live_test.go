package store_test

import (
	"context"
	"errors"
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
	if err := st.SetScopeOverride(ctx, tenant, "tls_b6681b043244c43f", &m2, audit); !errors.Is(err, store.ErrScopeOverrideTooWide) {
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
	if err := st.SetScopeOverride(ctx, tenant, "tls_b6681b043244c43f", &m2, audit); !errors.Is(err, store.ErrScopeOverrideTooWide) {
		t.Fatalf("override wider than the requested mode: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "tls_b6681b043244c43f", &m0, audit); err != nil {
		t.Fatalf("narrower override: %v", err)
	}
	if err := st.SetScopeOverride(ctx, tenant, "tls_b6681b043244c43f", nil, audit); err != nil {
		t.Fatalf("clear override: %v", err)
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
	if err := st.SetToolSanction(ctx, tenant, "tls_b6681b043244c43f", "unsanctioned", audit); err != nil {
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
		if tool.ToolFingerprint == "tls_b6681b043244c43f" && tool.SanctionedState == "unsanctioned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sanctioned tool not in %+v", s.Tools)
	}
}
