package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const tool = "claude-code"

// fakeNormalizer accepts one service.name and records what it was handed.
type fakeNormalizer struct {
	mu      sync.Mutex
	logs    []*logspb.ResourceLogs
	spans   []*tracepb.ResourceSpans
	senders []Sender
}

func (n *fakeNormalizer) Name() string                { return "fake" }
func (n *fakeNormalizer) Accepts(service string) bool { return service == tool }

func (n *fakeNormalizer) Logs(_ context.Context, from Sender, rl *logspb.ResourceLogs) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logs = append(n.logs, rl)
	n.senders = append(n.senders, from)
}

func (n *fakeNormalizer) Spans(_ context.Context, from Sender, rs *tracepb.ResourceSpans) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.spans = append(n.spans, rs)
	n.senders = append(n.senders, from)
}

func (n *fakeNormalizer) seen() (logs, spans int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, rl := range n.logs {
		for _, sl := range rl.GetScopeLogs() {
			logs += len(sl.GetLogRecords())
		}
	}
	for _, rs := range n.spans {
		for _, ss := range rs.GetScopeSpans() {
			spans += len(ss.GetSpans())
		}
	}
	return logs, spans
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

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type fixture struct {
	r     *Receiver
	norm  *fakeNormalizer
	log   *logLines
	owner *ownerTable
}

func newReceiver(t *testing.T, httpAddr, grpcAddr string) fixture {
	t.Helper()
	f := fixture{norm: &fakeNormalizer{}, log: &logLines{}}
	r, err := New(Config{
		TokenPath:   t.TempDir() + "/" + TokenFile,
		HTTPListen:  httpAddr,
		GRPCListen:  grpcAddr,
		Normalizers: []Normalizer{f.norm},
		Person:      testPerson,
		Log:         f.log,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	f.owner = newOwnerTable(t, r)
	r.owner = f.owner.lookup
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return f
}

func started(t *testing.T) fixture {
	t.Helper()
	f := newReceiver(t, "127.0.0.1:0", "127.0.0.1:0")
	if err := f.r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func toolResource() *resource.Resource {
	return resource.NewSchemaless(attribute.String("service.name", tool))
}

// exportOnce hands each record straight to the exporter and keeps the error, so the test sees the
// export's outcome rather than the SDK's error handler.
type exportOnce struct {
	exp sdklog.Exporter
	mu  sync.Mutex
	err error
}

func (p *exportOnce) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (p *exportOnce) Shutdown(ctx context.Context) error                     { return p.exp.Shutdown(ctx) }
func (p *exportOnce) ForceFlush(context.Context) error                       { return nil }
func (p *exportOnce) OnEmit(ctx context.Context, rec *sdklog.Record) error {
	err := p.exp.Export(ctx, []sdklog.Record{rec.Clone()})
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	return nil
}

func exportLogs(t *testing.T, exp sdklog.Exporter) error {
	t.Helper()
	ctx := context.Background()
	p := &exportOnce{exp: exp}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(toolResource()), sdklog.WithProcessor(p))
	defer lp.Shutdown(ctx)
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetBody(attribute.StringValue("a prompt"))
	lp.Logger("test").Emit(ctx, rec)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func spanStubs() tracetest.SpanStubs {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
	})
	now := time.Now()
	return tracetest.SpanStubs{{
		Name:                 "chat",
		SpanContext:          sc,
		StartTime:            now.Add(-time.Second),
		EndTime:              now,
		Resource:             toolResource(),
		InstrumentationScope: instrumentation.Scope{Name: "test"},
	}}
}

func metrics() *metricdata.ResourceMetrics {
	now := time.Now()
	return &metricdata.ResourceMetrics{
		Resource: toolResource(),
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope: instrumentation.Scope{Name: "test"},
			Metrics: []metricdata.Metrics{{
				Name: "token.usage",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints:  []metricdata.DataPoint[int64]{{StartTime: now.Add(-time.Minute), Time: now, Value: 42}},
				},
			}},
		}},
	}
}

func TestOfficialExportersExportOneBatch(t *testing.T) {
	ctx := context.Background()
	f := started(t)
	auth := map[string]string{"Authorization": "Bearer " + f.r.Token()}
	httpAddr, grpcAddr := f.r.HTTPAddr(), f.r.GRPCAddr()

	cases := []struct {
		name          string
		export        func() error
		logs, spans   int
		skippedMetric bool
	}{
		{name: "otlploghttp", logs: 1, export: func() error {
			exp, err := otlploghttp.New(ctx, otlploghttp.WithEndpoint(httpAddr), otlploghttp.WithInsecure(), otlploghttp.WithHeaders(auth))
			if err != nil {
				return err
			}
			return exportLogs(t, exp)
		}},
		{name: "otlploggrpc", logs: 1, export: func() error {
			exp, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpoint(grpcAddr), otlploggrpc.WithInsecure(), otlploggrpc.WithHeaders(auth))
			if err != nil {
				return err
			}
			return exportLogs(t, exp)
		}},
		{name: "otlptracehttp", spans: 1, export: func() error {
			exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(httpAddr), otlptracehttp.WithInsecure(), otlptracehttp.WithHeaders(auth))
			if err != nil {
				return err
			}
			defer exp.Shutdown(ctx)
			return exp.ExportSpans(ctx, spanStubs().Snapshots())
		}},
		{name: "otlptracegrpc", spans: 1, export: func() error {
			exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(grpcAddr), otlptracegrpc.WithInsecure(), otlptracegrpc.WithHeaders(auth))
			if err != nil {
				return err
			}
			defer exp.Shutdown(ctx)
			return exp.ExportSpans(ctx, spanStubs().Snapshots())
		}},
		{name: "otlpmetrichttp", skippedMetric: true, export: func() error {
			exp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(httpAddr), otlpmetrichttp.WithInsecure(), otlpmetrichttp.WithHeaders(auth))
			if err != nil {
				return err
			}
			defer exp.Shutdown(ctx)
			return exp.Export(ctx, metrics())
		}},
		{name: "otlpmetricgrpc", skippedMetric: true, export: func() error {
			exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(grpcAddr), otlpmetricgrpc.WithInsecure(), otlpmetricgrpc.WithHeaders(auth))
			if err != nil {
				return err
			}
			defer exp.Shutdown(ctx)
			return exp.Export(ctx, metrics())
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs0, spans0 := f.norm.seen()
			skipped0 := f.r.Counters().Cumulative()[protocol.CounterSkippedNotGenerative]
			if err := c.export(); err != nil {
				t.Fatalf("export: %v", err)
			}
			logs, spans := f.norm.seen()
			if logs-logs0 != c.logs || spans-spans0 != c.spans {
				t.Fatalf("normalizer saw %d log records and %d spans, want %d and %d", logs-logs0, spans-spans0, c.logs, c.spans)
			}
			skipped := f.r.Counters().Cumulative()[protocol.CounterSkippedNotGenerative] - skipped0
			if c.skippedMetric != (skipped == 1) {
				t.Fatalf("skipped_not_generative rose by %d", skipped)
			}
		})
	}
	f.norm.mu.Lock()
	for _, s := range f.norm.senders {
		if !s.Resolved || s.PID != uint32(os.Getpid()) || s.Person == nil || s.Person.UserRef != f.owner.ref() {
			t.Errorf("sender %+v is not this test process, the loopback client", s)
		}
	}
	f.norm.mu.Unlock()
	if h := f.r.Health(); h.State != protocol.StateHealthy || h.LastSuccess.IsZero() {
		t.Fatalf("health after exports: %s %q, last success %v", h.State, h.Detail, h.LastSuccess)
	}
}

func post(t *testing.T, url, contentType, token string, body []byte, gz bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHandBuiltJSONIsAccepted(t *testing.T) {
	f := started(t)
	body := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},
	  "scopeLogs":[{"scope":{"name":"t"},"logRecords":[{"timeUnixNano":"1700000000000000000","severityNumber":9,
	  "body":{"stringValue":"hello"},"traceId":"5B8EFFF798038103D269B633813FC60C","spanId":"EEE19B7EC3C1B174",
	  "aFieldFromAFutureVersion":true}]}]}]}`
	resp := post(t, "http://"+f.r.HTTPAddr()+"/v1/logs", "application/json", f.r.Token(), []byte(body), false)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || strings.TrimSpace(string(got)) != "{}" {
		t.Fatalf("got %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), got)
	}
	f.norm.mu.Lock()
	defer f.norm.mu.Unlock()
	if len(f.norm.logs) != 1 {
		t.Fatalf("normalizer got %d resources", len(f.norm.logs))
	}
	lr := f.norm.logs[0].ScopeLogs[0].LogRecords[0]
	if fmt.Sprintf("%X", lr.TraceId) != "5B8EFFF798038103D269B633813FC60C" || fmt.Sprintf("%X", lr.SpanId) != "EEE19B7EC3C1B174" {
		t.Fatalf("ids decoded as %X / %X, want the hex the request carried", lr.TraceId, lr.SpanId)
	}
}

func TestJSONTraceIDsAreHex(t *testing.T) {
	f := started(t)
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},
	  "scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174",
	  "parentSpanId":"0102030405060708","name":"chat","kind":2,
	  "links":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1112131415161718"}]}]}]}]}`
	resp := post(t, "http://"+f.r.HTTPAddr()+"/v1/traces", "application/json", f.r.Token(), []byte(body), false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	f.norm.mu.Lock()
	defer f.norm.mu.Unlock()
	s := f.norm.spans[0].ScopeSpans[0].Spans[0]
	if fmt.Sprintf("%x|%x|%x|%x|%x", s.TraceId, s.SpanId, s.ParentSpanId, s.Links[0].TraceId, s.Links[0].SpanId) !=
		"5b8efff798038103d269b633813fc60c|eee19b7ec3c1b174|0102030405060708|0102030405060708090a0b0c0d0e0f10|1112131415161718" {
		t.Fatalf("ids decoded wrongly: %x %x %x", s.TraceId, s.SpanId, s.ParentSpanId)
	}
}

func logsRequest(text string) []byte {
	b, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: tool}},
		}}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}},
		}}}},
	}}})
	if err != nil {
		panic(err)
	}
	return b
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func TestGzipProtobufIsAccepted(t *testing.T) {
	f := started(t)
	resp := post(t, "http://"+f.r.HTTPAddr()+"/v1/logs", "application/x-protobuf", f.r.Token(), gzipped(logsRequest("hello")), true)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if err := proto.Unmarshal(got, &collogspb.ExportLogsServiceResponse{}); err != nil {
		t.Fatalf("response is not an ExportLogsServiceResponse: %v", err)
	}
	if logs, _ := f.norm.seen(); logs != 1 {
		t.Fatalf("normalizer saw %d records", logs)
	}
}

// bodyProbe records whether anything read the request body.
type bodyProbe struct {
	io.Reader
	read bool
}

func (b *bodyProbe) Read(p []byte) (int, error) {
	b.read = true
	return b.Reader.Read(p)
}

func TestMissingOrWrongTokenIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	f := started(t)
	for name, header := range map[string]string{
		"missing":      "",
		"wrong":        "Bearer " + strings.Repeat("0", 64),
		"other scheme": "Basic " + f.r.Token(),
		"token only":   f.r.Token(),
	} {
		t.Run("http "+name, func(t *testing.T) {
			probe := &bodyProbe{Reader: bytes.NewReader(logsRequest("hello"))}
			req := httptest.NewRequest(http.MethodPost, "/v1/logs", probe)
			req.Header.Set("Content-Type", "application/x-protobuf")
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			w := httptest.NewRecorder()
			f.r.httpHandler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d", w.Code)
			}
			if probe.read {
				t.Fatal("the body was read before the token was checked")
			}
		})
	}

	// Over the listener as well, for every signal.
	for _, path := range []string{"/v1/logs", "/v1/traces", "/v1/metrics"} {
		resp := post(t, "http://"+f.r.HTTPAddr()+path, "application/x-protobuf", "", logsRequest("hello"), false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token: %d", path, resp.StatusCode)
		}
	}

	conn, err := grpc.NewClient(f.r.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := collogspb.NewLogsServiceClient(conn)
	req := &collogspb.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(logsRequest("hello"), req); err != nil {
		t.Fatal(err)
	}
	for name, md := range map[string]metadata.MD{
		"missing": nil,
		"wrong":   metadata.Pairs("authorization", "Bearer "+strings.Repeat("0", 64)),
	} {
		ctx := context.Background()
		if md != nil {
			ctx = metadata.NewOutgoingContext(ctx, md)
		}
		if _, err := client.Export(ctx, req); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("grpc %s token: %v", name, err)
		}
	}

	if logs, spans := f.norm.seen(); logs != 0 || spans != 0 {
		t.Fatalf("the normalizer saw %d log records and %d spans from unauthenticated requests", logs, spans)
	}
	if obs := f.r.Counters().Cumulative()[protocol.CounterObserved]; obs != 0 {
		t.Fatalf("observed %d records from unauthenticated requests", obs)
	}
}

func TestOversizedBodyGets413(t *testing.T) {
	f := started(t)
	big := make([]byte, 5<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	url := "http://" + f.r.HTTPAddr() + "/v1/logs"
	if resp := post(t, url, "application/x-protobuf", f.r.Token(), big, false); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("5 MiB body: %d", resp.StatusCode)
	}
	// A small gzip body that expands past the limit is refused the same way.
	if resp := post(t, url, "application/x-protobuf", f.r.Token(), gzipped(make([]byte, 5<<20)), true); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("5 MiB after decompression: %d", resp.StatusCode)
	}
	// A body sent without a length is cut off at the limit.
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", io.NopCloser(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+f.r.Token())
	w := httptest.NewRecorder()
	f.r.httpHandler().ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("5 MiB without a length: %d", w.Code)
	}
}

func TestRefusalsLogNoBodyContent(t *testing.T) {
	f := started(t)
	const canary = "CANARY-7f3a-prompt-text"
	url := "http://" + f.r.HTTPAddr() + "/v1/logs"
	post(t, url, "application/json", f.r.Token(), []byte(`{"resourceLogs":[{"`+canary+`":`), false)
	resp := post(t, url, "application/x-protobuf", f.r.Token(), []byte(canary), false)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("undecodable body: %d", resp.StatusCode)
	}
	if strings.Contains(string(got), canary) || strings.Contains(f.log.all(), canary) {
		t.Fatalf("a refusal quoted the body: response %q, log %q", got, f.log.all())
	}
	if !strings.Contains(f.log.all(), "/v1/logs") {
		t.Fatalf("refusals were not logged: %q", f.log.all())
	}
}

func TestRecordsWithoutANormalizerAreCountedObserved(t *testing.T) {
	r, err := New(Config{TokenPath: t.TempDir() + "/" + TokenFile, HTTPListen: "127.0.0.1:0", GRPCListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	resp := post(t, "http://"+r.HTTPAddr()+"/v1/logs", "application/x-protobuf", r.Token(), logsRequest("hello"), false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if obs := r.Counters().Cumulative()[protocol.CounterObserved]; obs != 1 {
		t.Fatalf("observed %d, want 1", obs)
	}
}

// freePort returns a loopback address nothing listens on at the time of the call.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestHeldPortFailsStartAndReleasesBoth(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	httpAddr := freePort(t)

	f := newReceiver(t, httpAddr, held.Addr().String())
	err = f.r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), string(protocol.DetailPortHeldByOther)) {
		t.Fatalf("Start with the grpc port held: %v", err)
	}
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		t.Fatalf("the http listener was not released: %v", err)
	}
	ln.Close()
	if h := f.r.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailPortHeldByOther {
		t.Fatalf("health after a held port: %s %q", h.State, h.Detail)
	}

	// Held the other way round, nothing is bound either.
	g := newReceiver(t, held.Addr().String(), freePort(t))
	if err := g.r.Start(context.Background()); err == nil {
		t.Fatal("Start with the http port held succeeded")
	}
	if g.r.HTTPAddr() != "" || g.r.GRPCAddr() != "" {
		t.Fatal("a listener is still bound after a failed Start")
	}
}

func TestNonLoopbackAddressIsRefused(t *testing.T) {
	f := newReceiver(t, "0.0.0.0:0", "127.0.0.1:0")
	if err := f.r.Start(context.Background()); err == nil {
		t.Fatal("Start bound a non-loopback address")
	}
	if f.r.HTTPAddr() != "" || f.r.GRPCAddr() != "" {
		t.Fatal("a listener is bound after a refused Start")
	}

	g := started(t)
	before := g.r.HTTPAddr()
	b := enabled("10.0.0.5:47318", "127.0.0.1:0")
	if err := g.r.ApplyPolicy(b); err == nil {
		t.Fatal("ApplyPolicy accepted a non-loopback address")
	}
	if g.r.HTTPAddr() != before {
		t.Fatal("a refused address changed the listener")
	}
}

func enabled(httpAddr, grpcAddr string) policy.Bundle {
	var b policy.Bundle
	b.Endpoint.OTel = policy.EndpointOTel{Enabled: true, HTTPListen: httpAddr, GRPCListen: grpcAddr}
	return b
}

func TestAddressChangeRebindsBoth(t *testing.T) {
	f := started(t)
	oldHTTP, oldGRPC := f.r.HTTPAddr(), f.r.GRPCAddr()
	newHTTP, newGRPC := freePort(t), freePort(t)
	if err := f.r.ApplyPolicy(enabled(newHTTP, newGRPC)); err != nil {
		t.Fatal(err)
	}
	if f.r.HTTPAddr() != newHTTP || f.r.GRPCAddr() != newGRPC {
		t.Fatalf("bound %s and %s, want %s and %s", f.r.HTTPAddr(), f.r.GRPCAddr(), newHTTP, newGRPC)
	}
	for _, a := range []string{oldHTTP, oldGRPC} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatalf("old address %s is still held: %v", a, err)
		}
		ln.Close()
	}
	resp := post(t, "http://"+newHTTP+"/v1/logs", "application/x-protobuf", f.r.Token(), logsRequest("hello"), false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export to the new address: %d", resp.StatusCode)
	}
	if h := f.r.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health after the rebind: %s %q", h.State, h.Detail)
	}

	// The same addresses again change nothing.
	if err := f.r.ApplyPolicy(enabled(newHTTP, newGRPC)); err != nil || f.r.HTTPAddr() != newHTTP {
		t.Fatalf("reapplying the same addresses: %v", err)
	}

	// A new address another process holds leaves the receiver degraded, the other listener serving.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := f.r.ApplyPolicy(enabled(newHTTP, held.Addr().String())); err == nil {
		t.Fatal("rebinding onto a held port reported success")
	}
	if h := f.r.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailPortHeldByOther {
		t.Fatalf("health with the grpc port held: %s %q", h.State, h.Detail)
	}
	if f.r.HTTPAddr() != newHTTP {
		t.Fatal("the http listener is not serving")
	}
}

func TestStopReleasesAndStartsAgain(t *testing.T) {
	f := started(t)
	addr := f.r.HTTPAddr()
	if err := f.r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := f.r.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after Stop: %s", h.State)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Stop did not release %s: %v", addr, err)
	}
	ln.Close()
	if err := f.r.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	if h := f.r.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health after a second Start: %s", h.State)
	}
}

func TestEnabledFollowsTheBundle(t *testing.T) {
	f := newReceiver(t, "127.0.0.1:0", "127.0.0.1:0")
	if f.r.Enabled(nil) {
		t.Fatal("enabled with no bundle in force")
	}
	if f.r.Enabled(&policy.Bundle{}) {
		t.Fatal("enabled by a bundle without the otel switch")
	}
	b := enabled("127.0.0.1:47318", "127.0.0.1:47317")
	if !f.r.Enabled(&b) {
		t.Fatal("not enabled by endpoint.otel.enabled")
	}
	if f.r.Name() != protocol.CollectorOTelReceiver {
		t.Fatalf("collector %q", f.r.Name())
	}
}

func TestTokenIsCreatedOnceAndProtected(t *testing.T) {
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := dir.Path(TokenFile)
	a, err := New(Config{TokenPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if !validToken(a.Token()) || len(a.Token()) != 64 {
		t.Fatalf("token %q is not 32 bytes of hex", a.Token())
	}
	if err := state.CheckFile(path); err != nil {
		t.Fatalf("token file is not protected: %v", err)
	}
	b, err := New(Config{TokenPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if b.Token() != a.Token() {
		t.Fatal("a second receiver made a new token")
	}
	stored, _ := os.ReadFile(path)
	if string(stored) != a.Token() {
		t.Fatalf("file holds %q", stored)
	}

	// A file that does not hold a token is replaced.
	if err := os.WriteFile(path, []byte("not a token"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{TokenPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if c.Token() == a.Token() || !validToken(c.Token()) {
		t.Fatalf("a malformed token file was kept: %q", c.Token())
	}
}
