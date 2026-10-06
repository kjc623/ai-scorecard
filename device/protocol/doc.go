// Package protocol defines the shapes the device components agree on: the HTTPS exchanges with
// the device edge (enrolment, policy, events, health, content upload) and the local channels
// between device processes.
//
//	extension ──native messaging (native.go)──▶ capture-core
//	capture-core ──length-prefixed frames (frames.go, classifier.go)──▶ classifier-host
//	capture-core ──spool records (spool.go)──▶ capture-spool
//	capture-core ──HTTPS (batch.go, auth.go, policy.go, health.go, content.go)──▶ device edge
//
// The event envelope itself travels as JSON bytes (json.RawMessage): the spool persists exactly
// the bytes the device will send, and ingest-api validates those bytes against the envelope
// schema. This package carries the envelope's closed vocabularies (kinds, routes, modes) and
// leaves its field layout to the component that mints it.
package protocol
