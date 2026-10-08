package copilot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// canary is the prompt text in the fixtures.
const canary = "SAC-CANARY-7f3a prompt text"

// fixtures decodes every Copilot fixture whose name matches pattern, keyed by its path under
// testdata. Unknown OTLP fields are refused, so a misspelt field fails here.
func fixtures[M proto.Message](t *testing.T, pattern string, newMsg func() M) map[string]M {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "testdata", "copilot", "*", pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Copilot fixtures match %s", pattern)
	}
	out := map[string]M{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := newMsg()
		if err := protojson.Unmarshal(body, m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		rel, _ := filepath.Rel(filepath.Join("..", "testdata"), f)
		out[filepath.ToSlash(rel)] = m
	}
	return out
}

// traceFixtures decodes the trace fixtures with their ids as the receiver hands them on: OTLP/JSON
// spells ids in hex, which protojson reads as base64.
func traceFixtures(t *testing.T) map[string]*coltracepb.ExportTraceServiceRequest {
	reqs := fixtures(t, "traces-*.json", func() *coltracepb.ExportTraceServiceRequest { return &coltracepb.ExportTraceServiceRequest{} })
	unhex := func(id []byte) []byte {
		if len(id) == 0 {
			return id
		}
		b, err := hex.DecodeString(base64.StdEncoding.EncodeToString(id))
		if err != nil {
			t.Fatalf("id %x is not hex in the fixture", id)
		}
		return b
	}
	for _, req := range reqs {
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					s.TraceId, s.SpanId, s.ParentSpanId = unhex(s.TraceId), unhex(s.SpanId), unhex(s.ParentSpanId)
				}
			}
		}
	}
	return reqs
}

func logFixtures(t *testing.T) map[string]*collogspb.ExportLogsServiceRequest {
	return fixtures(t, "logs-*.json", func() *collogspb.ExportLogsServiceRequest { return &collogspb.ExportLogsServiceRequest{} })
}

func traceFixture(t *testing.T, name string) *coltracepb.ExportTraceServiceRequest {
	t.Helper()
	req, ok := traceFixtures(t)[name]
	if !ok {
		t.Fatalf("no fixture %s", name)
	}
	return req
}

// fingerprintOf is the app a fixture's folder is for.
func fingerprintOf(file string) string {
	if strings.HasPrefix(file, "copilot/cli-") {
		return FingerprintCLI
	}
	return FingerprintVSCode
}

func keyList(kvs []*commonpb.KeyValue) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, kv.GetKey())
	}
	return out
}

// logEventName is a log record's event name: the event.name attribute, else the OTLP field.
func logEventName(lr *logspb.LogRecord) string {
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == "event.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return lr.GetEventName()
}

// raw is a span attribute as the fixture spells it, read without the normalizer's helpers.
func raw(s *tracepb.Span, key string) (*commonpb.AnyValue, bool) {
	for _, kv := range s.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue(), true
		}
	}
	return nil, false
}

func rawStr(s *tracepb.Span, key string) string {
	v, _ := raw(s, key)
	return v.GetStringValue()
}

// --- the pipeline the normalizer writes through ---

type sink struct {
	mu      sync.Mutex
	entries []protocol.Entry
}

func (s *sink) Append(e protocol.Entry) (protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = uint64(len(s.entries) + 1)
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *sink) Stats() protocol.SpoolStats { return protocol.SpoolStats{} }

func (s *sink) all() []protocol.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Entry(nil), s.entries...)
}

type classifier struct {
	mu  sync.Mutex
	got [][]byte
}

func (c *classifier) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), req.Content...))
	c.mu.Unlock()
	return protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "credential", Score: 0.9, RuleID: "test.rule"}},
		ClassifierVersion: "test-1",
		Confidence:        protocol.ConfidenceHigh,
	}, nil
}

func (c *classifier) requests() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

// readCounter forwards to the real pipeline and counts every call of the content readers it hands on.
type readCounter struct {
	*core.Pipeline
	reads atomic.Int64
}

type countedReader struct {
	r     core.ContentReader
	reads *atomic.Int64
}

func (c countedReader) Read(ctx context.Context) ([]byte, error) {
	c.reads.Add(1)
	return c.r.Read(ctx)
}

func (p *readCounter) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	if obs.Content != nil {
		obs.Content = countedReader{r: obs.Content, reads: &p.reads}
	}
	return p.Pipeline.Process(ctx, obs)
}

func uuid(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type harness struct {
	pipe     *readCounter
	sink     *sink
	cls      *classifier
	counters *core.CounterSet
	norm     *Normalizer
	from     otlp.Sender
	ident    core.Identity
}

// newHarness is a pipeline whose bundle resolves both Copilot apps to mode, enrolled to a random
// tenant.
func newHarness(t *testing.T, mode protocol.CollectionMode) *harness {
	t.Helper()
	s := &sink{}
	clock := func() time.Time { return time.Unix(1_791_460_900, 0) }
	p, err := core.NewPipeline(s, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := core.Identity{TenantID: uuid(t), DeviceID: uuid(t), UserRef: "console-user"}
	p.SetIdentity(id)
	p.Bundles = func() *policy.Bundle {
		return &policy.Bundle{
			Version:       "1",
			EffectiveAt:   time.Unix(1_791_000_000, 0),
			Actor:         "tenant-admin@example.invalid",
			TenantDefault: protocol.ModeM3,
			ToolModes:     map[string]protocol.CollectionMode{FingerprintVSCode: mode, FingerprintCLI: mode},
		}
	}
	cls := &classifier{}
	p.Classifier = cls
	h := &harness{
		pipe:     &readCounter{Pipeline: p},
		sink:     s,
		cls:      cls,
		counters: core.NewCounterSet(clock()),
		ident:    id,
		from: otlp.Sender{
			PID: 4242, Image: `C:\Users\someone\AppData\Local\Programs\Microsoft VS Code\Code.exe`,
			Person: &core.Person{UserRef: "sender-user"}, Resolved: true,
		},
	}
	h.norm = New(Config{Pipeline: h.pipe, Counters: h.counters, Clock: clock})
	return h
}

func (h *harness) sendSpans(t *testing.T, req *coltracepb.ExportTraceServiceRequest) {
	t.Helper()
	for _, rs := range req.GetResourceSpans() {
		service := readAttrs(rs.GetResource().GetAttributes()).str(attrServiceName)
		if !h.norm.Accepts(service) {
			t.Fatalf("the normalizer does not accept service.name %q", service)
		}
		h.norm.Spans(context.Background(), h.from, rs)
	}
}

func (h *harness) sendLogs(t *testing.T, req *collogspb.ExportLogsServiceRequest) {
	t.Helper()
	for _, rl := range req.GetResourceLogs() {
		service := readAttrs(rl.GetResource().GetAttributes()).str(attrServiceName)
		if !h.norm.Accepts(service) {
			t.Fatalf("the normalizer does not accept service.name %q", service)
		}
		h.norm.Logs(context.Background(), h.from, rl)
	}
}

func envelope(t *testing.T, e protocol.Entry) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func prompts(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range h.sink.all() {
		if env := envelope(t, e); env["kind"] == "prompt" {
			out = append(out, env)
		}
	}
	return out
}

// want is the envelope a span should become, nil for none.
func want(s *tracepb.Span) map[string]any {
	set := func(m map[string]any, field, key string) {
		if v, ok := raw(s, key); ok {
			if _, isInt := v.GetValue().(*commonpb.AnyValue_IntValue); isInt {
				m[field] = float64(v.GetIntValue())
			} else {
				m[field] = v.GetStringValue()
			}
		}
	}
	outcome := "success"
	if s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR || rawStr(s, "error.type") != "" {
		outcome = "error"
	}
	duration := float64((s.GetEndTimeUnixNano() - s.GetStartTimeUnixNano()) / uint64(time.Millisecond))
	switch rawStr(s, "gen_ai.operation.name") {
	case "chat":
		m := map[string]any{"kind": "agent_activity", "activity_type": "model_request", "outcome": outcome, "duration_ms": duration}
		set(m, "model", "gen_ai.request.model")
		set(m, "input_tokens", "gen_ai.usage.input_tokens")
		set(m, "output_tokens", "gen_ai.usage.output_tokens")
		return m
	case "execute_tool":
		m := map[string]any{"kind": "agent_activity", "activity_type": "tool_call", "outcome": outcome, "duration_ms": duration}
		set(m, "tool_name", "gen_ai.tool.name")
		return m
	case "invoke_agent":
		if len(s.GetParentSpanId()) == 0 {
			return map[string]any{"kind": "prompt"}
		}
	}
	return nil
}

// rootAgents counts a request's root invoke_agent spans: one per message the person sent.
func rootAgents(req *coltracepb.ExportTraceServiceRequest) int {
	n := 0
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				if rawStr(s, "gen_ai.operation.name") == "invoke_agent" && len(s.GetParentSpanId()) == 0 {
					n++
				}
			}
		}
	}
	return n
}

var dedupKey = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Every span of every fixture becomes the envelope its operation maps to, for the app its service
// names, attributed to the sending process's owner, and no other span becomes one.
func TestEveryFixtureSpanConverts(t *testing.T) {
	for file, req := range traceFixtures(t) {
		t.Run(file, func(t *testing.T) {
			h := newHarness(t, protocol.ModeM1)
			h.sendSpans(t, req)

			type expected struct {
				fields map[string]any
				at     time.Time
				name   string
			}
			var wants []expected
			spans := 0
			for _, rs := range req.GetResourceSpans() {
				for _, ss := range rs.GetScopeSpans() {
					for _, s := range ss.GetSpans() {
						spans++
						if w := want(s); w != nil {
							wants = append(wants, expected{w, time.Unix(0, int64(s.GetStartTimeUnixNano())).UTC(), s.GetName()})
						}
					}
				}
			}
			got := h.sink.all()
			if len(got) != len(wants) {
				t.Fatalf("%d envelopes for %d convertible spans", len(got), len(wants))
			}
			keys := map[string]bool{}
			for i, e := range got {
				w := wants[i]
				env := envelope(t, e)
				for k, v := range w.fields {
					if env[k] != v {
						t.Errorf("%s #%d: %s = %v, want %v", w.name, i, k, env[k], v)
					}
				}
				if env["source"] != "tool.otel" || env["tool_fingerprint"] != fingerprintOf(file) ||
					env["user_ref"] != "sender-user" || env["tenant_id"] != h.ident.TenantID {
					t.Errorf("%s #%d: envelope %s", w.name, i, e.Payload)
				}
				if !e.OccurredAt.Equal(w.at) {
					t.Errorf("%s #%d: occurred_at %v, want the span's start %v", w.name, i, e.OccurredAt, w.at)
				}
				if !dedupKey.MatchString(e.DedupKey) || (w.fields["kind"] != "prompt" && keys[e.DedupKey]) {
					t.Errorf("%s #%d: dedup_key %q is malformed or repeated", w.name, i, e.DedupKey)
				}
				keys[e.DedupKey] = true
				if w.fields["kind"] == "prompt" {
					d, _ := env["policy_decision"].(map[string]any)
					if d["action"] != "logged" || d["rule_id"] != "policy.default" || d["decided_locally"] != true {
						t.Errorf("prompt policy_decision %v", env["policy_decision"])
					}
				}
				for _, leak := range []string{canary, "SAC-PLACEHOLDER"} {
					if bytes.Contains(e.Payload, []byte(leak)) {
						t.Errorf("%s #%d: the envelope carries %q: %s", w.name, i, leak, e.Payload)
					}
				}
			}
			c := h.counters.Cumulative()
			if c[protocol.CounterEmitted] != uint64(len(wants)) || c[protocol.CounterSkippedNotGenerative] != uint64(spans-len(wants)) {
				t.Errorf("counters %v for %d envelopes of %d spans", c, len(wants), spans)
			}
		})
	}
}

// With content capture on, each prompt is the latest message the person sent and nothing else:
// not the conversation's earlier turns, not a subagent's input, not the title request's.
func TestThePromptIsTheLatestUserMessage(t *testing.T) {
	for _, name := range []string{"copilot/vscode-documented/traces-content-on.json", "copilot/cli-documented/traces-content-on.json"} {
		t.Run(name, func(t *testing.T) {
			req := traceFixture(t, name)
			h := newHarness(t, protocol.ModeM1)
			h.sendSpans(t, req)
			n := rootAgents(req)
			if got := h.pipe.reads.Load(); got != int64(n) {
				t.Fatalf("the content reader was called %d times, want once per prompt (%d)", got, n)
			}
			reqs := h.cls.requests()
			if len(reqs) != n {
				t.Fatalf("the classifier was called %d times, want %d", len(reqs), n)
			}
			for _, got := range reqs {
				if string(got) != canary {
					t.Fatalf("the classifier was handed %q, want the prompt text", got)
				}
			}
			ps := prompts(t, h)
			if len(ps) != n {
				t.Fatalf("%d prompt envelopes, want %d", len(ps), n)
			}
			for _, env := range ps {
				if env["collection_mode"] != "m1" || env["content_digest"] != dedup.ContentDigest(canary, nil) ||
					env["size_bytes"] != float64(len(canary)) || env["confidence"] != "high" {
					t.Errorf("prompt envelope %v", env)
				}
			}
		})
	}
}

// contentFields are the envelope fields derived from reading a prompt.
var contentFields = []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "prompt_kind", "attachments"}

// At m0 the prompt's reader is never called and the envelope carries no content-derived field.
func TestM0NeverReadsThePrompt(t *testing.T) {
	for _, name := range []string{"copilot/vscode-documented/traces-content-on.json", "copilot/cli-documented/traces-content-on.json"} {
		t.Run(name, func(t *testing.T) {
			req := traceFixture(t, name)
			h := newHarness(t, protocol.ModeM0)
			h.sendSpans(t, req)
			if n := h.pipe.reads.Load(); n != 0 {
				t.Fatalf("the content reader was called %d times at m0", n)
			}
			if n := len(h.cls.requests()); n != 0 {
				t.Fatalf("the classifier was called %d times at m0", n)
			}
			ps := prompts(t, h)
			if len(ps) != rootAgents(req) {
				t.Fatalf("%d prompt envelopes, want %d", len(ps), rootAgents(req))
			}
			for _, env := range ps {
				if env["collection_mode"] != "m0" || env["size_bytes"] != float64(len(canary)) {
					t.Errorf("prompt envelope %v", env)
				}
				for _, f := range contentFields {
					if _, ok := env[f]; ok {
						t.Errorf("the m0 prompt carries %s: %v", f, env)
					}
				}
			}
		})
	}
}

// With content capture off, each message the person sent is still a prompt, with no text: nothing
// is classified, and the record says classification did not complete.
func TestContentCaptureOffStillRecordsThePrompt(t *testing.T) {
	for _, name := range []string{"copilot/vscode-documented/traces-content-off.json", "copilot/cli-documented/traces-content-off.json"} {
		t.Run(name, func(t *testing.T) {
			req := traceFixture(t, name)
			h := newHarness(t, protocol.ModeM1)
			h.sendSpans(t, req)
			for _, got := range h.cls.requests() {
				if len(got) != 0 {
					t.Fatalf("the classifier was handed %q", got)
				}
			}
			ps := prompts(t, h)
			if len(ps) != rootAgents(req) || len(ps) == 0 {
				t.Fatalf("%d prompt envelopes, want %d", len(ps), rootAgents(req))
			}
			for _, env := range ps {
				if env["size_bytes"] != float64(0) || env["confidence"] != "degraded" {
					t.Errorf("prompt envelope %v", env)
				}
			}
		})
	}
}

// Copilot's log events repeat its spans or record editor actions, so none becomes an envelope or
// is read, and each is counted as skipped.
func TestLogEventsAreNotConverted(t *testing.T) {
	for file, req := range logFixtures(t) {
		t.Run(file, func(t *testing.T) {
			h := newHarness(t, protocol.ModeM3)
			h.sendLogs(t, req)
			if got := h.sink.all(); len(got) != 0 {
				t.Fatalf("%d envelopes from log events", len(got))
			}
			records := 0
			for _, rl := range req.GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					records += len(sl.GetLogRecords())
				}
			}
			if c := h.counters.Cumulative(); c[protocol.CounterSkippedNotGenerative] != uint64(records) {
				t.Errorf("skipped %d of %d records", c[protocol.CounterSkippedNotGenerative], records)
			}
		})
	}
}

// A messages value that is not JSON counts an error and is never read; the prompt is still
// recorded, from copilot_chat.user_request when the span has it.
func TestMalformedMessages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		userRequest string
		confidence  string
	}{
		{"alone", "", "degraded"},
		{"with user_request", canary, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := []*commonpb.KeyValue{
				{Key: attrOperation, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: opInvokeAgent}}},
				{Key: attrInputMessages, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: `[{"role":"user"` + canary}}},
			}
			if tc.userRequest != "" {
				attrs = append(attrs, &commonpb.KeyValue{Key: attrUserRequest, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: tc.userRequest}}})
			}
			h := newHarness(t, protocol.ModeM1)
			h.norm.Spans(context.Background(), h.from, &tracepb.ResourceSpans{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					{Key: attrServiceName, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ServiceVSCode}}},
				}},
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
					TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8),
					StartTimeUnixNano: 1_791_460_800_000_000_000, EndTimeUnixNano: 1_791_460_801_000_000_000,
					Attributes: attrs,
				}}}},
			})
			if c := h.counters.Cumulative(); c[protocol.CounterErrors] != 1 || c[protocol.CounterEmitted] != 1 {
				t.Errorf("counters %v", c)
			}
			for _, got := range h.cls.requests() {
				if string(got) != tc.userRequest {
					t.Errorf("the classifier was handed %q", got)
				}
			}
			ps := prompts(t, h)
			if len(ps) != 1 || ps[0]["confidence"] != tc.confidence {
				t.Fatalf("prompts %v", ps)
			}
		})
	}
}

func TestAcceptsOnlyCopilot(t *testing.T) {
	n := New(Config{})
	for service, want := range map[string]bool{
		"copilot-chat": true, "github-copilot": true,
		"": false, "claude-code": false, "Copilot-Chat": false, "github-copilot-chat": false,
	} {
		if n.Accepts(service) != want {
			t.Errorf("Accepts(%q) = %v", service, !want)
		}
	}
}

// The dedup key is sha256(tenant|device|tool_fingerprint|activity_type|event id|duration_ms), the
// event id being the span's trace and span ids in hex.
func TestActivityDedupKey(t *testing.T) {
	h := newHarness(t, protocol.ModeM0)
	h.sendSpans(t, traceFixture(t, "copilot/cli-documented/traces-content-off.json"))
	for _, e := range h.sink.all() {
		env := envelope(t, e)
		if env["activity_type"] != "tool_call" || env["tool_name"] != "powershell" {
			continue
		}
		in := strings.Join([]string{h.ident.TenantID, h.ident.DeviceID, FingerprintCLI, "tool_call",
			"7b2c9e4f1a3d5e6f8091a2b3c4d5e6f7" + "4d5e6f708192a3b4", "2080"}, "|")
		sum := sha256.Sum256([]byte(in))
		if want := "sha256:" + hex.EncodeToString(sum[:]); e.DedupKey != want {
			t.Fatalf("dedup_key %s, want %s", e.DedupKey, want)
		}
		return
	}
	t.Fatal("no powershell tool_call envelope")
}
