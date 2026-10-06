// Package pgtest runs the jobs' database tests against the PostgreSQL database named by
// SAC_TEST_PG_DSN, which must have services/database/schema.sql applied. Each test works inside one
// transaction, on a tenant with a random id, and the transaction is rolled back when the test
// ends, so the database is left as it was found. Without SAC_TEST_PG_DSN the tests skip.
package pgtest

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// EnvDSN names the test database, as a postgres:// URL. The role it connects as must be able to
// create fixtures and SET ROLE sac_ops.
const EnvDSN = "SAC_TEST_PG_DSN"

// Open returns the test database, or skips the test when SAC_TEST_PG_DSN is not set.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("database test skipped: set %s to a postgres:// URL of a database with services/database/schema.sql applied", EnvDSN)
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		t.Fatalf("%s must be a postgres:// URL", EnvDSN)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", EnvDSN, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("connect to %s: %v", EnvDSN, err)
	}
	return db
}

// Tx begins a transaction that is rolled back when the test ends.
func Tx(t testing.TB, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// Tenant creates a tenant with a random id and one device in tx, scopes tx to that tenant, and
// returns both ids.
func Tenant(t testing.TB, tx *sql.Tx) (tenant, device string) {
	t.Helper()
	tenant, device = NewUUID(t, tx), NewUUID(t, tx)
	Exec(t, tx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	Exec(t, tx, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode)
	             VALUES ($1, 'jobs-test', 'active', 'test-region', 'm3')`, tenant)
	Exec(t, tx, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1, $2, 'windows')`, tenant, device)
	return tenant, device
}

// AsJobs makes the rest of tx run as sac_ops, the role the jobs' login holds, until AsOwner.
func AsJobs(t testing.TB, tx *sql.Tx) {
	t.Helper()
	Exec(t, tx, `SET LOCAL ROLE sac_ops`)
}

// AsOwner returns tx to the role the test connected as.
func AsOwner(t testing.TB, tx *sql.Tx) {
	t.Helper()
	Exec(t, tx, `RESET ROLE`)
}

// Exec runs one statement and fails the test on error.
func Exec(t testing.TB, tx *sql.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

// Scalar runs a query returning one value and renders it as text, "<null>" for NULL.
func Scalar(t testing.TB, tx *sql.Tx, query string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := tx.QueryRowContext(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	if !v.Valid {
		return "<null>"
	}
	return v.String
}

// NewUUID returns a random UUID from the server.
func NewUUID(t testing.TB, tx *sql.Tx) string {
	t.Helper()
	return Scalar(t, tx, `SELECT gen_random_uuid()::text`)
}
