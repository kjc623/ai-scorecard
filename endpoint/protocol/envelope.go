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
	KindPrompt         Kind = "prompt"
	KindUsageRollup    Kind = "usage_rollup"
	KindModelDetection Kind = "model_detection"
)

// Route is the closed collection-route vocabulary. Fidelity ranking per route lives in
// ref.route_fidelity and decides the winner when two routes observe one submission.
type Route string

const (
	RouteExtWebRequest  Route = "ext.web_request"
	RouteExtPageContext Route = "ext.page_context"
	RouteExtDOM         Route = "ext.dom"
	RouteProxyTLS       Route = "proxy.tls"
	RouteProxyLoopback  Route = "proxy.loopback"
	RouteProcDetect     Route = "proc.detect"
	RouteCLIShim        Route = "cli.shim"
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
//
// Every value below appears in the document set; a value that appears nowhere and is needed
// belongs here rather than in a component, because a detail string invented locally is a
// coverage cause the reporting layer cannot group. The lists:
//
//   - proxy and broker states: docs/01-collectors.md §5.4, §5.6, §6.3, §6.4
//   - proc.detect: §4.4 — enumeration_partial, signature_set_stale
//   - classifier host: §9.7 — the six "emitted when" reasons degraded is produced
//   - document parser: §10 — parser_memory, parser_timeout, parser_crash, parser_output_cap
//   - kill switch: §5.5 — killed
//   - framing handshake: §3.4 — version_mismatch
type Detail string

const (
	DetailNone Detail = ""

	// Proxy and broker (docs/01-collectors.md §5.4, §5.6, §6.3, §6.4)
	DetailClassifierUnavailable Detail = "classifier_unavailable"
	DetailSpoolUnwritable       Detail = "spool_unwritable"
	DetailUpstreamFailure       Detail = "upstream_failure"
	DetailUpstreamUnreachable   Detail = "upstream_unreachable"
	DetailClientPinned          Detail = "client_pinned"
	DetailNotEffectiveProxy     Detail = "not_effective_proxy"
	DetailTLSProbeFailed        Detail = "tls_probe_failed"
	DetailPortHeldByOther       Detail = "port_held_by_other"
	DetailCoolingDown           Detail = "cooling_down"
	DetailKilled                Detail = "killed"

	// proc.detect (§4.4)
	DetailEnumerationPartial Detail = "enumeration_partial"
	DetailSignatureSetStale  Detail = "signature_set_stale"

	// Classifier host (§9.7's "emitted when" column, in its order)
	DetailBudgetExhausted      Detail = "budget_exhausted"      // a stage was skipped because its budget was exhausted
	DetailModelUnavailable     Detail = "model_unavailable"     // the model artefact was missing, unloadable or failed to verify
	DetailNormaliseTruncated   Detail = "normalise_truncated"   // normalisation truncated the payload so a rule could not see all of it
	DetailParserFailed         Detail = "parser_failed"         // the document parser failed, timed out or was killed (§10; see the specific codes below)
	DetailContentUnprocessable Detail = "content_unprocessable" // over-cap body or undecodable bytes handed over by a provider
	DetailHostUnreachable      Detail = "host_unreachable"      // the host was unreachable and the event was emitted unclassified
	DetailReleaseLoadFailed    Detail = "release_load_failed"   // a release failed to load and rules-only labels came from the retained release

	// Document parser (§10) — the specific causes behind DetailParserFailed
	DetailParserMemory    Detail = "parser_memory"
	DetailParserTimeout   Detail = "parser_timeout"
	DetailParserCrash     Detail = "parser_crash"
	DetailParserOutputCap Detail = "parser_output_cap"

	// Policy bundle verification failure (docs/01-collectors.md §13.3 rule 4). These are the four
	// causes behind the `tampered` state a policy-verification failure produces, and C10's "less
	// inspection, silently" failure mode: naming the cause is what makes the signal actionable.
	DetailBundleSignatureInvalid  Detail = "bundle_signature_invalid"
	DetailBundleSchemaInvalid     Detail = "bundle_schema_invalid"
	DetailBundleVersionRegression Detail = "bundle_version_regression"
	DetailBundleArtefactMissing   Detail = "bundle_artefact_missing"

	// Content-shape causes that reach the classifier
	DetailContentOverCap     Detail = "content_over_cap"
	DetailUndecodableContent Detail = "undecodable_content"

	// Framing and contract
	DetailVersionMismatch Detail = "version_mismatch"
	DetailModeViolation   Detail = "mode_violation"

	// Enforcement capability (docs/01-collectors.md §7.4, §15.2). An install that holds the
	// webRequestBlocking permission but was not *granted* it — which is every unpacked load, and
	// which the browser reports only as a console message — can still observe but cannot cancel a
	// request. That is a coverage state, and §15.2 forbids a coverage state with no name: a path
	// that cannot enforce must say so rather than reporting `healthy` while inspection is silently
	// wider than enforcement.
	DetailEnforcementUnavailable Detail = "enforcement_unavailable"

	// Device credential (ADR 0020). An x509 leaf past its NotAfter cannot authenticate, and it
	// cannot be renewed without a fresh enrolment token; naming the cause keeps the drain from
	// failing silently on a 401 forever.
	DetailCredentialExpired Detail = "credential_expired"
)

// AllDetails is the closed vocabulary, for validation and for a coverage report that needs to
// enumerate causes rather than discover them.
var AllDetails = [...]Detail{
	DetailClassifierUnavailable, DetailSpoolUnwritable, DetailUpstreamFailure,
	DetailUpstreamUnreachable, DetailClientPinned, DetailNotEffectiveProxy,
	DetailTLSProbeFailed, DetailPortHeldByOther, DetailCoolingDown, DetailKilled,
	DetailEnumerationPartial, DetailSignatureSetStale,
	DetailBudgetExhausted, DetailModelUnavailable, DetailNormaliseTruncated,
	DetailParserFailed, DetailContentUnprocessable, DetailHostUnreachable,
	DetailReleaseLoadFailed,
	DetailParserMemory, DetailParserTimeout, DetailParserCrash, DetailParserOutputCap,
	DetailBundleSignatureInvalid, DetailBundleSchemaInvalid,
	DetailBundleVersionRegression, DetailBundleArtefactMissing,
	DetailContentOverCap, DetailUndecodableContent,
	DetailVersionMismatch, DetailModeViolation, DetailEnforcementUnavailable,
	DetailCredentialExpired,
}

// Valid reports whether the detail is in the closed vocabulary. An empty detail is valid: a
// healthy provider has no cause to report.
func (d Detail) Valid() bool {
	if d == DetailNone {
		return true
	}
	for _, k := range AllDetails {
		if k == d {
			return true
		}
	}
	return false
}

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
//
// Its field names are the contract's `$defs/attachment` names verbatim, `content_digest`
// included, so the descriptor that crosses native messaging and the descriptor that ends up in
// an envelope are one shape. That matters because the contract is closed
// (`additionalProperties: false`): a local `digest` here and a contract `content_digest` there
// would be a rename waiting to be forgotten, and the first component to forward the descriptor
// unchanged would emit a record ingest rejects.
type AttachmentDescriptor struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"` // not a contract field: the contract records
	// the name and the bytes-derived fields, and media type is the collector's own hint.
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"` // sha256:<hex>, present only when bytes were read
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
	DeviceID    string             `json:"device_id"`
	Collector   string             `json:"collector"`
	State       CollectorState     `json:"state"`
	Detail      Detail             `json:"detail,omitempty"`
	LastSuccess *time.Time         `json:"last_success_at,omitempty"`
	Since       time.Time          `json:"since"`
	Counters    map[Counter]uint64 `json:"counters"`
	Version     string             `json:"version,omitempty"`
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
	if !h.Detail.Valid() {
		return fmt.Errorf("protocol: health report for %q carries detail %q outside the closed vocabulary", h.Collector, h.Detail)
	}
	return nil
}
