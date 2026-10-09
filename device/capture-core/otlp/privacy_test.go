package otlp_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/tabwriter"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/otlp/normalizers"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// A canary prompt is exported to the real receiver for every registered normalizer, at every mode,
// and searched for in everything the device writes or sends: the spool (through the spool's own
// reader, and its files as stored), the content store, the drained /v1/events bodies, the
// /v1/health report and every log line. The fixtures are testdata/privacy/<normalizer name>/*.json,
// OTLP/JSON export requests (logs-*.json or traces-*.json) in which {{CANARY}} marks the prompt text
// and {{DROPPED_CANARY}} a value the normalizer must drop. Each run replaces both with a fresh
// SAC-CANARY-<16 hex>. A registered normalizer without fixtures, or without one that carries a
// dropped value, fails the test.
//
// What each mode allows the prompt canary:
//   - m0: nothing anywhere, and the content reader is never called;
//   - m1: nothing in uploads, spool, logs, health or the content store; the envelope carries a
//     content digest and labels;
//   - m2: only inside content_excerpt.text, as the classifier's excerpt;
//   - m3: nothing in uploads, spool, logs or health; the content store holds it.
//
// The dropped canary is allowed nowhere at any mode.

const (
	privacyTenant = "44444444-4444-4444-8444-444444444444"
	privacyDevice = "aaaaaaaa-0000-7000-8000-000000000035"
	privacyUser   = "u_privacy"

	placeholderCanary  = "{{CANARY}}"
	placeholderDropped = "{{DROPPED_CANARY}}"
)

var privacyModes = []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM1, protocol.ModeM2, protocol.ModeM3}

func newCanary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "SAC-CANARY-" + hex.EncodeToString(b)
}

// encodings is the canary as text, as hex and as base64 at each byte alignment, less the
// characters an aligning prefix or the padding changes.
func encodings(canary string) []string {
	out := []string{canary, hex.EncodeToString([]byte(canary))}
	for pad := 0; pad < 3; pad++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, pad), canary...))
		start := (pad*8 + 5) / 6
		out = append(out, enc[start:len(enc)-4])
	}
	return out
}

// hits counts the canary in b in any of its encodings.
func hits(b []byte, canary string) int {
	n := 0
	for _, e := range encodings(canary) {
		n += bytes.Count(b, []byte(e))
	}
	return n
}

// excerptHits counts the canary inside content_excerpt.text of every JSON object in doc.
func excerptHits(doc []byte, canary string) (n int, excerpts []string) {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return 0, nil
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if ex, ok := e.(map[string]any); ok && k == "content_excerpt" {
					if s, ok := ex["text"].(string); ok {
						excerpts = append(excerpts, s)
						n += strings.Count(s, canary)
					}
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return n, excerpts
}

// logBuffer captures every log line of the device components under test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format, args...)
	l.buf.WriteByte('\n')
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf.Bytes()...)
}

// watchedPipeline forwards to the real pipeline and counts every call of the content readers the
// normalizers hand it.
type watchedPipeline struct {
	*core.Pipeline
	reads atomic.Int64
}

type watchedReader struct {
	r     core.ContentReader
	reads *atomic.Int64
}

func (w watchedReader) Read(ctx context.Context) ([]byte, error) {
	w.reads.Add(1)
	return w.r.Read(ctx)
}

func (p *watchedPipeline) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	if obs.Content != nil {
		obs.Content = watchedReader{r: obs.Content, reads: &p.reads}
	}
	for i, a := range obs.AttachmentContent {
		if a.Content != nil {
			obs.AttachmentContent[i].Content = watchedReader{r: a.Content, reads: &p.reads}
		}
	}
	return p.Pipeline.Process(ctx, obs)
}

// canaryClassifier stands in for classifier-host: a rule that matches the canary, whose M2 excerpt
// is the matched canary.
type canaryClassifier struct {
	calls atomic.Int64

	mu       sync.Mutex
	excerpts []string
}

func (c *canaryClassifier) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	resp, err := c.classify(req)
	if err == nil && resp.Excerpt != nil {
		c.mu.Lock()
		c.excerpts = append(c.excerpts, resp.Excerpt.Text)
		c.mu.Unlock()
	}
	return resp, err
}

func (c *canaryClassifier) given() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.excerpts...)
}

func (c *canaryClassifier) classify(req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.calls.Add(1)
	resp := protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: "privacy-1",
		Confidence:        protocol.ConfidenceHigh,
	}
	text := string(req.Content)
	i := strings.Index(text, "SAC-CANARY-")
	if i < 0 {
		return resp, nil
	}
	span := text[i : i+len("SAC-CANARY-")+16]
	resp.Labels = []protocol.Label{{Class: "health", Score: 0.9, RuleID: "PRIVACY_CANARY"}}
	if req.Mode == protocol.ModeM2 {
		resp.Excerpt = &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: span, MatchType: "health", OffsetStart: i, OffsetEnd: i + len(span)}
	}
	return resp, nil
}

// privacyFixture is one export request, with its placeholders, for one normalizer.
type privacyFixture struct {
	name    string
	path    string // the receiver's endpoint
	body    string
	dropped bool
}

func (f privacyFixture) with(canary, dropped string) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(f.body, placeholderCanary, canary), placeholderDropped, dropped))
}

// fixturesFor loads a normalizer's privacy fixtures and checks that the receiver hands each of them
// to that normalizer.
func fixturesFor(t *testing.T, n otlp.Normalizer, all []otlp.Normalizer) []privacyFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "privacy", n.Name(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("normalizer %q has no privacy fixtures in testdata/privacy/%s", n.Name(), n.Name())
	}
	var out []privacyFixture
	anyDropped := false
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f := privacyFixture{name: filepath.Base(file), body: string(raw), dropped: strings.Contains(string(raw), placeholderDropped)}
		if !strings.Contains(f.body, placeholderCanary) {
			t.Fatalf("%s has no %s prompt text", file, placeholderCanary)
		}
		anyDropped = anyDropped || f.dropped
		var services []string
		decoded := f.with("SAC-CANARY-0000000000000000", "SAC-CANARY-1111111111111111")
		switch {
		case strings.HasPrefix(f.name, "logs-"):
			f.path = "/v1/logs"
			req := &collogspb.ExportLogsServiceRequest{}
			if err := protojson.Unmarshal(decoded, req); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, rl := range req.GetResourceLogs() {
				services = append(services, serviceName(rl.GetResource().GetAttributes()))
			}
		case strings.HasPrefix(f.name, "traces-"):
			f.path = "/v1/traces"
			req := &coltracepb.ExportTraceServiceRequest{}
			if err := protojson.Unmarshal(decoded, req); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, rs := range req.GetResourceSpans() {
				services = append(services, serviceName(rs.GetResource().GetAttributes()))
			}
		default:
			t.Fatalf("%s: a privacy fixture is named logs-*.json or traces-*.json", file)
		}
		for _, s := range services {
			if got := firstAccepting(all, s); got != n.Name() {
				t.Fatalf("%s: service.name %q goes to normalizer %q, not %q", file, s, got, n.Name())
			}
		}
		out = append(out, f)
	}
	if !anyDropped {
		t.Fatalf("normalizer %q has no privacy fixture with a %s value it drops", n.Name(), placeholderDropped)
	}
	return out
}

func serviceName(attrs []*commonpb.KeyValue) string {
	for _, kv := range attrs {
		if kv.GetKey() == "service.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func firstAccepting(all []otlp.Normalizer, service string) string {
	for _, n := range all {
		if n.Accepts(service) {
			return n.Name()
		}
	}
	return ""
}

func privacyBundle(mode protocol.CollectionMode) *policy.Bundle {
	return &policy.Bundle{
		Version:         "PRIVACY-1",
		EffectiveAt:     time.Now().Add(-time.Hour),
		TenantDefault:   mode,
		ToolModes:       map[string]protocol.CollectionMode{},
		PopulationModes: map[string]protocol.CollectionMode{},
		DeviceModes:     map[string]protocol.CollectionMode{},
		ClassModes:      map[string]protocol.CollectionMode{},
	}
}

// fakeEdge is the device edge over TLS: it accepts every event and every health report and keeps
// each request body as received.
type fakeEdge struct {
	srv    *httptest.Server
	caFile string

	mu     sync.Mutex
	events [][]byte
	health [][]byte
}

func newFakeEdge(t *testing.T) *fakeEdge {
	t.Helper()
	e := &fakeEdge{}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rd io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "bad gzip", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			rd = gz
		}
		body, err := io.ReadAll(rd)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		now := time.Now().UTC()
		switch r.URL.Path {
		case "/v1/events":
			var batch protocol.EventBatch
			if err := json.Unmarshal(body, &batch); err != nil {
				http.Error(w, "bad batch", http.StatusBadRequest)
				return
			}
			e.mu.Lock()
			e.events = append(e.events, body)
			e.mu.Unlock()
			results := make([]protocol.EventResult, len(batch.Events))
			for i, ev := range batch.Events {
				var env struct {
					EventID string `json:"event_id"`
				}
				_ = json.Unmarshal(ev, &env)
				results[i] = protocol.EventResult{EventID: env.EventID, Outcome: protocol.OutcomeAccepted}
			}
			_ = json.NewEncoder(w).Encode(protocol.EventBatchResponse{
				SchemaVersion: "1.0", BatchID: batch.BatchID, ReceivedAt: now, ServerTime: now,
				Counts: protocol.BatchCounts{Accepted: len(results)}, Results: results,
			})
		case "/v1/health":
			e.mu.Lock()
			e.health = append(e.health, body)
			e.mu.Unlock()
			_ = json.NewEncoder(w).Encode(protocol.HealthResponse{AckedAt: now, ServerTime: now, NextReportAfterS: 60})
		default:
			http.NotFound(w, r)
		}
	}))
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	e.caFile = filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(e.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *fakeEdge) received() (events, health [][]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]byte(nil), e.events...), append([][]byte(nil), e.health...)
}

// deviceCredential is a stored credential for the drain, so it is already enrolled.
func deviceCredential(t *testing.T, dir string) *credential.Store {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(35), Subject: pkix.Name{CommonName: privacyDevice},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := credential.Open(filepath.Join(dir, "credential.sealed"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&credential.Credential{
		DeviceID: privacyDevice, TenantID: privacyTenant, HardwareIdentityHash: "sha256:privacy",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CertPEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		NotAfter:   notAfter,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func randomKey(t *testing.T, n int) []byte {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// filesUnder is the content of every file under dir.
func filesUnder(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// privacyRow is one run's count of canary hits where each mode allows none (excerpt is the
// allowed count inside content_excerpt.text, content the count in the content store).
type privacyRow struct {
	normalizer, fixture string
	mode                protocol.CollectionMode
	reads               int64
	uploads, excerpt    int
	spool, content      int
	logs, health        int
	dropped             int
}

// privacyRun is what one run wrote and sent.
type privacyRun struct {
	prompts []map[string]any // the prompt envelopes the edge received
	// excerpts are the uploaded content_excerpt texts, and classified the classifier's own.
	excerpts, classified []string
	row                  privacyRow
}

// runPrivacy exports one fixture to a fresh receiver whose normalizers are the registered ones, over
// a real pipeline, spool and content store, then drains to the fake edge, reports health, and
// counts the canaries in everything written and sent.
func runPrivacy(t *testing.T, normalizer string, f privacyFixture, mode protocol.CollectionMode, canary, dropped string) privacyRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	logs := &logBuffer{}

	spoolDir := filepath.Join(dir, "spool")
	sp, err := capturespool.Open(capturespool.Config{Dir: spoolDir, Key: randomKey(t, capturespool.KeySize), SyncEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	contentDir := filepath.Join(dir, "content")
	store, err := contentstore.Open(contentDir, randomKey(t, 32), time.Now)
	if err != nil {
		t.Fatal(err)
	}

	pipe, err := core.NewPipeline(sp, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	pipe.SetIdentity(core.Identity{TenantID: privacyTenant, DeviceID: privacyDevice, UserRef: privacyUser})
	pipe.Bundles = func() *policy.Bundle { return privacyBundle(mode) }
	cl := &canaryClassifier{}
	pipe.Classifier = cl
	pipe.Content = store
	pipe.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	watched := &watchedPipeline{Pipeline: pipe}

	counters := core.NewCounterSet(time.Now())
	rec, err := otlp.New(otlp.Config{
		TokenPath:  filepath.Join(dir, otlp.TokenFile),
		HTTPListen: "127.0.0.1:0",
		GRPCListen: "127.0.0.1:0",
		Normalizers: normalizers.Registered(normalizers.Deps{
			Pipeline: watched, Bundles: pipe.Bundles, Counters: counters, Log: logs, Clock: time.Now,
		}),
		Counters: counters,
		Log:      logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Stop(ctx) }()

	// The export, then the same export cut short after the canary, which the receiver refuses.
	body := f.with(canary, dropped)
	cut := bytes.Index(body, []byte(canary)) + len(canary) + 3
	for i, b := range [][]byte{body, body[:cut]} {
		req, err := http.NewRequest(http.MethodPost, "http://"+rec.HTTPAddr()+f.path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+rec.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if want := []int{http.StatusOK, http.StatusBadRequest}[i]; resp.StatusCode != want {
			t.Fatalf("export %d answered %d, want %d", i, resp.StatusCode, want)
		}
	}

	var run privacyRun
	run.row = privacyRow{normalizer: normalizer, fixture: f.name, mode: mode, reads: watched.reads.Load()}
	if mode == protocol.ModeM0 && cl.calls.Load() != 0 {
		t.Errorf("the classifier was called %d times at m0", cl.calls.Load())
	}
	count := func(b []byte, allowExcerpt bool) (outside, inside int) {
		all := hits(b, canary)
		run.row.dropped += hits(b, dropped)
		if allowExcerpt {
			inside, _ = excerptHits(b, canary)
		}
		return all - inside, inside
	}

	// The spool, through its own reader, and its files as stored.
	entries, err := sp.Peek(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was spooled")
	}
	for _, e := range entries {
		out, _ := count(e.Payload, true)
		run.row.spool += out
		e.Payload = nil
		meta, _ := json.Marshal(e)
		out, _ = count(meta, false)
		run.row.spool += out
	}
	for _, b := range filesUnder(t, spoolDir) {
		out, _ := count(b, false)
		run.row.spool += out
	}

	// The content store: each object opened through the store, and its files as stored.
	for path, b := range filesUnder(t, contentDir) {
		out, _ := count(b, false)
		run.row.content += out
		if id, ok := strings.CutSuffix(filepath.Base(path), ".sealed"); ok {
			plain, err := store.Get(id)
			if err != nil {
				t.Fatalf("content object %s: %v", id, err)
			}
			out, _ := count(plain, false)
			run.row.content += out
		}
	}

	// The drain to /v1/events and a health report to /v1/health.
	edge := newFakeEdge(t)
	d, err := drain.New(drain.Config{
		Endpoint: edge.srv.URL, CAFile: edge.caFile, TenantID: privacyTenant, AgentVersion: "privacy",
		BackoffBase: time.Millisecond, BackoffCap: 50 * time.Millisecond, DrainInterval: time.Second,
	}, func() (protocol.Store, error) { return sp, nil }, deviceCredential(t, dir), logs, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Drain(ctx, time.Now().Add(10*time.Second))
	if err != nil || res.Delivered != len(entries) {
		t.Fatalf("drain delivered %d of %d (%v)", res.Delivered, len(entries), err)
	}
	row := rec.Health().Report("", "privacy")
	row.Collector = string(rec.Name())
	st := sp.Stats()
	if _, err := d.ReportHealth(ctx, protocol.HealthRequest{
		SchemaVersion: protocol.HealthSchemaVersion, ReportedAt: time.Now().UTC(), AgentVersion: "privacy",
		CollectionMode: string(mode), PolicyBundleVersion: "PRIVACY-1",
		Spool:      protocol.SpoolHealth{DepthEvents: int64(st.Depth), DroppedTotal: st.DroppedTotal, RejectedTotal: st.RejectedTotal},
		Collectors: []protocol.HealthReport{row},
	}); err != nil {
		t.Fatalf("health report: %v", err)
	}
	events, health := edge.received()
	for _, b := range events {
		out, in := count(b, true)
		run.row.uploads += out
		run.row.excerpt += in
		_, ex := excerptHits(b, canary)
		run.excerpts = append(run.excerpts, ex...)
		var batch protocol.EventBatch
		if err := json.Unmarshal(b, &batch); err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch.Events {
			var env map[string]any
			if err := json.Unmarshal(ev, &env); err != nil {
				t.Fatal(err)
			}
			if env["kind"] == string(protocol.KindPrompt) {
				run.prompts = append(run.prompts, env)
			}
		}
	}
	for _, b := range health {
		out, _ := count(b, false)
		run.row.health += out
	}
	if len(health) != 1 {
		t.Fatalf("the edge received %d health reports, want 1", len(health))
	}

	_ = rec.Stop(ctx)
	lines := logs.bytes()
	for _, want := range []string{"envelope spooled", "request of", "drain:"} {
		if !bytes.Contains(lines, []byte(want)) {
			t.Fatalf("the captured log has no %q line, so it is not the components' log:\n%s", want, lines)
		}
	}
	out, _ := count(lines, false)
	run.row.logs = out
	run.classified = cl.given()
	return run
}

// checkPrivacy holds one run to what its mode allows.
func checkPrivacy(t *testing.T, run privacyRun) {
	t.Helper()
	r := run.row
	if r.dropped != 0 {
		t.Errorf("the dropped value's canary was found %d times", r.dropped)
	}
	if r.uploads != 0 || r.spool != 0 || r.logs != 0 || r.health != 0 {
		t.Errorf("the prompt canary was found outside what %s allows: uploads %d, spool %d, logs %d, health %d", r.mode, r.uploads, r.spool, r.logs, r.health)
	}
	if len(run.prompts) != 1 {
		t.Fatalf("the edge received %d prompt envelopes, want 1", len(run.prompts))
	}
	env := run.prompts[0]
	_, hasDigest := env["content_digest"]
	labels, _ := env["labels"].([]any)
	_, hasExcerpt := env["content_excerpt"]
	switch r.mode {
	case protocol.ModeM0:
		if r.reads != 0 {
			t.Errorf("the content reader was called %d times at m0", r.reads)
		}
		if hasDigest || env["labels"] != nil || hasExcerpt {
			t.Errorf("the m0 envelope carries content-derived fields: %v", env)
		}
	default:
		if r.reads == 0 {
			t.Errorf("the content reader was never called at %s", r.mode)
		}
		if !hasDigest || len(labels) == 0 {
			t.Errorf("the %s envelope carries no content digest or no labels: %v", r.mode, env)
		}
	}
	if r.mode == protocol.ModeM2 {
		if !hasExcerpt {
			t.Errorf("the m2 envelope carries no content_excerpt")
		}
		// Each uploaded excerpt is one the classifier returned, within the excerpt limit, or the
		// empty redacted window of an unclassified prompt.
		for _, ex := range run.excerpts {
			if len([]rune(ex)) > protocol.MaxExcerptChars || (ex != "" && !slices.Contains(run.classified, ex)) {
				t.Errorf("the m2 excerpt %q is not the classifier's excerpt %q", ex, run.classified)
			}
		}
	} else if r.excerpt != 0 || hasExcerpt {
		t.Errorf("a %s envelope carries an excerpt", r.mode)
	}
	if r.mode == protocol.ModeM3 {
		if r.content == 0 {
			t.Errorf("the m3 content store does not hold the prompt")
		}
	} else if r.content != 0 {
		t.Errorf("the content store holds the canary %d times at %s", r.content, r.mode)
	}
}

func printPrivacyTable(t *testing.T, rows []privacyRow) {
	t.Helper()
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "normalizer\tfixture\tmode\treads\tuploads\texcerpt\tspool\tcontent\tlogs\thealth\tdropped")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			r.normalizer, r.fixture, r.mode, r.reads, r.uploads, r.excerpt, r.spool, r.content, r.logs, r.health, r.dropped)
	}
	_ = w.Flush()
	t.Logf("canary hits per normalizer and mode (excerpt: inside content_excerpt.text; content: in the content store; dropped: the dropped value's canary anywhere):\n%s", b.String())
}

func TestCanaryPromptStaysWithinItsMode(t *testing.T) {
	registered := normalizers.Registered(normalizers.Deps{})
	if len(registered) == 0 {
		t.Fatal("no normalizers are registered")
	}
	var rows []privacyRow
	for _, n := range registered {
		fixtures := fixturesFor(t, n, registered)
		for _, f := range fixtures {
			for _, mode := range privacyModes {
				t.Run(n.Name()+"/"+f.name+"/"+string(mode), func(t *testing.T) {
					run := runPrivacy(t, n.Name(), f, mode, newCanary(t), newCanary(t))
					rows = append(rows, run.row)
					checkPrivacy(t, run)
				})
			}
		}
	}
	printPrivacyTable(t, rows)
}
