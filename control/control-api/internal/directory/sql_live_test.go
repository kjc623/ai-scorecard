//go:build sac_sql_driver

package directory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/sqlpg"
)

// The tagged live test: the exact statement text against a reachable PostgreSQL with
// database/schema.sql applied. It seeds its own tenant, removes it again, and SKIPS loudly when no
// server is reachable, because a skip with a reason is honest and a red gate on a machine without a
// database is not.
//
// The DSN follows the harness: SAC_PG_DSN first, then the standard PG* variables (the lab is
// reachable as postgres:5432 from inside the harness), then a localhost default.

func liveDSN() string {
	if v := os.Getenv("SAC_PG_DSN"); v != "" {
		return v
	}
	if host := os.Getenv("PGHOST"); host != "" {
		user := envOr("PGUSER", "postgres")
		db := envOr("PGDATABASE", "shadow")
		return fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", user, os.Getenv("PGPASSWORD"), host, db)
	}
	return "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func openLive(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(liveDSN())
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

// TestDirectorySQLStatementsPrepare proves every statement is valid SQL against the live schema,
// placeholders and all, without binding values.
func TestDirectorySQLStatementsPrepare(t *testing.T) {
	db := openLive(t)
	ctx := context.Background()
	for i, s := range DirectoryStatements {
		name := fmt.Sprintf("sac_dir_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		if _, err := db.ExecContext(ctx, "DEALLOCATE "+name); err != nil {
			t.Errorf("statement %q prepared but would not deallocate: %v", s.Name, err)
		}
	}
}

// TestSQLStoreSyncsRetiresAndLeavesUnmapped drives the real store and the real columns: a written
// row, a NULL department for the unmapped user, an encrypted identifier that opens, a retirement
// that marks inactive without deleting, and a hashed tenant that stores no display name.
func TestSQLStoreSyncsRetiresAndLeavesUnmapped(t *testing.T) {
	db := openLive(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)

	if _, err := db.ExecContext(ctx, `INSERT INTO ops.tenant
	  (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search)
	  VALUES ($1::uuid, 'directory-live', 'active', 'eu-west', 'vendor', 'm1', 'disabled')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM ops.user_dim WHERE tenant_id = $1::uuid`, tenant)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM ops.tenant WHERE tenant_id = $1::uuid`, tenant)
	})

	cipher := testCipher(t)
	store := NewSQL(db)
	syncer := &Syncer{Source: sliceSource{name: "live", users: []User{
		{UserRef: "u_eng", DirectoryID: "dir-eng", DisplayName: "Eng Person", Department: "Engineering", Status: StatusActive},
		{UserRef: "u_unmapped", DirectoryID: "dir-un", DisplayName: "Unmapped Person", Status: StatusActive},
		{UserRef: "u_legal", DirectoryID: "dir-legal", Department: "Legal", Status: StatusActive},
	}}, Store: store, Cipher: cipher}

	res, err := syncer.Sync(ctx, tenant)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Synced != 3 || res.Unmapped != 1 {
		t.Fatalf("sync = %+v, want synced 3 unmapped 1", res)
	}

	var rows, unmapped int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE department IS NULL)
	  FROM ops.user_dim WHERE tenant_id = $1::uuid`, tenant).Scan(&rows, &unmapped); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 3 || unmapped != 1 {
		t.Fatalf("rows = %d, unmapped = %d; want 3 and 1", rows, unmapped)
	}

	var sealed []byte
	if err := db.QueryRowContext(ctx, `SELECT directory_object_id_enc FROM ops.user_dim
	  WHERE tenant_id = $1::uuid AND user_ref = 'u_eng'`, tenant).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if got, err := cipher.Open(tenant, sealed); err != nil || got != "dir-eng" {
		t.Fatalf("Open = %q, %v; want dir-eng", got, err)
	}

	// Retire: the second read no longer returns Legal. The row stays with its department.
	syncer.Source = sliceSource{name: "live", users: []User{
		{UserRef: "u_eng", DirectoryID: "dir-eng", Department: "Engineering", Status: StatusActive},
		{UserRef: "u_unmapped", DirectoryID: "dir-un", Status: StatusActive},
	}}
	second, err := syncer.Sync(ctx, tenant)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if second.Retired != 1 {
		t.Fatalf("second sync retired %d, want 1", second.Retired)
	}
	var status, dept string
	if err := db.QueryRowContext(ctx, `SELECT status, department FROM ops.user_dim
	  WHERE tenant_id = $1::uuid AND user_ref = 'u_legal'`, tenant).Scan(&status, &dept); err != nil {
		t.Fatalf("read retired row: %v", err)
	}
	if status != "inactive" || strings.TrimSpace(dept) != "Legal" {
		t.Fatalf("retired row = (%s, %s); want inactive with its department kept", status, dept)
	}

	// A hashed tenant stores no clear display name, though the directory returned one.
	if _, err := db.ExecContext(ctx, `UPDATE ops.tenant SET device_identity = 'hashed' WHERE tenant_id = $1::uuid`, tenant); err != nil {
		t.Fatalf("set hashed: %v", err)
	}
	if _, err := syncer.Sync(ctx, tenant); err != nil {
		t.Fatalf("hashed Sync: %v", err)
	}
	var display sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT display_name FROM ops.user_dim
	  WHERE tenant_id = $1::uuid AND user_ref = 'u_eng'`, tenant).Scan(&display); err != nil {
		t.Fatalf("read display name: %v", err)
	}
	if display.Valid {
		t.Fatalf("a hashed tenant stored display name %q", display.String)
	}
}
