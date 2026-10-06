package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Spool record shape and the interface the spool exposes.
//
// The spool is the only durable device-side store of observations, it is written by capture-core
// alone, and it holds exactly the bytes the device will send. Sequence numbers are monotonic,
// which makes "the oldest undelivered observation" a well-defined thing to drop when the bound is
// reached; a drop is counted, never silent.
//
// The implementation is capture-spool. It stores opaque payload bytes and never parses an
// envelope.

// SpoolState is the delivery state of one spooled observation.
type SpoolState string

const (
	// SpoolPending is written but not yet delivered. Only pending records are retried.
	SpoolPending SpoolState = "pending"
	// SpoolInFlight is handed to a delivery attempt. A record left in-flight by a crash
	// returns to pending on the next open, because "in-flight" is not a delivery.
	SpoolInFlight SpoolState = "in_flight"
	// SpoolDelivered was acknowledged by ingest-api and is never sent again.
	SpoolDelivered SpoolState = "delivered"
	// SpoolRejected was terminally rejected by ingest (a closed reason code) and is removed
	// from the head so one poison record cannot block the queue behind it.
	SpoolRejected SpoolState = "rejected"
	// SpoolDropped was evicted by the bound before delivery, and is counted.
	SpoolDropped SpoolState = "dropped"
)

// Valid reports whether the state is in the closed set.
func (s SpoolState) Valid() bool {
	switch s {
	case SpoolPending, SpoolInFlight, SpoolDelivered, SpoolRejected, SpoolDropped:
		return true
	default:
		return false
	}
}

// Entry is one spooled observation.
//
// Seq is assigned by the spool on append, is strictly increasing, and is never reused: it is
// the ordering key for drop-oldest and the tie-break for "which of two routes did we keep".
// Payload is the envelope exactly as the device minted it, and the spool never rewrites it: an
// observation is immutable once spooled.
type Entry struct {
	Seq               uint64         `json:"seq"`
	ClientID          string         `json:"client_id,omitempty"` // correlates with the extension's observation id
	Kind              Kind           `json:"kind"`
	Route             Route          `json:"route"`
	CollectionMode    CollectionMode `json:"collection_mode"`
	ToolFingerprint   string         `json:"tool_fingerprint"`
	OccurredAt        time.Time      `json:"occurred_at"`
	MonotonicOffsetMS int64          `json:"monotonic_offset_ms"`

	// DedupKey is the idempotency key the device computed. The spool does not compute it; it is
	// mirrored here so a drain can group without decrypting the payload.
	DedupKey string `json:"dedup_key"`

	// Payload is the minted envelope. Opaque to the spool.
	Payload json.RawMessage `json:"payload"`

	// SizeBytes is the payload size, mirrored for the bound calculation.
	SizeBytes int64 `json:"size_bytes"`

	// ExpiresAt is the retention deadline for this record on the device. A record past it is
	// dropped and counted rather than delivered late: retention is a property of the
	// observation, not of the queue.
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	State    SpoolState `json:"state"`
	Attempts int        `json:"attempts"`

	// LastError is the last delivery failure, for the health report's cause vocabulary.
	LastError string `json:"last_error,omitempty"`

	// RejectReason is the closed ingest reason code when State is SpoolRejected.
	RejectReason string `json:"reject_reason,omitempty"`
}

// Validate rejects an entry the spool must not accept: an entry that cannot be delivered, or
// one whose payload cannot be identified, is a defect upstream and is refused at the door.
func (e Entry) Validate() error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("protocol: spool entry %d has no payload", e.Seq)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("protocol: spool entry %d has kind %q outside the closed registry", e.Seq, e.Kind)
	}
	if !e.CollectionMode.Valid() {
		return fmt.Errorf("protocol: spool entry %d has mode %q outside the closed set", e.Seq, e.CollectionMode)
	}
	if e.DedupKey == "" {
		return fmt.Errorf("protocol: spool entry %d has no dedup key; dedup is not optional", e.Seq)
	}
	if !e.State.Valid() {
		return fmt.Errorf("protocol: spool entry %d has state %q outside the closed set", e.Seq, e.State)
	}
	return nil
}

// Valid reports whether the kind is in the closed registry. A new kind is a contract change, so
// no kind can appear for raw process telemetry.
func (k Kind) Valid() bool {
	switch k {
	case KindPrompt, KindUsageRollup, KindModelDetection:
		return true
	default:
		return false
	}
}

// Valid reports whether the route is in the closed vocabulary. The collector name must come
// from ref.collector, so a provider cannot invent a name for a coverage path the reporting
// layer does not know.
func (r Route) Valid() bool {
	switch r {
	case RouteExtWebRequest, RouteExtPageContext, RouteExtDOM,
		RouteProxyTLS, RouteProxyLoopback, RouteProcDetect, RouteCLIShim:
		return true
	default:
		return false
	}
}

// SpoolStats is what the health report reads: the depth and the counters. The dropped total is
// reported, never buried. The provider-level dropped counter and DroppedTotal are separate: an
// observation lost before the spool and one evicted from it are different failures with
// different fixes.
type SpoolStats struct {
	Depth           int       `json:"spool_depth"`
	DroppedTotal    uint64    `json:"spool_dropped_total"`
	RejectedTotal   uint64    `json:"spool_rejected_total"`
	DeliveredTotal  uint64    `json:"spool_delivered_total"`
	OldestSpooledAt time.Time `json:"oldest_spooled_at,omitempty"`
	BoundBytes      int64     `json:"bound_bytes"`
	UsedBytes       int64     `json:"used_bytes"`
}

// Store is the interface capture-core uses and capture-spool implements.
//
// One writer, one encryption key, one place the bound is enforced. Append is the only way in;
// there is deliberately no Update that could rewrite a spooled observation.
type Store interface {
	// Append writes one entry and returns it with its assigned sequence number. It blocks
	// until the entry is durable. A full spool evicts oldest-pending first and reports the
	// eviction through Stats, never by silently discarding the new entry.
	Append(e Entry) (Entry, error)

	// Peek returns up to n pending entries in sequence order without changing their state.
	Peek(n int) ([]Entry, error)

	// MarkInFlight transitions entries to in-flight. A record left in-flight by a crash
	// returns to pending on the next Open.
	MarkInFlight(seqs []uint64) error

	// Settle records the terminal outcome of a delivery attempt: delivered, or rejected with
	// the closed reason code. Delivered and rejected entries are never retried.
	Settle(seq uint64, state SpoolState, reason string) error

	// Stats reports depth and the counters for the health report. It never blocks.
	Stats() SpoolStats

	// Close releases the single writer. It is idempotent.
	Close() error
}
