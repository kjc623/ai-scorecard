package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

// The live test: every statement prepared verbatim, then the store driven as sac_control under
// forced row-level security and the definer function's grant. It runs only against the database
// SAC_TEST_PG_DSN names.

func TestSessionStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	for i, s := range session.Statements {
		name := fmt.Sprintf("sac_session_probe_%d", i)
		if _, err := db.Exec("PREPARE " + name + " AS " + s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		_, _ = db.Exec("DEALLOCATE " + name)
	}
}

func TestSessionSQLStoreAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	storeDB := pgtest.OpenAs(t, "sac_control")
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eu-west")
	var connID string
	if err := owner.QueryRowContext(ctx, `INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id, status, activated_at, activated_by)
	  VALUES ($1::uuid, 'entra', $2, 'active', now(), 'live-test') RETURNING connection_id::text`, tenant, pgtest.UUID(t)).Scan(&connID); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	m, err := session.NewManager(session.NewSQL(storeDB), session.ManagerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	id, rec, err := m.Create(ctx, session.Record{TenantID: tenant, ConnectionID: connID, Subject: "oid-1", Actor: "live@example.test",
		Roles: []string{"admin", "viewer"}, RefreshEnc: []byte{1, 2, 3}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := m.Resume(ctx, id)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.TenantID != tenant || got.ConnectionID != connID || len(got.Roles) != 2 || got.RefreshedAt == nil || string(got.RefreshEnc) != "\x01\x02\x03" {
		t.Fatalf("round trip = %+v", got)
	}
	if err := m.Touch(ctx, rec); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := m.SetRefresh(ctx, rec, []byte{9}); err != nil {
		t.Fatalf("SetRefresh: %v", err)
	}
	if ended, err := m.End(ctx, rec); err != nil || !ended {
		t.Fatalf("End = %v, %v", ended, err)
	}
	if ended, err := m.End(ctx, rec); err != nil || ended {
		t.Fatalf("second End = %v, %v; a revoke must take effect once", ended, err)
	}
	if _, err := m.Resume(ctx, id); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("Resume after End: %v", err)
	}
	if _, err := m.Lookup(ctx, id); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the definer function returned a revoked session: %v", err)
	}
}
