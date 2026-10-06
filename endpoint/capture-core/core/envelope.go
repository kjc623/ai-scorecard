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

// EnvelopeInput is everything a prompt envelope needs. Content-derived fields are present so the
// builder can refuse them at a mode that forbids them rather than silently dropping them: a
// dropped digest would hide a mode violation, a refused one reports it.
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

	// Content-derived: M1 and above only.
	ContentDigest     string
	Labels            []protocol.Label
	ClassifierVersion string
	Confidence        protocol.Confidence
	Excerpt           *protocol.Excerpt
	Attachments       []protocol.AttachmentDescriptor

	// Decision is carried at every mode, including M0.
	Decision *protocol.Decision
}

// ErrContentAtM0 is the defect signal: something populated a content-derived field for an
// observation the device was not permitted to read. The envelope is refused before it is minted.
var ErrContentAtM0 = errors.New("core: content-derived field set at M0, which forbids reading content")

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
}

// BuildEnvelope mints the device submission record for one prompt observation. The device emits
// only the prompt kind.
//
// The mode decides what may appear, and the checks are refusals rather than omissions:
//
//   - M0 carries no content-derived field and no attachment descriptor: its closed list is
//     device, user, tool, timestamp, size and destination.
//   - M1 and above require the classifier's output including its version, so a change in
//     classifier behaviour shows up as a version change.
//   - M2 requires a minimised excerpt. M3 forbids one: M3 content moves only on a per-event
//     grant, and the envelope says nothing about content held on the device.
func BuildEnvelope(in EnvelopeInput) ([]byte, error) {
	if in.Kind != protocol.KindPrompt {
		return nil, fmt.Errorf("core: the device mints only %s envelopes, not %q", protocol.KindPrompt, in.Kind)
	}
	if !in.Mode.Valid() {
		return nil, fmt.Errorf("core: envelope has mode %q outside the closed set", in.Mode)
	}
	if !in.Route.Valid() {
		return nil, fmt.Errorf("core: envelope has route %q outside the closed vocabulary", in.Route)
	}
	if in.PromptKind != "" && !in.PromptKind.Valid() {
		return nil, fmt.Errorf("core: envelope has prompt_kind %q outside the closed set", in.PromptKind)
	}
	name := strings.TrimSpace(in.Identity.SubjectName)
	if n := len([]rune(name)); n > maxSubjectNameChars {
		return nil, fmt.Errorf("core: subject_name is %d characters, over the %d-character cap", n, maxSubjectNameChars)
	}
	contentDerived := in.ContentDigest != "" || len(in.Labels) > 0 || in.ClassifierVersion != "" ||
		in.Confidence != "" || in.Excerpt != nil || len(in.Attachments) > 0
	if in.Mode == protocol.ModeM0 && contentDerived {
		return nil, fmt.Errorf("%w (route=%s)", ErrContentAtM0, in.Route)
	}

	e := envelopeWire{
		SchemaVersion:     SchemaVersion,
		EventID:           in.EventID,
		TenantID:          in.Identity.TenantID,
		DeviceID:          in.Identity.DeviceID,
		UserRef:           in.Identity.UserRef,
		SubjectName:       name,
		ToolFingerprint:   in.ToolFingerprint,
		Direction:         "egress",
		Kind:              in.Kind,
		PromptKind:        in.PromptKind,
		OccurredAt:        in.OccurredAt.UTC(),
		MonotonicOffsetMS: in.MonotonicOffsetMS,
		Source:            in.Route,
		CollectionMode:    in.Mode,
		DedupKey:          in.DedupKey,
		SizeBytes:         in.SizeBytes,
		PolicyDecision:    in.Decision,
	}
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

	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("core: encoding envelope: %w", err)
	}
	if err := ValidateEnvelopeMode(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ValidateEnvelopeMode re-checks the minted JSON against the envelope's field and mode rules. It
// is a second pass over the bytes rather than over the input struct, so it is the check that
// catches a mistake in the builder itself.
func ValidateEnvelopeMode(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("core: envelope is not a JSON object: %w", err)
	}
	if kind := protocol.Kind(unquoted(fields["kind"])); kind != protocol.KindPrompt {
		return fmt.Errorf("core: envelope kind %q is not %s", kind, protocol.KindPrompt)
	}
	mode := protocol.CollectionMode(unquoted(fields["collection_mode"]))
	if !mode.Valid() {
		return fmt.Errorf("core: envelope mode %q outside the closed set", mode)
	}
	for name := range fields {
		if _, known := promptFields[name]; !known {
			return fmt.Errorf("core: envelope carries field %q that the envelope does not define", name)
		}
	}
	for name, required := range promptFields {
		if _, present := fields[name]; required && !present {
			return fmt.Errorf("core: envelope has no %q, which is required", name)
		}
	}
	return checkModeFields(mode, fields)
}

// promptFields is every field a prompt envelope may carry, and whether it is required at every
// mode. TestEnvelopeFieldTableMatchesTheWireStruct keeps it in step with envelopeWire, so a field
// added to the struct without a decision here fails a test rather than ingest.
var promptFields = map[string]bool{
	"schema_version": true, "event_id": true, "tenant_id": true, "device_id": true, "user_ref": true,
	"tool_fingerprint": true, "direction": true, "kind": true, "occurred_at": true,
	"monotonic_offset_ms": true, "source": true, "collection_mode": true, "dedup_key": true,
	"size_bytes": true, "policy_decision": true,
	"subject_name": false, "prompt_kind": false,
	"content_digest": false, "labels": false, "classifier_version": false, "confidence": false,
	"content_excerpt": false, "attachments": false,
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
