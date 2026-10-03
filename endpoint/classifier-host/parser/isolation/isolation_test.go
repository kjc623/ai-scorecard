//go:build !js

package isolation_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/internal/testhook"
	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
)

// TestParserChildHelper is not a test of the parent: it *is* the child. The isolation tests spawn
// this test binary with CLASSIFIER_HOST_HELPER=parse-child, so the child under the parent's
// enforcement is the production child path (parser.Execute + the documented test hooks) rather
// than a mock that cannot misbehave.
func TestParserChildHelper(t *testing.T) {
	if os.Getenv("CLASSIFIER_HOST_HELPER") != "parse-child" {
		t.Skip("helper process: only runs when spawned by the isolation tests")
	}
	os.Exit(parser.Execute(os.Stdin, os.Stdout, parser.DefaultLimits(), testhook.FromEnv()))
}

func helperCmd(env ...string) isolation.Command {
	return isolation.Command{
		Path: os.Args[0],
		Args: []string{"-test.run=TestParserChildHelper", "--"},
		Env:  append([]string{"CLASSIFIER_HOST_HELPER=parse-child"}, env...),
	}
}

func runner(t *testing.T, lim isolation.Limits, env ...string) *isolation.Runner {
	t.Helper()
	r := isolation.New(helperCmd(env...), lim)
	if !r.Available() {
		t.Fatal("the runner reports itself unavailable")
	}
	return r
}

func TestParseSendsOneDocumentAndGetsTypedTextBack(t *testing.T) {
	r := runner(t, isolation.DefaultLimits())
	res := r.Parse(context.Background(), "text/plain", []byte("card 4111 1111 1111 1111 and  more"))
	if res.Cause != docparse.CauseOK {
		t.Fatalf("parse failed: %+v", res)
	}
	if !strings.Contains(res.Text, "4111 1111 1111 1111") {
		t.Errorf("text was not extracted: %q", res.Text)
	}
	if res.Duration <= 0 {
		t.Error("no duration was measured")
	}
	if res.PeakBytes <= 0 {
		t.Errorf("the parent did not observe the child's memory use: %+v", res)
	}
}

// TestUnavailableRunnerDegrades is §9.1's table (document parsing is unavailable in the wasm copy)
// expressed as a result the caller must degrade rather than ignore.
func TestUnavailableRunnerDegrades(t *testing.T) {
	r := isolation.New(isolation.Command{}, isolation.DefaultLimits())
	if r.Available() {
		t.Fatal("a runner with no command claims to be available")
	}
	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	if res.Cause != docparse.CauseUnavailable || res.Detail != res.Cause.Detail() {
		t.Fatalf("unavailable runner produced %+v", res)
	}
	if res.Detail.Valid() == false {
		t.Errorf("detail %q is outside the closed vocabulary", res.Detail)
	}
}

// TestDeepNestingBecomesABoundedFailure runs §10's "deeply nested" hostile document through a real
// child: a 5000-deep JSON body must come back as a status, and the parent must still be usable.
func TestDeepNestingBecomesABoundedFailure(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 5 * time.Second
	r := runner(t, lim)
	depth := 5000
	body := []byte(strings.Repeat("[", depth) + `"x"` + strings.Repeat("]", depth))
	res := r.Parse(context.Background(), "application/json", body)
	if res.Cause != docparse.CauseDepthExceeded {
		t.Fatalf("a %d-deep body produced %+v", depth, res)
	}
	if res.Text != "" {
		t.Errorf("a refused document still returned text: %q", res.Text)
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
	if res := runner(t, isolation.DefaultLimits()).Parse(context.Background(), "text/plain", []byte("still alive")); res.Cause != docparse.CauseOK {
		t.Fatalf("the parent did not survive a nesting bomb: %+v", res)
	}
}

// TestHugeDeclaredDocumentIsRefusedBeforeAnyChildStarts: §10's declared-size cap is checked in the
// parent, so a 100 MB document does not even cost a process spawn.
func TestHugeDeclaredDocumentIsRefusedBeforeAnyChildStarts(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.DeclaredCapBytes = 1024
	r := runner(t, lim)
	start := time.Now()
	res := r.Parse(context.Background(), "text/plain", []byte(strings.Repeat("x", 2048)))
	if res.Cause != docparse.CauseInputCap {
		t.Fatalf("an over-cap document produced %+v", res)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("refusing an over-cap document took %v; it must not spawn a child", time.Since(start))
	}
}

// TestDecompressionBombBecomesABoundedFailure is §10's hostile document, end to end through a real
// child process.
func TestDecompressionBombBecomesABoundedFailure(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 128; i++ {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	lim := isolation.DefaultLimits()
	lim.Timeout = 5 * time.Second
	r := runner(t, lim)
	res := r.Parse(context.Background(), "application/gzip", gz.Bytes())
	if res.Cause != docparse.CauseOutputCap {
		t.Fatalf("a decompression bomb produced %+v", res)
	}
	if !res.Truncated {
		t.Error("the capped result is not marked truncated")
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
}

// TestTimeoutIsEnforcedByTheParentAndTheChildIsKilled: the child hangs, the parent's clock ends it.
func TestTimeoutIsEnforcedByTheParentAndTheChildIsKilled(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 250 * time.Millisecond
	r := runner(t, lim, testhook.SleepEnv+"=10000")

	start := time.Now()
	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	elapsed := time.Since(start)

	if res.Cause != docparse.CauseTimeout {
		t.Fatalf("a hanging child produced %+v after %v", res, elapsed)
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the parent waited %v for a %v timeout", elapsed, lim.Timeout)
	}
	// The parent is still alive and can parse the next document. The hook belongs to one runner,
	// so the recovery check uses a runner that spawns a well-behaved child.
	if res := runner(t, isolation.DefaultLimits()).Parse(context.Background(), "text/plain", []byte("hello again")); res.Cause != docparse.CauseOK {
		t.Fatalf("the parent did not recover after killing a child: %+v", res)
	}
}

// TestMemoryBreachIsKilledByTheParent is §10's memory cap: "a parent-imposed memory cap ... kills
// the child on breach, emits the event with confidence: degraded and detail=parser_memory, and does
// not retry the document".
func TestMemoryBreachIsKilledByTheParent(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 10 * time.Second
	lim.MemoryCapBytes = 1 << 30     // the job cap is deliberately generous here
	lim.ResidencyCapBytes = 96 << 20 // so this test proves the parent's own sampling kills it
	lim.ResidencyPoll = time.Millisecond
	r := runner(t, lim, testhook.AllocEnv+"=512")

	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	if res.Cause != docparse.CauseMemory {
		t.Fatalf("a child that allocated 512 MB against a 96 MB residency cap produced %+v", res)
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
	if !strings.Contains(res.Err, "residency cap") {
		t.Errorf("the failure does not name the cap that was breached: %q", res.Err)
	}

	// "does not retry the document": the parent did not spawn a second child to re-parse it. It
	// also survives, which is the property the user actually depends on.
	if res := runner(t, isolation.DefaultLimits()).Parse(context.Background(), "text/plain", []byte("second document")); res.Cause != docparse.CauseOK {
		t.Fatalf("the parent did not survive a memory breach: %+v", res)
	}
}

// TestJobObjectCapBoundsAnAllocationBomb is the Windows job-object half: with the residency poll
// disabled, the only thing standing between the host and a 2 GB allocation is the job limit, and
// the child must still die a bounded death.
func TestJobObjectCapBoundsAnAllocationBomb(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 15 * time.Second
	lim.MemoryCapBytes = 192 << 20
	lim.ResidencyCapBytes = 0 // rely on the job object alone
	r := runner(t, lim, testhook.AllocEnv+"=2048")

	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	// A child that allocates past its cap must die a bounded death and the parent must survive it.
	if res.Cause != docparse.CauseMemory && res.Cause != docparse.CauseCrash {
		t.Fatalf("an allocation bomb against a 192 MB job cap produced %+v", res)
	}
	if res.Detail == "" || !res.Detail.Valid() {
		t.Errorf("the failure carries no valid detail: %+v", res)
	}
	if res.PeakBytes < 128<<20 {
		t.Errorf("the parent's peak observation is %d bytes; the 192 MB job cap was not in effect", res.PeakBytes)
	}
	if res := runner(t, isolation.DefaultLimits()).Parse(context.Background(), "text/plain", []byte("after the bomb")); res.Cause != docparse.CauseOK {
		t.Fatalf("the parent did not survive an allocation bomb: %+v", res)
	}
}

func TestCrashIsTreatedAsABoundedFailure(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 5 * time.Second
	r := runner(t, lim, testhook.ExitEnv+"=3")
	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	if res.Cause != docparse.CauseCrash {
		t.Fatalf("a child exiting 3 produced %+v", res)
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
}

func TestMalformedResultIsTreatedAsACrash(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 5 * time.Second
	r := runner(t, lim, testhook.GarbageEnv+"=1")
	res := r.Parse(context.Background(), "text/plain", []byte("hello"))
	if res.Cause != docparse.CauseMalformed {
		t.Fatalf("a child writing a non-frame produced %+v", res)
	}
	if res.Detail != res.Cause.Detail() {
		t.Errorf("detail %q does not match cause %q", res.Detail, res.Cause)
	}
}

// TestParentOutputCapBoundsAMonstrousResult: the parent reads at most OutputCapBytes from the
// child, so a parser that tries to hand back a gigabyte is a bounded failure rather than a host
// allocation.
func TestParentOutputCapBoundsAMonstrousResult(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 10 * time.Second
	lim.OutputCapBytes = 4096
	r := runner(t, lim)
	res := r.Parse(context.Background(), "text/plain", []byte(strings.Repeat("sensitive text ", 5000)))
	if res.Cause != docparse.CauseOutputCap {
		t.Fatalf("a result over the parent's read cap produced %+v", res)
	}
}

func TestBoundedFanOutIsRespected(t *testing.T) {
	lim := isolation.DefaultLimits()
	lim.Timeout = 10 * time.Second
	lim.MaxConcurrent = 4
	r := runner(t, lim)

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := r.Parse(context.Background(), "text/plain", []byte("document number "+strings.Repeat("x", i)))
			if res.Cause != docparse.CauseOK {
				errs <- res.Err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent parse failed: %s", e)
	}
}

// TestPerFormatBreakerDisablesARepeatedlyFailingFormat is §10's "per-format failure count disables
// parses for a repeatedly failing format for a period".
func TestPerFormatBreakerDisablesARepeatedlyFailingFormat(t *testing.T) {
	now := time.Now()
	b := isolation.NewBreaker(3, time.Minute, 5*time.Minute, func() time.Time { return now })
	if !b.Allow("application/pdf") {
		t.Fatal("a fresh format was already disabled")
	}
	b.Record("application/pdf", false)
	b.Record("application/pdf", false)
	if !b.Allow("application/pdf") {
		t.Fatal("the format was disabled before the failure count was reached")
	}
	b.Record("application/pdf", false)
	if b.Allow("application/pdf") {
		t.Fatal("three failures in the window did not disable the format")
	}
	if !b.Allow("application/zip") {
		t.Fatal("one format's failures disabled another format")
	}

	now = now.Add(6 * time.Minute)
	if !b.Allow("application/pdf") {
		t.Fatal("the format stayed disabled past its cooldown")
	}

	// A success clears the window, so a transient failure does not accumulate into a disable.
	b.Record("application/pdf", false)
	b.Record("application/pdf", true)
	b.Record("application/pdf", false)
	b.Record("application/pdf", false)
	if !b.Allow("application/pdf") {
		t.Fatal("a success did not reset the failure window")
	}

	stats := b.Stats()
	if len(stats) != 1 {
		t.Fatalf("per-format coverage rows: %+v", stats)
	}
	if stats[0].Format != "application/pdf" || stats[0].Total != 7 || stats[0].Failed != 6 {
		t.Errorf("the coverage row does not account for the format's parses: %+v", stats[0])
	}
}

func TestSemaphoreBoundsAcquisition(t *testing.T) {
	s := isolation.NewSemaphore(1)
	if !s.Acquire(context.Background()) {
		t.Fatal("the first acquisition failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.Acquire(ctx) {
		t.Fatal("a saturated semaphore granted another slot")
	}
	s.Release()
	if !s.Acquire(context.Background()) {
		t.Fatal("a released slot was not reusable")
	}
}

// TestTheChildNeverReadsASecondDocument is §10's "one document, one process": a second document
// queued behind the first is never read, because the child exits after its one result.
func TestTheChildNeverReadsASecondDocument(t *testing.T) {
	var stdin, stdout bytes.Buffer
	doc := []byte("first document")
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: int64(len(doc))}, doc); err != nil {
		t.Fatal(err)
	}
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: 7}, []byte("second!")); err != nil {
		t.Fatal(err)
	}
	if code := parser.Execute(&stdin, &stdout, parser.DefaultLimits(), nil); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	first, err := parser.ReadResult(&stdout)
	if err != nil {
		t.Fatalf("the first result is unreadable: %v", err)
	}
	if first.Status != parser.StatusOK || first.Text != string(doc) {
		t.Fatalf("first result: %+v", first)
	}
	if _, err := parser.ReadResult(&stdout); err == nil {
		t.Fatal("the child produced a second result for a second document")
	}
}
