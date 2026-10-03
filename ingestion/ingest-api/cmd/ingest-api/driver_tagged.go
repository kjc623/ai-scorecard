//go:build sac_sql_driver

package main

import (
	// Blank import for its side effect: this pulls in sqlpg, which registers the "pgx" driver with
	// database/sql. Without it the tagged binary would still have no driver and `-store sql` would
	// fail at sql.Open with "unknown driver" — the same refusal as the default build, one layer
	// further in.
	_ "github.com/shadow-ai-capture/ingest-api/sqlpg"
)

func init() {
	// The tagged build knows which driver it carries, so -driver is optional. The default build
	// leaves this empty and refuses `-store sql` with the actionable message in main.go.
	defaultDriverName = "pgx"
}
