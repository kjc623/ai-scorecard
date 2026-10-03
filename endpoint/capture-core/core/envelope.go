package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Identity is the part of the envelope that identifies the subject. It is resolved from the
// device's enrolment, never from a provider, so no provider can attribute an observation to
// another tenant or user.
type Identity struct {
	TenantID string
	DeviceID string
	UserRef  string
}

// SchemaVersion is the contract version these records validate against.
const SchemaVersion = "1.0"

// Attachment descriptors use protocol.AttachmentDescriptor, whose field names are the
// contract's `$defs/attachment` names verbatim (`content_digest`). One shape, one name: the
// same descriptor crosses native messaging from the extension into capture-core and ends up
// inside an envelope, and the contract is closed with `additionalProperties: false`.

// EnvelopeInput is everything an envelope needs. Content-derived fields are present here so
// the builder can *refuse* them at a mode that forbids them rather than silently dropping
// them: a dropped digest is a silent mode violation, a refused one is a defect report.
type EnvelopeInput struct {
	Identity Identity
	EventID  string

	Kind  protocol.Kind
	Route protocol.Route
	Mode  protocol.CollectionMode

	ToolFingerprint   string
	OccurredAt        time.Time
	MonotonicOffsetMS int64
	DedupKey          string
	SizeBytes         *int64

	// Content-derived: M1 and above only, and forbidden outright for the rollup and
	// detection kinds.
	ContentDigest     string
	Labels            []protocol.Label
	ClassifierVersion string
	Confidence        protocol.Confidence
	Excerpt           *protocol.Excerpt
	Attachments       []protocol.AttachmentDescriptor

	// Metadata that every prompt carries, including M0.
	Decision *protocol.Decision

	// Rollup and detection kinds.
	WindowStart     *time.Time
	WindowEnd       *time.Time
	SubmissionCount *int
	BytesTotal      *int64
	DetectionBasis  string
}

// ErrContentAtM0 is the defect signal: something populated a content-derived field for an
// observation the device was not permitted to read. §11.2 makes that an ingest rejection on
// the wire; here it is refused before the envelope is ever minted, which is strictly earlier.
var ErrContentAtM0 = errors.New("core: content-derived field set at M0, which forbids reading content")

type envelopeWire struct {
	SchemaVersion string `json:"schema_version"`
	EventID       string `json:"event_id"`
	TenantID      string `json:"tenant_id"`
	DeviceID      string `json:"device_id"`
	UserRef       string `json:"user_ref"`

	ToolFingerprint   string                  `json:"tool_fingerprint"`
	Direction         string                  `json:"direction"`
	Kind              protocol.Kind           `json:"kind"`
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

	WindowStart     *time.Time `json:"window_start,omitempty"`
	WindowEnd       *time.Time `json:"window_end,omitempty"`
	SubmissionCount *int       `json:"submission_count,omitempty"`
	BytesTotal      *int64     `json:"bytes_total,omitempty"`
	DetectionBasis  string     `json:"detection_basis,omitempty"`
}

// BuildEnvelope mints the deviceSubmission record for one observation.
//
// The mode decides what may appear, and the checks are refusals rather than omissions:
//
//   - M0 carries no content-derived field, and no attachment descriptor either — M0's closed
//     list is device, user, tool, timestamp, size and destination, so a filename is not on it.
//   - M1+ requires the classifier's output including its version, so a change in classifier
//     behaviour shows up as a version change rather than a mysterious shift in the numbers.
//   - M2 requires a minimised excerpt; M3 forbids one, because M3's content path is the
//     approved per-event retrieval path, not the wire.
//
// **ADR 0017 (decided):** the M3 content-state marker is device-local. §11.3's phrase "a
// local content-state marker" has no field in contracts/event-envelope.schema.json, whose
// `additionalProperties: false` and version-change convention leave nowhere to put one, so
// the M3 envelope carries exactly the M1 fields, no excerpt, and nothing about content held.
// The "content is held locally" fact lives in the content store and the spool record, where
// the content actually is; Pipeline.ContentState exposes the count for the coverage row.
func BuildEnvelope(in EnvelopeInput) ([]byte, error) {
	if !in.Mode.Valid() {
		return nil, fmt.Errorf("core: envelope has mode %q outside the closed set", in.Mode)
	}
	if !in.Kind.Valid() {
		return nil, fmt.Errorf("core: envelope has kind %q outside the closed registry", in.Kind)
	}
	if !in.Route.Valid() {
		return nil, fmt.Errorf("core: envelope has route %q outside the closed vocabulary", in.Route)
	}
	contentDerived := in.ContentDigest != "" || len(in.Labels) > 0 || in.ClassifierVersion != "" ||
		in.Confidence != "" || in.Excerpt != nil || len(in.Attachments) > 0
	if in.Mode == protocol.ModeM0 && contentDerived {
		return nil, fmt.Errorf("%w (kind=%s route=%s)", ErrContentAtM0, in.Kind, in.Route)
	}
	if in.Kind != protocol.KindPrompt && contentDerived {
		return nil, fmt.Errorf("core: kind %s must not carry content-derived fields; the schema forbids them and no route may read content for it", in.Kind)
	}

	e := envelopeWire{
		SchemaVersion:     SchemaVersion,
		EventID:           in.EventID,
		TenantID:          in.Identity.TenantID,
		DeviceID:          in.Identity.DeviceID,
		UserRef:           in.Identity.UserRef,
		ToolFingerprint:   in.ToolFingerprint,
		Kind:              in.Kind,
		OccurredAt:        in.OccurredAt.UTC(),
		MonotonicOffsetMS: in.MonotonicOffsetMS,
		Source:            in.Route,
		CollectionMode:    in.Mode,
		DedupKey:          in.DedupKey,
		SizeBytes:         in.SizeBytes,
		PolicyDecision:    in.Decision,
		WindowStart:       in.WindowStart,
		WindowEnd:         in.WindowEnd,
		SubmissionCount:   in.SubmissionCount,
		BytesTotal:        in.BytesTotal,
		DetectionBasis:    in.DetectionBasis,
	}
	switch in.Kind {
	case protocol.KindPrompt:
		e.Direction = "egress"
	case protocol.KindUsageRollup, protocol.KindModelDetection:
		e.Direction = "none"
	}
	if in.Mode.ReadsContent() && in.Kind == protocol.KindPrompt {
		e.ContentDigest = in.ContentDigest
		// The schema requires `labels` at M1 and above, and an empty label set is a legitimate
		// output ("the classifier ran and found nothing"). It is emitted as an empty array
		// rather than omitted, because absence and emptiness are different facts.
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

	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("core: encoding envelope: %w", err)
	}
	if err := ValidateEnvelopeMode(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ValidateEnvelopeMode re-checks the minted JSON against the contract's per-kind and per-mode
// branches. It is deliberately a second pass over the bytes rather than over the input struct:
// it is the only check that can catch a mistake in the builder itself, and it is the same check
// a test uses to assert that an M0 record carries no content-derived key.
//
// It is driven by kindFieldPolicy, so a field added to envelopeWire without deciding which
// kinds may carry it fails here (and in TestEnvelope_KindFieldPolicyIsExhaustive) rather than
// at ingest. That is ADR 0018's rule: a kind's branch is exhaustive over the fields the other
// kinds own.
func ValidateEnvelopeMode(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("core: envelope is not a JSON object: %w", err)
	}
	kind := protocol.Kind(unquoted(fields["kind"]))
	mode := protocol.CollectionMode(unquoted(fields["collection_mode"]))
	if !kind.Valid() {
		return fmt.Errorf("core: envelope kind %q outside the closed registry", kind)
	}
	if !mode.Valid() {
		return fmt.Errorf("core: envelope mode %q outside the closed set", mode)
	}
	if err := checkKindFields(kind, mode, fields); err != nil {
		return err
	}
	if kind == protocol.KindPrompt {
		return checkPromptModeFields(mode, fields)
	}
	return nil
}

// fieldRule is what a kind's branch says about one field.
type fieldRule int

const (
	// fieldForbidden: the contract's `not: required` for this kind. Minting it is a defect.
	fieldForbidden fieldRule = iota
	// fieldRequired: the contract's `required` for this kind.
	fieldRequired
	// fieldOptional: permitted but not required for this kind (the mode branch refines it).
	fieldOptional
)

// kindFieldPolicy is the per-kind field permission table — the device-side mirror of
// contracts/event-envelope.schema.json's allOf branches. Every JSON name envelopeWire can
// emit must appear here for all three kinds; the test enumerates the struct by reflection and
// fails if one is undecided, so this table cannot silently fall behind the struct.
var kindFieldPolicy = map[protocol.Kind]map[string]fieldRule{
	protocol.KindPrompt: {
		"schema_version": fieldRequired, "event_id": fieldRequired, "tenant_id": fieldRequired,
		"device_id": fieldRequired, "user_ref": fieldRequired, "tool_fingerprint": fieldRequired,
		"direction": fieldRequired, "kind": fieldRequired, "occurred_at": fieldRequired,
		"monotonic_offset_ms": fieldRequired, "source": fieldRequired, "collection_mode": fieldRequired,
		"dedup_key":       fieldRequired,
		"size_bytes":      fieldRequired, // available at every mode, including M0
		"policy_decision": fieldRequired, // a tenant can block a tool without reading content
		"content_digest":  fieldOptional, "labels": fieldOptional, "classifier_version": fieldOptional,
		"confidence": fieldOptional, "content_excerpt": fieldOptional, "attachments": fieldOptional,
		"window_start": fieldForbidden, "window_end": fieldForbidden,
		"submission_count": fieldForbidden, "bytes_total": fieldForbidden,
		"detection_basis": fieldForbidden,
	},
	protocol.KindUsageRollup: {
		"schema_version": fieldRequired, "event_id": fieldRequired, "tenant_id": fieldRequired,
		"device_id": fieldRequired, "user_ref": fieldRequired, "tool_fingerprint": fieldRequired,
		"direction": fieldRequired, "kind": fieldRequired, "occurred_at": fieldRequired,
		"monotonic_offset_ms": fieldRequired, "source": fieldRequired, "collection_mode": fieldRequired,
		"dedup_key":        fieldRequired,
		"window_start":     fieldRequired,
		"window_end":       fieldRequired,
		"submission_count": fieldRequired,
		"bytes_total":      fieldRequired,
		"size_bytes":       fieldForbidden,
		"policy_decision":  fieldForbidden,
		"content_digest":   fieldForbidden, "labels": fieldForbidden, "classifier_version": fieldForbidden,
		"confidence": fieldForbidden, "content_excerpt": fieldForbidden, "attachments": fieldForbidden,
		"detection_basis": fieldForbidden,
	},
	protocol.KindModelDetection: {
		"schema_version": fieldRequired, "event_id": fieldRequired, "tenant_id": fieldRequired,
		"device_id": fieldRequired, "user_ref": fieldRequired, "tool_fingerprint": fieldRequired,
		"direction": fieldRequired, "kind": fieldRequired, "occurred_at": fieldRequired,
		"monotonic_offset_ms": fieldRequired, "source": fieldRequired, "collection_mode": fieldRequired,
		"dedup_key":       fieldRequired,
		"detection_basis": fieldRequired,
		"size_bytes":      fieldForbidden,
		"policy_decision": fieldForbidden,
		"content_digest":  fieldForbidden, "labels": fieldForbidden, "classifier_version": fieldForbidden,
		"confidence": fieldForbidden, "content_excerpt": fieldForbidden, "attachments": fieldForbidden,
		"window_start": fieldForbidden, "window_end": fieldForbidden,
		"submission_count": fieldForbidden, "bytes_total": fieldForbidden,
	},
}

// checkKindFields applies kindFieldPolicy to a marshalled envelope. A field the table does not
// decide for this kind is refused, which is what makes an undecided new field fail loudly at
// mint time instead of quietly at ingest.
func checkKindFields(kind protocol.Kind, mode protocol.CollectionMode, fields map[string]json.RawMessage) error {
	policy, ok := kindFieldPolicy[kind]
	if !ok {
		return fmt.Errorf("core: kind %q has no field policy; a kind without a decided field set must not be minted", kind)
	}
	for _, name := range wireFieldNames() {
		rule, decided := policy[name]
		_, present := fields[name]
		if !decided {
			return fmt.Errorf("core: field %q is not decided for kind %s; ADR 0018 requires every kind's branch to decide every field", name, kind)
		}
		switch rule {
		case fieldForbidden:
			if present {
				if isContentDerivedField(name) && mode == protocol.ModeM0 {
					return fmt.Errorf("%w: envelope carries %q with collection_mode=m0", ErrContentAtM0, name)
				}
				return fmt.Errorf("core: kind %s must not carry %q; the contract forbids it and no route may produce it", kind, name)
			}
		case fieldRequired:
			if !present {
				return fmt.Errorf("core: kind %s envelope has no %q, which the contract requires", kind, name)
			}
		case fieldOptional:
			// Decided as permitted; the mode branch below may still refine it.
		}
	}
	// Refuse a field that is present but that the table does not know at all: a JSON name the
	// struct emits but the table has never heard of is exactly the drift this guards against.
	for name := range fields {
		if _, known := policy[name]; !known {
			return fmt.Errorf("core: kind %s envelope carries field %q that no kind policy decides", kind, name)
		}
	}
	return nil
}

// isContentDerivedField is the set M0 forbids outright: reading content is not permitted, so a
// digest, a label set, an excerpt or even a filename is evidence of a defect (§11.2, §11.3).
func isContentDerivedField(name string) bool {
	switch name {
	case "content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments":
		return true
	default:
		return false
	}
}

// checkPromptModeFields refines the prompt branch by mode: M0's closed list, M1's required
// classifier attribution, M2's excerpt, M3's forbidden excerpt.
func checkPromptModeFields(mode protocol.CollectionMode, fields map[string]json.RawMessage) error {
	if mode == protocol.ModeM0 {
		for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"} {
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
	if mode == protocol.ModeM2 {
		if _, ok := fields["content_excerpt"]; !ok {
			return fmt.Errorf("core: m2 envelope has no content_excerpt")
		}
	}
	if mode == protocol.ModeM3 {
		if _, ok := fields["content_excerpt"]; ok {
			return fmt.Errorf("core: m3 envelope carries content_excerpt; the schema forbids it because content moves only on a per-event grant")
		}
	}
	if ex, ok := fields["content_excerpt"]; ok {
		var e protocol.Excerpt
		if err := json.Unmarshal(ex, &e); err != nil {
			return fmt.Errorf("core: content_excerpt is not an excerpt: %w", err)
		}
		if len([]rune(e.Text)) > protocol.MaxExcerptChars {
			return fmt.Errorf("core: excerpt is %d characters, over the %d-character cap", len([]rune(e.Text)), protocol.MaxExcerptChars)
		}
	}
	return nil
}

// wireFieldNames lists every JSON name envelopeWire can emit, by reflection. It is the join
// between the struct and kindFieldPolicy: TestEnvelope_KindFieldPolicyIsExhaustive asserts
// every name is decided for every kind, so adding a field to the struct without deciding the
// kinds that may carry it fails the build's tests rather than the ingest path.
func wireFieldNames() []string {
	t := reflect.TypeOf(envelopeWire{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
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
