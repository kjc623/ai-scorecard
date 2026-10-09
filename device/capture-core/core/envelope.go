package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Identity identifies the subject of an envelope. The tenant and device come from enrolment and
// the person from the operating system, never from a provider, so no provider can attribute an
// observation to another tenant or user. SubjectName is the clear account name, present only while
// the tenant's device identity setting is clear; otherwise the record carries only the
// pseudonymous UserRef.
type Identity struct {
	TenantID    string
	DeviceID    string
	UserRef     string
	SubjectName string
}

// SchemaVersion is the event envelope version these records validate against.
const SchemaVersion = "1.0"

// maxSubjectNameChars is the envelope's cap on subject_name.
const maxSubjectNameChars = 200

// The envelope's caps on the discovery and agent_activity fields, in characters.
const (
	maxAppVersionChars      = 64
	maxPublisherChars       = 200
	maxHostAppChars         = 128
	maxDestinationHostChars = 253
	maxModelNames           = 64
	maxModelNameChars       = 200
	maxModelChars           = 128
	maxToolNameChars        = 128
)

// destinationHost is the envelope's pattern for destination_host: lower-case host name labels
// separated by dots, never a URL, a path or an upper-case name.
var destinationHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// FactFields are the fields of a discovery or agent_activity record. A prompt carries none of
// them, a discovery only the discovery fields and an agent_activity only the activity fields.
type FactFields struct {
	// Discovery: what was found and how.
	DiscoveryType   protocol.DiscoveryType
	DetectionBasis  protocol.DetectionBasis
	AppVersion      string
	Publisher       string
	HostApp         string
	DestinationHost string
	ModelNames      []string

	// Agent activity: what a tool's own telemetry says it did.
	ActivityType protocol.ActivityType
	Model        string
	InputTokens  *int64
	OutputTokens *int64
	DurationMS   *int64
	ToolName     string
	Outcome      protocol.ActivityOutcome
}

// discoverySet reports whether any discovery field is set.
func (f FactFields) discoverySet() bool {
	return f.DiscoveryType != "" || f.DetectionBasis != "" || f.AppVersion != "" || f.Publisher != "" ||
		f.HostApp != "" || f.DestinationHost != "" || len(f.ModelNames) > 0
}

// activitySet reports whether any agent_activity field is set.
func (f FactFields) activitySet() bool {
	return f.ActivityType != "" || f.Model != "" || f.InputTokens != nil || f.OutputTokens != nil ||
		f.DurationMS != nil || f.ToolName != "" || f.Outcome != ""
}

// EnvelopeInput is everything an envelope needs. Content-derived fields are present so the
// builder can refuse them at a mode or kind that forbids them rather than silently dropping them:
// a dropped digest would hide a mode violation, a refused one reports it.
type EnvelopeInput struct {
	Identity Identity
	EventID  string

	Kind  protocol.Kind
	Route protocol.Route
	Mode  protocol.CollectionMode

	// PromptKind is the device's request-kind decision, recorded at M1 and above.
	PromptKind protocol.PromptKind

	ToolFingerprint   string
	OccurredAt        time.Time
	MonotonicOffsetMS int64
	DedupKey          string
	SizeBytes         *int64

	// Content-derived: prompts at M1 and above only.
	ContentDigest     string
	Labels            []protocol.Label
	ClassifierVersion string
	Confidence        protocol.Confidence
	Excerpt           *protocol.Excerpt
	Attachments       []protocol.AttachmentDescriptor

	// Decision is carried by every prompt, at every mode including M0, and by no other kind.
	Decision *protocol.Decision

	FactFields
}

// ErrContentAtM0 is the defect signal: something populated a content-derived field for an
// observation the device was not permitted to read. The envelope is refused before it is minted.
var ErrContentAtM0 = errors.New("core: content-derived field set at M0, which forbids reading content")

// ErrFieldForbidden is the defect signal for a field the record's kind does not carry: content on
// a discovery or agent_activity record, a discovery field on a prompt, and so on. The envelope is
// refused before it is minted.
var ErrFieldForbidden = errors.New("core: field forbidden for this kind")

type envelopeWire struct {
	SchemaVersion string `json:"schema_version"`
	EventID       string `json:"event_id"`
	TenantID      string `json:"tenant_id"`
	DeviceID      string `json:"device_id"`
	UserRef       string `json:"user_ref"`
	SubjectName   string `json:"subject_name,omitempty"`

	ToolFingerprint   string                  `json:"tool_fingerprint"`
	Direction         string                  `json:"direction"`
	Kind              protocol.Kind           `json:"kind"`
	PromptKind        protocol.PromptKind     `json:"prompt_kind,omitempty"`
	OccurredAt        time.Time               `json:"occurred_at"`
	MonotonicOffsetMS int64                   `json:"monotonic_offset_ms"`
	Source            protocol.Route          `json:"source"`
	CollectionMode    protocol.CollectionMode `json:"collection_mode"`
	DedupKey          string                  `json:"dedup_key"`

	SizeBytes         *int64                          `json:"size_bytes,omitempty"`
	ContentDigest     string                          `json:"content_digest,omitempty"`
	Labels            *[]protocol.Label               `json:"labels,omitempty"`
	ClassifierVersion string                          `json:"classifier_version,omitempty"`
	Confidence        protocol.Confidence             `json:"confidence,omitempty"`
	ContentExcerpt    *protocol.Excerpt               `json:"content_excerpt,omitempty"`
	Attachments       []protocol.AttachmentDescriptor `json:"attachments,omitempty"`
	PolicyDecision    *protocol.Decision              `json:"policy_decision,omitempty"`

	DiscoveryType   protocol.DiscoveryType  `json:"discovery_type,omitempty"`
	DetectionBasis  protocol.DetectionBasis `json:"detection_basis,omitempty"`
	AppVersion      string                  `json:"app_version,omitempty"`
	Publisher       string                  `json:"publisher,omitempty"`
	HostApp         string                  `json:"host_app,omitempty"`
	DestinationHost string                  `json:"destination_host,omitempty"`
	ModelNames      []string                `json:"model_names,omitempty"`

	ActivityType protocol.ActivityType    `json:"activity_type,omitempty"`
	Model        string                   `json:"model,omitempty"`
	InputTokens  *int64                   `json:"input_tokens,omitempty"`
	OutputTokens *int64                   `json:"output_tokens,omitempty"`
	DurationMS   *int64                   `json:"duration_ms,omitempty"`
	ToolName     string                   `json:"tool_name,omitempty"`
	Outcome      protocol.ActivityOutcome `json:"outcome,omitempty"`
}

// BuildEnvelope mints the device submission record for one observation: a prompt, a discovery or
// an agent_activity. The device mints no usage_rollup.
//
// The kind and, for a prompt, the mode decide what may appear, and the checks are refusals rather
// than omissions:
//
//   - A prompt at M0 carries no content-derived field and no attachment descriptor: its closed
//     list is device, user, tool, timestamp, size and destination.
//   - A prompt at M1 and above requires the classifier's output including its version, so a
//     change in classifier behaviour shows up as a version change.
//   - A prompt at M2 requires a minimised excerpt. M3 forbids one: M3 content moves only on a
//     per-event grant, and the envelope says nothing about content held on the device.
//   - A discovery or agent_activity record is metadata at every mode: no content-derived field,
//     no prompt kind and no policy decision, and none of the other kind's fields. A discovery
//     carries no size either.
func BuildEnvelope(in EnvelopeInput) ([]byte, error) {
	if !in.Mode.Valid() {
		return nil, fmt.Errorf("core: envelope has mode %q outside the closed set", in.Mode)
	}
	if !in.Route.Valid() {
		return nil, fmt.Errorf("core: envelope has route %q outside the closed vocabulary", in.Route)
	}
	name := strings.TrimSpace(in.Identity.SubjectName)
	if n := len([]rune(name)); n > maxSubjectNameChars {
		return nil, fmt.Errorf("core: subject_name is %d characters, over the %d-character cap", n, maxSubjectNameChars)
	}

	e := envelopeWire{
		SchemaVersion:     SchemaVersion,
		EventID:           in.EventID,
		TenantID:          in.Identity.TenantID,
		DeviceID:          in.Identity.DeviceID,
		UserRef:           in.Identity.UserRef,
		SubjectName:       name,
		ToolFingerprint:   in.ToolFingerprint,
		Kind:              in.Kind,
		OccurredAt:        in.OccurredAt.UTC(),
		MonotonicOffsetMS: in.MonotonicOffsetMS,
		Source:            in.Route,
		CollectionMode:    in.Mode,
		DedupKey:          in.DedupKey,
	}
	var err error
	switch in.Kind {
	case protocol.KindPrompt:
		err = buildPrompt(in, &e)
	case protocol.KindDiscovery:
		err = buildDiscovery(in, &e)
	case protocol.KindAgentActivity:
		err = buildActivity(in, &e)
	default:
		err = fmt.Errorf("core: the device mints %s, %s and %s envelopes, not %q",
			protocol.KindPrompt, protocol.KindDiscovery, protocol.KindAgentActivity, in.Kind)
	}
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("core: encoding envelope: %w", err)
	}
	if err := ValidateEnvelopeMode(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func buildPrompt(in EnvelopeInput, e *envelopeWire) error {
	if in.PromptKind != "" && !in.PromptKind.Valid() {
		return fmt.Errorf("core: envelope has prompt_kind %q outside the closed set", in.PromptKind)
	}
	if in.FactFields.discoverySet() || in.FactFields.activitySet() {
		return fmt.Errorf("%w: a prompt carries no discovery or agent_activity field", ErrFieldForbidden)
	}
	contentDerived := in.ContentDigest != "" || len(in.Labels) > 0 || in.ClassifierVersion != "" ||
		in.Confidence != "" || in.Excerpt != nil || len(in.Attachments) > 0
	if in.Mode == protocol.ModeM0 && contentDerived {
		return fmt.Errorf("%w (route=%s)", ErrContentAtM0, in.Route)
	}
	e.Direction = "egress"
	e.PromptKind = in.PromptKind
	e.SizeBytes = in.SizeBytes
	e.PolicyDecision = in.Decision
	if in.Mode.ReadsContent() {
		e.ContentDigest = in.ContentDigest
		// labels is required at M1 and above, and an empty set is a legitimate answer ("the
		// classifier ran and found nothing"), so it is emitted as an empty array: absence and
		// emptiness are different facts.
		labels := in.Labels
		if labels == nil {
			labels = []protocol.Label{}
		}
		e.Labels = &labels
		e.ClassifierVersion = in.ClassifierVersion
		e.Confidence = in.Confidence
		e.ContentExcerpt = in.Excerpt
		e.Attachments = in.Attachments
	}
	return nil
}

// refuseContent refuses every field a discovery or agent_activity record never carries: what
// classification, excerpting or a policy decision would produce.
func refuseContent(in EnvelopeInput) error {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"content_digest", in.ContentDigest != ""},
		{"labels", in.Labels != nil},
		{"classifier_version", in.ClassifierVersion != ""},
		{"confidence", in.Confidence != ""},
		{"content_excerpt", in.Excerpt != nil},
		{"attachments", in.Attachments != nil},
		{"prompt_kind", in.PromptKind != ""},
		{"policy_decision", in.Decision != nil},
	} {
		if f.set {
			return fmt.Errorf("%w: a %s record carries no %s", ErrFieldForbidden, in.Kind, f.name)
		}
	}
	return nil
}

func buildDiscovery(in EnvelopeInput, e *envelopeWire) error {
	if err := refuseContent(in); err != nil {
		return err
	}
	if in.SizeBytes != nil {
		return fmt.Errorf("%w: a discovery record carries no size_bytes", ErrFieldForbidden)
	}
	if in.FactFields.activitySet() {
		return fmt.Errorf("%w: a discovery record carries no agent_activity field", ErrFieldForbidden)
	}
	f := in.FactFields
	if !f.DiscoveryType.Valid() {
		return fmt.Errorf("core: discovery has discovery_type %q outside the closed set", f.DiscoveryType)
	}
	if !f.DetectionBasis.Valid() {
		return fmt.Errorf("core: discovery has detection_basis %q outside the closed set", f.DetectionBasis)
	}
	if err := firstErr(
		charCap("app_version", f.AppVersion, maxAppVersionChars),
		charCap("publisher", f.Publisher, maxPublisherChars),
		charCap("host_app", f.HostApp, maxHostAppChars),
		charCap("destination_host", f.DestinationHost, maxDestinationHostChars),
	); err != nil {
		return err
	}
	if f.DestinationHost != "" && !destinationHost.MatchString(f.DestinationHost) {
		return fmt.Errorf("core: destination_host %q is not a lower-case host name", f.DestinationHost)
	}
	if len(f.ModelNames) > maxModelNames {
		return fmt.Errorf("core: model_names has %d entries, over the cap of %d", len(f.ModelNames), maxModelNames)
	}
	for _, m := range f.ModelNames {
		if m == "" {
			return errors.New("core: model_names carries an empty name")
		}
		if err := charCap("model_names entry", m, maxModelNameChars); err != nil {
			return err
		}
	}
	e.Direction = "none"
	e.DiscoveryType = f.DiscoveryType
	e.DetectionBasis = f.DetectionBasis
	e.AppVersion = f.AppVersion
	e.Publisher = f.Publisher
	e.HostApp = f.HostApp
	e.DestinationHost = f.DestinationHost
	e.ModelNames = f.ModelNames
	return nil
}

func buildActivity(in EnvelopeInput, e *envelopeWire) error {
	if err := refuseContent(in); err != nil {
		return err
	}
	if in.FactFields.discoverySet() {
		return fmt.Errorf("%w: an agent_activity record carries no discovery field", ErrFieldForbidden)
	}
	f := in.FactFields
	if !f.ActivityType.Valid() {
		return fmt.Errorf("core: agent_activity has activity_type %q outside the closed set", f.ActivityType)
	}
	if f.Outcome != "" && !f.Outcome.Valid() {
		return fmt.Errorf("core: agent_activity has outcome %q outside the closed set", f.Outcome)
	}
	if err := firstErr(
		charCap("model", f.Model, maxModelChars),
		charCap("tool_name", f.ToolName, maxToolNameChars),
		nonNegative("size_bytes", in.SizeBytes),
		nonNegative("input_tokens", f.InputTokens),
		nonNegative("output_tokens", f.OutputTokens),
		nonNegative("duration_ms", f.DurationMS),
	); err != nil {
		return err
	}
	e.Direction = "none"
	e.SizeBytes = in.SizeBytes
	e.ActivityType = f.ActivityType
	e.Model = f.Model
	e.InputTokens = f.InputTokens
	e.OutputTokens = f.OutputTokens
	e.DurationMS = f.DurationMS
	e.ToolName = f.ToolName
	e.Outcome = f.Outcome
	return nil
}

func charCap(field, value string, limit int) error {
	if n := len([]rune(value)); n > limit {
		return fmt.Errorf("core: %s is %d characters, over the %d-character cap", field, n, limit)
	}
	return nil
}

func nonNegative(field string, v *int64) error {
	if v != nil && *v < 0 {
		return fmt.Errorf("core: %s is %d; it cannot be negative", field, *v)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ValidateEnvelopeMode re-checks the minted JSON against the envelope's per-kind field rules and,
// for a prompt, its mode rules. It is a second pass over the bytes rather than over the input
// struct, so it is the check that catches a mistake in the builder itself.
func ValidateEnvelopeMode(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("core: envelope is not a JSON object: %w", err)
	}
	kind := protocol.Kind(unquoted(fields["kind"]))
	table, ok := kindFields[kind]
	if !ok {
		return fmt.Errorf("core: envelope kind %q is not one the device mints", kind)
	}
	mode := protocol.CollectionMode(unquoted(fields["collection_mode"]))
	if !mode.Valid() {
		return fmt.Errorf("core: envelope mode %q outside the closed set", mode)
	}
	for name := range fields {
		_, common := commonFields[name]
		_, own := table[name]
		if !common && !own {
			return fmt.Errorf("core: %s envelope carries field %q, which that kind does not define", kind, name)
		}
	}
	for _, t := range []map[string]bool{commonFields, table} {
		for name, required := range t {
			if _, present := fields[name]; required && !present {
				return fmt.Errorf("core: %s envelope has no %q, which is required", kind, name)
			}
		}
	}
	direction := "none"
	if kind == protocol.KindPrompt {
		direction = "egress"
	}
	if got := unquoted(fields["direction"]); got != direction {
		return fmt.Errorf("core: %s envelope has direction %q, want %q", kind, got, direction)
	}
	if kind != protocol.KindPrompt {
		return nil
	}
	return checkModeFields(mode, fields)
}

// commonFields is every field any envelope may carry, and whether it is required. kindFields adds
// each kind's own fields. TestEnvelopeFieldTableMatchesTheWireStruct keeps the tables in step with
// envelopeWire, so a field added to the struct without a decision here fails a test rather than
// ingest.
var commonFields = map[string]bool{
	"schema_version": true, "event_id": true, "tenant_id": true, "device_id": true, "user_ref": true,
	"tool_fingerprint": true, "direction": true, "kind": true, "occurred_at": true,
	"monotonic_offset_ms": true, "source": true, "collection_mode": true, "dedup_key": true,
	"subject_name": false,
}

var kindFields = map[protocol.Kind]map[string]bool{
	protocol.KindPrompt: {
		"size_bytes": true, "policy_decision": true, "prompt_kind": false,
		"content_digest": false, "labels": false, "classifier_version": false, "confidence": false,
		"content_excerpt": false, "attachments": false,
	},
	protocol.KindDiscovery: {
		"discovery_type": true, "detection_basis": true,
		"app_version": false, "publisher": false, "host_app": false, "destination_host": false,
		"model_names": false,
	},
	protocol.KindAgentActivity: {
		"activity_type": true, "size_bytes": false, "model": false,
		"input_tokens": false, "output_tokens": false, "duration_ms": false, "tool_name": false,
		"outcome": false,
	},
}

// checkModeFields applies the per-mode rules: M0's closed list, M1's required classifier
// attribution, M2's excerpt, M3's forbidden excerpt.
func checkModeFields(mode protocol.CollectionMode, fields map[string]json.RawMessage) error {
	if mode == protocol.ModeM0 {
		// A metadata-only device read no body from which to decide a prompt kind either.
		for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments", "prompt_kind"} {
			if _, ok := fields[f]; ok {
				return fmt.Errorf("%w: envelope carries %q with collection_mode=m0", ErrContentAtM0, f)
			}
		}
		return nil
	}
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
		if _, ok := fields[f]; !ok {
			return fmt.Errorf("core: %s envelope has no %s; M1 and above must carry the classifier's output", mode, f)
		}
	}
	_, hasExcerpt := fields["content_excerpt"]
	if mode == protocol.ModeM2 && !hasExcerpt {
		return fmt.Errorf("core: m2 envelope has no content_excerpt")
	}
	if mode == protocol.ModeM3 && hasExcerpt {
		return fmt.Errorf("core: m3 envelope carries content_excerpt; M3 content moves only on a per-event grant")
	}
	if hasExcerpt {
		var e protocol.Excerpt
		if err := json.Unmarshal(fields["content_excerpt"], &e); err != nil {
			return fmt.Errorf("core: content_excerpt is not an excerpt: %w", err)
		}
		if len([]rune(e.Text)) > protocol.MaxExcerptChars {
			return fmt.Errorf("core: excerpt is %d characters, over the %d-character cap", len([]rune(e.Text)), protocol.MaxExcerptChars)
		}
	}
	return nil
}

// wireFieldNames lists every JSON name envelopeWire can emit, by reflection.
func wireFieldNames() []string {
	t := reflect.TypeOf(envelopeWire{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func unquoted(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
