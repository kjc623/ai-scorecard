// Package pgtest opens the PostgreSQL database a live test runs against.
//
// A live test runs only when SAC_TEST_PG_DSN names a database (a postgres:// URL) that has
// services/database/schema.sql applied; otherwise it skips. The DSN's user seeds fixture rows and must be
// able to SET ROLE to the runtime roles, so OpenAs can exercise the service's statements under the
// same grants and row-level security the deployed service has. Every fixture uses a random tenant
// id, so a test never reads or writes another tenant's rows.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// EnvDSN names the test database.
const EnvDSN = "SAC_TEST_PG_DSN"

// Open returns a pool on the test database as the DSN's own user, or skips the test.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	return open(t, "")
}

// OpenAs returns a pool whose every connection runs SET ROLE role first, or skips the test.
func OpenAs(t testing.TB, role string) *sql.DB {
	t.Helper()
	return open(t, role)
}

func open(t testing.TB, role string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("live database test skipped: set %s to a postgres:// URL of a database with services/database/schema.sql applied", EnvDSN)
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		t.Fatalf("%s must be a postgres:// URL", EnvDSN)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("%s: %v", EnvDSN, err)
	}
	var opts []stdlib.OptionOpenDB
	if role != "" {
		opts = append(opts, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}))
	}
	db := stdlib.OpenDB(*cfg, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("%s is set but the database is not usable: %v", EnvDSN, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// UUID returns a random version-4 UUID.
func UUID(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Tenant inserts an active tenant with a random id through db (the owner pool) and returns the id.
func Tenant(t testing.TB, db *sql.DB, region string) string {
	t.Helper()
	id := UUID(t)
	if _, err := db.Exec(`INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode)
VALUES ($1::uuid, $2, 'active', $3, 'm1')`, id, "live-test "+id[:8], region); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}
