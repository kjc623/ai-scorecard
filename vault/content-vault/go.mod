module github.com/shadow-ai-capture/content-vault

go 1.27.0

require (
	// Imported only by cmd/content-vault/driver_tagged.go under the `sac_sql_driver` build tag,
	// exactly as control-api and ingest-api do. Module-graph pruning keeps it out of the default
	// build, which stays standard-library only.
	github.com/jackc/pgx/v5 v5.11.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	// The same pins control-api carries, so the tagged build resolves offline.
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

// The device-side protocol package is consumed for the collection-mode vocabulary (m0..m3) that
// the search tiers and the grant path branch on, and for the sealed-object format the device writes
// and this service opens, so neither has a second spelling here. It is consumed, never redefined.
replace github.com/shadow-ai-capture/device/protocol => ../../endpoint/protocol
