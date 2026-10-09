package hooks_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// The service's side of a hook decision, without the hook's process: a hook_evaluate frame with a
// 4 KB prompt goes in over a loopback socket, the relay classifies it with a real classifier-host
// under its component supervisor, evaluates the bundle's rules and writes the decision frame back.
// Its p99 is a fraction of the whole --hook process's budget that a shared CI runner meets.
const (
	decisionRuns       = 2000
	decisionSecretEach = 10 // every tenth prompt carries an AWS key and must be blocked
	decisionBudget     = 20 * time.Millisecond
)

func TestHookDecisionBudget(t *testing.T) {
	classifier, host := startClassifier(t, buildClassifier(t, t.TempDir()))
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
	relay := newRelay(t, pipe, classifier)
	addr := serveHooks(t, relay)

	clean := frameBytes(t, evaluateFrame(t, "claude_code", benchPrompt("")))
	secret := frameBytes(t, evaluateFrame(t, "claude_code", benchPrompt(awsKey)))
	times := make([]time.Duration, 0, decisionRuns)
	wrong := 0
	for i := range decisionRuns {
		frame, want := clean, protocol.HookAllow
		if i%decisionSecretEach == 0 {
			frame, want = secret, protocol.HookBlock
		}
		start := time.Now()
		d, err := decide(addr, frame)
		times = append(times, time.Since(start))
		if err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
		// A decision made with the labels unknown measured a classification that ran out of time.
		if d.Action != want || d.RuleID == "" {
			wrong++
		}
	}

	t.Logf("%s/%s, %d logical CPUs, classifier-host restarted %d times", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), host.Restarts())
	t.Logf("n=%d p50=%v p95=%v p99=%v max=%v", len(times), percentile(times, 50), percentile(times, 95), percentile(times, 99), slices.Max(times))
	if p99 := percentile(times, 99); p99 >= decisionBudget {
		t.Errorf("p99 of the service's hook decision is %v, the budget is under %v", p99, decisionBudget)
	}
	if wrong > 0 {
		t.Errorf("%d of %d decisions were not the bundle's answer for the prompt", wrong, decisionRuns)
	}
	// Stopping the relay waits for the last answered prompt to be recorded.
	_ = relay.Stop(context.Background())
	if got := pipe.Counters(protocol.RouteToolHook).Cumulative()[protocol.CounterEmitted]; got != decisionRuns {
		t.Errorf("the service recorded %d prompts, want %d", got, decisionRuns)
	}
}

// serveHooks answers hook connections on a loopback socket the way the service's native endpoint
// does: the first frame of a connection is handed to the relay.
func serveHooks(t *testing.T, relay *hooks.Relay) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				payload, err := localipc.ReadFrame(conn)
				if err != nil {
					return
				}
				var first protocol.NativeMessage
				if json.Unmarshal(payload, &first) == nil {
					relay.Serve(conn, hostinfo.User{Account: "hook-user"}, first)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func frameBytes(t *testing.T, m protocol.NativeMessage) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decide is one hook's exchange: connect, send the frame, read the decision.
func decide(addr string, frame []byte) (protocol.HookDecision, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := localipc.WriteFrame(conn, frame); err != nil {
		return protocol.HookDecision{}, err
	}
	payload, err := localipc.ReadFrame(conn)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	var answer protocol.NativeMessage
	var d protocol.HookDecision
	if err := json.Unmarshal(payload, &answer); err != nil {
		return d, err
	}
	if answer.Type != protocol.TypeHookDecision {
		return d, errors.New("the relay answered " + string(answer.Type))
	}
	return d, json.Unmarshal(answer.Body, &d)
}
