package protocol

import (
	"fmt"
	"time"
)

// Kind is the envelope's closed kind registry. A new kind is a contract change.
type Kind string

const (
	KindPrompt        Kind = "prompt"
	KindUsageRollup   Kind = "usage_rollup"
	KindDiscovery     Kind = "discovery"
	KindAgentActivity Kind = "agent_activity"
)

// DiscoveryType is what a discovery record reports was found on the device.
type DiscoveryType string

const (
	DiscoveryTypeAppInstalled        DiscoveryType = "app_installed"
	DiscoveryTypeAppRunning          DiscoveryType = "app_running"
	DiscoveryTypeCLIInstalled        DiscoveryType = "cli_installed"
	DiscoveryTypeIDEExtension        DiscoveryType = "ide_extension"
	DiscoveryTypeLocalModel          DiscoveryType = "local_model"
	DiscoveryTypeInferenceConnection DiscoveryType = "inference_connection"
)

// Valid reports whether the discovery type is in the closed set.
func (t DiscoveryType) Valid() bool {
	switch t {
	case DiscoveryTypeAppInstalled, DiscoveryTypeAppRunning, DiscoveryTypeCLIInstalled,
		DiscoveryTypeIDEExtension, DiscoveryTypeLocalModel, DiscoveryTypeInferenceConnection:
		return true
	default:
		return false
	}
}

// DetectionBasis is how a discovery was made. The mechanisms differ in confidence and coverage,
// so a record names the one that found it.
type DetectionBasis string

const (
	DetectionBasisInstalledScan DetectionBasis = "installed_scan"
	DetectionBasisPackageScan   DetectionBasis = "package_scan"
	DetectionBasisExtensionScan DetectionBasis = "extension_scan"
	DetectionBasisProcessEvent  DetectionBasis = "process_event"
	DetectionBasisModelStore    DetectionBasis = "model_store"
	DetectionBasisPortListen    DetectionBasis = "port_listen"
	DetectionBasisFlowMetadata  DetectionBasis = "flow_metadata"
)

// Valid reports whether the detection basis is in the closed set.
func (b DetectionBasis) Valid() bool {
	switch b {
	case DetectionBasisInstalledScan, DetectionBasisPackageScan, DetectionBasisExtensionScan,
		DetectionBasisProcessEvent, DetectionBasisModelStore, DetectionBasisPortListen,
		DetectionBasisFlowMetadata:
		return true
	default:
		return false
	}
}

// ActivityType is what an agent did, as its own telemetry reports it.
type ActivityType string

const (
	ActivityTypeModelRequest ActivityType = "model_request"
	ActivityTypeToolCall     ActivityType = "tool_call"
)

// Valid reports whether the activity type is in the closed set.
func (t ActivityType) Valid() bool {
	return t == ActivityTypeModelRequest || t == ActivityTypeToolCall
}

// ActivityOutcome is how a model request or tool call ended. Denied means a permission check or a
// policy refused it.
type ActivityOutcome string

const (
	ActivityOutcomeSuccess ActivityOutcome = "success"
	ActivityOutcomeError   ActivityOutcome = "error"
	ActivityOutcomeDenied  ActivityOutcome = "denied"
)

// Valid reports whether the outcome is in the closed set.
func (o ActivityOutcome) Valid() bool {
	switch o {
	case ActivityOutcomeSuccess, ActivityOutcomeError, ActivityOutcomeDenied:
		return true
	default:
		return false
	}
}

// PromptKind is the device's decision about what kind of prompt a captured request is. It is
// request-shape metadata, decided on the device from the payload and the extracted text, never
// from the payload's meaning:
//
//   - PromptKindUser is text a person authored.
//   - PromptKindClientGenerated is a request the client made for itself — titling, a summary,
//     telemetry, an injected message — which carries no typed turn.
//   - PromptKindUnknown is a record whose device could not decide. The read layer treats an
//     absent value as unknown.
//
// The default is PromptKindUser when unsure, so nothing a person typed is hidden.
type PromptKind string

const (
	PromptKindUser            PromptKind = "user"
	PromptKindClientGenerated PromptKind = "client_generated"
	PromptKindUnknown         PromptKind = "unknown"
)

// Valid rejects anything outside the closed set rather than defaulting it: a default here would
// silently reclassify a person's prompt as the client's own, which is the one failure the kind
// exists to avoid.
func (k PromptKind) Valid() bool {
	switch k {
	case PromptKindUser, PromptKindClientGenerated, PromptKindUnknown:
		return true
	default:
		return false
	}
}

// Route is the closed collection-route vocabulary. The server ranks routes by fidelity to decide
// the winner when two routes observe one submission.
type Route string

const (
	RouteExtWebRequest  Route = "ext.web_request"
	RouteExtPageContext Route = "ext.page_context"
	RouteExtDOM         Route = "ext.dom"
	RouteProxyTLS       Route = "proxy.tls"
	RouteProxyLoopback  Route = "proxy.loopback"
	RouteProcDetect     Route = "proc.detect"
	RouteCLIShim        Route = "cli.shim"
	RouteToolHook       Route = "tool.hook"
	RouteToolOTel       Route = "tool.otel"
	RouteInvScan        Route = "inv.scan"
	RouteNetFlow        Route = "net.flow"
)

// Collector is the closed collector vocabulary: the component whose health is reported, keyed as
// ref.collector.collector_code. A route names how an observation was collected; a collector may
// emit several routes or none.
type Collector string

const (
	CollectorEgressProxy      Collector = "egress_proxy"
	CollectorLoopbackBroker   Collector = "loopback_broker"
	CollectorCLIShim          Collector = "cli_shim"
	CollectorProcessDetector  Collector = "process_detector"
	CollectorClassifierHost   Collector = "classifier_host"
	CollectorCaptureExtension Collector = "capture_extension"
	CollectorDesktopProxy     Collector = "desktop_proxy"
	CollectorOTelReceiver     Collector = "otel_receiver"
	CollectorUserHelper       Collector = "user_helper"
	// CollectorToolConfigClaudeCode writes Claude Code's managed settings; it emits nothing itself.
	CollectorToolConfigClaudeCode Collector = "tool_config_claude_code"
	CollectorHookRelay        Collector = "hook_relay"
)

// Valid reports whether the collector is in the closed set. control-api refuses a whole health
// report that names a collector ref.collector does not hold.
func (c Collector) Valid() bool {
	switch c {
	case CollectorEgressProxy, CollectorLoopbackBroker, CollectorCLIShim,
		CollectorProcessDetector, CollectorClassifierHost, CollectorCaptureExtension,
		CollectorDesktopProxy, CollectorOTelReceiver, CollectorUserHelper,
		CollectorToolConfigClaudeCode, CollectorHookRelay:
		return true
	default:
		return false
	}
}

// CollectionMode is the effective mode resolved on the device from the signed scope matrix,
// taking the most restrictive applicable value across tool, data class and user population. The
// mode is applied before content is read, so it is an input to the content path, never a
// consequence of it.
type CollectionMode string

const (
	ModeM0 CollectionMode = "m0" // device, user, tool, timestamp, size, destination. No content read at all.
	ModeM1 CollectionMode = "m1" // digest and labels; no excerpt.
	ModeM2 CollectionMode = "m2" // M1 plus a minimised excerpt.
	ModeM3 CollectionMode = "m3" // as M2, plus the content is held locally for grant-bound retrieval.
)

// ReadsContent reports whether the mode permits reading the payload at all. M0 does not, and
// every content path consults this before touching bytes.
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

// Confidence is the classifier confidence band. degraded means classification was attempted
// and did not complete: a failed classifier is never reported as "no sensitive data found".
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

// Counter is the closed, small per-provider counter set. A provider reports these seven and
// nothing else: anything richer would turn health into a high-cardinality event stream.
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
// channel, so a coverage report can group by cause without parsing prose. A cause a component
// needs is added here rather than invented locally, where the reporting layer could not group it.
type Detail string

const (
	DetailNone Detail = ""

	// Proxy and broker.
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

	// Process detection.
	DetailEnumerationPartial Detail = "enumeration_partial"
	DetailSignatureSetStale  Detail = "signature_set_stale"

	// Classifier host: the reasons a classification is degraded.
	DetailBudgetExhausted      Detail = "budget_exhausted"      // a stage was skipped because its budget was exhausted
	DetailModelUnavailable     Detail = "model_unavailable"     // the model artefact was missing, unloadable or failed to verify
	DetailNormaliseTruncated   Detail = "normalise_truncated"   // normalisation truncated the payload so a rule could not see all of it
	DetailParserFailed         Detail = "parser_failed"         // the document parser failed, timed out or was killed (see the specific codes below)
	DetailContentUnprocessable Detail = "content_unprocessable" // over-cap body or undecodable bytes handed over by a provider
	DetailHostUnreachable      Detail = "host_unreachable"      // the host was unreachable and the event was emitted unclassified
	DetailReleaseLoadFailed    Detail = "release_load_failed"   // a release failed to load and rules-only labels came from the retained release

	// Document parser: the specific causes behind DetailParserFailed.
	DetailParserMemory    Detail = "parser_memory"
	DetailParserTimeout   Detail = "parser_timeout"
	DetailParserCrash     Detail = "parser_crash"
	DetailParserOutputCap Detail = "parser_output_cap"

	// Policy bundle verification failure: the four causes behind the tampered state a refused
	// bundle produces. Naming the cause is what makes the signal actionable.
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

	// Enforcement capability. An extension that holds the webRequestBlocking permission but was
	// not granted it can observe but cannot cancel a request; it says so rather than reporting
	// healthy while inspection is wider than enforcement.
	DetailEnforcementUnavailable Detail = "enforcement_unavailable"

	// Device credential. An x509 leaf past its NotAfter cannot authenticate; naming the cause
	// keeps the drain from failing silently on a 401.
	DetailCredentialExpired Detail = "credential_expired"

	// Trust store installation and the CLI trust shim. Installing the per-device root CA into
	// the wrong store fails silently, so the install and its verification are separate causes.
	// The shim's three checks (profile present, CA bundle parses and carries the root,
	// environment inherited) each have a name.
	DetailTrustInstallFailed     Detail = "trust_install_failed"
	DetailTrustVerifyFailed      Detail = "trust_verify_failed"
	DetailShimProfileMissing     Detail = "shim_profile_missing"
	DetailShimCABundleUnreadable Detail = "shim_ca_bundle_unreadable"
	DetailShimNotInherited       Detail = "shim_not_inherited"

	// Identity. A device whose credential has not been issued yet refuses to mint an envelope
	// rather than stamp a placeholder identity; the state is named so the coverage row
	// distinguishes "no credential yet" from "nothing observed".
	DetailIdentityUnresolved Detail = "identity_unresolved"

	// Collector lifecycle. A collector the signed policy switched off is out of the path by
	// request, which is neither a fault nor interference.
	DetailDisabledByPolicy Detail = "disabled_by_policy"

	// User-session helper. A signed-in session has no connected helper: it could not be started,
	// it keeps exiting, or the platform has none.
	DetailHelperUnavailable Detail = "helper_unavailable"

	// Supervised components. A child process that exited more often than its restart budget allows
	// is left stopped until the budget allows another start.
	DetailComponentCrashLoop Detail = "component_crash_loop"

	// Tool configuration. The tool is not installed, the platform has no managed location for it
	// yet, or the agent could not write the configuration it manages.
	DetailToolNotInstalled       Detail = "tool_not_installed"
	DetailToolVersionUnsupported Detail = "tool_version_unsupported"
	DetailConfigWriteFailed      Detail = "config_write_failed"
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
	DetailTrustInstallFailed, DetailTrustVerifyFailed,
	DetailShimProfileMissing, DetailShimCABundleUnreadable, DetailShimNotInherited,
	DetailIdentityUnresolved, DetailDisabledByPolicy,
	DetailHelperUnavailable, DetailComponentCrashLoop,
	DetailToolNotInstalled, DetailToolVersionUnsupported, DetailConfigWriteFailed,
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

// AttachmentDescriptor describes one attachment. It is the manifest entry the extension sends
// before attachment bytes, so capture-core can refuse an oversized upload before transfer, and it
// is the attachment shape the envelope carries: one shape, with the envelope's field names, so a
// descriptor forwarded unchanged never becomes a record ingest rejects. A file name alone is a
// valid descriptor.
type AttachmentDescriptor struct {
	Name          string `json:"name"`
	MediaType     string `json:"media_type,omitempty"`
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

// MaxAttachmentBytes is the transport ceiling the extension checks a file against before it
// sends a manifest. capture-core may hold less (no more than the classifier can parse) and says
// so by refusing the manifest with attachment_too_large.
const MaxAttachmentBytes = 64 << 20

// HealthReport is what a component sends on the health channel. It is carried on
// POST /v1/health, upserted by key, and never as an event stream.
type HealthReport struct {
	DeviceID    string             `json:"device_id,omitempty"`
	Collector   string             `json:"collector"`
	State       CollectorState     `json:"state"`
	Detail      Detail             `json:"detail,omitempty"`
	LastSuccess *time.Time         `json:"last_success_at,omitempty"`
	Since       time.Time          `json:"since"`
	Counters    map[Counter]uint64 `json:"counters"`
	Version     string             `json:"version,omitempty"`
	// Permissions is the state of each permission the collector needs (granted | denied |
	// not-applicable). It is not part of the closed counter set: a permission is a capability an
	// operator must see, not a throughput count.
	Permissions map[string]string `json:"permissions,omitempty"`
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
