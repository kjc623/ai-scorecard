package contract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"shadow-ai-capture.invalid/contracts/generated/go/envelope"
)

// This file is the adapter between the raw wire bytes and the rest of the service.
//
// The wire shape is NOT declared here. Two things are consumed instead, and neither is a copy:
//
//   - contracts/generated/go/envelope (T1, ADR 0010) is the compile-time field vocabulary. Every
//     DecodeEnvelope call goes through envelope.DecodeDeviceSubmission, so a renamed or retyped
//     field breaks this build rather than drifting quietly.
//   - contracts/event-envelope.schema.json is the validation authority, walked for the JSON Pointer
//     and violated constraint that §7's rejection `detail` is made of.
//
// What remains below is a projection: the field names the ingest path needs to *read* (tenant,
// device, kind, mode, sizes, digests, timestamps) plus the redaction and presence-map helpers that
// operate on the raw object. `AdditionalProperties:false` means the raw object and the generated
// types hold the same key set, so the projection cannot invent a field the contract does not have.

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// Envelope is a decoded device submission: the raw object, the generated contract value, and the
// generated decoder's verdict.
type Envelope struct {
	raw      map[string]json.RawMessage
	rawBytes []byte
	fields   []string // sorted field names present, for the §7 presence map

	gen    envelope.DeviceSubmission
	genErr error
}

// DecodeEnvelope reads one envelope object and decodes it with the generated contract types.
//
// It returns an error only when the element is not a JSON object: that element has no field
// vocabulary to validate and cannot carry the event_id the per-event result contract is keyed by,
// so it is a batch-level defect. A contract violation inside a well-formed object is NOT an error
// here — it is the per-event rejection that Schema.ValidateEnvelope reports.
func DecodeEnvelope(raw []byte) (*Envelope, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("contract: envelope is not a JSON object")
	}
	fields := make([]string, 0, len(m))
	for k := range m {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	e := &Envelope{raw: m, rawBytes: append([]byte(nil), raw...), fields: fields}
	e.gen, e.genErr = envelope.DecodeDeviceSubmission(raw)
	return e, nil
}

// Generated returns the generated contract value, or nil when the decoder refused the envelope.
func (e *Envelope) Generated() envelope.DeviceSubmission { return e.gen }

// GeneratedError returns the generated decoder's verdict, nil when the envelope is well-formed.
func (e *Envelope) GeneratedError() error { return e.genErr }

// Has reports whether a field is present.
func (e *Envelope) Has(name string) bool { _, ok := e.raw[name]; return ok }

// Presence returns the sorted field names present on the envelope (§7's presence map).
func (e *Envelope) Presence() []string {
	out := make([]string, len(e.fields))
	copy(out, e.fields)
	return out
}

// Missing returns the named fields that are not present, in the order given.
func (e *Envelope) Missing(names []string) []string {
	var out []string
	for _, n := range names {
		if !e.Has(n) {
			out = append(out, n)
		}
	}
	return out
}

func (e *Envelope) Raw(name string) (json.RawMessage, bool) {
	v, ok := e.raw[name]
	return v, ok
}

// RawOrNil returns the field's raw bytes, or nil when absent.
func (e *Envelope) RawOrNil(name string) json.RawMessage {
	if v, ok := e.raw[name]; ok {
		return v
	}
	return nil
}

// IsUUID reports whether s has the shape the contract's uuid fields require.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

// DeterministicUUID derives a stable uuid from a purpose-separated input. It is used where an
// identifier must be recomputable from the credential rather than stored: a re-issued certificate
// is a different credential, and the value must not depend on the process.
func DeterministicUUID(prefix string, data []byte) string {
	sum := sha256.Sum256(append([]byte(prefix), data...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (e *Envelope) String(name string) (string, bool) {
	v, ok := e.raw[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

func (e *Envelope) Int(name string) (int64, bool) {
	v, ok := e.raw[name]
	if !ok {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, false
	}
	return n, true
}

func (e *Envelope) Time(name string) (time.Time, bool) {
	s, ok := e.String(name)
	if !ok {
		return time.Time{}, false
	}
	t, err := parseTime(s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// --- the accessor set the ingest path uses -------------------------------------------------

func (e *Envelope) EventID() (string, bool)    { return e.String("event_id") }
func (e *Envelope) TenantID() (string, bool)   { return e.String("tenant_id") }
func (e *Envelope) DeviceID() (string, bool)   { return e.String("device_id") }
func (e *Envelope) UserRef() (string, bool)    { return e.String("user_ref") }
func (e *Envelope) Tool() (string, bool)       { return e.String("tool_fingerprint") }
func (e *Envelope) Direction() (string, bool)  { return e.String("direction") }
func (e *Envelope) Kind() (string, bool)       { return e.String("kind") }
func (e *Envelope) Mode() (string, bool)       { return e.String("collection_mode") }
func (e *Envelope) Source() (string, bool)     { return e.String("source") }
func (e *Envelope) DedupKey() (string, bool)   { return e.String("dedup_key") }
func (e *Envelope) SchemaVersion() (string, bool) { return e.String("schema_version") }

func (e *Envelope) OccurredAt() (time.Time, bool) { return e.Time("occurred_at") }
func (e *Envelope) WindowStart() (time.Time, bool) { return e.Time("window_start") }
func (e *Envelope) WindowEnd() (time.Time, bool)   { return e.Time("window_end") }

func (e *Envelope) SizeBytes() (int64, bool) { return e.Int("size_bytes") }

func (e *Envelope) ContentDigest() (string, bool) { return e.String("content_digest") }
func (e *Envelope) ClassifierVersion() (string, bool) { return e.String("classifier_version") }
func (e *Envelope) Confidence() (string, bool)     { return e.String("confidence") }
func (e *Envelope) DetectionBasis() (string, bool) { return e.String("detection_basis") }
func (e *Envelope) SubmissionCount() (int64, bool) { return e.Int("submission_count") }
func (e *Envelope) BytesTotal() (int64, bool)      { return e.Int("bytes_total") }

// AttachmentView is a projection of one wire attachment descriptor onto the fields §4.2 C7 and
// §4.5 need. It is a projection, not a wire type: the wire type is the generated
// envelope.Attachment, whose field names are `name`, `size_bytes` and `content_digest`.
type AttachmentView struct {
	Name          string
	MediaType     string // not carried by the v1.0 contract; C7's default applies when empty
	SizeBytes     int64
	ContentDigest string
	// Readable is false when the collector could not obtain the bytes (E3), which is what makes an
	// observation Tier T-B rather than T-A.
	Readable bool
}

// Attachments returns the attachment descriptors in wire order.
func (e *Envelope) Attachments() []AttachmentView {
	raw, ok := e.raw["attachments"]
	if !ok {
		return nil
	}
	var list []struct {
		Name          string `json:"name"`
		SizeBytes     *int64 `json:"size_bytes"`
		ContentDigest string `json:"content_digest"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]AttachmentView, 0, len(list))
	for _, a := range list {
		v := AttachmentView{Name: a.Name, ContentDigest: a.ContentDigest, Readable: a.ContentDigest != ""}
		if a.SizeBytes != nil {
			v.SizeBytes = *a.SizeBytes
		}
		out = append(out, v)
	}
	return out
}

// AttachmentNames returns the attachment `name` values in wire order, for the §4.5 names digest.
func (e *Envelope) AttachmentNames() []string {
	views := e.Attachments()
	out := make([]string, 0, len(views))
	for _, a := range views {
		out = append(out, a.Name)
	}
	return out
}

// quarantineForbidden are the keys ingest.rejected refuses to hold at all. They are the strictest
// of the two rules in play: §7 permits content_digest in the redacted envelope when it matches the
// schema pattern, and db/schema.sql's rejected_no_digest/rejected_holds_no_content constraints
// refuse content_digest, content_excerpt, content_bytes, prompt_text and attachments outright.
// The database's constraint wins, because a row that violates it cannot be written at all.
var quarantineForbidden = []string{
	"content_excerpt", "content_bytes", "prompt_text", "attachments", "content_digest",
}

// Redacted returns the §7 content-stripped envelope: identifiers, timestamps, the declared mode,
// labels and the policy decision survive; content and anything derived from it does not.
func (e *Envelope) Redacted() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(e.raw))
	for k, v := range e.raw {
		out[k] = v
	}
	for _, k := range quarantineForbidden {
		delete(out, k)
	}
	return out
}

// RedactedKeys returns the keys removed by Redacted, for the operator-facing detail.
func (e *Envelope) RedactedKeys() []string {
	var out []string
	for _, k := range quarantineForbidden {
		if e.Has(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// FieldPresence is the structured presence map stored in ingest.rejected.field_presence. §7 asks
// for "the field names present, plus the ones required and missing"; the wire shape
// protocol.BatchRejectionDetail carries a flat list, so the missing half is carried here and in
// the rejection's `expected`.
type FieldPresence struct {
	Present []string `json:"present"`
	Missing []string `json:"missing,omitempty"`
}

// PresenceOf builds the presence record.
func (e *Envelope) PresenceOf(required []string) FieldPresence {
	return FieldPresence{Present: e.Presence(), Missing: e.Missing(required)}
}

// Describe renders an instance shape for a diagnostic without ever echoing a value.
func DescribeShape(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "absent"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "not JSON"
	}
	return shapeOf(v)
}

func shapeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return fmt.Sprintf("string(len=%d)", len(t))
	case float64:
		return "number"
	case []any:
		return fmt.Sprintf("array(len=%d)", len(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object(" + strings.Join(keys, ",") + ")"
	}
	return "unknown"
}
