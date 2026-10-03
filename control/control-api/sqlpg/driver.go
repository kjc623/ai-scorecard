//go:build sac_sql_driver

package sqlpg

import (
	"database/sql"
	"fmt"
	"time"

	// Registers the "pgx" driver with database/sql. The blank import is the whole point of this
	// package: it is the one line that turns internal/store's SQL statements into a working store,
	// and it lives here so that nothing else in the module imports it.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// DriverName is the name database/sql knows this driver by: sql.Open(DriverName, dsn).
const DriverName = "pgx"

// DefaultMaxOpenConns bounds the pool. Control traffic is enrolment and token issuance, not an event
// stream, so the pool is sized for concurrency rather than throughput.
const DefaultMaxOpenConns = 16

// Open opens a PostgreSQL pool and returns the store the control path serves from. It does not ping:
// whether the database is reachable is a readiness question, answered by /readyz, which reads
// ops.tenant through this store. An Open that pinged would make a slow database a startup failure
// instead of an unready replica.
func Open(dsn string) (*store.SQLStore, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlpg: open %s: %w", DriverName, err)
	}
	db.SetMaxOpenConns(DefaultMaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return store.NewSQL(db), nil
}

// OpenDB is the same pool without the store around it, for callers that need raw SQL -- the
// integration test seeds tenants and devices with it.
func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlpg: open %s: %w", DriverName, err)
	}
	db.SetMaxOpenConns(DefaultMaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
