package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// The batch shapes for POST /v1/events (docs/02-ingest-and-transport.md §5.3, §6, §7).
//
// Two properties this file exists to keep honest:
//
//   - A batch that parses always returns 200, even when every event inside it is rejected. The
//     per-event outcome is the contract, so a device never has to guess which events landed.
//   - `tenant_id`, `device_id` and region come from the authenticated principal and never from
//     the body. A body tenant_id that disagrees is rejected `tenant_mismatch`, not reconciled,
//     which is why nothing in this file is authoritative for tenant resolution.

// Batch caps from §5.3.
const (
	MinBatchEvents       = 1
	MaxBatchEvents       = 500
	MaxRequestBodyBytes  = 8 << 20  // compressed
	MaxDecompressedBytes = 32 << 20 // decompression-bomb guard
	MaxEnvelopeBytes     = 256 << 10
)

// EventBatch is the request body. EventCount is redundant with len(Events) on purpose: a
// disagreement between the two is a `400`, which catches a truncated or padded body that would
// otherwise be silently accepted.
type EventBatch struct {
	SchemaVersion string            `json:"schema_version"`
	BatchID       string            `json:"batch_id"`
	DeviceSentAt  time.Time         `json:"device_sent_at"`
	EventCount    int               `json:"event_count"`
	Events        []json.RawMessage `json:"events"`
}

// Validate checks the batch envelope itself. The per-event verdict is ingest-api's, not this
// function's: a batch can be well-formed and every event inside it rejected.
func (b EventBatch) Validate() error {
	if b.SchemaVersion == "" {
		return fmt.Errorf("protocol: batch has no schema_version")
	}
	if b.BatchID == "" {
		return fmt.Errorf("protocol: batch has no batch_id; idempotency depends on it")
	}
	if len(b.Events) < MinBatchEvents || len(b.Events) > MaxBatchEvents {
		return fmt.Errorf("protocol: batch carries %d events, outside %d-%d", len(b.Events), MinBatchEvents, MaxBatchEvents)
	}
	if b.EventCount != len(b.Events) {
		return fmt.Errorf("protocol: batch declares event_count %d but carries %d events", b.EventCount, len(b.Events))
	}
	for i, e := range b.Events {
		if len(e) > MaxEnvelopeBytes {
			return fmt.Errorf("protocol: event %d is %d bytes, over the %d-byte envelope cap", i, len(e), MaxEnvelopeBytes)
		}
	}
	return nil
}

// Outcome is the per-event result. The three values are distinct facts and are never collapsed:
// an `accepted` that was merged into an existing submission is not the same as a fresh one.
type Outcome string

const (
	OutcomeAccepted  Outcome = "accepted"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeRejected  Outcome = "rejected"
)

// ReasonCode is the closed rejection set of §7. A new code is a contract change.
type ReasonCode string

const (
	ReasonSchemaViolation          ReasonCode = "schema_violation"
	ReasonUnsupportedSchemaVersion ReasonCode = "unsupported_schema_version"
	ReasonUnknownKind              ReasonCode = "unknown_kind"
	ReasonUnknownTenant            ReasonCode = "unknown_tenant"
	ReasonTenantMismatch           ReasonCode = "tenant_mismatch"
	ReasonRevokedDevice            ReasonCode = "revoked_device"
	ReasonRegionMismatch           ReasonCode = "region_mismatch"
	ReasonModeViolation            ReasonCode = "mode_violation"
	ReasonDuplicateBatch           ReasonCode = "duplicate_batch"
	ReasonOversize                 ReasonCode = "oversize"
)

// AllReasonCodes is the closed set, in the order the document lists it.
var AllReasonCodes = [...]ReasonCode{
	ReasonSchemaViolation, ReasonUnsupportedSchemaVersion, ReasonUnknownKind,
	ReasonUnknownTenant, ReasonTenantMismatch, ReasonRevokedDevice,
	ReasonRegionMismatch, ReasonModeViolation, ReasonDuplicateBatch, ReasonOversize,
}

// Retryable reports whether the device should retry the event after this rejection. Every code
// is terminal for that event except `duplicate_batch`, which is retryable with a fresh batch_id:
// one poison event must not block the queue behind it (§8).
func (r ReasonCode) Retryable() bool { return r == ReasonDuplicateBatch }

// Valid reports whether the code is in the closed set.
func (r ReasonCode) Valid() bool {
	for _, c := range AllReasonCodes {
		if c == r {
			return true
		}
	}
	return false
}

// BatchRejectionDetail is the diagnosability payload of §7: a reason code, a JSON Pointer, the
// violated constraint's *shape*, and the sorted field-name presence map. It never echoes the
// offending value, because the offending value can be content.
type BatchRejectionDetail struct {
	Pointer     string   `json:"pointer,omitempty"`
	Expected    string   `json:"expected,omitempty"`
	Supported   []string `json:"supported,omitempty"` // for unsupported_schema_version
	PresenceMap []string `json:"presence_map,omitempty"`
}

// EventResult is one entry of `results`, in request order.
type EventResult struct {
	EventID         string                `json:"event_id"`
	Outcome         Outcome               `json:"outcome"`
	SubmissionID    string                `json:"submission_id,omitempty"`
	DedupTier       string                `json:"dedup_tier,omitempty"` // T (tier T) or S (§4)
	WonFields       *bool                 `json:"won_fields,omitempty"` // true when this observation won the tie-break
	FirstReceivedAt *time.Time            `json:"first_received_at,omitempty"`
	Reason          ReasonCode            `json:"reason,omitempty"`
	Detail          *BatchRejectionDetail `json:"detail,omitempty"`
}

// Validate rejects a result that contradicts itself, so a device cannot be told "accepted" and
// "rejected" at once, or handed a rejection with no reason to count.
func (r EventResult) Validate() error {
	if r.EventID == "" {
		return fmt.Errorf("protocol: event result without an event_id cannot be matched to what was sent")
	}
	switch r.Outcome {
	case OutcomeAccepted, OutcomeDuplicate:
		if r.Reason != "" {
			return fmt.Errorf("protocol: event %s is %s but carries rejection reason %q", r.EventID, r.Outcome, r.Reason)
		}
	case OutcomeRejected:
		if r.Reason == "" {
			return fmt.Errorf("protocol: event %s is rejected with no reason code", r.EventID)
		}
		if !r.Reason.Valid() {
			return fmt.Errorf("protocol: event %s carries reason %q outside the closed set", r.EventID, r.Reason)
		}
	default:
		return fmt.Errorf("protocol: event %s has outcome %q outside the closed set", r.EventID, r.Outcome)
	}
	return nil
}

// BatchCounts summarises a response. It is derived, never authoritative: the per-event results
// are what the device acts on, and a counts/result mismatch is a server defect.
type BatchCounts struct {
	Accepted  int `json:"accepted"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

// EventBatchResponse is the 200 response. ReceivedAt is stamped once per request and is
// identical for every event in the batch; a retry does not re-stamp it, because a retry is not
// a second receipt.
type EventBatchResponse struct {
	SchemaVersion string        `json:"schema_version"`
	BatchID       string        `json:"batch_id"`
	ReceivedAt    time.Time     `json:"received_at"`
	ServerTime    time.Time     `json:"server_time"`
	Counts        BatchCounts   `json:"counts"`
	Results       []EventResult `json:"results"`
}

// Validate checks the response a device is about to trust. It is deliberately strict: the
// device's spool settles records on the strength of this, so a malformed response must be
// treated as a transport failure and retried, not parsed optimistically.
func (r EventBatchResponse) Validate(sent []string) error {
	if r.BatchID == "" {
		return fmt.Errorf("protocol: response has no batch_id")
	}
	if len(sent) > 0 && len(r.Results) != len(sent) {
		return fmt.Errorf("protocol: sent %d events, response carries %d results", len(sent), len(r.Results))
	}
	var accepted, duplicate, rejected int
	for i, res := range r.Results {
		if err := res.Validate(); err != nil {
			return err
		}
		if len(sent) > 0 && sent[i] != res.EventID {
			return fmt.Errorf("protocol: result %d is for event %s, request order says %s", i, res.EventID, sent[i])
		}
		switch res.Outcome {
		case OutcomeAccepted:
			accepted++
		case OutcomeDuplicate:
			duplicate++
		case OutcomeRejected:
			rejected++
		}
	}
	if r.Counts.Accepted != accepted || r.Counts.Duplicate != duplicate || r.Counts.Rejected != rejected {
		return fmt.Errorf("protocol: counts %+v disagree with %d accepted, %d duplicate, %d rejected in results",
			r.Counts, accepted, duplicate, rejected)
	}
	return nil
}

// Settle maps one event outcome onto the spool state a device should record. This is the single
// place that mapping lives, so the drain loop cannot decide differently from the spool.
func (o Outcome) SettleState(reason ReasonCode) (SpoolState, bool) {
	switch o {
	case OutcomeAccepted, OutcomeDuplicate:
		return SpoolDelivered, true
	case OutcomeRejected:
		if reason.Retryable() {
			return SpoolPending, false
		}
		return SpoolRejected, true
	default:
		return SpoolPending, false
	}
}
