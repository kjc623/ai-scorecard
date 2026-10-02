module github.com/shadow-ai-capture/ingest-api

go 1.27

require github.com/shadow-ai-capture/device/protocol v0.0.0

// The device-side protocol types are the on-disk source of truth for the wire shapes of
// POST /v1/events (docs/02-ingest-and-transport.md §5.3). They are consumed, never forked.
replace github.com/shadow-ai-capture/device/protocol => ../protocol
