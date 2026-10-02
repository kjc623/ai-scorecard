package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope is the device-side event record: the contract's deviceSubmission shape,
// which is the common core with `received_at` forbidden, because a device-supplied
// receive time would be neither device time nor server time (contract comment on
// $defs/deviceSubmission; brief §3.6 requires exactly two clocks).
//
// The generated types in contracts/generated/go/envelope are the source of field
// names. This package deliberately keeps the envelope as its JSON representation
// rather than duplicating the struct: the spool must persist exactly the bytes the
// device will send, and ingest-api validates the same bytes against the schema. A
// second Go struct here would be a second source of truth for the wire shape, which
// is precisely what ADR 0010 exists to prevent.
//
// Validation is not performed here. The device validates before emitting, and
// ingest-api validates again as the one validating write path (ADR 0001). What this
// package provides is the *shape check* the device uses to avoid spooling something
// that ingest would certainly reject.
type Envelope = json.RawMessage

// Kind is the contract's closed kind registry. A new kind is an ADR, not a code change.
type Kind string

const (
	KindPrompt        Kind = "prompt"
	KindUsageRollup   Kind = "usage_rollup"
	KindModelDetection Kind = "model_detection"
)

// Route is the closed collection-route vocabulary. Fidelity ranking per route lives in
// ref.route_fidelity and decides the winner when two routes observe one submission.
type Route string

const (
	RouteExtWebRequest Route = "ext.web_request"
	RouteExtPageContext Route = "ext.page_context"
	RouteExtDOM        Route = "ext.dom"
	RouteProxyTLS      Route = "proxy.tls"
	RouteProxyLoopback Route = "proxy.loopback"
	RouteProcDetect    Route = "proc.detect"
	RouteCLIShim       Route = "cli.shim"
)

// CollectionMode is the effective mode resolved on the device from the signed scope
// matrix, taking the most restrictive applicable value across tool, data class and user
// population. The mode is applied *before* content is read (docs/01-collectors.md §11.2),
// so the mode is an input to the content path, never a consequence of it.
type CollectionMode string

const (
	ModeM0 CollectionMode = "m0" // device, user, tool, timestamp, size, destination. No content read at all.
	ModeM1 CollectionMode = "m1" // digest and labels; no excerpt.
	ModeM2 CollectionMode = "m2" // M1 plus a minimised excerpt.
	ModeM3 CollectionMode = "m3" // as M2, plus the content is held locally for grant-bound retrieval.
)

// readsContent reports whether the mode permits reading the payload at all. M0 does not,
// and every content path must consult this before touching bytes.
func (m CollectionMode) ReadsContent() bool { return m != ModeM0 && m != "" }

// Valid rejects anything outside the closed set rather than defaulting it. A default
// here would be a silent mode widening, which is the one failure this enum exists to
// make impossible.
func (m CollectionMode) Valid() bool {
	switch m {
	case ModeM0, ModeM1, ModeM2, ModeM3:
		return true
	default:
		return false
	}
}

// Confidence is the classifier confidence band. `degraded` means classification was
// attempted and did not complete: the explicit signal that a failed classifier is never
// reported as "no sensitive data found" (contract $defs/envelopeCore.confidence).
type Confidence string

const (
	ConfidenceHigh     Confidence = "high"
	ConfidenceMedium   Confidence = "medium"
	ConfidenceLow      Confidence = "low"
	ConfidenceDegraded Confidence = "degraded"
)

// CollectorState is the per-provider coverage state. `tampered` is external-facing: it is
// the tamper signal and the only state that raises a security finding rather than an
// operations one.
type CollectorState string

const (
	StateHealthy  CollectorState = "healthy"
	StateDegraded CollectorState = "degraded"
	StateAbsent   CollectorState = "absent"
	StateTampered CollectorState = "tampered"
)

// Counter is the closed, small counter set of docs/01-collectors.md §4.3. A provider
// reports these seven and nothing else: anything richer is the high-cardinality stream
// problem one level down.
type Counter string

const (
	CounterObserved             Counter = "observed"
	CounterEmitted              Counter = "emitted"
	CounterSkippedNotGenerative Counter = "skipped_not_generative"
	CounterBlindTunnelled       Counter = "blind_tunnelled"
	CounterNotCooperative       Counter = "not_cooperative"
	CounterDropped              Counter = "dropped"
	CounterErrors               Counter = "errors"
)

// AllCounters is the closed set, for validation and for reporting a complete row even
// when a provider has nothing to say about a counter.
var AllCounters = [...]Counter{
	CounterObserved, CounterEmitted, CounterSkippedNotGenerative,
	CounterBlindTunnelled, CounterNotCooperative, CounterDropped, CounterErrors,
}

// Detail is the closed per-provider detail vocabulary carried as error_code on the health
// channel, so a coverage report can group by cause without parsing prose.
type Detail string

const (
	DetailNone                Detail = ""
	DetailClassifierUnavailable Detail = "classifier_unavailable"
	DetailSpoolUnwritable     Detail = "spool_unwritable"
	DetailUpstreamFailure     Detail = "upstream_failure"
	DetailUpstreamUnreachable Detail = "upstream_unreachable"
	DetailClientPinned        Detail = "client_pinned"
	DetailNotEffectiveProxy   Detail = "not_effective_proxy"
	DetailTLSProbeFailed      Detail = "tls_probe_failed"
	DetailPortHeldByOther     Detail = "port_held_by_other"
	DetailCoolingDown         Detail = "cooling_down"
	DetailKilled              Detail = "killed"
	DetailEnumerationPartial  Detail = "enumeration_partial"
	DetailSignatureSetStale   Detail = "signature_set_stale"
	DetailVersionMismatch     Detail = "version_mismatch"
	DetailParseFailed         Detail = "parse_failed"
	DetailDocumentTooLarge    Detail = "document_too_large"
	DetailBudgetExceeded      Detail = "budget_exceeded"
)

// Decision is what policy did about an observation. The three values are never merged:
// `blocked`, `warned` and `logged` are distinct facts about the same outcome.
type Decision struct {
	RuleID         string `json:"rule_id"`
	Action         string `json:"action"` // blocked | warned | logged
	DecidedLocally bool   `json:"decided_locally"`
}

// Action values for Decision.
const (
	ActionBlocked = "blocked"
	ActionWarned  = "warned"
	ActionLogged  = "logged"
)

// AttachmentDescriptor is the manifest entry sent *before* attachment bytes, so that
// capture-core can refuse an oversized upload before transfer (docs/01-collectors.md §3.4).
// A filename alone is still a valid descriptor: attachment_names is metadata obtainable at
// M1 without reading the file at all.
type AttachmentDescriptor struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Digest    string `json:"digest,omitempty"` // sha256:<hex>, present only when bytes were read
}

// AttachmentManifest opens a chunked attachment transfer. TransferID correlates the
// manifest, its chunks and the completion message.
type AttachmentManifest struct {
	TransferID  string               `json:"transfer_id"`
	Observation string               `json:"observation_id"` // client-side id of the observation this attaches to
	Descriptor  AttachmentDescriptor `json:"descriptor"`
}

// AttachmentChunk carries one slice of attachment bytes. Seq is zero-based and
// contiguous; capture-core refuses a gap rather than assembling a partial file.
type AttachmentChunk struct {
	TransferID string `json:"transfer_id"`
	Seq        int    `json:"seq"`
	Data       []byte `json:"data"` // JSON base64 by encoding/json
}

// AttachmentComplete ends a transfer. Err is non-empty when the read failed; a failed
// attachment read never fails the submission, it is counted and reported.
type AttachmentComplete struct {
	TransferID string `json:"transfer_id"`
	Chunks     int    `json:"chunks"`
	Err        string `json:"error,omitempty"`
}

// MaxAttachmentBytes is the default cap a manifest is checked against before any byte
// moves. The effective cap is per-tenant policy data (the bundle's mode cap), so this is
// a ceiling for the transport, not the policy.
const MaxAttachmentBytes = 64 << 20

// HealthReport is what a component sends on the health channel. It is carried on
// POST /v1/health, upserted by key, and never as an event stream.
type HealthReport struct {
	DeviceID   string             `json:"device_id"`
	Collector  string             `json:"collector"`
	State      CollectorState     `json:"state"`
	Detail     Detail             `json:"detail,omitempty"`
	LastSuccess *time.Time        `json:"last_success_at,omitempty"`
	Since      time.Time          `json:"since"`
	Counters   map[Counter]uint64 `json:"counters"`
	Version    string             `json:"version,omitempty"`
}

// NewHealthReport returns a report with every counter present, so a missing counter is
// never confused with a counter that is genuinely zero.
func NewHealthReport(deviceID, collector, version string, now time.Time) HealthReport {
	c := make(map[Counter]uint64, len(AllCounters))
	for _, k := range AllCounters {
		c[k] = 0
	}
	return HealthReport{DeviceID: deviceID, Collector: collector, Version: version, Since: now, Counters: c}
}

// Validate rejects a report that names a counter outside the closed set. An unknown
// counter means a provider invented a name for a coverage path the reporting layer does
// not know, which is exactly what the closed set prevents.
func (h HealthReport) Validate() error {
	if h.Collector == "" {
		return fmt.Errorf("protocol: health report has no collector name")
	}
	switch h.State {
	case StateHealthy, StateDegraded, StateAbsent, StateTampered:
	default:
		return fmt.Errorf("protocol: health report for %q has state %q outside the closed set", h.Collector, h.State)
	}
	for k := range h.Counters {
		known := false
		for _, c := range AllCounters {
			if c == k {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("protocol: health report for %q carries counter %q outside the closed set", h.Collector, k)
		}
	}
	return nil
}
