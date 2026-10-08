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
	decided := map[string]bool{}
	for name := range commonFields {
		decided[name] = true
	}
	for kind, table := range kindFields {
		for name := range table {
			if commonFields[name] {
				t.Errorf("%s decides %q, which is already a common field", kind, name)
			}
			decided[name] = true
		}
	}
	for _, name := range names {
		if !decided[name] {
			t.Errorf("field %q is not decided in commonFields or kindFields", name)
		}
	}
	for name := range decided {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			t.Errorf("the field tables decide %q, which the wire struct cannot emit", name)
		}
	}
}

func TestEnvelopeRefusesKindsTheDeviceDoesNotMint(t *testing.T) {
	for _, kind := range []protocol.Kind{protocol.KindUsageRollup, "model_detection", "invented"} {
		in := m1Input()
		in.Kind = kind
		if _, err := BuildEnvelope(in); err == nil {
			t.Errorf("kind %q was minted; the device emits prompt, discovery and agent_activity only", kind)
		}
	}
}

// discoveryInput is a well-formed discovery envelope input.
func discoveryInput() EnvelopeInput {
	return EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "unattributed"},
		EventID:         "11111111-2222-4333-8444-555555555555",
		Kind:            protocol.KindDiscovery,
		Route:           protocol.RouteInvScan,
		Mode:            protocol.ModeM1,
		ToolFingerprint: "app:cursor",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		DedupKey:        "sha256:" + strings.Repeat("c", 64),
		FactFields: FactFields{
			DiscoveryType:  protocol.DiscoveryTypeAppInstalled,
			DetectionBasis: protocol.DetectionBasisInstalledScan,
			AppVersion:     "0.48.1",
			Publisher:      "Anysphere, Inc.",
		},
	}
}

// activityInput is a well-formed agent_activity envelope input.
func activityInput() EnvelopeInput {
	tokens, duration := int64(1200), int64(850)
	return EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:         "11111111-2222-4333-8444-555555555555",
		Kind:            protocol.KindAgentActivity,
		Route:           protocol.RouteToolOTel,
		Mode:            protocol.ModeM0,
		ToolFingerprint: "app:claude_code",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		DedupKey:        "sha256:" + strings.Repeat("d", 64),
		FactFields: FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        "claude-sonnet-4-5",
			InputTokens:  &tokens,
			OutputTokens: &tokens,
			DurationMS:   &duration,
			Outcome:      protocol.ActivityOutcomeSuccess,
		},
	}
}

func envelopeFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return fields
}

// A discovery and an agent_activity record are minted with direction none and their own fields,
// at any mode, and carry nothing a prompt would.
func TestEnvelopeBuildsDiscoveryAndAgentActivity(t *testing.T) {
	for _, mode := range []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM3} {
		in := discoveryInput()
		in.Mode = mode
		raw, err := BuildEnvelope(in)
		if err != nil {
			t.Fatalf("discovery at %s: %v", mode, err)
		}
		f := envelopeFields(t, raw)
		if string(f["direction"]) != `"none"` || string(f["discovery_type"]) != `"app_installed"` ||
			string(f["detection_basis"]) != `"installed_scan"` || string(f["publisher"]) != `"Anysphere, Inc."` {
			t.Fatalf("discovery envelope = %s", raw)
		}
		for _, absent := range []string{"size_bytes", "policy_decision", "labels", "content_digest", "activity_type"} {
			if _, ok := f[absent]; ok {
				t.Errorf("discovery envelope carries %q", absent)
			}
		}

		a := activityInput()
		a.Mode = mode
		raw, err = BuildEnvelope(a)
		if err != nil {
			t.Fatalf("agent_activity at %s: %v", mode, err)
		}
		f = envelopeFields(t, raw)
		if string(f["direction"]) != `"none"` || string(f["activity_type"]) != `"model_request"` ||
			string(f["input_tokens"]) != "1200" || string(f["outcome"]) != `"success"` {
			t.Fatalf("agent_activity envelope = %s", raw)
		}
		for _, absent := range []string{"policy_decision", "labels", "content_digest", "discovery_type", "detection_basis"} {
			if _, ok := f[absent]; ok {
				t.Errorf("agent_activity envelope carries %q", absent)
			}
		}
	}
}

// contentFields sets, one at a time, every field a discovery or agent_activity record never
// carries.
var contentFields = map[string]func(*EnvelopeInput){
	"content_digest":     func(in *EnvelopeInput) { in.ContentDigest = "sha256:" + strings.Repeat("b", 64) },
	"labels":             func(in *EnvelopeInput) { in.Labels = []protocol.Label{{Class: "credential", Score: 1}} },
	"empty labels":       func(in *EnvelopeInput) { in.Labels = []protocol.Label{} },
	"classifier_version": func(in *EnvelopeInput) { in.ClassifierVersion = "rel-1" },
	"confidence":         func(in *EnvelopeInput) { in.Confidence = protocol.ConfidenceHigh },
	"content_excerpt":    func(in *EnvelopeInput) { in.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "x"} },
	"attachments":        func(in *EnvelopeInput) { in.Attachments = []protocol.AttachmentDescriptor{{Name: "a.pdf"}} },
	"prompt_kind":        func(in *EnvelopeInput) { in.PromptKind = protocol.PromptKindUser },
	"policy_decision": func(in *EnvelopeInput) {
		in.Decision = &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged}
	},
}

var discoveryOnly = map[string]func(*EnvelopeInput){
	"discovery_type":   func(in *EnvelopeInput) { in.DiscoveryType = protocol.DiscoveryTypeAppInstalled },
	"detection_basis":  func(in *EnvelopeInput) { in.DetectionBasis = protocol.DetectionBasisInstalledScan },
	"app_version":      func(in *EnvelopeInput) { in.AppVersion = "1.0" },
	"publisher":        func(in *EnvelopeInput) { in.Publisher = "Contoso" },
	"host_app":         func(in *EnvelopeInput) { in.HostApp = "app:vscode" },
	"destination_host": func(in *EnvelopeInput) { in.DestinationHost = "api.openai.com" },
	"model_names":      func(in *EnvelopeInput) { in.ModelNames = []string{"llama3"} },
}

var activityOnly = map[string]func(*EnvelopeInput){
	"activity_type": func(in *EnvelopeInput) { in.ActivityType = protocol.ActivityTypeToolCall },
	"model":         func(in *EnvelopeInput) { in.Model = "gpt-5" },
	"input_tokens":  func(in *EnvelopeInput) { n := int64(1); in.InputTokens = &n },
	"output_tokens": func(in *EnvelopeInput) { n := int64(1); in.OutputTokens = &n },
	"duration_ms":   func(in *EnvelopeInput) { n := int64(1); in.DurationMS = &n },
	"tool_name":     func(in *EnvelopeInput) { in.ToolName = "Bash" },
	"outcome":       func(in *EnvelopeInput) { in.Outcome = protocol.ActivityOutcomeDenied },
}

// Each kind refuses, at mint time, every field its kind forbids: content and a policy decision on
// the metadata kinds, the other metadata kind's fields, and either metadata kind's fields on a
// prompt. The contract never sees them.
func TestEnvelopeEachKindRefusesItsForbiddenFields(t *testing.T) {
	refuses := func(t *testing.T, base func() EnvelopeInput, forbidden map[string]func(*EnvelopeInput)) {
		t.Helper()
		for name, set := range forbidden {
			in := base()
			set(&in)
			if _, err := BuildEnvelope(in); !errors.Is(err, ErrFieldForbidden) {
				t.Errorf("%s with %s: err = %v, want ErrFieldForbidden", in.Kind, name, err)
			}
		}
	}
	refuses(t, discoveryInput, contentFields)
	refuses(t, discoveryInput, activityOnly)
	refuses(t, discoveryInput, map[string]func(*EnvelopeInput){
		"size_bytes": func(in *EnvelopeInput) { n := int64(1); in.SizeBytes = &n },
	})
	refuses(t, activityInput, contentFields)
	refuses(t, activityInput, discoveryOnly)
	refuses(t, m1Input, discoveryOnly)
	refuses(t, m1Input, activityOnly)
}

// The metadata kinds require their own discriminators and hold their values to the contract's
// closed sets, caps and patterns.
func TestEnvelopeMetadataKindsCheckTheirValues(t *testing.T) {
	for name, set := range map[string]func(*EnvelopeInput){
		"no discovery_type":       func(in *EnvelopeInput) { in.DiscoveryType = "" },
		"no detection_basis":      func(in *EnvelopeInput) { in.DetectionBasis = "" },
		"unknown discovery_type":  func(in *EnvelopeInput) { in.DiscoveryType = "process_list" },
		"over-long app_version":   func(in *EnvelopeInput) { in.AppVersion = strings.Repeat("1", maxAppVersionChars+1) },
		"upper-case host":         func(in *EnvelopeInput) { in.DestinationHost = "API.openai.com" },
		"a URL for a host":        func(in *EnvelopeInput) { in.DestinationHost = "https://api.openai.com/v1" },
		"too many model names":    func(in *EnvelopeInput) { in.ModelNames = make([]string, maxModelNames+1) },
		"an empty model name":     func(in *EnvelopeInput) { in.ModelNames = []string{"llama3", ""} },
		"an over-long model name": func(in *EnvelopeInput) { in.ModelNames = []string{strings.Repeat("m", maxModelNameChars+1)} },
	} {
		in := discoveryInput()
		set(&in)
		if _, err := BuildEnvelope(in); err == nil {
			t.Errorf("a discovery with %s was minted", name)
		}
	}
	for name, set := range map[string]func(*EnvelopeInput){
		"no activity_type":      func(in *EnvelopeInput) { in.ActivityType = "" },
		"unknown outcome":       func(in *EnvelopeInput) { in.Outcome = "accepted" },
		"negative input_tokens": func(in *EnvelopeInput) { n := int64(-1); in.InputTokens = &n },
		"negative duration_ms":  func(in *EnvelopeInput) { n := int64(-1); in.DurationMS = &n },
		"over-long tool_name":   func(in *EnvelopeInput) { in.ToolName = strings.Repeat("t", maxToolNameChars+1) },
	} {
		in := activityInput()
		set(&in)
		if _, err := BuildEnvelope(in); err == nil {
			t.Errorf("an agent_activity with %s was minted", name)
		}
	}
	host := discoveryInput()
	host.DiscoveryType, host.DetectionBasis = protocol.DiscoveryTypeInferenceConnection, protocol.DetectionBasisFlowMetadata
	host.DestinationHost = "api.openai.com"
	if _, err := BuildEnvelope(host); err != nil {
		t.Errorf("a lower-case destination host was refused: %v", err)
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

// The second pass holds the metadata kinds to their own field tables and direction: a label, a
// prompt's direction or a missing discriminator is refused in the bytes too.
func TestValidateEnvelopeModeAppliesEachKindsTable(t *testing.T) {
	raw, err := BuildEnvelope(discoveryInput())
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	for name, edit := range map[string]func(map[string]any){
		"labels":            func(f map[string]any) { f["labels"] = []any{} },
		"size_bytes":        func(f map[string]any) { f["size_bytes"] = 4 },
		"activity_type":     func(f map[string]any) { f["activity_type"] = "tool_call" },
		"egress direction":  func(f map[string]any) { f["direction"] = "egress" },
		"no discovery_type": func(f map[string]any) { delete(f, "discovery_type") },
	} {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		edit(fields)
		edited, _ := json.Marshal(fields)
		if err := ValidateEnvelopeMode(edited); err == nil {
			t.Errorf("a discovery with %s passed validation", name)
		}
	}
}
