package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

func discoveryFact() Fact {
	return Fact{
		Kind:            protocol.KindDiscovery,
		Route:           protocol.RouteInvScan,
		ToolFingerprint: "app:cursor",
		Person:          &Person{UserRef: "unattributed"},
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

func activityFact() Fact {
	in, out, ms := int64(1200), int64(310), int64(850)
	return Fact{
		Kind:            protocol.KindAgentActivity,
		Route:           protocol.RouteToolOTel,
		ToolFingerprint: "app:claude_code",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		DedupKey:        "sha256:" + strings.Repeat("d", 64),
		FactFields: FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        "claude-sonnet-4-5",
			InputTokens:  &in,
			OutputTokens: &out,
			DurationMS:   &ms,
			Outcome:      protocol.ActivityOutcomeSuccess,
		},
	}
}

// A discovery is minted under the resolved mode, attributed to the fact's person, and spooled with
// the caller's dedup key and the provider's counters.
func TestRecordSpoolsADiscovery(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	if err := p.Record(context.Background(), discoveryFact()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	e := sink.last()
	if e.Kind != protocol.KindDiscovery || e.Route != protocol.RouteInvScan || e.CollectionMode != protocol.ModeM3 ||
		e.DedupKey != "sha256:"+strings.Repeat("c", 64) || e.ToolFingerprint != "app:cursor" {
		t.Fatalf("spool entry = %+v", e)
	}
	var env map[string]any
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env["kind"] != "discovery" || env["collection_mode"] != "m3" || env["user_ref"] != "unattributed" ||
		env["direction"] != "none" || env["app_version"] != "0.48.1" || env["tenant_id"] != "tenant-1" {
		t.Fatalf("envelope = %s", e.Payload)
	}
	got := p.Counters(protocol.RouteInvScan).Cumulative()
	if got[protocol.CounterObserved] != 1 || got[protocol.CounterEmitted] != 1 || got[protocol.CounterErrors] != 0 {
		t.Fatalf("counters = %v", got)
	}
	if p.LastSuccess(protocol.RouteInvScan).IsZero() {
		t.Fatal("a spooled record did not mark the route's last success")
	}

	if err := p.Record(context.Background(), activityFact()); err != nil {
		t.Fatalf("Record agent_activity: %v", err)
	}
	if e := sink.last(); e.Kind != protocol.KindAgentActivity || !bytes.Contains(e.Payload, []byte(`"user_ref":"user-1"`)) {
		t.Fatalf("agent_activity entry = %+v %s", e, e.Payload)
	}
}

// Before enrolment issues an identity nothing is minted, as for a prompt.
func TestRecordRefusesWithoutAnIdentity(t *testing.T) {
	sink := &recordingSink{}
	p, err := NewPipeline(sink, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Record(context.Background(), discoveryFact()); !errors.Is(err, ErrIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrIdentityUnresolved", err)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("the sink holds %d entries, want 0", len(sink.entries))
	}
}

// Record mints only the metadata kinds, and a malformed fact is refused and counted, never spooled.
func TestRecordRefusesOtherKindsAndMalformedFacts(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	for _, kind := range []protocol.Kind{protocol.KindPrompt, protocol.KindUsageRollup, "model_detection"} {
		f := discoveryFact()
		f.Kind = kind
		if err := p.Record(context.Background(), f); err == nil {
			t.Errorf("Record minted kind %q", kind)
		}
	}
	f := discoveryFact()
	f.DiscoveryType = ""
	if err := p.Record(context.Background(), f); err == nil {
		t.Error("a discovery with no discovery_type was recorded")
	}
	f = activityFact()
	f.AppVersion = "1.0"
	if err := p.Record(context.Background(), f); !errors.Is(err, ErrFieldForbidden) {
		t.Errorf("an agent_activity carrying a discovery field: err = %v, want ErrFieldForbidden", err)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("the sink holds %d entries, want 0", len(sink.entries))
	}
	if n := p.Counters(protocol.RouteInvScan).Cumulative()[protocol.CounterErrors]; n != 4 {
		t.Fatalf("errors counter = %d, want 4", n)
	}
}

// spoolLog captures the pipeline's log as JSON lines.
func spoolLog(p *Pipeline) *bytes.Buffer {
	var buf bytes.Buffer
	p.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	return &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := keys(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s fields = %v, want %v", what, got, want)
	}
}

// Every spooled envelope writes one "envelope spooled" line naming what was emitted, with each
// kind's metadata fields when present and nothing else.
func TestSpoolRecordLogFieldSet(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	buf := spoolLog(p)

	d := discoveryFact()
	d.HostApp = "app:vscode"
	d.DiscoveryType, d.DetectionBasis = protocol.DiscoveryTypeIDEExtension, protocol.DetectionBasisExtensionScan
	if err := p.Record(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := p.Record(context.Background(), activityFact()); err != nil {
		t.Fatal(err)
	}
	m := discoveryFact()
	m.DiscoveryType, m.DetectionBasis = protocol.DiscoveryTypeLocalModel, protocol.DetectionBasisModelStore
	m.AppVersion, m.Publisher = "", ""
	m.ModelNames = []string{"llama3.1:8b", "qwen2.5-coder:7b"}
	if err := p.Record(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	c := discoveryFact()
	c.DiscoveryType, c.DetectionBasis = protocol.DiscoveryTypeInferenceConnection, protocol.DetectionBasisFlowMetadata
	c.AppVersion, c.Publisher = "", ""
	c.DestinationHost = "api.openai.com"
	if err := p.Record(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, buf)
	if len(lines) != 4 {
		t.Fatalf("log lines = %d, want one per spooled envelope (4):\n%s", len(lines), buf)
	}
	base := []string{"time", "level", "msg", "event_id", "kind", "source", "tool_fingerprint", "collection_mode", "user_ref"}
	for _, l := range lines {
		if l["msg"] != "envelope spooled" || l["level"] != "INFO" {
			t.Fatalf("log line = %v", l)
		}
	}
	sameKeys(t, "ide_extension discovery", lines[0], append(base, "discovery_type", "app_version", "publisher", "host_app")...)
	sameKeys(t, "agent_activity", lines[1], append(base, "activity_type", "model")...)
	sameKeys(t, "local_model discovery", lines[2], append(base, "discovery_type", "model_names")...)
	sameKeys(t, "inference_connection discovery", lines[3], append(base, "discovery_type", "destination_host")...)
	if lines[0]["event_id"] != "11111111-2222-4333-8444-555555555555" || lines[0]["kind"] != "discovery" ||
		lines[0]["source"] != "inv.scan" || lines[0]["tool_fingerprint"] != "app:cursor" ||
		lines[0]["collection_mode"] != "m0" || lines[0]["user_ref"] != "unattributed" || lines[0]["host_app"] != "app:vscode" {
		t.Errorf("discovery line = %v", lines[0])
	}
	if lines[1]["activity_type"] != "model_request" || lines[1]["model"] != "claude-sonnet-4-5" || lines[1]["user_ref"] != "user-1" {
		t.Errorf("agent_activity line = %v", lines[1])
	}
	if names, _ := lines[2]["model_names"].([]any); len(names) != 2 || names[0] != "llama3.1:8b" {
		t.Errorf("model_names = %v", lines[2]["model_names"])
	}
}

// A prompt's line carries its identity, route, mode and policy decision, and none of what was read:
// no digest, label, excerpt, attachment name or text, even at M2 where the envelope carries them.
func TestSpoolRecordLogCarriesNoContent(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, testBundle("b5", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM2}))
	buf := spoolLog(p)
	const (
		prompt  = "please summarise the attached invoice for ACME"
		excerpt = "card ending 4242 [redacted]"
	)
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "payment_card", Score: 0.99, RuleID: "PAN_LUHN_RULE"}},
		ClassifierVersion: "rel-canary-7",
		Confidence:        protocol.ConfidenceHigh,
		Excerpt:           &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: excerpt},
	}}
	att := dedup.Attachment{Name: "invoice-canary.pdf", SizeBytes: 12, ContentDigest: SHA256Hex([]byte("pdf bytes"))}
	_, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(prompt)),
		Content:         &countingReader{body: []byte(prompt)},
		Enforce:         decided(protocol.Decision{RuleID: "block_credentials", Action: protocol.ActionWarned}),
		Attachments:     []dedup.Attachment{att},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return prompt, []dedup.Attachment{att}, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env struct {
		ContentDigest string                          `json:"content_digest"`
		Labels        []protocol.Label                `json:"labels"`
		Excerpt       *protocol.Excerpt               `json:"content_excerpt"`
		Attachments   []protocol.AttachmentDescriptor `json:"attachments"`
		DedupKey      string                          `json:"dedup_key"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.ContentDigest == "" || len(env.Labels) != 1 || env.Excerpt == nil || len(env.Attachments) != 1 {
		t.Fatalf("the M2 envelope does not carry the content-derived values this test looks for: %s", sink.last().Payload)
	}

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1:\n%s", len(lines), buf)
	}
	sameKeys(t, "prompt", lines[0], "time", "level", "msg", "event_id", "kind", "source", "tool_fingerprint",
		"collection_mode", "user_ref", "policy_decision")
	decision, _ := lines[0]["policy_decision"].(map[string]any)
	sameKeys(t, "policy_decision", decision, "action", "rule_id")
	if decision["action"] != "warned" || decision["rule_id"] != "block_credentials" || lines[0]["collection_mode"] != "m2" {
		t.Errorf("prompt line = %v", lines[0])
	}
	line := buf.String()
	for _, leaked := range []string{
		env.ContentDigest, strings.TrimPrefix(env.ContentDigest, "sha256:"), env.DedupKey,
		"payment_card", "PAN_LUHN_RULE", "rel-canary-7", excerpt, "4242",
		"invoice-canary.pdf", att.ContentDigest, prompt, "ACME", "summarise",
	} {
		if strings.Contains(line, leaked) {
			t.Errorf("the spool record log carries %q:\n%s", leaked, line)
		}
	}
}
