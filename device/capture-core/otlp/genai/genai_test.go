package genai

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	service   = "acme-agent"
	knownTool = "known-tool"

	system  = "Answer briefly."
	earlier = "an earlier question in the conversation"
	latest  = "summarise the quarterly numbers for ACME-CANARY-7731"
	older   = "what is the capital of France?"

	acmeImage = `C:\Program Files\Acme\AcmeAgent.exe`
	// acmeFP is exe: and the first 16 hex characters of sha256("acmeagent.exe").
	acmeFP = "exe:c4961f6032ed9d43"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

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

// envelopes are the spooled envelopes, decoded.
func (s *sink) envelopes(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, e := range s.entries {
		var env map[string]any
		if err := json.Unmarshal(e.Payload, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	return out
}

func (s *sink) payloads() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, e := range s.entries {
		b.Write(e.Payload)
	}
	return b.String()
}

type classifier struct{}

func (classifier) Classify(context.Context, protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	return protocol.ClassifyResponse{
		ClassifierVersion: "test-1",
		Confidence:        protocol.ConfidenceHigh,
		Labels:            []protocol.Label{{Class: "financial", Score: 0.9}},
	}, nil
}

// store is the M3 content store: what it holds is exactly the text each prompt event reported.
type store struct {
	mu   sync.Mutex
	held []string
}

func (s *store) Put(_ context.Context, _ string, content []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, string(content))
	return nil
}

func (s *store) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.held...)
}

// probe counts the calls to each prompt's content reader.
type probe struct {
	*core.Pipeline
	reads atomic.Int32
}

type countingReader struct {
	r     core.ContentReader
	reads *atomic.Int32
}

func (c countingReader) Read(ctx context.Context) ([]byte, error) {
	c.reads.Add(1)
	return c.r.Read(ctx)
}

func (p *probe) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	if obs.Content != nil {
		obs.Content = countingReader{r: obs.Content, reads: &p.reads}
	}
	return p.Pipeline.Process(ctx, obs)
}

type env struct {
	sink     *sink
	store    *store
	pipe     *probe
	counters *core.CounterSet
	n        *Normalizer
}

func newEnv(t *testing.T, mode protocol.CollectionMode, appByExe func(string) (string, bool)) *env {
	t.Helper()
	e := &env{sink: &sink{}, store: &store{}, counters: core.NewCounterSet(t0)}
	p, err := core.NewPipeline(e.sink, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.SetIdentity(core.Identity{TenantID: "tenant-1", DeviceID: "device-1", UserRef: "console-user"})
	bundle := &policy.Bundle{Version: "1", EffectiveAt: t0, Actor: "admin@example.invalid", TenantDefault: mode}
	p.Bundles = func() *policy.Bundle { return bundle }
	p.Classifier = classifier{}
	p.Content = e.store
	e.pipe = &probe{Pipeline: p}
	e.n = New(Config{Pipeline: e.pipe, Counters: e.counters, AppByExe: appByExe})
	return e
}

func (e *env) count(c protocol.Counter) uint64 { return e.counters.Cumulative()[c] }

// --- the official SDK, exporting to a real receiver ---

// recorder is a normalizer that keeps what the receiver hands it, then passes it on.
type recorder struct {
	accepts func(string) bool
	next    otlp.Normalizer

	mu      sync.Mutex
	spans   []*tracepb.ResourceSpans
	logs    []*logspb.ResourceLogs
	senders []otlp.Sender
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Accepts(service string) bool { return r.accepts(service) }

func (r *recorder) Spans(ctx context.Context, from otlp.Sender, rs *tracepb.ResourceSpans) {
	r.mu.Lock()
	r.spans = append(r.spans, proto.Clone(rs).(*tracepb.ResourceSpans))
	r.senders = append(r.senders, from)
	r.mu.Unlock()
	if r.next != nil {
		r.next.Spans(ctx, from, rs)
	}
}

func (r *recorder) Logs(ctx context.Context, from otlp.Sender, rl *logspb.ResourceLogs) {
	r.mu.Lock()
	r.logs = append(r.logs, proto.Clone(rl).(*logspb.ResourceLogs))
	r.senders = append(r.senders, from)
	r.mu.Unlock()
	if r.next != nil {
		r.next.Logs(ctx, from, rl)
	}
}

func (r *recorder) spanCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rs := range r.spans {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return n
}

// receiver starts an OTLP receiver with these normalizers, in order.
func receiver(t *testing.T, counters *core.CounterSet, normalizers ...otlp.Normalizer) *otlp.Receiver {
	t.Helper()
	r, err := otlp.New(otlp.Config{
		TokenPath:   t.TempDir() + "/" + otlp.TokenFile,
		HTTPListen:  "127.0.0.1:0",
		GRPCListen:  "127.0.0.1:0",
		Normalizers: normalizers,
		Counters:    counters,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r
}

// exportSpans sends the spans emit makes, as the service, through the official trace exporter.
func exportSpans(t *testing.T, r *otlp.Receiver, serviceName string, emit func(trace.Tracer)) {
	t.Helper()
	ctx := context.Background()
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(r.HTTPAddr()), otlptracehttp.WithInsecure(),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer " + r.Token()}))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(serviceName))))
	emit(tp.Tracer("acme-agent/llm"))
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// exportLogs sends the records emit makes, as the service, in one batch through the official log
// exporter.
func exportLogs(t *testing.T, r *otlp.Receiver, serviceName string, emit func(context.Context, otellog.Logger)) {
	t.Helper()
	ctx := context.Background()
	exp, err := otlploghttp.New(ctx, otlploghttp.WithEndpoint(r.HTTPAddr()), otlploghttp.WithInsecure(),
		otlploghttp.WithHeaders(map[string]string{"Authorization": "Bearer " + r.Token()}))
	if err != nil {
		t.Fatal(err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(resource.NewSchemaless(semconv.ServiceName(serviceName))))
	emit(ctx, lp.Logger("acme-agent/llm"))
	if err := lp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func messagesJSON(t *testing.T, msgs ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func text(role, content string) map[string]any {
	return map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": content}}}
}

// chatSpan is a model request whose messages end with the user's latest message, after an
// earlier turn.
func chatSpan(t *testing.T) func(trace.Tracer) {
	return func(tr trace.Tracer) {
		_, s := tr.Start(context.Background(), "chat gpt-4o", trace.WithTimestamp(t0), trace.WithAttributes(
			semconv.GenAIOperationNameChat,
			semconv.GenAIProviderNameOpenAI,
			semconv.GenAIRequestModel("gpt-4o"),
			semconv.GenAIResponseModel("gpt-4o-2024-08-06"),
			semconv.GenAIUsageInputTokens(120),
			semconv.GenAIUsageOutputTokens(45),
			semconv.GenAIInputMessagesKey.String(messagesJSON(t,
				text("system", system), text("user", earlier), text("assistant", "an earlier answer"), text("user", latest))),
		))
		s.End(trace.WithTimestamp(t0.Add(1500 * time.Millisecond)))
	}
}

func allSpans(t *testing.T) func(trace.Tracer) {
	return func(tr trace.Tracer) {
		ctx := context.Background()
		chatSpan(t)(tr)

		_, s := tr.Start(ctx, "execute_tool web_search", trace.WithTimestamp(t0.Add(2*time.Second)), trace.WithAttributes(
			semconv.GenAIOperationNameExecuteTool, semconv.GenAIToolName("web_search")))
		s.SetStatus(codes.Error, "upstream timed out")
		s.End(trace.WithTimestamp(t0.Add(2250 * time.Millisecond)))

		// The agent loop's next request: its messages end with the tool's response, so the user's
		// message in it was reported with the first request.
		_, s = tr.Start(ctx, "chat gpt-4o", trace.WithTimestamp(t0.Add(3*time.Second)), trace.WithAttributes(
			semconv.GenAIOperationNameChat, semconv.GenAIProviderNameOpenAI, semconv.GenAIRequestModel("gpt-4o"),
			semconv.GenAIInputMessagesKey.String(messagesJSON(t, text("user", latest),
				map[string]any{"role": "tool", "parts": []any{map[string]any{"type": "tool_call_response", "response": "42"}}})),
		))
		s.End(trace.WithTimestamp(t0.Add(4 * time.Second)))

		// The older conventions: gen_ai.system with a request model, and the prompt on a
		// gen_ai.content.prompt event in the OpenAI messages format.
		_, s = tr.Start(ctx, "chat gpt-3.5-turbo", trace.WithTimestamp(t0.Add(5*time.Second)), trace.WithAttributes(
			attribute.String("gen_ai.system", "openai"), semconv.GenAIRequestModel("gpt-3.5-turbo")))
		s.AddEvent("gen_ai.content.prompt", trace.WithAttributes(attribute.String("gen_ai.prompt",
			`[{"role":"system","content":"Answer briefly."},{"role":"user","content":"`+older+`"}]`)))
		s.End(trace.WithTimestamp(t0.Add(5200 * time.Millisecond)))

		// Not generative: no gen_ai.* attribute.
		_, s = tr.Start(ctx, "GET /health", trace.WithAttributes(attribute.String("http.request.method", "GET")))
		s.End()

		// Generative, but neither a model request nor a tool call.
		_, s = tr.Start(ctx, "embeddings text-embedding-3-small", trace.WithAttributes(
			semconv.GenAIOperationNameEmbeddings, semconv.GenAIRequestModel("text-embedding-3-small")))
		s.End()
	}
}

func byKind(envs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range envs {
		if e["kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// Spans from the official SDK, with the semconv attribute names, become agent_activity and prompt
// envelopes; the normalizer is registered last and sees only what the tool's normalizer refuses.
func TestSDKSpansBecomeEnvelopes(t *testing.T) {
	e := newEnv(t, protocol.ModeM3, nil)
	tool := &recorder{accepts: func(s string) bool { return s == knownTool }}
	seen := &recorder{accepts: func(string) bool { return true }, next: e.n}
	r := receiver(t, e.counters, tool, seen)

	exportSpans(t, r, knownTool, chatSpan(t))
	exportSpans(t, r, service, allSpans(t))

	if tool.spanCount() != 1 || seen.spanCount() != 6 {
		t.Fatalf("the tool's normalizer saw %d spans and the generic one %d, want 1 and 6", tool.spanCount(), seen.spanCount())
	}
	envs := e.sink.envelopes(t)
	acts, prompts := byKind(envs, "agent_activity"), byKind(envs, "prompt")
	if len(acts) != 4 || len(prompts) != 2 || len(envs) != 6 {
		t.Fatalf("got %d activity and %d prompt envelopes of %d, want 4 and 2", len(acts), len(prompts), len(envs))
	}
	for _, env := range envs {
		// The receiver was given no person resolver, so no sender is resolved.
		if env["source"] != "tool.otel" || env["tool_fingerprint"] != "exe:unknown" || env["user_ref"] != "unattributed" {
			t.Fatalf("envelope = %v", env)
		}
	}

	chat := acts[0]
	if chat["activity_type"] != "model_request" || chat["model"] != "gpt-4o" || num(chat["input_tokens"]) != 120 ||
		num(chat["output_tokens"]) != 45 || num(chat["duration_ms"]) != 1500 || chat["outcome"] != "success" ||
		chat["direction"] != "none" || chat["occurred_at"] == nil {
		t.Fatalf("chat activity = %v", chat)
	}
	toolCall := acts[1]
	if toolCall["activity_type"] != "tool_call" || toolCall["tool_name"] != "web_search" || num(toolCall["duration_ms"]) != 250 ||
		toolCall["outcome"] != "error" {
		t.Fatalf("tool activity = %v", toolCall)
	}
	if loop := acts[2]; loop["activity_type"] != "model_request" || num(loop["duration_ms"]) != 1000 {
		t.Fatalf("agent loop activity = %v", loop)
	}
	if old := acts[3]; old["activity_type"] != "model_request" || old["model"] != "gpt-3.5-turbo" || num(old["duration_ms"]) != 200 {
		t.Fatalf("older-convention activity = %v", old)
	}

	for _, p := range prompts {
		d, _ := p["policy_decision"].(map[string]any)
		if p["collection_mode"] != "m3" || d["action"] != "logged" || p["content_digest"] == nil {
			t.Fatalf("prompt = %v", p)
		}
	}
	if num(prompts[0]["size_bytes"]) != int64(len(latest)) || num(prompts[1]["size_bytes"]) != int64(len(older)) {
		t.Fatalf("prompt sizes = %v and %v", prompts[0]["size_bytes"], prompts[1]["size_bytes"])
	}
	// What each prompt event reported is the latest user message alone; earlier turns, the system
	// message and the agent loop's repeat are not reported.
	if held := e.store.texts(); len(held) != 2 || held[0] != latest || held[1] != older {
		t.Fatalf("held content = %q", held)
	}
	if p := e.sink.payloads(); strings.Contains(p, "ACME-CANARY") || strings.Contains(p, "capital of France") {
		t.Fatal("prompt text reached an envelope")
	}

	if got := e.count(protocol.CounterEmitted); got != 6 {
		t.Fatalf("emitted = %d, want 6", got)
	}
	if got := e.count(protocol.CounterSkippedNotGenerative); got != 2 {
		t.Fatalf("skipped_not_generative = %d, want 2", got)
	}
}

// recordedChat is the chat span as the official SDK exports it, the way the receiver hands it on.
func recordedChat(t *testing.T, serviceName string) *tracepb.ResourceSpans {
	t.Helper()
	rec := &recorder{accepts: func(string) bool { return true }}
	r := receiver(t, nil, rec)
	exportSpans(t, r, serviceName, chatSpan(t))
	if len(rec.spans) != 1 {
		t.Fatalf("recorded %d exports, want 1", len(rec.spans))
	}
	return rec.spans[0]
}

func resolved(image string) otlp.Sender {
	return otlp.Sender{PID: 4242, Image: image, Person: &core.Person{UserRef: "u_alice"}, Resolved: true}
}

// The app is the sender's: a catalog match, else a hash of its executable's name, else exe:unknown.
// The service.name the telemetry declares is never used, even when it names a catalog app.
func TestFingerprintIsTheSendingProcesss(t *testing.T) {
	rs := recordedChat(t, "cursor")
	catalog := func(base string) (string, bool) {
		switch base {
		case "acmeagent.exe":
			return "acme_agent", true
		case "cursor", "cursor.exe":
			return "cursor", true
		}
		return "", false
	}
	cases := []struct {
		name     string
		catalog  func(string) (string, bool)
		from     otlp.Sender
		fp, user string
	}{
		{"catalog hit", catalog, resolved(acmeImage), "app:acme_agent", "u_alice"},
		{"catalog miss", nil, resolved(acmeImage), acmeFP, "u_alice"},
		{"catalog miss, POSIX path", nil, resolved("/opt/acme/AcmeAgent.exe"), acmeFP, "u_alice"},
		{"catalog miss, upper-case name", nil, resolved(`D:\ACME\ACMEAGENT.EXE`), acmeFP, "u_alice"},
		{"unresolved", catalog, otlp.Sender{PID: 4242, Image: acmeImage, Person: &core.Person{UserRef: "unattributed"}}, "exe:unknown", "unattributed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, protocol.ModeM1, c.catalog)
			e.n.Spans(context.Background(), c.from, rs)
			envs := e.sink.envelopes(t)
			if len(envs) != 2 {
				t.Fatalf("got %d envelopes, want 2", len(envs))
			}
			for _, env := range envs {
				if env["tool_fingerprint"] != c.fp || env["user_ref"] != c.user {
					t.Fatalf("%s envelope has tool_fingerprint %v and user_ref %v, want %s and %s",
						env["kind"], env["tool_fingerprint"], env["user_ref"], c.fp, c.user)
				}
			}
		})
	}
}

// A span with no gen_ai.* attribute is counted skipped_not_generative and dropped.
func TestNonGenerativeSpanIsSkipped(t *testing.T) {
	e := newEnv(t, protocol.ModeM3, nil)
	r := receiver(t, e.counters, e.n)
	exportSpans(t, r, service, func(tr trace.Tracer) {
		_, s := tr.Start(context.Background(), "POST /v1/upload", trace.WithAttributes(
			attribute.String("http.request.method", "POST"), attribute.String("prompt", "not a gen_ai attribute")))
		s.End()
	})
	if n := len(e.sink.envelopes(t)); n != 0 {
		t.Fatalf("%d envelopes were spooled, want none", n)
	}
	if got := e.count(protocol.CounterSkippedNotGenerative); got != 1 {
		t.Fatalf("skipped_not_generative = %d, want 1", got)
	}
	if got := e.count(protocol.CounterEmitted); got != 0 {
		t.Fatalf("emitted = %d, want 0", got)
	}
}

// At m0 the message's reader is never called: the prompt event carries the message's length and no
// content-derived field. At m1 it is read once, and the envelope carries a digest and labels but not
// the text.
func TestModeGatesTheMessageText(t *testing.T) {
	rs := recordedChat(t, service)

	e := newEnv(t, protocol.ModeM0, nil)
	e.n.Spans(context.Background(), resolved(acmeImage), rs)
	if n := e.pipe.reads.Load(); n != 0 {
		t.Fatalf("the message was read %d times at m0", n)
	}
	prompts := byKind(e.sink.envelopes(t), "prompt")
	if len(prompts) != 1 {
		t.Fatalf("got %d prompt envelopes at m0, want 1", len(prompts))
	}
	p := prompts[0]
	for _, field := range []string{"content_digest", "labels", "classifier_version", "content_excerpt", "confidence"} {
		if _, ok := p[field]; ok {
			t.Errorf("the m0 prompt carries %s", field)
		}
	}
	if p["collection_mode"] != "m0" || num(p["size_bytes"]) != int64(len(latest)) {
		t.Fatalf("m0 prompt = %v", p)
	}
	if len(e.store.texts()) != 0 {
		t.Fatal("content was held at m0")
	}

	e = newEnv(t, protocol.ModeM1, nil)
	e.n.Spans(context.Background(), resolved(acmeImage), rs)
	if n := e.pipe.reads.Load(); n != 1 {
		t.Fatalf("the message was read %d times at m1, want 1", n)
	}
	p = byKind(e.sink.envelopes(t), "prompt")[0]
	if p["collection_mode"] != "m1" || p["content_digest"] == nil || p["labels"] == nil {
		t.Fatalf("m1 prompt = %v", p)
	}
	if strings.Contains(e.sink.payloads(), "ACME-CANARY") {
		t.Fatal("the message text reached an m1 envelope")
	}
}

// Log records carry prompts: gen_ai.input.messages on an event, and the older per-message events,
// where only a request's last message is reported and only when it is the user's.
func TestSDKLogsBecomePrompts(t *testing.T) {
	e := newEnv(t, protocol.ModeM3, nil)
	r := receiver(t, e.counters, e.n)
	request := func(ctx context.Context, span byte) context.Context {
		return trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanID:  trace.SpanID{0, 0, 0, 0, 0, 0, 0, span},
		}))
	}
	event := func(ctx context.Context, l otellog.Logger, name string, body attribute.Value, attrs ...attribute.KeyValue) {
		var rec otellog.Record
		rec.SetEventName(name)
		rec.SetTimestamp(t0)
		rec.SetBody(body)
		rec.AddAttributes(append(attrs, attribute.String("gen_ai.system", "openai"))...)
		l.Emit(ctx, rec)
	}
	content := func(s string) attribute.Value { return attribute.MapValue(attribute.String("content", s)) }
	part := func(role, s string) attribute.Value {
		return attribute.MapValue(attribute.String("role", role),
			attribute.Slice("parts", attribute.MapValue(attribute.String("type", "text"), attribute.String("content", s))))
	}

	exportLogs(t, r, service, func(ctx context.Context, l otellog.Logger) {
		// The current conventions: structured messages on the operation details event.
		var rec otellog.Record
		rec.SetEventName("gen_ai.client.inference.operation.details")
		rec.SetTimestamp(t0)
		rec.AddAttributes(semconv.GenAIOperationNameChat, semconv.GenAIRequestModel("gpt-4o"),
			attribute.Slice(string(semconv.GenAIInputMessagesKey), part("user", earlier), part("assistant", "an earlier answer"), part("user", latest)))
		l.Emit(request(ctx, 1), rec)

		// The older per-message events: the second request's last message is the user's.
		r2 := request(ctx, 2)
		event(r2, l, "gen_ai.system.message", content(system))
		event(r2, l, "gen_ai.user.message", content(earlier))
		event(r2, l, "gen_ai.assistant.message", content("an earlier answer"))
		event(r2, l, "gen_ai.user.message", content(older))
		event(r2, l, "gen_ai.choice", attribute.MapValue(attribute.Int("index", 0)))

		// The third request ends with a tool's message, so its user message is an earlier turn.
		r3 := request(ctx, 3)
		event(r3, l, "gen_ai.user.message", content(older))
		event(r3, l, "gen_ai.tool.message", content("42"))

		// Not generative.
		var plain otellog.Record
		plain.SetTimestamp(t0)
		plain.SetBody(attribute.StringValue("cache warmed"))
		l.Emit(ctx, plain)
	})

	envs := e.sink.envelopes(t)
	if len(envs) != 2 || len(byKind(envs, "prompt")) != 2 {
		t.Fatalf("got %d envelopes, want 2 prompts: %v", len(envs), envs)
	}
	if held := e.store.texts(); len(held) != 2 || held[0] != latest || held[1] != older {
		t.Fatalf("held content = %q", held)
	}
	if got := e.count(protocol.CounterEmitted); got != 2 {
		t.Fatalf("emitted = %d, want 2", got)
	}
	// Seven records yielded nothing: the second request's events other than its last user message,
	// both of the third request's, and the plain record.
	if got := e.count(protocol.CounterSkippedNotGenerative); got != 7 {
		t.Fatalf("skipped_not_generative = %d, want 7", got)
	}
}

// A messages attribute that is not JSON counts an error; the activity is still recorded.
func TestMalformedMessagesCountAnError(t *testing.T) {
	e := newEnv(t, protocol.ModeM1, nil)
	r := receiver(t, e.counters, e.n)
	exportSpans(t, r, service, func(tr trace.Tracer) {
		_, s := tr.Start(context.Background(), "chat gpt-4o", trace.WithAttributes(
			semconv.GenAIOperationNameChat, semconv.GenAIRequestModel("gpt-4o"),
			semconv.GenAIInputMessagesKey.String(`[{"role":"user","parts":[{"type":"text","content":"cut off`)))
		s.End()
	})
	envs := e.sink.envelopes(t)
	if len(envs) != 1 || envs[0]["kind"] != "agent_activity" {
		t.Fatalf("envelopes = %v", envs)
	}
	if e.count(protocol.CounterErrors) != 1 || e.pipe.reads.Load() != 0 {
		t.Fatalf("errors = %d, reads = %d", e.count(protocol.CounterErrors), e.pipe.reads.Load())
	}
}

func TestAcceptsEveryService(t *testing.T) {
	n := New(Config{})
	for _, s := range []string{"", service, knownTool, "claude-code"} {
		if !n.Accepts(s) {
			t.Errorf("Accepts(%q) = false", s)
		}
	}
}
