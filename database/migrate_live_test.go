package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMigrateLive applies the real migrations to the database SAC_TEST_PG_DSN names, twice. The
// database must be empty, or one this test has migrated before; the connecting role must be able to
// create roles. The test leaves the migrated schema in place.
func TestMigrateLive(t *testing.T) {
	dsn := os.Getenv("SAC_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SAC_TEST_PG_DSN is not set: set it to a postgres:// URL naming an empty database to run the live migration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := Migrate(ctx, db, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var versions int
	var last time.Time
	if err := db.QueryRowContext(ctx, `SELECT count(*), max(applied_at) FROM public.schema_migration`).Scan(&versions, &last); err != nil {
		t.Fatal(err)
	}
	if versions != len(ms) {
		t.Fatalf("schema_migration holds %d versions, want %d", versions, len(ms))
	}

	if err := Migrate(ctx, db, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var again int
	var lastAgain time.Time
	if err := db.QueryRowContext(ctx, `SELECT count(*), max(applied_at) FROM public.schema_migration`).Scan(&again, &lastAgain); err != nil {
		t.Fatal(err)
	}
	if again != versions || !lastAgain.Equal(last) {
		t.Fatalf("the second run changed the ledger: %d versions at %s, then %d at %s", versions, last, again, lastAgain)
	}

	var forced bool
	if err := db.QueryRowContext(ctx, `SELECT relforcerowsecurity FROM pg_catalog.pg_class WHERE oid = 'ops.content'::regclass`).Scan(&forced); err != nil {
		t.Fatal(err)
	}
	if !forced {
		t.Fatal("ops.content does not force row-level security")
	}
}
