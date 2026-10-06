package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Native messaging between the browser extension and capture-core.
//
// Chromium enforces a message size ceiling and the extension cannot read the spool, so
// observations flow one way, attachment bytes flow chunked behind a manifest that lets
// capture-core refuse an oversized upload before transfer, and anything undeliverable is held in
// extension memory only, bounded, dropped oldest-first with a counter.
//
// Every message is one JSON object with a `type` discriminator. A message whose type is
// unknown is counted and dropped; it is never guessed at.

// NativeMessage is the envelope every native-messaging message shares.
type NativeMessage struct {
	Type    string          `json:"type"`
	Version byte            `json:"version"`
	ID      string          `json:"id,omitempty"` // correlation id for request/response pairs
	Body    json.RawMessage `json:"body,omitempty"`
}

// Native message types, extension -> capture-core.
const (
	TypeObservation        = "observation"         // one observed submission, already shape-classified
	TypeAttachmentManifest = "attachment_manifest" // descriptor first: refuse before transfer
	TypeAttachmentChunk    = "attachment_chunk"
	TypeAttachmentComplete = "attachment_complete"
	TypeHealth             = "health"          // the extension's own coverage row
	TypePolicySync         = "policy_sync"     // ask for the current bundle / version
	TypeModeQuery          = "mode_query"      // ask for a destination's effective mode
	TypeDecisionRecord     = "decision_record" // a locally decided warn/block, including blocked requests
)

// Native message types, capture-core -> extension.
const (
	TypeAck            = "ack"
	TypeRefusal        = "refusal" // typed, with a reason from the closed set
	TypePolicyBundle   = "policy_bundle"
	TypeModeAnswer     = "mode_answer"
	TypeHealthSnapshot = "health_snapshot"
)

// RefusalReason is the closed set of reasons capture-core refuses an extension message. A
// refusal is never silent and never a partial acceptance.
type RefusalReason string

const (
	RefusalAttachmentTooLarge RefusalReason = "attachment_too_large"
	RefusalModeForbidsRead    RefusalReason = "mode_forbids_read"
	RefusalUnknownType        RefusalReason = "unknown_type"
	RefusalVersionMismatch    RefusalReason = "version_mismatch"
	RefusalMalformed          RefusalReason = "malformed"
	RefusalChannelClosing     RefusalReason = "channel_closing"
	RefusalQueueFull          RefusalReason = "queue_full"
)

// ObservationMessage is one observation delivered by the extension. It carries the fields the
// extension can see and nothing it cannot: capture-core mints the envelope, because the
// envelope needs the device's identity, the effective mode and the spool sequence.
type ObservationMessage struct {
	// ClientID correlates this observation with its later attachment chunks and decision.
	ClientID string `json:"client_id"`

	// Route is which extension mechanism observed it: web_request, page_context or dom.
	Route Route `json:"route"`

	// ToolFingerprint is behaviour-derived, computed by the shape predicate, never a brand name.
	ToolFingerprint string `json:"tool_fingerprint"`

	OccurredAt        time.Time `json:"occurred_at"`
	MonotonicOffsetMS int64     `json:"monotonic_offset_ms"`
	SizeBytes         int64     `json:"size_bytes"`
	ContentDigest     string    `json:"content_digest,omitempty"`

	// HasContent is true when the extension is entitled to hand over the payload at this mode.
	// At M0 it is false and no body field may be populated; capture-core rejects the message if
	// the two disagree, so a defect that read content at M0 is caught rather than stored.
	HasContent bool `json:"has_content"`

	// Content is the strict-UTF-8-decoded payload, or the raw bytes when the decode failed.
	// Deliberately absent at M0.
	Content []byte `json:"content,omitempty"`

	// ContentIsBinary records that the payload was treated as binary rather than lossily
	// decoded: a lossy decode would corrupt the digest and break dedup across routes.
	ContentIsBinary bool `json:"content_is_binary,omitempty"`

	// OverCap records that the body exceeded the cap, so it was hashed and sized and the
	// classification is `confidence: degraded` rather than absent.
	OverCap bool `json:"over_cap,omitempty"`

	// Attachments carries descriptors the extension already knows. Bytes follow separately.
	Attachments []AttachmentDescriptor `json:"attachments,omitempty"`

	// Decision is present when the extension decided inline. A blocked request is still an
	// event: otherwise the product could not answer "what did we stop".
	Decision *Decision `json:"decision,omitempty"`

	// DegradedReason is set when the extension's inline evaluation failed or exceeded budget
	// and the request was allowed through (fail open).
	DegradedReason Detail `json:"degraded_reason,omitempty"`

	// PageContextAttachments reports whether a File handle was actually reachable. A filename
	// alone is not attachment capture, so this is recorded as `content_no_attachments`.
	PageContextAttachments bool `json:"page_context_attachments,omitempty"`
}

// Validate rejects an observation that contradicts itself. The M0 rule is the important one:
// content present at a mode that forbids reading it is a defect, not data.
func (o ObservationMessage) Validate() error {
	switch o.Route {
	case RouteExtWebRequest, RouteExtPageContext, RouteExtDOM:
	default:
		return errRefusal(RefusalMalformed, "route %q is not an extension route", o.Route)
	}
	if o.ToolFingerprint == "" {
		return errRefusal(RefusalMalformed, "observation has no tool fingerprint")
	}
	if !o.HasContent && len(o.Content) > 0 {
		return errRefusal(RefusalMalformed, "observation carries %d content bytes while has_content is false", len(o.Content))
	}
	if o.Decision != nil {
		switch o.Decision.Action {
		case ActionBlocked, ActionWarned, ActionLogged:
		default:
			return errRefusal(RefusalMalformed, "decision action %q outside the closed set", o.Decision.Action)
		}
	}
	for _, a := range o.Attachments {
		if a.Name == "" {
			return errRefusal(RefusalMalformed, "attachment descriptor without a name")
		}
		if a.SizeBytes > MaxAttachmentBytes {
			return errRefusal(RefusalAttachmentTooLarge, "attachment %q is %d bytes, cap is %d", a.Name, a.SizeBytes, MaxAttachmentBytes)
		}
	}
	return nil
}

// Refusal is the typed rejection of an extension message.
type Refusal struct {
	Reason  RefusalReason `json:"reason"`
	Message string        `json:"message"`
}

func errRefusal(r RefusalReason, format string, args ...any) error {
	return &RefusalError{Reason: r, Message: fmt.Sprintf(format, args...)}
}

// RefusalError carries the closed reason code alongside the prose.
type RefusalError struct {
	Reason  RefusalReason
	Message string
}

func (e *RefusalError) Error() string { return string(e.Reason) + ": " + e.Message }

// ModeQuery asks for a destination's effective mode so the inline decision can use policy the
// extension already holds instead of a round trip.
type ModeQuery struct {
	ToolFingerprint string `json:"tool_fingerprint"`
	Host            string `json:"host,omitempty"`
	MediaType       string `json:"media_type,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
}

// ModeAnswer is the resolved effective mode, with the reason it resolved that way, so an
// operator can explain a decision without reading a policy bundle by hand.
type ModeAnswer struct {
	Mode          CollectionMode `json:"mode"`
	PolicyVersion string         `json:"policy_version"`
	Reason        string         `json:"reason,omitempty"`
}

// PolicySyncRequest asks for the current signed bundle. The extension holds no durable state, so
// this is how it gets policy after a restart.
type PolicySyncRequest struct {
	KnownVersion string `json:"known_version,omitempty"` // 304-aware: the server answers "unchanged"
}

// PolicyBundleMessage carries a signed bundle to the extension. Verification happens in
// capture-core, so the extension receives an already-verified bundle and a version to report.
type PolicyBundleMessage struct {
	PolicyVersion string          `json:"policy_version"`
	Bundle        json.RawMessage `json:"bundle"`
	Unchanged     bool            `json:"unchanged,omitempty"`
}

// Ack confirms a message was accepted. Acceptance is not delivery to the server: the spool is
// what makes that distinction, and it is why an ack never claims an event was ingested.
type Ack struct {
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MaxNativeMessageBytes is the effective ceiling the extension must assume for one native
// message. Chromium enforces its own limit; this constant is the one the extension queues
// against, and attachment bytes never travel whole because of it.
const MaxNativeMessageBytes = 1 << 20
