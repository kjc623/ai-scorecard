package loopback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeRelocator records each move and, at each restore, whether the broker still held its port.
// Held means the port cannot be bound, which is what the tool's returning server needs: a dial can
// still connect for a moment after a listener closes, and Windows accepts into the closed backlog.
type fakeRelocator struct {
	held int // the port the broker holds for the tool

	mu            sync.Mutex
	events        []string
	heldAtRestore bool
	err           error
}

func (f *fakeRelocator) Relocate(upstreamPort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf("relocate %d (held %t)", upstreamPort, portHeld(f.held)))
	return f.err
}

func (f *fakeRelocator) Restore() error {
	open := portHeld(f.held)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "restore")
	f.heldAtRestore = f.heldAtRestore || open
	return nil
}

func (f *fakeRelocator) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// The tool's server is moved before the first preflight, the port is taken only once the upstream
// answers there, and on stop the port is released before the server is moved back.
func TestBrokerRelocatesFirstAndBindsOnlyAfterTheUpstreamPreflight(t *testing.T) {
	upstream := newStubUpstream(t)
	upstreamPort := upstream.Port()
	upstream.Close() // the server has not restarted on its new port yet
	held := freePort(t)
	rel := &fakeRelocator{held: held}
	cfg := testConfig(upstreamPort, held, &recordingPipeline{mode: protocol.ModeM1})
	cfg.Relocators = map[string]Relocator{"local_inference": rel}
	b := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := rel.log(); len(got) != 1 || got[0] != fmt.Sprintf("relocate %d (held false)", upstreamPort) {
		t.Fatalf("relocator saw %v, want one move to %d before anything was held", got, upstreamPort)
	}
	// With nothing answering on the upstream port the broker keeps its hands off the port.
	time.Sleep(200 * time.Millisecond)
	if portOpen(held) {
		t.Fatal("the broker took the port before the upstream answered its preflight")
	}

	upstream.Reopen(t)
	waitFor(t, 3*time.Second, "the broker to hold the port once the upstream answers", func() bool { return portOpen(held) })
	if n := upstream.countPath("/health"); n == 0 {
		t.Fatal("the port was taken without a preflight to the upstream")
	}
	if got := rel.log(); len(got) != 1 {
		t.Fatalf("relocator saw %v, want the one move", got)
	}

	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := rel.log()
	if len(got) != 2 || got[1] != "restore" {
		t.Fatalf("relocator saw %v, want the move then a restore", got)
	}
	if rel.heldAtRestore {
		t.Fatal("the server was moved back while the broker still held its port")
	}
}

// A move that fails is counted and reported as a configuration write failure, and the broker still
// looks for the upstream.
func TestBrokerReportsAFailedRelocation(t *testing.T) {
	held := freePort(t)
	rel := &fakeRelocator{held: held, err: errors.New("access denied")}
	cfg := testConfig(freePort(t), held, &recordingPipeline{mode: protocol.ModeM1})
	cfg.Relocators = map[string]Relocator{"local_inference": rel}
	b := New(cfg)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	if h := b.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
	if b.Counters().Cumulative()[protocol.CounterErrors] == 0 {
		t.Fatal("the failed move was not counted")
	}
}

func TestBrokerIsEnabledByThePortMap(t *testing.T) {
	b := New(Config{})
	if b.Enabled(nil) || b.Enabled(&policy.Bundle{}) {
		t.Fatal("enabled without a port in the bundle")
	}
	if !b.Enabled(&policy.Bundle{Loopback: policy.LoopbackPolicy{Ports: []policy.LoopbackPort{{ToolFingerprint: "app:ollama", Port: 11434, UpstreamPort: 21434}}}}) {
		t.Fatal("not enabled with a port in the bundle")
	}
}

// The bundle's timings reach the broker; one it leaves out takes the default.
func TestBrokerTakesItsTimingsFromTheBundle(t *testing.T) {
	b := New(Config{})
	if err := b.ApplyPolicy(policy.Bundle{Loopback: policy.LoopbackPolicy{
		Ports:                    []policy.LoopbackPort{{ToolFingerprint: "app:ollama", Port: 11434, UpstreamPort: 21434}},
		ProbeIntervalSeconds:     5,
		PreflightIntervalSeconds: 60,
		PreflightTimeoutMS:       3000,
		MaxConsecutiveFailures:   3,
		CoolDownSeconds:          300,
	}}); err != nil {
		t.Fatal(err)
	}
	c := b.runCfg
	if c.ProbeInterval != 5*time.Second || c.PreflightInterval != time.Minute || c.PreflightTimeout != 3*time.Second ||
		c.MaxConsecutiveFailures != 3 || c.CoolDown != 5*time.Minute || len(c.Ports) != 1 || c.Ports[0].UpstreamPort != 21434 {
		t.Fatalf("config after the bundle = %+v", c)
	}
	if err := b.ApplyPolicy(policy.Bundle{}); err != nil {
		t.Fatal(err)
	}
	if c := b.runCfg; c.MaxConsecutiveFailures != 5 || c.CoolDown != 10*time.Minute || len(c.Ports) != 0 {
		t.Fatalf("config after a bundle without the section = %+v", c)
	}
}

// Through the registry, as the service runs it: a bundle with the port map starts the broker, which
// moves the server and holds the port; a bundle without it releases the port and moves the server
// back; a bundle with it again starts the broker anew.
func TestBrokerFollowsThePolicySwitch(t *testing.T) {
	upstream := newStubUpstream(t)
	held := freePort(t)
	rel := &fakeRelocator{held: held}
	cfg := testConfig(upstream.Port(), held, &recordingPipeline{mode: protocol.ModeM1})
	cfg.Ports = nil
	cfg.Relocators = map[string]Relocator{"app:ollama": rel}
	b := New(cfg)

	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(b); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	if h, _ := reg.HealthFor(protocol.CollectorLoopbackBroker); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("without a bundle: %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}

	on := policy.Bundle{Loopback: policy.LoopbackPolicy{
		Ports:              []policy.LoopbackPort{{ToolFingerprint: "app:ollama", Port: held, UpstreamPort: upstream.Port(), PreflightPath: "/api/version", Mode: protocol.ModeM1}},
		PreflightTimeoutMS: 250,
	}}
	reg.ApplyPolicy(on)
	waitFor(t, 3*time.Second, "the broker to hold the port", func() bool { return portOpen(held) })
	if got := rel.log(); len(got) != 1 || got[0] != fmt.Sprintf("relocate %d (held false)", upstream.Port()) {
		t.Fatalf("relocator saw %v", got)
	}

	reg.ApplyPolicy(policy.Bundle{})
	waitFor(t, 3*time.Second, "the broker to release the port", func() bool { return !portOpen(held) })
	waitFor(t, 3*time.Second, "the server to be moved back", func() bool { return len(rel.log()) == 2 })
	if rel.log()[1] != "restore" || rel.heldAtRestore {
		t.Fatalf("relocator saw %v, held at restore %t", rel.log(), rel.heldAtRestore)
	}
	if h, _ := reg.HealthFor(protocol.CollectorLoopbackBroker); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("switched off: %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}

	reg.ApplyPolicy(on)
	waitFor(t, 3*time.Second, "the broker to hold the port again", func() bool { return portOpen(held) })
	if got := rel.log(); len(got) != 3 || got[2] != fmt.Sprintf("relocate %d (held false)", upstream.Port()) {
		t.Fatalf("relocator saw %v", got)
	}
	reg.StopExcept(context.Background())
	if portOpen(held) {
		t.Fatal("the port is held after the broker stopped")
	}
}
