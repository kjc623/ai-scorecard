package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// m1Input is a well-formed M1 prompt envelope input.
func m1Input() EnvelopeInput {
	size := int64(4)
	return EnvelopeInput{
		Identity:          Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:           "11111111-2222-4333-8444-555555555555",
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
}

// Every field the wire struct can emit is decided in the field table, and the table names nothing
// the struct cannot emit, so a field added to one without the other fails here rather than at
// ingest.
func TestEnvelopeFieldTableMatchesTheWireStruct(t *testing.T) {
	names := wireFieldNames()
	if len(names) < 20 {
		t.Fatalf("reflection found %d wire fields; the wire struct or its json tags changed shape", len(names))
	}
	for _, name := range names {
		if _, ok := promptFields[name]; !ok {
			t.Errorf("field %q is not decided in promptFields", name)
		}
	}
	for name := range promptFields {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			t.Errorf("promptFields decides %q, which the wire struct cannot emit", name)
		}
	}
}

func TestEnvelopeRefusesOtherKinds(t *testing.T) {
	for _, kind := range []protocol.Kind{protocol.KindUsageRollup, protocol.KindModelDetection, "invented"} {
		in := m1Input()
		in.Kind = kind
		if _, err := BuildEnvelope(in); err == nil {
			t.Errorf("kind %q was minted; the device emits prompts only", kind)
		}
	}
}

// An M0 prompt with a label is refused, not silently stripped.
func TestEnvelopeM0RefusalIsNotAnOmission(t *testing.T) {
	in := m1Input()
	in.Mode = protocol.ModeM0
	in.ContentDigest, in.ClassifierVersion, in.Confidence = "", "", ""
	in.Labels = []protocol.Label{{Class: "source_code", Score: 0.5}}
	if _, err := BuildEnvelope(in); !errors.Is(err, ErrContentAtM0) {
		t.Fatalf("err = %v, want ErrContentAtM0", err)
	}
	in.Labels = nil
	in.Attachments = []protocol.AttachmentDescriptor{{Name: "a.pdf"}}
	if _, err := BuildEnvelope(in); !errors.Is(err, ErrContentAtM0) {
		t.Fatalf("an M0 record with an attachment name: err = %v, want ErrContentAtM0", err)
	}
}

func TestEnvelopeModeRules(t *testing.T) {
	m1 := m1Input()
	m1.ClassifierVersion = ""
	if _, err := BuildEnvelope(m1); err == nil {
		t.Error("an M1 record without a classifier version was minted")
	}

	m2 := m1Input()
	m2.Mode = protocol.ModeM2
	if _, err := BuildEnvelope(m2); err == nil {
		t.Error("an M2 record without an excerpt was minted")
	}
	m2.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: strings.Repeat("x", protocol.MaxExcerptChars+1)}
	if _, err := BuildEnvelope(m2); err == nil {
		t.Error("an M2 record with an over-cap excerpt was minted")
	}
	m2.Excerpt.Text = "x"
	if _, err := BuildEnvelope(m2); err != nil {
		t.Errorf("a well-formed M2 record was refused: %v", err)
	}

	m3 := m1Input()
	m3.Mode = protocol.ModeM3
	m3.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "x"}
	if _, err := BuildEnvelope(m3); err == nil {
		t.Error("an M3 record carrying an excerpt was minted")
	}
}

// The request kind rides on a prompt at M1 and above and is refused at M0.
func TestEnvelopePromptKindIsM1Plus(t *testing.T) {
	in := m1Input()
	in.PromptKind = protocol.PromptKindUser
	raw, err := BuildEnvelope(in)
	if err != nil {
		t.Fatalf("M1 prompt with prompt_kind: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := string(fields["prompt_kind"]); got != `"user"` {
		t.Fatalf("prompt_kind = %s, want \"user\"", got)
	}
	if _, ok := fields["labels"]; !ok {
		t.Error("an empty label set was omitted; absence and emptiness are different facts at M1+")
	}

	m0 := in
	m0.Mode = protocol.ModeM0
	m0.ContentDigest, m0.Labels, m0.ClassifierVersion, m0.Confidence = "", nil, "", ""
	if _, err := BuildEnvelope(m0); err == nil || !strings.Contains(err.Error(), "prompt_kind") {
		t.Fatalf("M0 prompt with prompt_kind: err = %v, want a refusal naming prompt_kind", err)
	}
}

// The clear account name rides on the envelope when the identity carries one, is absent when it
// does not (a hashed tenant), and an over-long name is refused at mint time.
func TestEnvelopeSubjectNameIsOptionalAndCapped(t *testing.T) {
	in := m1Input()
	in.Identity.SubjectName = "alice@contoso"
	raw, err := BuildEnvelope(in)
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

	in.Identity.SubjectName = ""
	raw, err = BuildEnvelope(in)
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

	in.Identity.SubjectName = strings.Repeat("x", maxSubjectNameChars+1)
	if _, err := BuildEnvelope(in); err == nil {
		t.Error("an over-long subject_name was minted")
	}
}

func TestValidateEnvelopeModeRefusesUnknownFields(t *testing.T) {
	raw, err := BuildEnvelope(m1Input())
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fields["window_start"] = "2026-01-01T00:00:00Z"
	extra, _ := json.Marshal(fields)
	if err := ValidateEnvelopeMode(extra); err == nil {
		t.Fatal("an envelope carrying an undefined field passed validation")
	}
}
