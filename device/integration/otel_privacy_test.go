package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/tabwriter"
	"time"

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

// A canary prompt goes through the whole OTel path for every registered normalizer at every mode:
// an OTLP/JSON export to the real receiver, the registered normalizers, the real pipeline with the
// real classifier-host under its supervisor, the real spool and content store, and the real drain to
// the edge's /v1/events with a health report to /v1/health. The canary is then searched for in the
// edge's request bodies, the spool, the content store and every log line. The fixtures are capture-core's
// otlp/testdata/privacy/<normalizer name>/*.json, where {{CANARY}} marks the prompt text and
// {{DROPPED_CANARY}} a value the normalizer drops; each run replaces them with fresh canaries.
//
// What each mode allows the prompt canary: nothing at m0, where the content reader is never called;
// nothing at m1, where the envelope carries a digest and labels; at m2 only what classifier-host
// returns as the excerpt, inside content_excerpt.text; at m3 only the content store. The dropped
// canary is allowed nowhere.

const (
	otelCanaryMark  = "{{CANARY}}"
	otelDroppedMark = "{{DROPPED_CANARY}}"
)

func otelCanary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "SAC-CANARY-" + hex.EncodeToString(b)
}

// canaryHits counts the canary in b as text, as hex, and as base64 at each byte alignment.
func canaryHits(b []byte, canary string) int {
	forms := []string{canary, hex.EncodeToString([]byte(canary))}
	for pad := 0; pad < 3; pad++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, pad), canary...))
		forms = append(forms, enc[(pad*8+5)/6:len(enc)-4])
	}
	n := 0
	for _, f := range forms {
		n += bytes.Count(b, []byte(f))
	}
	return n
}

// excerptTexts is every content_excerpt.text in a JSON document.
func excerptTexts(doc []byte) []string {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if ex, ok := e.(map[string]any); ok && k == "content_excerpt" {
					if s, ok := ex["text"].(string); ok {
						out = append(out, s)
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
	return out
}

// privacyLog captures the log lines of the components under test.
type privacyLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *privacyLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format, args...)
	l.buf.WriteByte('\n')
}

func (l *privacyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *privacyLog) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf.Bytes()...)
}

// readWatch forwards to the real pipeline and counts every call of the content readers it is given.
type readWatch struct {
	*core.Pipeline
	reads atomic.Int64
}

type watchedContent struct {
	r     core.ContentReader
	reads *atomic.Int64
}

func (w watchedContent) Read(ctx context.Context) ([]byte, error) {
	w.reads.Add(1)
	return w.r.Read(ctx)
}

func (p *readWatch) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	if obs.Content != nil {
		obs.Content = watchedContent{r: obs.Content, reads: &p.reads}
	}
	return p.Pipeline.Process(ctx, obs)
}

// excerptWatch forwards to the classifier and keeps the excerpts it returns.
type excerptWatch struct {
	core.Classifier
	mu       sync.Mutex
	calls    int
	excerpts []string
}

func (c *excerptWatch) Classify(ctx context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	resp, err := c.Classifier.Classify(ctx, req)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if err == nil && resp.Excerpt != nil {
		c.excerpts = append(c.excerpts, resp.Excerpt.Text)
	}
	return resp, err
}

type otelFixture struct {
	name, path, body string
}

func otelFixtures(t *testing.T, normalizer string) []otelFixture {
	t.Helper()
	dir := filepath.Join("..", "capture-core", "otlp", "testdata", "privacy", normalizer)
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("normalizer %q has no privacy fixtures in %s", normalizer, dir)
	}
	var out []otelFixture
	dropped := false
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f := otelFixture{name: filepath.Base(file), path: "/v1/traces", body: string(raw)}
		if strings.HasPrefix(f.name, "logs-") {
			f.path = "/v1/logs"
		}
		dropped = dropped || strings.Contains(f.body, otelDroppedMark)
		out = append(out, f)
	}
	if !dropped {
		t.Fatalf("normalizer %q has no privacy fixture with a value it drops", normalizer)
	}
	return out
}

type otelPrivacyRow struct {
	normalizer, fixture string
	mode                protocol.CollectionMode
	reads               int64
	uploads, excerpt    int
	spool, content      int
	logs, health        int
	dropped             int
}

func TestOTelPath_CanaryPromptStaysWithinItsMode(t *testing.T) {
	hostLog := &privacyLog{}
	cl, host := startClassifierHost(t, hostLog)
	schema := deviceSubmissionSchema(t)
	deviceCA, deviceCAKey := mustCA(t, "integration device CA")

	var rows []otelPrivacyRow
	for _, n := range normalizers.Registered(normalizers.Deps{}) {
		for _, f := range otelFixtures(t, n.Name()) {
			for _, mode := range []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM1, protocol.ModeM2, protocol.ModeM3} {
				t.Run(n.Name()+"/"+f.name+"/"+string(mode), func(t *testing.T) {
					ctx := context.Background()
					canary, dropped := otelCanary(t), otelCanary(t)
					row := otelPrivacyRow{normalizer: n.Name(), fixture: f.name, mode: mode}
					defer func() { rows = append(rows, row) }()
					count := func(b []byte, allowExcerpt bool) (outside, inside int) {
						row.dropped += canaryHits(b, dropped)
						if allowExcerpt {
							for _, ex := range excerptTexts(b) {
								inside += strings.Count(ex, canary)
							}
						}
						return canaryHits(b, canary) - inside, inside
					}

					dir := t.TempDir()
					logs := &privacyLog{}
					spoolKey, contentKey := make([]byte, capturespool.KeySize), make([]byte, 32)
					_, _ = rand.Read(spoolKey)
					_, _ = rand.Read(contentKey)
					spoolDir := filepath.Join(dir, "spool")
					sp, err := capturespool.Open(capturespool.Config{Dir: spoolDir, Key: spoolKey, SyncEvery: -1})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = sp.Close() })
					contentDir := filepath.Join(dir, "content")
					store, err := contentstore.Open(contentDir, contentKey, time.Now)
					if err != nil {
						t.Fatal(err)
					}
					p, err := core.NewPipeline(sp, time.Now, nil)
					if err != nil {
						t.Fatal(err)
					}
					p.SetIdentity(core.Identity{TenantID: testTenant, DeviceID: testDevice, UserRef: testUser})
					p.Bundles = func() *policy.Bundle { return bundleWith(mode) }
					classifier := &excerptWatch{Classifier: cl}
					p.Classifier = classifier
					p.ClassifyBudget = 30 * time.Second
					p.Content = store
					p.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
					watched := &readWatch{Pipeline: p}

					counters := core.NewCounterSet(time.Now())
					rec, err := otlp.New(otlp.Config{
						TokenPath: filepath.Join(dir, otlp.TokenFile), HTTPListen: "127.0.0.1:0", GRPCListen: "127.0.0.1:0",
						Normalizers: normalizers.Registered(normalizers.Deps{
							Pipeline: watched, Bundles: p.Bundles, Counters: counters, Log: logs, Clock: time.Now,
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

					// The export, then the same export cut short after the canary, which is refused.
					body := []byte(strings.ReplaceAll(strings.ReplaceAll(f.body, otelCanaryMark, canary), otelDroppedMark, dropped))
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
					row.reads = watched.reads.Load()

					// The spool through its own reader, and its files as stored.
					entries, err := sp.Peek(1000)
					if err != nil || len(entries) == 0 {
						t.Fatalf("the spool holds %d entries (%v)", len(entries), err)
					}
					for _, e := range entries {
						out, _ := count(e.Payload, true)
						row.spool += out
						e.Payload = nil
						meta, _ := json.Marshal(e)
						out, _ = count(meta, false)
						row.spool += out
					}
					// The content store: each object opened through the store, and its files as stored.
					for _, d := range []string{spoolDir, contentDir} {
						err := filepath.WalkDir(d, func(path string, de fs.DirEntry, err error) error {
							if err != nil || de.IsDir() {
								return err
							}
							raw, err := os.ReadFile(path)
							if err != nil {
								return err
							}
							out, _ := count(raw, false)
							if d == spoolDir {
								row.spool += out
								return nil
							}
							row.content += out
							if id, ok := strings.CutSuffix(de.Name(), ".sealed"); ok {
								plain, err := store.Get(id)
								if err != nil {
									return err
								}
								out, _ := count(plain, false)
								row.content += out
							}
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
					}

					// The drain to the edge, and one health report with the receiver's and the
					// classifier host's rows.
					peer := &ingestPeer{clientRoot: x509.NewCertPool()}
					peer.clientRoot.AddCert(deviceCA)
					srv, caFile := startTLSIngest(t, peer)
					creds, err := credential.Open(filepath.Join(dir, "credential.sealed"), make([]byte, 32))
					if err != nil {
						t.Fatal(err)
					}
					if err := creds.Save(issuedCredential(t, deviceCA, deviceCAKey)); err != nil {
						t.Fatal(err)
					}
					d, err := drain.New(drain.Config{
						Endpoint: srv.URL, CAFile: caFile, TenantID: testTenant, AgentVersion: "integration",
						BackoffBase: time.Millisecond, BackoffCap: 50 * time.Millisecond, DrainInterval: time.Second,
					}, func() (protocol.Store, error) { return sp, nil }, creds, logs, time.Now)
					if err != nil {
						t.Fatal(err)
					}
					res, err := d.Drain(ctx, time.Now().Add(10*time.Second))
					if err != nil || res.Delivered != len(entries) {
						t.Fatalf("drain delivered %d of %d (%v)", res.Delivered, len(entries), err)
					}
					var reports []protocol.HealthReport
					for _, prov := range []core.Provider{rec, host} {
						r := prov.Health().Report("", "integration")
						r.Collector = string(prov.Name())
						reports = append(reports, r)
					}
					st := sp.Stats()
					if _, err := d.ReportHealth(ctx, protocol.HealthRequest{
						SchemaVersion: protocol.HealthSchemaVersion, ReportedAt: time.Now().UTC(), AgentVersion: "integration",
						CollectionMode: string(mode), PolicyBundleVersion: "INTEGRATION-1",
						Spool:      protocol.SpoolHealth{DepthEvents: int64(st.Depth), DroppedTotal: st.DroppedTotal, RejectedTotal: st.RejectedTotal},
						Collectors: reports,
					}); err != nil {
						t.Fatalf("health report: %v", err)
					}

					peer.mu.Lock()
					uploads, health := peer.bodies, peer.health
					peer.mu.Unlock()
					if len(health) != 1 {
						t.Fatalf("the edge received %d health reports, want 1", len(health))
					}
					var prompts []map[string]any
					var excerpts []string
					for _, b := range uploads {
						out, in := count(b, true)
						row.uploads += out
						row.excerpt += in
						excerpts = append(excerpts, excerptTexts(b)...)
						var batch protocol.EventBatch
						if err := json.Unmarshal(b, &batch); err != nil {
							t.Fatal(err)
						}
						for _, ev := range batch.Events {
							if err := acceptLikeIngest(t, schema, ev); err != nil {
								t.Errorf("ingest would refuse the envelope: %v\n%s", err, ev)
							}
							var env map[string]any
							if err := json.Unmarshal(ev, &env); err != nil {
								t.Fatal(err)
							}
							if env["kind"] == string(protocol.KindPrompt) {
								prompts = append(prompts, env)
							}
						}
					}
					out, _ := count(health[0], false)
					row.health = out
					_ = rec.Stop(ctx)
					lines := append(logs.bytes(), hostLog.bytes()...)
					for _, want := range []string{"envelope spooled", "request of", "drain:"} {
						if !bytes.Contains(lines, []byte(want)) {
							t.Fatalf("the captured log has no %q line, so it is not the components' log", want)
						}
					}
					row.logs, _ = count(lines, false)

					// What the mode allows.
					if row.dropped != 0 {
						t.Errorf("the dropped value's canary was found %d times", row.dropped)
					}
					if row.uploads != 0 || row.spool != 0 || row.logs != 0 || row.health != 0 {
						t.Errorf("the prompt canary was found outside what %s allows: uploads %d, spool %d, logs %d, health %d", mode, row.uploads, row.spool, row.logs, row.health)
					}
					if len(prompts) != 1 {
						t.Fatalf("the edge received %d prompt envelopes, want 1", len(prompts))
					}
					env := prompts[0]
					_, hasDigest := env["content_digest"]
					labels, _ := env["labels"].([]any)
					_, hasExcerpt := env["content_excerpt"]
					classifier.mu.Lock()
					calls, given := classifier.calls, classifier.excerpts
					classifier.mu.Unlock()
					if mode == protocol.ModeM0 {
						if row.reads != 0 || calls != 0 {
							t.Errorf("at m0 the content reader was called %d times and the classifier %d times", row.reads, calls)
						}
						if hasDigest || env["labels"] != nil || hasExcerpt {
							t.Errorf("the m0 envelope carries content-derived fields: %v", env)
						}
					} else if row.reads == 0 || !hasDigest || len(labels) == 0 {
						t.Errorf("at %s the reader was called %d times; the envelope must carry a digest and labels: %v", mode, row.reads, env)
					}
					if mode == protocol.ModeM2 {
						if !hasExcerpt {
							t.Error("the m2 envelope carries no content_excerpt")
						}
						for _, ex := range excerpts {
							if len([]rune(ex)) > protocol.MaxExcerptChars || (ex != "" && !slices.Contains(given, ex)) {
								t.Errorf("the m2 excerpt %q is not classifier-host's excerpt %q", ex, given)
							}
						}
					} else if row.excerpt != 0 || hasExcerpt {
						t.Errorf("a %s envelope carries an excerpt", mode)
					}
					if mode == protocol.ModeM3 {
						if row.content == 0 {
							t.Error("the m3 content store does not hold the prompt")
						}
					} else if row.content != 0 {
						t.Errorf("the content store holds the canary %d times at %s", row.content, mode)
					}
				})
			}
		}
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "normalizer\tfixture\tmode\treads\tuploads\texcerpt\tspool\tcontent\tlogs\thealth\tdropped")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			r.normalizer, r.fixture, r.mode, r.reads, r.uploads, r.excerpt, r.spool, r.content, r.logs, r.health, r.dropped)
	}
	_ = w.Flush()
	t.Logf("canary hits per normalizer and mode through classifier-host and the drain (excerpt: inside content_excerpt.text; content: in the content store; dropped: the dropped value's canary anywhere):\n%s", b.String())
}
