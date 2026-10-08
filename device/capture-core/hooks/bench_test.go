package hooks_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// The hook benchmark measures the wall time of whole `capture-core --hook test prompt` processes,
// started as the invoking user the way a tool starts its hook, against a service running in this
// process: the hook relay over a real spool and a real classifier-host under its component
// supervisor, on a private endpoint, with a bundle at m1 that blocks credentials. It runs only
// with SAC_HOOK_BENCH=1:
//
//	SAC_HOOK_BENCH=1 go test -run HookBench -v ./hooks/
//
// With the go tool it builds capture-core, classifier-host and a signed release of the shipped
// classifier rules. Where there is none, it uses prebuilt ones beside the test binary:
// capture-core linked with -ldflags "-X main.hookBenchEndpoint=<benchEndpoint()>",
// classifier-host, the release in classifier-release/ and its public key (hex) in
// classifier-release.pub.

const (
	benchRuns       = 1000
	benchSecretRuns = 100
	benchPromptSize = 4 << 10
	benchBudget     = 50 * time.Millisecond
)

// benchEndpoint is the benchmark service's endpoint, never the installed service's.
func benchEndpoint() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\ShadowAICapture.native.hookbench`
	}
	return filepath.Join(os.TempDir(), "sac-hookbench", "native.sock")
}

// benchBinaries returns capture-core and the classifier release: prebuilt beside the test binary,
// or built here.
func benchBinaries(t *testing.T) (string, classifierRelease) {
	t.Helper()
	if self, err := os.Executable(); err == nil {
		dir := filepath.Dir(self)
		exe := filepath.Join(dir, exeName("capture-core"))
		pub, perr := os.ReadFile(filepath.Join(dir, "classifier-release.pub"))
		if _, err := os.Stat(exe); err == nil && perr == nil {
			return exe, classifierRelease{
				exe:    filepath.Join(dir, exeName("classifier-host")),
				dir:    filepath.Join(dir, "classifier-release"),
				pubkey: strings.TrimSpace(string(pub)),
			}
		}
	}
	out := t.TempDir()
	exe := filepath.Join(out, exeName("capture-core"))
	goBuild(t, "..", "./cmd/capture-core", exe, "-s -w -X main.hookBenchEndpoint="+benchEndpoint())
	return exe, buildClassifier(t, out)
}

// benchPrompt is benchPromptSize bytes of ordinary prose, with secret in the middle when given.
func benchPrompt(secret string) string {
	const prose = "Please review the quarterly planning notes below and suggest a clearer structure for the summary section. "
	text := strings.Repeat(prose, benchPromptSize/len(prose)+1)[:benchPromptSize]
	if secret == "" {
		return text
	}
	mid := benchPromptSize / 2
	return text[:mid] + " " + secret + " " + text[mid+len(secret)+2:]
}

func TestHookBench(t *testing.T) {
	if os.Getenv("SAC_HOOK_BENCH") != "1" {
		t.Skip("the hook benchmark runs with SAC_HOOK_BENCH=1")
	}
	exe, rel := benchBinaries(t)
	classifier, host := startClassifier(t, rel)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	spool, err := capturespool.Open(capturespool.Config{Dir: t.TempDir(), Key: key, OnCorrupt: capturespool.CorruptQuarantine})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	pipe := newPipeline(t, spool, testBundle(protocol.ModeM1, blockCredentials))
	pipe.Classifier = classifier
	relay := hooks.New(hooks.Config{
		Pipeline:   pipe,
		Bundles:    pipe.Bundles,
		Classifier: classifier,
		Person:     func(hostinfo.User) core.Person { return core.Person{UserRef: testUserRef} },
	})
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })

	addr := benchEndpoint()
	if runtime.GOOS != "windows" {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(addr)) })
	}
	srv := localipc.NewServer(addr, func(conn net.Conn, peer hostinfo.User) {
		payload, err := localipc.ReadFrame(conn)
		if err != nil {
			return
		}
		var first protocol.NativeMessage
		if json.Unmarshal(payload, &first) == nil {
			relay.Serve(conn, peer, first)
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv.Start(); err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	t.Cleanup(srv.Stop)

	clean := run(t, exe, benchPrompt(""), benchRuns, protocol.HookAllow)
	secret := run(t, exe, benchPrompt(awsKey), benchSecretRuns, protocol.HookBlock)
	all := append(slices.Clone(clean), secret...)

	t.Logf("%s/%s, %d logical CPUs, classifier-host restarted %d times", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), host.Restarts())
	for _, s := range []struct {
		name string
		d    []time.Duration
	}{{"4 KB prompt, no secret", clean}, {"4 KB prompt, AWS key", secret}, {"all runs", all}} {
		t.Logf("%-24s n=%-5d p50=%-8v p95=%-8v p99=%-8v max=%v", s.name, len(s.d),
			percentile(s.d, 50), percentile(s.d, 95), percentile(s.d, 99), slices.Max(s.d))
	}
	if p99 := percentile(all, 99); p99 >= benchBudget {
		t.Errorf("p99 of the whole hook process is %v, the budget is under %v", p99, benchBudget)
	}
	// Stopping the relay waits for the last answered prompt to be recorded.
	_ = relay.Stop(context.Background())
	if got := pipe.Counters(protocol.RouteToolHook).Cumulative()[protocol.CounterEmitted]; got != benchRuns+benchSecretRuns {
		t.Errorf("the service recorded %d prompts, want %d", got, benchRuns+benchSecretRuns)
	}
}

// run starts the hook n times with prompt and returns each process's wall time. A run whose
// output is not the service's decision, want, is a failure: a hook that failed open measured
// nothing.
func run(t *testing.T, exe, prompt string, n int, want protocol.HookAction) []time.Duration {
	t.Helper()
	input, err := json.Marshal(map[string]string{"tool": "claude_code", "session_id": "bench", "prompt_text": prompt})
	if err != nil {
		t.Fatal(err)
	}
	times := make([]time.Duration, 0, n)
	wrong := 0
	for range n {
		var stdout bytes.Buffer
		cmd := exec.Command(exe, "--hook", hooks.TestTool, "prompt")
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stdout = &stdout
		start := time.Now()
		err := cmd.Run()
		times = append(times, time.Since(start))
		var d protocol.HookDecision
		if err != nil || json.Unmarshal(stdout.Bytes(), &d) != nil || d.Action != want || d.RuleID == "" {
			wrong++
		}
	}
	if wrong > 0 {
		t.Errorf("%d of %d hooks did not print the service's %s decision", wrong, n, want)
	}
	return times
}

// percentile is the nearest-rank percentile of d.
func percentile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(i, 0)]
}
