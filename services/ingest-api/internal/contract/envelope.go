package contract

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This file is the adapter between the raw wire bytes and the rest of the service.
//
// It is deliberately *not* a struct declaration of the envelope. It holds the raw JSON object and
// reads named fields out of it, so there is exactly one source of truth for the wire shape --
// contracts/event-envelope.schema.json -- and no second Go definition of it (ADR 0010). The field
// names below are the *accessor* set the ingest path needs; validation of every one of them,
// including their required/forbidden status per kind and mode, is performed by the schema in
// schema.go, not here.
//
// When contracts/generated/go/envelope lands (T1), Decode changes to unmarshal into the generated
// type and these accessors become one-line delegations. Nothing else in the service moves.

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// Envelope is a decoded device submission kept in its JSON form.
type Envelope struct {
	raw    map[string]json.RawMessage
	fields []string // sorted field names present, for the §7 presence map
}

// DecodeEnvelope reads one envelope object. It rejects anything that is not a JSON object: a
// non-object has no field vocabulary to validate against, and §5.3's per-event result contract is
// keyed by event_id, which a non-object cannot carry.
func DecodeEnvelope(raw []byte) (*Envelope, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("contract: envelope is not a JSON object: %w", err)
	}
	fields := make([]string, 0, len(m))
	for k := range m {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	return &Envelope{raw: m, fields: fields}, nil
}

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

// AttachmentNames returns the attachment `name` values in wire order, for the §4.5 names digest.
func (e *Envelope) AttachmentNames() []string {
	raw, ok := e.raw["attachments"]
	if !ok {
		return nil
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}

// AttachmentCount returns the number of attachment descriptors present, 0 when absent. A count of
// -1 means the field is present but not a JSON array, which only happens on an envelope that the
// schema has already rejected.
func (e *Envelope) AttachmentCount() int {
	raw, ok := e.raw["attachments"]
	if !ok {
		return 0
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return -1
	}
	return len(list)
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
