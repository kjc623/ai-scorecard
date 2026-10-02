module github.com/shadow-ai-capture/ingest-api

go 1.27.0

require (
	github.com/shadow-ai-capture/device/protocol v0.0.0
	shadow-ai-capture.invalid/contracts/generated/go v0.0.0
)

// The device-side protocol types are the on-disk source of truth for the wire shapes of
// POST /v1/events (docs/02-ingest-and-transport.md §5.3). They are consumed, never forked.
replace github.com/shadow-ai-capture/device/protocol => ../../device/protocol

// T1's generated envelope types (ADR 0010: the schema is the only permitted source of field
// names). They are consumed for the envelope's *field vocabulary*, so a wire shape cannot drift
// without a compile error here.
replace shadow-ai-capture.invalid/contracts/generated/go => ../../contracts/generated/go
