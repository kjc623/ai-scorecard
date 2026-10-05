//go:build sac_sql_driver

package session_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/session"
	"github.com/shadow-ai-capture/control-api/sqlpg"
)

// The tagged live test: the exact statement text, and the store through the real driver, against a
// PostgreSQL with database/schema.sql applied. It SKIPS loudly when no server is reachable.
//
// Point it at a throwaway database, not the lab's. It seeds its own tenant and connection and removes
// them, leaving the tenant row closed (other suites' audit rows reference tenants, and ops.audit is
// append-only). SAC_PG_DSN (then the PG* variables, then a localhost default) is the owner connection
// that seeds; SAC_PG_STORE_DSN, when set, is the one the store uses, so it can run as a member of
// sac_control under forced row-level security and the definer function's grant.

func liveDSN(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if name == "SAC_PG_STORE_DSN" {
		return liveDSN("SAC_PG_DSN")
	}
	if host := os.Getenv("PGHOST"); host != "" {
		return fmt.Sprintf("postgres://%s:%s@%s:5432/shadow?sslmode=disable", envOr("PGUSER", "postgres"), os.Getenv("PGPASSWORD"), host)
	}
	return "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func openLive(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(liveDSN(name))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("SKIPPING (not a failure): no PostgreSQL reachable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSessionStatementsPrepare(t *testing.T) {
	db := openLive(t, "SAC_PG_DSN")
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
	owner := openLive(t, "SAC_PG_DSN")
	storeDB := openLive(t, "SAC_PG_STORE_DSN")
	ctx := context.Background()
	n := time.Now().UnixNano() % 1_000_000_000_000
	tenant := fmt.Sprintf("00000000-0000-4000-8005-%012d", n)
	tid := fmt.Sprintf("00000000-0000-4000-8006-%012d", n)
	var connID string
	// Seeded with the tenant set, so it also works for an owner subject to forced RLS.
	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
	  VALUES ($1::uuid, 'session-live', 'active', 'eu-west', 'vendor', 'm1')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id, status, activated_at, activated_by)
	  VALUES ($1::uuid, 'entra', $2, 'active', now(), 'live-test') RETURNING connection_id::text`, tenant, tid).Scan(&connID); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM ops.auth_session WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.identity_connection WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'session live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := owner.ExecContext(context.Background(), q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

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
