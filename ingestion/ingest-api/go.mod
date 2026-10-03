module github.com/shadow-ai-capture/ingest-api

go 1.27.0

require (
	// Imported only by sqlpg under the `sac_sql_driver` build tag. An unused requirement is not
	// loaded by a build that does not use it — module-graph pruning means the default build needs
	// no module cache and no network for it, which is what keeps `go build ./...` green on a clean
	// machine. See sqlpg/doc.go for the two commands.
	github.com/jackc/pgx/v5 v5.11.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
	shadow-ai-capture.invalid/contracts/generated/go v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	// pgx's own requirements are older than the versions available here (pgx v5.11.0 asks for
	// golang.org/x/text v0.29.0 and x/sync v0.17.0). Pinning the newer ones is ordinary minimal
	// version selection — the higher requirement wins — and it is what lets the tagged build resolve
	// entirely offline. They are inert for the default build for the same reason pgx is.
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

// The device-side protocol types are the on-disk source of truth for the wire shapes of
// POST /v1/events (docs/02-ingest-and-transport.md §5.3). They are consumed, never forked.
replace github.com/shadow-ai-capture/device/protocol => ../../endpoint/protocol

// T1's generated envelope types (ADR 0010: the schema is the only permitted source of field
// names). They are consumed for the envelope's *field vocabulary*, so a wire shape cannot drift
// without a compile error here.
replace shadow-ai-capture.invalid/contracts/generated/go => ../../contracts/generated/go
