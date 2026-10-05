package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// TestEnvelope_ADR0018_EveryKindDecidesEveryField is the structural half of the kind-consistency
// rule ADR 0018 records: a kind's branch is exhaustive over the fields the other kinds own, and
// the generator is the wrong place to absorb the answer. It enumerates the mirror struct by
// reflection, so *adding a field to envelopeWire without deciding which kinds may carry it*
// fails here rather than at ingest.
func TestEnvelope_ADR0018_EveryKindDecidesEveryField(t *testing.T) {
	names := wireFieldNames()
	if len(names) < 20 {
		t.Fatalf("reflection found %d wire fields; the mirror struct or its json tags changed shape", len(names))
	}
	for _, kind := range []protocol.Kind{protocol.KindPrompt, protocol.KindUsageRollup, protocol.KindModelDetection} {
		policy, ok := kindFieldPolicy[kind]
		if !ok {
			t.Fatalf("kind %s has no field policy", kind)
		}
		for _, name := range names {
			if _, decided := policy[name]; !decided {
				t.Errorf("ADR 0018: field %q is not decided for kind %s; every kind's branch must decide every field", name, kind)
			}
		}
		for name := range policy {
			found := false
			for _, n := range names {
				if n == name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("kind %s policy decides field %q, which the mirror struct cannot emit", kind, name)
			}
		}
	}
}

// TestEnvelope_ADR0018_ForbiddenFieldsAreRefusedAtMintTime walks the table: for each kind, each
// field the policy forbids is populated and the mint must refuse it. The three window fields on
// `prompt` and on `model_detection` are the live case ADR 0018 was written for.
func TestEnvelope_ADR0018_ForbiddenFieldsAreRefusedAtMintTime(t *testing.T) {
	size := int64(10)
	count := 3
	total := int64(300)
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(24 * time.Hour)

	base := func(kind protocol.Kind, mode protocol.CollectionMode) EnvelopeInput {
		return EnvelopeInput{
			Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
			EventID:         "11111111-2222-4333-8444-555555555555",
			Kind:            kind,
			Route:           protocol.RouteProxyLoopback,
			Mode:            mode,
			ToolFingerprint: "tool",
			OccurredAt:      start,
			DedupKey:        "sha256:" + strings.Repeat("a", 64),
		}
	}

	// Every setter corresponds to one field policy decision. A field forbidden for the kind must
	// make BuildEnvelope fail; a required or optional field must not (given the rest of the
	// record is well formed for that kind).
	setters := map[string]func(in *EnvelopeInput){
		"size_bytes":         func(in *EnvelopeInput) { in.SizeBytes = &size },
		"policy_decision":    func(in *EnvelopeInput) { in.Decision = &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged} },
		"content_digest":     func(in *EnvelopeInput) { in.ContentDigest = "sha256:" + strings.Repeat("b", 64) },
		"labels":             func(in *EnvelopeInput) { in.Labels = []protocol.Label{{Class: "source_code", Score: 0.5}} },
		"classifier_version": func(in *EnvelopeInput) { in.ClassifierVersion = "rel-1" },
		"confidence":         func(in *EnvelopeInput) { in.Confidence = protocol.ConfidenceHigh },
		"content_excerpt":    func(in *EnvelopeInput) { in.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "x"} },
		"attachments": func(in *EnvelopeInput) {
			in.Attachments = []protocol.AttachmentDescriptor{{Name: "a.pdf", SizeBytes: 1}}
		},
		"window_start":     func(in *EnvelopeInput) { in.WindowStart = &start },
		"window_end":       func(in *EnvelopeInput) { in.WindowEnd = &end },
		"submission_count": func(in *EnvelopeInput) { in.SubmissionCount = &count },
		"bytes_total":      func(in *EnvelopeInput) { in.BytesTotal = &total },
		"detection_basis":  func(in *EnvelopeInput) { in.DetectionBasis = "process_scan" },
	}

	// Modes are chosen so a *required* field is satisfiable: M1 for a prompt, and the kind's own
	// mode for the non-prompt kinds (where the mode still has to be in the closed set).
	kinds := []struct {
		kind protocol.Kind
		mode protocol.CollectionMode
	}{
		{protocol.KindPrompt, protocol.ModeM1},
		{protocol.KindUsageRollup, protocol.ModeM1},
		{protocol.KindModelDetection, protocol.ModeM1},
	}

	for _, k := range kinds {
		policy := kindFieldPolicy[k.kind]
		for _, field := range wireFieldNames() {
			setter, hasSetter := setters[field]
			if !hasSetter {
				continue // core identity fields are always required and always set
			}
			rule := policy[field]
			// Provide the fields the kind requires so the only possible failure is the field
			// under test.
			in := base(k.kind, k.mode)
			required := map[string]func(*EnvelopeInput){
				"size_bytes":         func(i *EnvelopeInput) { i.SizeBytes = &size },
				"policy_decision":    func(i *EnvelopeInput) { i.Decision = &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged} },
				"content_digest":     func(i *EnvelopeInput) { i.ContentDigest = "sha256:" + strings.Repeat("c", 64) },
				"labels":             func(i *EnvelopeInput) { i.Labels = []protocol.Label{} },
				"classifier_version": func(i *EnvelopeInput) { i.ClassifierVersion = "rel-1" },
				"confidence":         func(i *EnvelopeInput) { i.Confidence = protocol.ConfidenceHigh },
				"window_start":       func(i *EnvelopeInput) { i.WindowStart = &start },
				"window_end":         func(i *EnvelopeInput) { i.WindowEnd = &end },
				"submission_count":   func(i *EnvelopeInput) { i.SubmissionCount = &count },
				"bytes_total":        func(i *EnvelopeInput) { i.BytesTotal = &total },
				"detection_basis":    func(i *EnvelopeInput) { i.DetectionBasis = "process_scan" },
			}
			for name, rule := range policy {
				if rule != fieldRequired {
					continue
				}
				if f, ok := required[name]; ok {
					f(&in)
				}
			}
			// The prompt branch is refined by mode: M1 and above also require the classifier's
			// output, so the test must supply it before setting the field under test.
			if k.kind == protocol.KindPrompt && k.mode.ReadsContent() {
				required["content_digest"](&in)
				required["labels"](&in)
				required["classifier_version"](&in)
				required["confidence"](&in)
			}
			setter(&in)

			_, err := BuildEnvelope(in)
			switch rule {
			case fieldForbidden:
				if err == nil {
					t.Errorf("kind %s accepted forbidden field %q; the refusal must be at mint time, not at ingest", k.kind, field)
				}
			default:
				if err != nil && !strings.Contains(err.Error(), "forbidden field") {
					t.Errorf("kind %s rejected field %q (rule %v), which its policy permits: %v", k.kind, field, int(rule), err)
				}
			}
		}
	}
}

// The ADR 0018 case in one assertion: a model_detection carrying a window must be refused.
func TestEnvelope_ADR0018_ModelDetectionRefusesWindowFields(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(time.Hour)
	in := EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:         "e",
		Kind:            protocol.KindModelDetection,
		Route:           protocol.RouteProcDetect,
		Mode:            protocol.ModeM1,
		ToolFingerprint: "tool",
		OccurredAt:      start,
		DedupKey:        "sha256:" + strings.Repeat("a", 64),
		DetectionBasis:  "process_scan",
		WindowStart:     &start,
		WindowEnd:       &end,
	}
	if _, err := BuildEnvelope(in); err == nil {
		t.Fatal("ADR 0018: a model_detection carrying window_start/window_end was minted")
	}

	// And the rollup still works with the same fields, so the refusal is kind-specific rather
	// than a blanket ban.
	count := 1
	total := int64(10)
	in.Kind = protocol.KindUsageRollup
	in.Route = protocol.RouteProcDetect
	in.DetectionBasis = ""
	in.SubmissionCount = &count
	in.BytesTotal = &total
	if _, err := BuildEnvelope(in); err != nil {
		t.Fatalf("usage_rollup with a window was refused: %v", err)
	}
}

// A prompt carrying a window is refused too: the window fields are the rollup's, and a prompt
// with submission_count is a caller that confused two kinds.
func TestEnvelope_ADR0018_PromptRefusesWindowFields(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	size := int64(4)
	count := 2
	in := EnvelopeInput{
		Identity:          Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:           "e",
		Kind:              protocol.KindPrompt,
		Route:             protocol.RouteProxyTLS,
		Mode:              protocol.ModeM1,
		ToolFingerprint:   "tool",
		OccurredAt:        start,
		DedupKey:          "sha256:" + strings.Repeat("a", 64),
		SizeBytes:         &size,
		Decision:          &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		ContentDigest:     "sha256:" + strings.Repeat("b", 64),
		Labels:            []protocol.Label{},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
		SubmissionCount:   &count,
		WindowStart:       &start,
	}
	_, err := BuildEnvelope(in)
	if err == nil {
		t.Fatal("a prompt carrying submission_count/window_start was minted")
	}
	if !strings.Contains(err.Error(), "submission_count") && !strings.Contains(err.Error(), "window_start") {
		t.Fatalf("refusal named neither offending field: %v", err)
	}
}

// An M0 prompt with a digest is the other half of the same rule: the refusal is a refusal, not a
// silently omitted field.
func TestEnvelope_M0RefusalIsNotAnOmission(t *testing.T) {
	size := int64(4)
	in := EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:         "e",
		Kind:            protocol.KindPrompt,
		Route:           protocol.RouteProxyTLS,
		Mode:            protocol.ModeM0,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		DedupKey:        "sha256:" + strings.Repeat("a", 64),
		SizeBytes:       &size,
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Labels:          []protocol.Label{{Class: "source_code", Score: 0.5}},
	}
	_, err := BuildEnvelope(in)
	if !errors.Is(err, ErrContentAtM0) {
		t.Fatalf("err = %v, want ErrContentAtM0", err)
	}
}

// The minted JSON must not carry a key the contract does not define, because the contract is
// closed with additionalProperties:false.
func TestEnvelope_NoUndecidedJSONKeys(t *testing.T) {
	size := int64(4)
	in := EnvelopeInput{
		Identity:          Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:           "e",
		Kind:              protocol.KindPrompt,
		Route:             protocol.RouteProxyTLS,
		Mode:              protocol.ModeM1,
		ToolFingerprint:   "tool",
		OccurredAt:        time.Unix(1_700_000_000, 0),
		DedupKey:          "sha256:" + strings.Repeat("a", 64),
		SizeBytes:         &size,
		Decision:          &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		ContentDigest:     "sha256:" + strings.Repeat("b", 64),
		Labels:            []protocol.Label{},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}
	raw, err := BuildEnvelope(in)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	policy := kindFieldPolicy[protocol.KindPrompt]
	for name := range fields {
		if _, ok := policy[name]; !ok {
			t.Errorf("minted envelope carries %q, which no kind policy defines", name)
		}
	}
	if _, ok := fields["labels"]; !ok {
		t.Error("an empty label set was omitted; absence and emptiness are different facts at M1+")
	}
}

// TestEnvelope_SubjectNameIsOptionalAndGated covers ADR 0021 at the device boundary: the clear
// account name rides on the envelope when set, is absent when the identity carries none (a 'hashed'
// tenant), and an over-long name is refused at mint time rather than sent to be rejected by ingest.
func TestEnvelope_SubjectNameIsOptionalAndGated(t *testing.T) {
	size := int64(10)
	start := time.Unix(1_700_000_000, 0).UTC()
	base := EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u", SubjectName: "alice@contoso"},
		EventID:         "11111111-2222-4333-8444-555555555555",
		Kind:            protocol.KindPrompt,
		Route:           protocol.RouteProxyLoopback,
		Mode:            protocol.ModeM0,
		ToolFingerprint: "tool",
		OccurredAt:      start,
		SizeBytes:       &size,
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		DedupKey:        "sha256:" + strings.Repeat("a", 64),
	}

	raw, err := BuildEnvelope(base)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := string(fields["subject_name"]); got != `"alice@contoso"` {
		t.Fatalf("subject_name = %s, want the identity's clear name", got)
	}

	// No name in the identity: the field is omitted, never emitted empty (absence is a fact).
	hashed := base
	hashed.Identity = Identity{TenantID: "t", DeviceID: "d", UserRef: "u"}
	raw, err = BuildEnvelope(hashed)
	if err != nil {
		t.Fatalf("BuildEnvelope (no name): %v", err)
	}
	fields = map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["subject_name"]; ok {
		t.Error("a record with no clear name carried subject_name")
	}

	// The contract caps the name at 200 characters; the device refuses rather than emitting it.
	tooLong := base
	tooLong.SubjectName = strings.Repeat("x", 201)
	if _, err := BuildEnvelope(tooLong); err == nil {
		t.Error("an over-long subject_name was minted; ingest would reject it")
	}
}
