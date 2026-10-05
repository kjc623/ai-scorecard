//go:build sac_sql_driver

package main

import (
	// Blank import for its side effect: it registers the "pgx" driver with database/sql, which is
	// what turns internal/store's SQL statements into a working store. It sits behind the build tag
	// so the default build stays standard-library only, exactly as control-api and ingest-api do.
	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	// The tagged build knows which driver it carries. The default build leaves this empty and
	// refuses `--store sql` with the actionable message in main.go.
	defaultDriverName = "pgx"
}
