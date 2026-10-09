package classifierlink

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/component"
	"github.com/shadow-ai-capture/device/protocol"
)

// The test binary is its own fake classifier host: run with hostArg first, it serves the classifier
// protocol on its stdin and stdout, as classifier-host does, instead of running the tests.
const hostArg = "classifierlink-test-host"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == hostArg {
		os.Exit(runHost())
	}
	os.Exit(m.Run())
}

// runHost answers the handshake, then each request in order with its content as the rule id. The
// request "slow" is answered after 300 ms.
func runHost() int {
	if _, err := protocol.ReadFrameChecked(os.Stdin); err != nil {
		return 1
	}
	hs, _ := json.Marshal(protocol.HandshakeResponse{OK: true, ClassifierVersion: "v1"})
	if err := protocol.WriteFrame(os.Stdout, hs); err != nil {
		return 1
	}
	for {
		payload, err := protocol.ReadFrameChecked(os.Stdin)
		if err != nil {
			return 0
		}
		var req protocol.ClassifyRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return 1
		}
		if string(req.Content) == "slow" {
			time.Sleep(300 * time.Millisecond)
		}
		b, _ := json.Marshal(protocol.ClassifyResponse{
			Labels:            []protocol.Label{{Class: "source_code", Score: 0.5, RuleID: string(req.Content)}},
			ClassifierVersion: "v1",
			Confidence:        protocol.ConfidenceHigh,
		})
		if err := protocol.WriteFrame(os.Stdout, b); err != nil {
			return 1
		}
	}
}

// startSupervisedHost runs the fake host under a component supervisor, connected over the link
// as the service connects classifier-host.
func startSupervisedHost(t *testing.T) (*Client, *component.Supervisor) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var c *Client
	host := component.New(component.Spec{
		Collector: protocol.CollectorClassifierHost,
		Path:      exe,
		Args:      []string{hostArg},
		Stdio:     true,
		Ready:     func(ctx context.Context) error { return c.Connect(ctx) },
	}, nil)
	c = NewWithDialer(host.Dial, "core-1", 5*time.Second)
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background()) })
	if h := host.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("the host is %s/%s after its handshake", h.State, h.Detail)
	}
	return c, host
}

// A call that times out leaves the supervised child running, and the next call is answered by it.
func TestLinkTimedOutCallKeepsTheChild(t *testing.T) {
	c, host := startSupervisedHost(t)

	resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("slow"), Mode: protocol.ModeM1, BudgetMS: 30,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.Confidence != protocol.ConfidenceDegraded || resp.ClassifierVersion != RulesOnlyVersion {
		t.Fatalf("the timed-out call = %+v, want the rules-only fallback", resp)
	}

	resp, err = c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("next"), Mode: protocol.ModeM1, BudgetMS: 3000,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := ruleID(resp); got != "next" {
		t.Fatalf("the call after a timed-out call received %s, want its own answer", got)
	}
	if degraded, reason := c.Degraded(); degraded {
		t.Errorf("the link is still degraded (%s) after the host answered", reason)
	}
	if got := c.ClassifierVersion(); got != "v1" {
		t.Errorf("ClassifierVersion = %q, want the host's", got)
	}
	// A restart waits a second after the child exits; wait past it.
	time.Sleep(1500 * time.Millisecond)
	if n := host.Restarts(); n != 0 {
		t.Errorf("the child was restarted %d times", n)
	}
	if h := host.Health(); h.State != protocol.StateHealthy {
		t.Errorf("the host is %s/%s", h.State, h.Detail)
	}
}
