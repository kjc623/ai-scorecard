package otlp

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/shadow-ai-capture/device/protocol"
)

// The receiver's budget: 2,000 log records a second, sustained for 10 s, through the official
// OTLP/HTTP log exporter, none dropped, and a p99 request handling time under 20 ms. The records go
// out as the SDK's batch processor sends them, one request at a time, here every 10 ms.
const (
	budgetRate     = 2000
	budgetDuration = 10 * time.Second
	budgetInterval = 10 * time.Millisecond
	budgetP99      = 20 * time.Millisecond
)

// countingNormalizer stands in for the normalizers and the pipeline behind them: it counts the log
// records it is handed.
type countingNormalizer struct{ logs atomic.Int64 }

func (n *countingNormalizer) Name() string                { return "counting" }
func (n *countingNormalizer) Accepts(service string) bool { return service == tool }

func (n *countingNormalizer) Spans(context.Context, Sender, *tracepb.ResourceSpans) {}

func (n *countingNormalizer) Logs(_ context.Context, _ Sender, rl *logspb.ResourceLogs) {
	for _, sl := range rl.GetScopeLogs() {
		n.logs.Add(int64(len(sl.GetLogRecords())))
	}
}

// timedTransport records how long each request took, from sending it to the response's headers.
type timedTransport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	times []time.Duration
}

func (t *timedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	d := time.Since(start)
	t.mu.Lock()
	t.times = append(t.times, d)
	t.mu.Unlock()
	return resp, err
}

// collector keeps the records a logger emits so they can be exported as one batch.
type collector struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (c *collector) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (c *collector) Shutdown(context.Context) error                         { return nil }
func (c *collector) ForceFlush(context.Context) error                       { return nil }
func (c *collector) OnEmit(_ context.Context, rec *sdklog.Record) error {
	c.mu.Lock()
	c.recs = append(c.recs, rec.Clone())
	c.mu.Unlock()
	return nil
}

func (c *collector) take() []sdklog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.recs
	c.recs = nil
	return out
}

func TestOTLPBudget(t *testing.T) {
	ctx := context.Background()
	norm := &countingNormalizer{}
	r, err := New(Config{
		TokenPath:   t.TempDir() + "/" + TokenFile,
		HTTPListen:  "127.0.0.1:0",
		GRPCListen:  "127.0.0.1:0",
		Normalizers: []Normalizer{norm},
		Person:      testPerson,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	timed := &timedTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	// Without retries a refused or failed request is a dropped batch, not a slow one.
	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(r.HTTPAddr()), otlploghttp.WithInsecure(),
		otlploghttp.WithHeaders(map[string]string{"Authorization": "Bearer " + r.Token()}),
		otlploghttp.WithHTTPClient(&http.Client{Transport: timed}),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	coll := &collector{}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(toolResource()), sdklog.WithProcessor(coll))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	logger := lp.Logger("budget")

	batches := int(budgetDuration / budgetInterval)
	perBatch := budgetRate * int(budgetInterval) / int(time.Second)
	prompt := strings.Repeat("Summarise the attached meeting notes. ", 28)[:1024]
	failed := 0
	start := time.Now()
	for b := range batches {
		// Each batch leaves on its schedule; a late batch is sent at once, so the rate holds.
		time.Sleep(time.Until(start.Add(time.Duration(b) * budgetInterval)))
		for i := range perBatch {
			var rec otellog.Record
			rec.SetTimestamp(time.Now())
			rec.SetBody(attribute.StringValue("claude_code.user_prompt"))
			rec.AddAttributes(
				attribute.String("event.name", "user_prompt"),
				attribute.String("session.id", "budget-session"),
				attribute.String("prompt.id", fmt.Sprintf("%d-%d", b, i)),
				attribute.Int("prompt_length", len(prompt)),
				attribute.String("prompt", prompt),
			)
			logger.Emit(ctx, rec)
		}
		if err := exp.Export(ctx, coll.take()); err != nil {
			failed++
		}
	}
	elapsed := time.Since(start)

	sent := batches * perBatch
	timed.mu.Lock()
	times := slices.Clone(timed.times)
	timed.mu.Unlock()
	t.Logf("%s/%s, %d logical CPUs: %d records in %d requests over %v (%.0f records/s)",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), sent, len(times), elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds())
	t.Logf("request handling: p50=%v p95=%v p99=%v max=%v", percentile(times, 50), percentile(times, 95), percentile(times, 99), slices.Max(times))

	if failed > 0 {
		t.Errorf("%d of %d requests failed", failed, batches)
	}
	got := norm.logs.Load()
	counts := r.Counters().Cumulative()
	if got != int64(sent) || counts[protocol.CounterObserved] != uint64(sent) || counts[protocol.CounterErrors] != 0 {
		t.Errorf("sent %d records; the normalizer was handed %d, the receiver counted %d observed and %d errors",
			sent, got, counts[protocol.CounterObserved], counts[protocol.CounterErrors])
	}
	if p99 := percentile(times, 99); p99 >= budgetP99 {
		t.Errorf("p99 request handling time is %v, the budget is under %v", p99, budgetP99)
	}
}

// percentile is the nearest-rank percentile of d.
func percentile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(i, 0)]
}
