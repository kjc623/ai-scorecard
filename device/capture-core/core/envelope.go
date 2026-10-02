package core

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ValidateEnvelopeMode re-checks the minted JSON against the contract's mode branches. It is
// deliberately a second pass over the bytes rather than over the input struct: it is the only
// check that can catch a mistake in the builder itself, and it is the same check a test uses
// to assert that an M0 record carries no content-derived key.
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

	forbiddenAtM0 := []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"}
	switch kind {
	case protocol.KindPrompt:
		if _, ok := fields["size_bytes"]; !ok {
			return fmt.Errorf("core: prompt envelope has no size_bytes; it is available at every mode")
		}
		if _, ok := fields["policy_decision"]; !ok {
			return fmt.Errorf("core: prompt envelope has no policy_decision; a tenant can block a tool without reading content")
		}
		if mode == protocol.ModeM0 {
			for _, f := range forbiddenAtM0 {
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
	case protocol.KindUsageRollup, protocol.KindModelDetection:
		for _, f := range append([]string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"}, "size_bytes", "policy_decision") {
			if _, ok := fields[f]; ok {
				return fmt.Errorf("core: %s envelope carries %q, which the schema forbids for this kind", kind, f)
			}
		}
	}
	return nil
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
