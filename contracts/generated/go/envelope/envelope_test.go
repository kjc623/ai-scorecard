package envelope

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// fixture returns a valid device submission for the named variant as a JSON object.
func fixture(variant string) map[string]any {
	core := map[string]any{
		"schema_version":      "1.0",
		"event_id":            "11111111-1111-4111-8111-111111111111",
		"tenant_id":           "22222222-2222-4222-8222-222222222222",
		"device_id":           "33333333-3333-4333-8333-333333333333",
		"user_ref":            "user-1",
		"tool_fingerprint":    "fingerprint-1",
		"occurred_at":         "2026-10-02T13:00:00Z",
		"monotonic_offset_ms": 12,
		"source":              "ext.web_request",
		"dedup_key":           sha("b"),
	}
	set := func(fields map[string]any) map[string]any {
		for k, v := range fields {
			core[k] = v
		}
		return core
	}
	prompt := map[string]any{
		"direction":       "egress",
		"kind":            "prompt",
		"size_bytes":      42,
		"policy_decision": map[string]any{"rule_id": "R-1", "action": "logged", "decided_locally": true},
	}
	classified := map[string]any{
		"confidence":         "high",
		"content_digest":     sha("a"),
		"labels":             []any{map[string]any{"class": "credential", "score": 0.9}},
		"classifier_version": "classifier-1",
	}
	switch variant {
	case "DevicePromptM0":
		set(prompt)
		return set(map[string]any{"collection_mode": "m0"})
	case "DevicePromptM1", "DevicePromptM3":
		set(prompt)
		set(classified)
		return set(map[string]any{"collection_mode": strings.ToLower(variant[len(variant)-2:])})
	case "DevicePromptM2":
		set(prompt)
		set(classified)
		return set(map[string]any{"collection_mode": "m2", "content_excerpt": map[string]any{"kind": "match_span", "text": "abc"}})
	case "DeviceUsageRollup":
		return set(map[string]any{
			"direction": "none", "kind": "usage_rollup", "collection_mode": "m1", "source": "proc.detect",
			"window_start": "2026-10-02T12:55:00Z", "window_end": "2026-10-02T13:00:00Z",
			"submission_count": 3, "bytes_total": 4096,
		})
	case "DeviceModelDetection":
		return set(map[string]any{
			"direction": "none", "kind": "model_detection", "collection_mode": "m1", "source": "proc.detect",
			"detection_basis": "process_scan",
		})
	}
	panic("unknown variant " + variant)
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestSchemaIsTheContractDocument(t *testing.T) {
	var doc struct {
		ID   string `json:"$id"`
		Defs struct {
			EnvelopeCore struct {
				Properties struct {
					SchemaVersion struct {
						Const string `json:"const"`
					} `json:"schema_version"`
					Kind struct {
						Enum []Kind `json:"enum"`
					} `json:"kind"`
				} `json:"properties"`
			} `json:"envelopeCore"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(Schema), &doc); err != nil {
		t.Fatalf("the embedded schema is not JSON: %v", err)
	}
	if doc.ID != SchemaID {
		t.Errorf("embedded $id = %q, SchemaID = %q", doc.ID, SchemaID)
	}
	if got := doc.Defs.EnvelopeCore.Properties.SchemaVersion.Const; got != SchemaVersion {
		t.Errorf("embedded schema_version const = %q, SchemaVersion = %q", got, SchemaVersion)
	}
	if got := doc.Defs.EnvelopeCore.Properties.Kind.Enum; !slices.Equal(got, AllKinds()) {
		t.Errorf("embedded kind enum = %v, AllKinds() = %v", got, AllKinds())
	}
}

func TestDecodeDispatchesEveryVariant(t *testing.T) {
	for _, variant := range []string{
		"DevicePromptM0", "DevicePromptM1", "DevicePromptM2", "DevicePromptM3",
		"DeviceUsageRollup", "DeviceModelDetection",
	} {
		t.Run(variant, func(t *testing.T) {
			sub, err := DecodeDeviceSubmission(encode(t, fixture(variant)))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := strings.TrimPrefix(fmt.Sprintf("%T", sub), "*envelope."); got != variant {
				t.Errorf("decoded %s, want %s", got, variant)
			}
			if sub.Core().EventID != "11111111-1111-4111-8111-111111111111" {
				t.Errorf("core not populated: %+v", sub.Core())
			}
		})
	}
}

func TestDecodeKeepsOptionalPresence(t *testing.T) {
	m := fixture("DevicePromptM1")
	m["attachments"] = []any{}
	sub, err := DecodeDeviceSubmission(encode(t, m))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := sub.(*DevicePromptM1)
	if p.Attachments == nil || len(*p.Attachments) != 0 {
		t.Errorf("an empty attachments array must decode as present and empty, got %v", p.Attachments)
	}
	if p.ContentDigest != sha("a") {
		t.Errorf("content_digest = %q", p.ContentDigest)
	}
}

func TestDecodeRefusesWhatNoVariantDeclares(t *testing.T) {
	cases := []struct {
		name string
		doc  func() any
		want string
	}{
		{"unknown kind", func() any { m := fixture("DevicePromptM1"); m["kind"] = "prompt_v2"; return m }, "closed registry"},
		{"unknown mode", func() any { m := fixture("DevicePromptM1"); m["collection_mode"] = "m9"; return m }, "closed set"},
		{"undeclared field", func() any { m := fixture("DevicePromptM1"); m["bogus_field"] = 1; return m }, "bogus_field"},
		{"field the kind must not carry", func() any { m := fixture("DevicePromptM1"); m["window_start"] = "2026-10-02T12:55:00Z"; return m }, "window_start"},
		{"size_bytes on a rollup", func() any { m := fixture("DeviceUsageRollup"); m["size_bytes"] = 7; return m }, "size_bytes"},
		{"received_at from a device", func() any { m := fixture("DevicePromptM1"); m["received_at"] = "2026-10-02T13:00:01Z"; return m }, "received_at"},
		{"null", func() any { return nil }, "closed registry"},
		{"array", func() any { return []any{} }, "not a JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeDeviceSubmission(encode(t, c.doc()))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a refusal mentioning %q", err, c.want)
			}
		})
	}
}

func TestEnumsAreClosed(t *testing.T) {
	for _, k := range AllKinds() {
		if !k.Valid() {
			t.Errorf("%q is in AllKinds but not Valid", k)
		}
	}
	if Kind("prompt_v2").Valid() || CollectionMode("m9").Valid() || Route("ext.telepathy").Valid() {
		t.Error("a value outside the closed set reported Valid")
	}
}
