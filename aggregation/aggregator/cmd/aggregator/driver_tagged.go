//go:build sac_sql_driver

package main

import (
	// Blank import for its side effect: it registers the "pgx" driver with database/sql, which is
	// what lets the runner's statements reach PostgreSQL. Behind the build tag so the default build
	// stays standard-library only, exactly as ingest-api, control-api and content-vault do.
	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	defaultDriverName = "pgx"
}
