package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// canary is the prompt text in the prompts-on fixture.
const canary = "SAC-CANARY-7f3a prompt text"

// logFixtures decodes every Codex log fixture, keyed by its path under testdata.
func logFixtures(t *testing.T) map[string]*collogspb.ExportLogsServiceRequest {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "testdata", "codex", "*", "logs-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Codex log fixtures")
	}
	out := map[string]*collogspb.ExportLogsServiceRequest{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		req := &collogspb.ExportLogsServiceRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		rel, _ := filepath.Rel(filepath.Join("..", "testdata"), f)
		out[filepath.ToSlash(rel)] = req
	}
	return out
}

func fixture(t *testing.T, name string) *collogspb.ExportLogsServiceRequest {
	t.Helper()
	for file, req := range logFixtures(t) {
		if filepath.Base(file) == name {
			return req
		}
	}
	t.Fatalf("no fixture %s", name)
	return nil
}

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
	pipe  *readCounter
	sink  *sink
	cls   *classifier
	norm  *Normalizer
	log   *logLines
	from  otlp.Sender
	ident core.Identity
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// newHarness is a pipeline whose bundle resolves Codex to mode, enrolled to a random tenant.
func newHarness(t *testing.T, mode protocol.CollectionMode) *harness {
	t.Helper()
	s := &sink{}
	p, err := core.NewPipeline(s, func() time.Time { return time.Unix(1_791_460_900, 0) }, nil)
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
			ToolModes:     map[string]protocol.CollectionMode{ToolFingerprint: mode},
		}
	}
	cls := &classifier{}
	p.Classifier = cls
	h := &harness{
		pipe:  &readCounter{Pipeline: p},
		sink:  s,
		cls:   cls,
		log:   &logLines{},
		ident: id,
		from: otlp.Sender{
			PID: 4242, Image: `C:\Users\someone\AppData\Roaming\npm\node_modules\@openai\codex\vendor\x86_64-pc-windows-msvc\codex\codex.exe`,
			Person: &core.Person{UserRef: "sender-user"}, Resolved: true,
		},
	}
	h.norm = New(Config{Pipeline: h.pipe, Log: h.log})
	return h
}

func (h *harness) send(t *testing.T, req *collogspb.ExportLogsServiceRequest) {
	t.Helper()
	for _, rl := range req.GetResourceLogs() {
		var service string
		for _, kv := range rl.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = kv.GetValue().GetStringValue()
			}
		}
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

// rawAttr is a record attribute as the fixture spells it, read without the normalizer's helpers.
func rawAttr(lr *logspb.LogRecord, key string) (string, bool) {
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() != key {
			continue
		}
		v := kv.GetValue()
		if _, ok := v.GetValue().(*commonpb.AnyValue_IntValue); ok {
			return strconv.FormatInt(v.GetIntValue(), 10), true
		}
		return v.GetStringValue(), true
	}
	return "", false
}

// expectation computes, from the fixture alone, the envelope each record should become.
type expectation struct {
	denied map[string]bool
}

// want is the envelope a record should become, nil for none. Codex sends counts as decimal text,
// and the envelope carries them as numbers.
func (x *expectation) want(t *testing.T, lr *logspb.LogRecord) map[string]any {
	t.Helper()
	str := func(k string) string { v, _ := rawAttr(lr, k); return v }
	has := func(k string) bool { _, ok := rawAttr(lr, k); return ok }
	set := func(m map[string]any, keys map[string]string) map[string]any {
		for from, to := range keys {
			v, ok := rawAttr(lr, from)
			if !ok {
				continue
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && from != "model" && from != "tool_name" {
				m[to] = float64(n)
			} else {
				m[to] = v
			}
		}
		return m
	}
	call := str("conversation.id") + "/" + str("call_id")
	switch str("event.name") {
	case "codex.user_prompt":
		n, err := strconv.ParseInt(str("prompt_length"), 10, 64)
		if err != nil {
			t.Fatalf("prompt_length %q", str("prompt_length"))
		}
		return map[string]any{"kind": "prompt", "size_bytes": float64(n)}
	case "codex.tool_decision":
		switch str("decision") {
		case "denied", "denied_with_network_policy_deny", "abort", "timed_out":
			x.denied[call] = true
			return set(map[string]any{"kind": "agent_activity", "activity_type": "tool_call", "outcome": "denied"},
				map[string]string{"tool_name": "tool_name"})
		}
		return nil
	case "codex.tool_result":
		if x.denied[call] {
			return nil
		}
		outcome := "success"
		if str("success") == "false" {
			outcome = "error"
		}
		return set(map[string]any{"kind": "agent_activity", "activity_type": "tool_call", "outcome": outcome},
			map[string]string{"tool_name": "tool_name", "duration_ms": "duration_ms"})
	case "codex.api_request":
		status, _ := strconv.Atoi(str("http.response.status_code"))
		if !has("error.message") && status >= 200 && status <= 299 {
			return nil
		}
		return set(map[string]any{"kind": "agent_activity", "activity_type": "model_request", "outcome": "error"},
			map[string]string{"model": "model", "duration_ms": "duration_ms"})
	case "codex.websocket_request":
		if !has("error.message") && str("success") != "false" {
			return nil
		}
		return set(map[string]any{"kind": "agent_activity", "activity_type": "model_request", "outcome": "error"},
			map[string]string{"model": "model", "duration_ms": "duration_ms"})
	case "codex.sse_event":
		if str("event.kind") != "response.completed" {
			return nil
		}
		if has("error.message") {
			return set(map[string]any{"kind": "agent_activity", "activity_type": "model_request", "outcome": "error"},
				map[string]string{"model": "model"})
		}
		return set(map[string]any{"kind": "agent_activity", "activity_type": "model_request", "outcome": "success"},
			map[string]string{"model": "model", "input_token_count": "input_tokens", "output_token_count": "output_tokens"})
	}
	return nil
}

// recordTime is the record's time, as the fixture gives it.
func recordTime(lr *logspb.LogRecord) time.Time {
	ns := lr.GetTimeUnixNano()
	if ns == 0 {
		ns = lr.GetObservedTimeUnixNano()
	}
	return time.Unix(0, int64(ns)).UTC()
}

var dedupKey = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Every record of every fixture becomes the envelope its event maps to, attributed to the sending
// process's owner, and no other record becomes one.
func TestEveryFixtureEventConverts(t *testing.T) {
	for file, req := range logFixtures(t) {
		t.Run(file, func(t *testing.T) {
			h := newHarness(t, protocol.ModeM1)
			h.send(t, req)

			type expected struct {
				fields       map[string]any
				at           time.Time
				event        string
				conversation string
			}
			x := &expectation{denied: map[string]bool{}}
			var wants []expected
			for _, rl := range req.GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						if w := x.want(t, lr); w != nil {
							name, _ := rawAttr(lr, "event.name")
							conv, _ := rawAttr(lr, "conversation.id")
							wants = append(wants, expected{w, recordTime(lr), name, conv})
						}
					}
				}
			}
			got := h.sink.all()
			if len(got) != len(wants) {
				t.Fatalf("%d envelopes for %d convertible records; log %v", len(got), len(wants), h.log.lines)
			}
			keys := map[string]bool{}
			for i, e := range got {
				w := wants[i]
				env := envelope(t, e)
				for k, v := range w.fields {
					if env[k] != v {
						t.Errorf("%s #%d: %s = %v, want %v", w.event, i, k, env[k], v)
					}
				}
				for _, k := range []string{"input_tokens", "output_tokens", "duration_ms", "model", "tool_name"} {
					if _, ok := w.fields[k]; !ok && env[k] != nil {
						t.Errorf("%s #%d: %s = %v, want none", w.event, i, k, env[k])
					}
				}
				if env["source"] != "tool.otel" || env["tool_fingerprint"] != ToolFingerprint ||
					env["user_ref"] != "sender-user" || env["tenant_id"] != h.ident.TenantID {
					t.Errorf("%s #%d: envelope %s", w.event, i, e.Payload)
				}
				if !e.OccurredAt.Equal(w.at) {
					t.Errorf("%s #%d: occurred_at %v, want the record's time %v", w.event, i, e.OccurredAt, w.at)
				}
				if !dedupKey.MatchString(e.DedupKey) || (w.fields["kind"] != "prompt" && keys[e.DedupKey]) {
					t.Errorf("%s #%d: dedup_key %q is malformed or repeated", w.event, i, e.DedupKey)
				}
				keys[e.DedupKey] = true
				if w.fields["kind"] == "prompt" {
					if e.ClientID != w.conversation {
						t.Errorf("prompt client id %q, want the conversation id %q", e.ClientID, w.conversation)
					}
					d, _ := env["policy_decision"].(map[string]any)
					if d["action"] != "logged" || d["rule_id"] != "policy.default" || d["decided_locally"] != true {
						t.Errorf("prompt policy_decision %v", env["policy_decision"])
					}
				}
				for _, leak := range []string{canary, "SAC-PLACEHOLDER", redacted} {
					if bytes.Contains(e.Payload, []byte(leak)) {
						t.Errorf("%s #%d: the envelope carries %q: %s", w.event, i, leak, e.Payload)
					}
				}
			}
			if len(h.log.lines) != 0 {
				t.Errorf("log %v", h.log.lines)
			}
		})
	}
}

// contentFields are the envelope fields derived from reading a prompt.
var contentFields = []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "prompt_kind", "attachments"}

// At m0 the prompt's reader is never called and the envelope carries no content-derived field.
func TestM0NeverReadsThePrompt(t *testing.T) {
	h := newHarness(t, protocol.ModeM0)
	h.send(t, fixture(t, "logs-prompts-on.json"))
	if n := h.pipe.reads.Load(); n != 0 {
		t.Fatalf("the content reader was called %d times at m0", n)
	}
	if n := len(h.cls.requests()); n != 0 {
		t.Fatalf("the classifier was called %d times at m0", n)
	}
	prompts := 0
	for _, e := range h.sink.all() {
		env := envelope(t, e)
		if env["kind"] != "prompt" {
			continue
		}
		prompts++
		if env["collection_mode"] != "m0" || env["size_bytes"] != float64(len(canary)) {
			t.Errorf("prompt envelope %s", e.Payload)
		}
		for _, f := range contentFields {
			if _, ok := env[f]; ok {
				t.Errorf("the m0 prompt carries %s: %s", f, e.Payload)
			}
		}
	}
	if prompts != 1 {
		t.Fatalf("%d prompt envelopes, want 1", prompts)
	}
}

// At m1 the canary is read and classified on the device; the envelope carries its digest and
// labels and never the text.
func TestM1CarriesADigestAndLabelsButNoText(t *testing.T) {
	h := newHarness(t, protocol.ModeM1)
	h.send(t, fixture(t, "logs-prompts-on.json"))
	if n := h.pipe.reads.Load(); n != 1 {
		t.Fatalf("the content reader was called %d times, want once", n)
	}
	got := h.cls.requests()
	if len(got) != 1 || string(got[0]) != canary {
		t.Fatalf("the classifier was handed %q, want the prompt text", got)
	}
	prompts := 0
	for _, e := range h.sink.all() {
		env := envelope(t, e)
		if env["kind"] != "prompt" {
			continue
		}
		prompts++
		if env["collection_mode"] != "m1" || env["content_digest"] != dedup.ContentDigest(canary, nil) ||
			env["confidence"] != "high" || env["prompt_kind"] != "user" {
			t.Errorf("prompt envelope %s", e.Payload)
		}
		labels, _ := env["labels"].([]any)
		if len(labels) != 1 || labels[0].(map[string]any)["class"] != "credential" {
			t.Errorf("labels %v", env["labels"])
		}
		if _, ok := env["content_excerpt"]; ok {
			t.Errorf("an m1 prompt carries an excerpt: %s", e.Payload)
		}
		if bytes.Contains(e.Payload, []byte("SAC-CANARY")) {
			t.Errorf("the envelope carries the prompt text: %s", e.Payload)
		}
	}
	if prompts != 1 {
		t.Fatalf("%d prompt envelopes, want 1", prompts)
	}
}

// With log_user_prompt off the record carries [REDACTED], which is never classified as a prompt:
// the record keeps its size and says classification did not complete.
func TestARedactedPromptIsNotClassified(t *testing.T) {
	h := newHarness(t, protocol.ModeM1)
	h.send(t, fixture(t, "logs-prompts-off.json"))
	for _, got := range h.cls.requests() {
		if strings.Contains(string(got), redacted) {
			t.Fatalf("the classifier was handed %q", got)
		}
	}
	prompts := 0
	for _, e := range h.sink.all() {
		env := envelope(t, e)
		if env["kind"] != "prompt" {
			continue
		}
		prompts++
		if env["size_bytes"] != float64(len(canary)) || env["confidence"] != "degraded" {
			t.Errorf("prompt envelope %s", e.Payload)
		}
		if env["content_digest"] == dedup.ContentDigest(redacted, nil) {
			t.Errorf("the digest is the redaction marker's: %s", e.Payload)
		}
	}
	if prompts != 1 {
		t.Fatalf("%d prompt envelopes, want 1", prompts)
	}
}

func TestAcceptsOnlyCodex(t *testing.T) {
	n := New(Config{})
	for service, want := range map[string]bool{
		"codex_cli_rs": true, "codex_exec": true, "codex-app-server": true,
		"": false, "codex": false, "claude-code": false, "Codex_Exec": false, "codex_vscode": false,
	} {
		if n.Accepts(service) != want {
			t.Errorf("Accepts(%q) = %v", service, !want)
		}
	}
}

// The dedup key is sha256(tenant|device|tool_fingerprint|activity_type|event id|duration_ms), the
// event id being the event, its conversation, its timestamp, and its call id and attempt.
func TestActivityDedupKey(t *testing.T) {
	h := newHarness(t, protocol.ModeM0)
	h.send(t, fixture(t, "logs-prompts-off.json"))
	conv := "019a6f31-0c5e-7b48-a1d3-8f2e6b4c9d07"
	cases := map[string]string{
		"model_request/error": strings.Join([]string{h.ident.TenantID, h.ident.DeviceID, ToolFingerprint, "model_request",
			"codex.api_request/" + conv + "/2026-10-08T12:00:01.500Z//1", "1873"}, "|"),
		"model_request/success": strings.Join([]string{h.ident.TenantID, h.ident.DeviceID, ToolFingerprint, "model_request",
			"codex.sse_event/" + conv + "/2026-10-08T12:00:03.600Z//", ""}, "|"),
		"tool_call/denied": strings.Join([]string{h.ident.TenantID, h.ident.DeviceID, ToolFingerprint, "tool_call",
			"codex.tool_decision/" + conv + "/2026-10-08T12:00:04.000Z/call_SACFIXTURE0102/", ""}, "|"),
	}
	found := map[string]bool{}
	for _, e := range h.sink.all() {
		env := envelope(t, e)
		k := fmt.Sprint(env["activity_type"], "/", env["outcome"])
		in, ok := cases[k]
		if !ok || found[k] {
			continue
		}
		found[k] = true
		sum := sha256.Sum256([]byte(in))
		if want := "sha256:" + hex.EncodeToString(sum[:]); e.DedupKey != want {
			t.Errorf("%s: dedup_key %s, want %s", k, e.DedupKey, want)
		}
	}
	for k := range cases {
		if !found[k] {
			t.Errorf("no %s envelope", k)
		}
	}
}

// logRequest is an export of the records, from codex exec.
func logRequest(records ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex_exec"}},
		}}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}
}

func toolRecord(event, call string, extra map[string]string) *logspb.LogRecord {
	lr := &logspb.LogRecord{ObservedTimeUnixNano: uint64(time.Unix(1_791_460_800, 0).UnixNano())}
	add := func(k, v string) {
		lr.Attributes = append(lr.Attributes, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}})
	}
	add("event.name", event)
	add("conversation.id", "conv-1")
	add("call_id", call)
	add("tool_name", "shell")
	for k, v := range extra {
		add(k, v)
	}
	return lr
}

// A denied call is one denied record even when its tool_result arrives in a later export, and a
// call without a denial is recorded from its result.
func TestADeniedCallIsRecordedOnce(t *testing.T) {
	h := newHarness(t, protocol.ModeM0)
	h.send(t, logRequest(toolRecord(eventToolDecision, "call-1", map[string]string{"decision": "abort"})))
	h.send(t, logRequest(
		toolRecord(eventToolResult, "call-1", map[string]string{"success": "false", "duration_ms": "5"}),
		toolRecord(eventToolResult, "call-2", map[string]string{"success": "false", "duration_ms": "6"}),
	))
	var outcomes []any
	for _, e := range h.sink.all() {
		outcomes = append(outcomes, envelope(t, e)["outcome"])
	}
	if fmt.Sprint(outcomes) != "[denied error]" {
		t.Fatalf("outcomes %v, want [denied error]", outcomes)
	}
}

// The remembered denials are bounded: the oldest is forgotten first.
func TestDeniedCallsAreBounded(t *testing.T) {
	var d deniedCalls
	for i := 0; i <= maxDeniedCalls; i++ {
		d.add(strconv.Itoa(i))
	}
	if len(d.set) != maxDeniedCalls || len(d.order) != maxDeniedCalls {
		t.Fatalf("%d remembered, %d in order, want %d", len(d.set), len(d.order), maxDeniedCalls)
	}
	if d.take("0") {
		t.Error("the oldest denial is still remembered")
	}
	if !d.take(strconv.Itoa(maxDeniedCalls)) || d.take(strconv.Itoa(maxDeniedCalls)) {
		t.Error("the newest denial is not taken exactly once")
	}
}
