//go:build sac_sql_driver

package sqlpg

import (
	"database/sql"
	"fmt"
	"time"

	// Registers the "pgx" driver with database/sql. The blank import is the whole point of this
	// package: it is the one line that turns internal/store's SQL statements into a working store,
	// and it lives here so that nothing else in the module has to import it.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// DriverName is the name database/sql knows this driver by: sql.Open(DriverName, dsn).
const DriverName = "pgx"

// DefaultMaxOpenConns bounds the pool. The sizing model is 0.14 events/s mean and ≤500 events/s
// worst case (master §1.4) at ~2 requests/s, so the pool is sized for concurrency, not throughput.
const DefaultMaxOpenConns = 16

// Open opens a PostgreSQL pool and returns the store the ingest write path serves from.
//
// It does not ping: whether the database is reachable is a readiness question, answered by the
// /readyz probe (cmd/ingest-api/probes.go), which reads ref.route_fidelity through this store. An
// Open that pinged would make a slow database a startup failure instead of an unready replica.
func Open(dsn string) (*store.SQLStore, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlpg: open %s: %w", DriverName, err)
	}
	db.SetMaxOpenConns(DefaultMaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return store.NewSQL(db), nil
}

// OpenDB is the same pool without the store around it, for callers that need raw SQL — the
// integration test seeds tenants and devices with it, and a migration or diagnostic tool would too.
func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlpg: open %s: %w", DriverName, err)
	}
	db.SetMaxOpenConns(DefaultMaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
