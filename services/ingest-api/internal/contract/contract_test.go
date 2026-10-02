package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	generated "shadow-ai-capture.invalid/contracts/generated/go/envelope"
)

func load(t *testing.T) *Schema {
	t.Helper()
	path, err := Discover(".")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func validM1(t *testing.T) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"schema_version":      "1.0",
		"event_id":            "11111111-1111-4111-8111-111111111111",
		"tenant_id":           "22222222-2222-4222-8222-222222222222",
		"device_id":           "33333333-3333-4333-8333-333333333333",
		"user_ref":            "user-1",
		"tool_fingerprint":    "tool-1",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         "2026-10-02T14:00:00Z",
		"monotonic_offset_ms": 1,
		"source":              "ext.web_request",
		"collection_mode":     "m1",
		"confidence":          "high",
		"size_bytes":          10,
		"content_digest":      "sha256:" + strings.Repeat("a", 64),
		"labels":              []any{},
		"classifier_version":  "2026.01.0-shadow",
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           "sha256:" + strings.Repeat("b", 64),
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestValidEnvelopePasses(t *testing.T) {
	s := load(t)
	raw := validM1(t)
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if v, err := s.ValidateEnvelope(env); err != nil {
		t.Fatalf("ValidateEnvelope: %v", err)
	} else if v != nil {
		t.Fatalf("a valid M1 prompt was rejected at %s: %s", v.Pointer, v.Expected)
	}
	if env.GeneratedError() != nil {
		t.Errorf("the generated decoder refused a valid envelope: %v", env.GeneratedError())
	}
	if env.Generated() == nil {
		t.Error("the generated contract value is nil after a successful decode")
	}
}

// TestTwoValidatorsAgreeAndTheSchemaWalkIsStricter records a deliberate interpretation.
//
// The generated types assert `format` only as non-emptiness (their own documented choice, and
// defensible: draft 2020-12 treats format as an annotation). The schema walk asserts the uuid
// shape. Ingest must assert it, because ingest.record_event() casts these fields with ::uuid and a
// malformed value aborts the whole batch inside the database. Rather than leave that as a comment,
// the test pins both verdicts so a future change to either one is visible.
func TestTwoValidatorsAgreeAndTheSchemaWalkIsStricter(t *testing.T) {
	s := load(t)

	var m map[string]any
	if err := json.Unmarshal(validM1(t), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["event_id"] = "not-a-uuid"
	raw, _ := json.Marshal(m)

	if _, err := generated.DecodeDeviceSubmission(raw); err != nil {
		t.Errorf("expected the generated decoder to accept a non-uuid event_id (it asserts non-emptiness only), got %v", err)
	}
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	v, err := s.ValidateEnvelope(env)
	if err != nil {
		t.Fatalf("ValidateEnvelope: %v", err)
	}
	if v == nil {
		t.Fatal("the schema walk must reject a non-uuid event_id: the store casts it to uuid and would abort the batch")
	}
	if v.Pointer != "/event_id" {
		t.Errorf("pointer = %q, want /event_id", v.Pointer)
	}
	if !strings.Contains(v.Expected, "format uuid") {
		t.Errorf("expected = %q, want it to name the format constraint", v.Expected)
	}
}

func TestSchemaViolationsAreLocated(t *testing.T) {
	s := load(t)
	cases := []struct {
		name        string
		mutate      func(m map[string]any)
		wantPointer string
		// valueMarker must NOT appear in detail.expected: §7 says the report describes the violated
		// constraint's *shape* and never echoes the offending value, which can be content.
		valueMarker string
	}{
		{"missing required field", func(m map[string]any) { delete(m, "dedup_key") }, "/dedup_key", ""},
		{"bad pattern", func(m map[string]any) { m["dedup_key"] = "sha256:NOT-A-DIGEST" }, "/dedup_key", "NOT-A-DIGEST"},
		{"unknown field", func(m map[string]any) { m["prompt_text"] = "SECRET-CONTENT-MARKER" }, "/prompt_text", "SECRET-CONTENT-MARKER"},
		{"wrong type", func(m map[string]any) { m["size_bytes"] = "TEN-BYTES" }, "/size_bytes", "TEN-BYTES"},
		{"enum violation", func(m map[string]any) { m["source"] = "ext.telepathy" }, "/source", "ext.telepathy"},
		{"device-sent received_at", func(m map[string]any) { m["received_at"] = "2026-10-02T14:00:00Z" }, "/received_at", ""},
		{"m1 without labels", func(m map[string]any) { delete(m, "labels") }, "/labels", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(validM1(t), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			c.mutate(m)
			raw, _ := json.Marshal(m)
			env, err := DecodeEnvelope(raw)
			if err != nil {
				t.Fatalf("DecodeEnvelope: %v", err)
			}
			v, err := s.ValidateEnvelope(env)
			if err != nil {
				t.Fatalf("ValidateEnvelope: %v", err)
			}
			if v == nil {
				t.Fatal("expected a violation")
			}
			if v.Pointer != c.wantPointer {
				t.Errorf("pointer = %q, want %q (expected: %s)", v.Pointer, c.wantPointer, v.Expected)
			}
			if v.Expected == "" {
				t.Error("a violation must describe the violated constraint")
			}
			if c.valueMarker != "" && strings.Contains(v.Expected, c.valueMarker) {
				t.Errorf("the violation echoes the offending value %q: %q", c.valueMarker, v.Expected)
			}
		})
	}
}

func TestSchemaVocabularyComesFromTheFile(t *testing.T) {
	s := load(t)
	req := s.RequiredFields()
	want := []string{
		"collection_mode", "dedup_key", "device_id", "direction", "event_id", "kind",
		"monotonic_offset_ms", "occurred_at", "schema_version", "source", "tenant_id",
		"tool_fingerprint", "user_ref",
	}
	if strings.Join(req, ",") != strings.Join(want, ",") {
		t.Errorf("RequiredFields = %v, want %v", req, want)
	}
	kinds := s.KindRegistry()
	if strings.Join(kinds, ",") != "model_detection,prompt,usage_rollup" {
		t.Errorf("KindRegistry = %v", kinds)
	}
	routes := s.RouteRegistry()
	if len(routes) != 7 {
		t.Errorf("RouteRegistry = %v, want the seven closed routes", routes)
	}
}

func TestPresenceAndRedaction(t *testing.T) {
	raw := validM1(t)
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	presence := env.PresenceOf(load(t).RequiredFields())
	if len(presence.Missing) != 0 {
		t.Errorf("missing = %v, want none on a valid envelope", presence.Missing)
	}
	if !contains(presence.Present, "content_digest") {
		t.Errorf("present = %v, want the content fields named", presence.Present)
	}

	// §7: the quarantine holds the diagnosis; the content and anything derived from it is removed.
	// db/schema.sql is stricter than §7's wording here, and the stricter rule is the one that has to
	// be satisfied, so content_digest goes too.
	red := env.Redacted()
	for _, forbidden := range []string{"content_digest", "content_excerpt", "prompt_text", "content_bytes", "attachments"} {
		if _, present := red[forbidden]; present {
			t.Errorf("redacted envelope still carries %s", forbidden)
		}
	}
	for _, kept := range []string{"event_id", "tenant_id", "device_id", "kind", "collection_mode", "occurred_at", "labels", "policy_decision"} {
		if _, present := red[kept]; !present {
			t.Errorf("redacted envelope dropped %s, which §7 keeps", kept)
		}
	}
	if got := env.RedactedKeys(); len(got) != 1 || got[0] != "content_digest" {
		t.Errorf("RedactedKeys = %v, want [content_digest]", got)
	}
}

func TestDecodeEnvelopeRefusesNonObject(t *testing.T) {
	for _, raw := range []string{`"a string"`, `[1,2,3]`, `null`, `not json`} {
		if _, err := DecodeEnvelope([]byte(raw)); err == nil {
			t.Errorf("DecodeEnvelope(%s) accepted a non-object", raw)
		}
	}
}

func TestDiscoverWalksUp(t *testing.T) {
	dir, err := os.MkdirTemp("", "discover")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Discover(nested); err == nil {
		t.Error("Discover must fail when no schema exists above the directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(dir, "contracts", "event-envelope.schema.json")
	if err := os.WriteFile(want, []byte(`{"$defs":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Discover(nested)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != want {
		t.Errorf("Discover = %q, want %q", got, want)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
