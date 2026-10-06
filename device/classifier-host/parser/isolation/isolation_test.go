package isolation_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
	"github.com/shadow-ai-capture/device/protocol"
)

// The tests start this test binary as the parser child. With CHILD_BEHAVIOUR unset it runs the
// production child (parser.Execute); otherwise it reads the request and then misbehaves in the
// named way, so the parent's limits are exercised against a real process.
func TestMain(m *testing.M) {
	behaviour, isChild := os.LookupEnv("CHILD_BEHAVIOUR")
	if !isChild {
		os.Exit(m.Run())
	}
	if behaviour == "" {
		os.Exit(parser.Execute(os.Stdin, os.Stdout, parser.DefaultLimits()))
	}
	if _, err := protocol.ReadFrameChecked(os.Stdin); err != nil {
		os.Exit(2)
	}
	name, arg, _ := strings.Cut(behaviour, "=")
	n, _ := strconv.Atoi(arg)
	switch name {
	case "hang":
		time.Sleep(time.Hour)
	case "exit":
		os.Exit(n)
	case "garbage":
		os.Stdout.WriteString("this is not a result frame")
	case "flood":
		chunk := []byte(strings.Repeat("x", 1<<16))
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(1)
			}
		}
	case "allocate":
		var held [][]byte
		for i := 0; i < n/4; i++ {
			b := make([]byte, 4<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1 // touch every page so it is committed
			}
			held = append(held, b)
		}
		time.Sleep(time.Hour)
		_ = held
	}
	os.Exit(0)
}

func runner(lim isolation.Limits, behaviour string) *isolation.Runner {
	exe, _ := os.Executable()
	return isolation.New(isolation.Command{Path: exe, Env: []string{"CHILD_BEHAVIOUR=" + behaviour}}, lim)
}

func limits(timeout time.Duration) isolation.Limits {
	lim := isolation.DefaultLimits()
	lim.Timeout = timeout
	return lim
}

func parse(r *isolation.Runner, mediaType, doc string) isolation.Result {
	return r.Parse(context.Background(), mediaType, []byte(doc))
}

func requireAlive(t *testing.T) {
	t.Helper()
	if res := parse(runner(limits(5*time.Second), ""), "text/plain", "still alive"); res.Cause != isolation.CauseOK || res.Text != "still alive" {
		t.Fatalf("a well-behaved child failed afterwards: %+v", res)
	}
}

func TestAChildParsesOneDocument(t *testing.T) {
	res := parse(runner(limits(5*time.Second), ""), "text/plain", "card 4111 1111 1111 1111 and  more")
	if res.Cause != isolation.CauseOK || res.Text != "card 4111 1111 1111 1111 and more" {
		t.Fatalf("parse: %+v", res)
	}
	if res.Duration <= 0 {
		t.Errorf("no duration was measured: %+v", res)
	}
	if res.PeakBytes <= 0 && !strings.Contains(res.Err, "memory limit not enforced") {
		t.Errorf("the parent observed no memory use and did not say why: %+v", res)
	}
}

func TestChildStatusesBecomeCauses(t *testing.T) {
	r := runner(limits(5*time.Second), "")
	cases := []struct {
		mediaType, doc string
		want           isolation.Cause
	}{
		{"application/json", strings.Repeat("[", 5000) + `"x"` + strings.Repeat("]", 5000), isolation.CauseNesting},
		{"application/pdf", "%PDF-1.7", isolation.CauseUnsupported},
		{"text/plain", "a\xffb", isolation.CauseUndecodable},
	}
	for _, tc := range cases {
		res := parse(r, tc.mediaType, tc.doc)
		if res.Cause != tc.want || res.Text != "" || res.Cause.Detail() == protocol.DetailNone || !res.Cause.Detail().Valid() {
			t.Errorf("%s: %+v, want %s", tc.mediaType, res, tc.want)
		}
	}
}

func TestOversizedDocumentIsRefusedWithoutAChild(t *testing.T) {
	lim := limits(5 * time.Second)
	lim.DocumentBytes = 1024
	res := parse(runner(lim, "hang"), "text/plain", strings.Repeat("x", 2048))
	if res.Cause != isolation.CauseInputCap || res.Duration > 100*time.Millisecond {
		t.Fatalf("an over-limit document: %+v", res)
	}
}

func TestHangingChildIsKilledAtTheTimeout(t *testing.T) {
	start := time.Now()
	res := parse(runner(limits(250*time.Millisecond), "hang"), "text/plain", "hello")
	if res.Cause != isolation.CauseTimeout || res.Cause.Detail() != protocol.DetailParserTimeout {
		t.Fatalf("a hanging child: %+v", res)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the parent waited %v for a 250ms timeout", took)
	}
	requireAlive(t)
}

func TestMemoryBreachIsKilledByTheParent(t *testing.T) {
	if strings.Contains(parse(runner(limits(5*time.Second), ""), "text/plain", "x").Err, "memory limit not enforced") {
		t.Skip("this platform offers no memory sampling of the child")
	}
	lim := limits(20 * time.Second)
	lim.MemoryBytes = 1 << 30
	lim.ResidencyBytes = 96 << 20
	lim.ResidencyPoll = time.Millisecond
	res := parse(runner(lim, "allocate=512"), "text/plain", "hello")
	if res.Cause != isolation.CauseMemory || !strings.Contains(res.Err, "residency limit") {
		t.Fatalf("512 MB against a 96 MB limit: %+v", res)
	}
	requireAlive(t)
}

func TestCrashAndGarbageAreBoundedFailures(t *testing.T) {
	if res := parse(runner(limits(5*time.Second), "exit=3"), "text/plain", "hello"); res.Cause != isolation.CauseCrash {
		t.Errorf("a child exiting 3: %+v", res)
	}
	if res := parse(runner(limits(5*time.Second), "garbage"), "text/plain", "hello"); res.Cause != isolation.CauseMalformed {
		t.Errorf("a child writing a non-frame: %+v", res)
	}
	requireAlive(t)
}

func TestOutputFloodIsKilledAtTheOutputLimit(t *testing.T) {
	lim := limits(10 * time.Second)
	lim.OutputBytes = 4096
	start := time.Now()
	res := parse(runner(lim, "flood"), "text/plain", "hello")
	if res.Cause != isolation.CauseOutputCap || time.Since(start) > 5*time.Second {
		t.Fatalf("a flooding child: %+v after %v", res, time.Since(start))
	}
	requireAlive(t)
}

func TestConcurrentParsesAreBoundedAndSucceed(t *testing.T) {
	lim := limits(10 * time.Second)
	lim.MaxConcurrent = 2
	r := runner(lim, "")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doc := "document " + strconv.Itoa(i)
			if res := parse(r, "text/plain", doc); res.Cause != isolation.CauseOK || res.Text != doc {
				t.Errorf("concurrent parse %d: %+v", i, res)
			}
		}(i)
	}
	wg.Wait()
}

func TestRepeatedFailuresSuspendAFormat(t *testing.T) {
	r := runner(limits(5*time.Second), "")
	for i := 0; i < 3; i++ {
		if res := parse(r, "application/pdf", "%PDF"); res.Cause != isolation.CauseUnsupported {
			t.Fatalf("attempt %d: %+v", i, res)
		}
	}
	if res := parse(r, "application/pdf", "%PDF"); res.Cause != isolation.CauseSuspended || res.Cause.Detail() != protocol.DetailParserFailed {
		t.Fatalf("a fourth PDF after three failures: %+v", res)
	}
	if res := parse(r, "text/plain", "other formats still parse"); res.Cause != isolation.CauseOK {
		t.Fatalf("another format: %+v", res)
	}
}

func TestBreakerWindowAndCooldown(t *testing.T) {
	now := time.Now()
	b := isolation.NewBreaker(3, time.Minute, 5*time.Minute, func() time.Time { return now })
	b.Record("f", false)
	b.Record("f", false)
	b.Record("f", true) // a success clears the failures
	b.Record("f", false)
	b.Record("f", false)
	if !b.Allow("f") {
		t.Fatal("failures before a success were counted")
	}
	now = now.Add(2 * time.Minute) // the two failures fall out of the window
	b.Record("f", false)
	if !b.Allow("f") {
		t.Fatal("failures outside the window were counted")
	}
	b.Record("f", false)
	b.Record("f", false)
	if b.Allow("f") {
		t.Fatal("three failures within the window did not suspend the format")
	}
	now = now.Add(6 * time.Minute)
	if !b.Allow("f") {
		t.Fatal("the format stayed suspended past its cooldown")
	}
}
