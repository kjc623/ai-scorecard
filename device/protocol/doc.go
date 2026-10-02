// Package protocol is the device-side wire and IPC contract.
//
// OWNER: the Lead. Every device component (capture-core, capture-spool, classifier-host,
// capture-extension) consumes this package and none of them redefines these shapes. If a
// shape here is wrong, message the Lead; do not fork it locally.
//
// The envelope itself is NOT defined here. It is defined by
// contracts/event-envelope.schema.json, and the generated Go types live in
// contracts/generated/go/envelope. Those generated types are aliased in envelope.go so that
// a change to the contract schema cannot silently diverge from what the device emits.
//
// What this package adds is what the contract deliberately does not cover: the local paths
// between device processes, which never appear on the wire.
//
//	extension ──native messaging (see native.go)──▶ capture-core
//	capture-core ──length-prefixed frames (see frames.go)──▶ classifier-host
//	capture-core ──spool records (see spool.go)──▶ device spool
//	capture-core ──HTTPS batch (see batch.go)──▶ ingest-api
package protocol
