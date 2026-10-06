package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// promptM1 is a valid device submission at m1.
func promptM1() map[string]any {
	return map[string]any{
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
		"content_digest":      sha("a"),
		"labels":              []any{map[string]any{"class": "credential", "score": 0.9}},
		"classifier_version":  "2026.01.0",
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           sha("b"),
	}
}

func promptM0() map[string]any {
	m := promptM1()
	m["collection_mode"] = "m0"
	for _, f := range []string{"confidence", "content_digest", "labels", "classifier_version"} {
		delete(m, f)
	}
	return m
}

func rollup() map[string]any {
	m := promptM0()
	for _, f := range []string{"size_bytes", "policy_decision"} {
		delete(m, f)
	}
	m["kind"], m["direction"], m["collection_mode"], m["source"] = "usage_rollup", "none", "m1", "proc.detect"
	m["window_start"], m["window_end"] = "2026-10-02T13:55:00Z", "2026-10-02T14:00:00Z"
	m["submission_count"], m["bytes_total"] = 3, 4096
	return m
}

func parse(t *testing.T, m map[string]any) *Envelope {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return env
}

func validator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func TestValidEnvelopesPass(t *testing.T) {
	v := validator(t)
	m2 := promptM1()
	m2["collection_mode"] = "m2"
	m2["content_excerpt"] = map[string]any{"kind": "match_span", "text": "4111"}
	detection := rollup()
	for _, f := range []string{"window_start", "window_end", "submission_count", "bytes_total"} {
		delete(detection, f)
	}
	detection["kind"], detection["detection_basis"] = "model_detection", "process_scan"

	for name, m := range map[string]map[string]any{
		"prompt m0": promptM0(), "prompt m1": promptM1(), "prompt m2": m2,
		"usage rollup": rollup(), "model detection": detection,
	} {
		if viol := v.Validate(parse(t, m)); viol != nil {
			t.Errorf("%s: rejected with %+v", name, *viol)
		}
	}
}

func TestViolationsAreLocated(t *testing.T) {
	cases := []struct {
		name     string
		doc      func() map[string]any
		pointer  string
		expected string
		mode     bool
	}{
		{"content at m0", func() map[string]any { m := promptM0(); m["content_digest"] = sha("c"); return m },
			"/content_digest", "absent when kind is prompt and collection_mode is m0", true},
		{"prompt_kind at m0", func() map[string]any { m := promptM0(); m["prompt_kind"] = "user"; return m },
			"/prompt_kind", "absent when kind is prompt and collection_mode is m0", true},
		{"labels missing at m1", func() map[string]any { m := promptM1(); delete(m, "labels"); return m },
			"/labels", `required when kind is prompt and collection_mode is one of ["m1","m2","m3"]`, true},
		{"excerpt missing at m2", func() map[string]any { m := promptM1(); m["collection_mode"] = "m2"; return m },
			"/content_excerpt", "required when kind is prompt and collection_mode is m2", true},
		{"excerpt at m3", func() map[string]any {
			m := promptM1()
			m["collection_mode"] = "m3"
			m["content_excerpt"] = map[string]any{"kind": "match_span", "text": "x"}
			return m
		}, "/content_excerpt", "absent when kind is prompt and collection_mode is m3", true},
		{"size_bytes on a rollup", func() map[string]any { m := rollup(); m["size_bytes"] = 7; return m },
			"/size_bytes", "absent when kind is usage_rollup", false},
		{"direction pinned per kind", func() map[string]any { m := promptM1(); m["direction"] = "none"; return m },
			"/direction", `const "egress" when kind is prompt`, false},
		{"undeclared field", func() map[string]any { m := promptM1(); m["prompt_text"] = "secret"; return m },
			"/prompt_text", "no such field in this schema version", false},
		{"received_at from a device", func() map[string]any { m := promptM1(); m["received_at"] = "2026-10-02T14:00:01Z"; return m },
			"/received_at", "absent", false},
		{"missing core field", func() map[string]any { m := promptM1(); delete(m, "dedup_key"); return m },
			"/dedup_key", "required", false},
		{"event_id not a uuid", func() map[string]any { m := promptM1(); m["event_id"] = "not-a-uuid"; return m },
			"/event_id", "format uuid", false},
		{"occurred_at not a timestamp", func() map[string]any { m := promptM1(); m["occurred_at"] = "yesterday"; return m },
			"/occurred_at", "format date-time", false},
		{"malformed digest", func() map[string]any { m := promptM1(); m["dedup_key"] = "sha256:short"; return m },
			"/dedup_key", "pattern ^sha256:[0-9a-f]{64}$", false},
		{"label score out of range", func() map[string]any {
			m := promptM1()
			m["labels"] = []any{map[string]any{"class": "credential", "score": 2}}
			return m
		}, "/labels/0/score", "maximum 1", false},
		{"route outside the vocabulary", func() map[string]any { m := promptM1(); m["source"] = "ext.telepathy"; return m },
			"/source", "one of", false},
	}
	v := validator(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viol := v.Validate(parse(t, c.doc()))
			if viol == nil {
				t.Fatal("accepted")
			}
			if viol.Pointer != c.pointer || !strings.HasPrefix(viol.Expected, c.expected) || viol.Mode != c.mode {
				t.Errorf("got %+v, want pointer %q, expected %q, mode %v", *viol, c.pointer, c.expected, c.mode)
			}
		})
	}
}

func TestViolationNeverEchoesTheValue(t *testing.T) {
	const secret = "4111-1111-1111-1111 the customer's card"
	m := promptM1()
	m["collection_mode"] = "m2"
	m["content_excerpt"] = map[string]any{"kind": "match_span", "text": strings.Repeat(secret, 100)}
	viol := validator(t).Validate(parse(t, m))
	if viol == nil {
		t.Fatal("an excerpt over maxLength was accepted")
	}
	if strings.Contains(viol.Expected, "4111") || strings.Contains(viol.Pointer, "4111") {
		t.Errorf("the violation echoes the value: %+v", *viol)
	}
	if viol.Pointer != "/content_excerpt/text" || viol.Expected != "maxLength 2048" {
		t.Errorf("got %+v", *viol)
	}
}

func TestMostSpecificViolationIsReportedDeterministically(t *testing.T) {
	m := promptM0()
	m["content_digest"] = sha("c")
	m["zzz_unknown"] = true
	m["user_ref"] = ""
	v := validator(t)
	for range 20 {
		viol := v.Validate(parse(t, m))
		if viol == nil || viol.Pointer != "/content_digest" || !viol.Mode {
			t.Fatalf("got %+v, want the m0 content rule reported first on every run", viol)
		}
	}
}

func TestRedactedKeepsOnlyDeclaredNonContentFields(t *testing.T) {
	m := promptM1()
	m["content_excerpt"] = map[string]any{"kind": "match_span", "text": "secret"}
	m["attachments"] = []any{map[string]any{"name": "a.txt"}}
	m["prompt_text"] = "secret"
	env := parse(t, m)
	red := validator(t).Redacted(env)
	for _, gone := range []string{"content_digest", "content_excerpt", "attachments", "prompt_text"} {
		if _, ok := red[gone]; ok {
			t.Errorf("redacted envelope still carries %s", gone)
		}
	}
	for _, kept := range []string{"event_id", "kind", "collection_mode", "labels", "policy_decision"} {
		if _, ok := red[kept]; !ok {
			t.Errorf("redacted envelope lost %s", kept)
		}
	}
	if present := env.Present(); !strings.Contains(strings.Join(present, ","), "prompt_text") {
		t.Errorf("the presence list must still name the dropped field, got %v", present)
	}
}

func TestParseRefusesWhatIsNotAnObject(t *testing.T) {
	for _, raw := range []string{`[]`, `"x"`, `null`, `{"a":1} trailing`, `{`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) accepted", raw)
		}
	}
}
