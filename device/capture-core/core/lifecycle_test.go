package core

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// toggledProvider is a provider the bundle switches on and off. It is off under the bundle
// versions in off, and on with no bundle in force.
type toggledProvider struct {
	collector protocol.Collector
	log       *orderLog
	off       map[string]bool
	// panicAfter, when set, makes every Start after the first panicAfter panic.
	panicAfter int

	mu      sync.Mutex
	running bool
	starts  int
	stops   int
}

func (p *toggledProvider) Name() protocol.Collector { return p.collector }

func (p *toggledProvider) Enabled(b *policy.Bundle) bool { return b == nil || !p.off[b.Version] }

func (p *toggledProvider) Start(context.Context) error {
	p.mu.Lock()
	p.starts++
	n := p.starts
	p.mu.Unlock()
	if p.panicAfter > 0 && n > p.panicAfter {
		panic("provider exploded during a policy start")
	}
	p.log.add("provider.Start:" + string(p.collector))
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()
	return nil
}

func (p *toggledProvider) Stop(context.Context) error {
	p.log.add("provider.Stop:" + string(p.collector))
	p.mu.Lock()
	p.stops++
	p.running = false
	p.mu.Unlock()
	return nil
}

// Health says healthy whenever the provider ever ran, so the registry's override is what the
// tests observe.
func (p *toggledProvider) Health() Health {
	return Health{State: protocol.StateHealthy, Counters: map[protocol.Counter]uint64{}}
}

func (p *toggledProvider) ApplyPolicy(policy.Bundle) error { return nil }

func (p *toggledProvider) counts() (starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts, p.stops
}

var _ Toggled = (*toggledProvider)(nil)

// applyingPolicy is the supervisor's load step as the service wires it: the bundle in force is
// applied to the registry before any provider starts.
type applyingPolicy struct {
	log    *orderLog
	reg    *Registry
	bundle *policy.Bundle
}

func (p *applyingPolicy) Load(context.Context) error {
	p.log.add("policy.Load")
	if p.bundle != nil {
		p.reg.ApplyPolicy(*p.bundle)
	}
	return nil
}

func addProvider(t *testing.T, f *orderingFixture, p Provider) {
	t.Helper()
	if err := f.sup.Registry.Add(p); err != nil {
		t.Fatalf("Add(%s): %v", p.Name(), err)
	}
}

func assertRow(t *testing.T, reg *Registry, c protocol.Collector, state protocol.CollectorState, detail protocol.Detail) {
	t.Helper()
	h, ok := reg.HealthFor(c)
	if !ok {
		t.Fatalf("no health row for %s", c)
	}
	if h.State != state || h.Detail != detail {
		t.Fatalf("%s row = %s/%q, want %s/%q", c, h.State, h.Detail, state, detail)
	}
}

// A provider registered after the fixed three is started by start_collectors, after the loopback
// broker, and stopped with the remaining providers, before the spool drains.
func TestStartCollectorsStartsALaterProviderAndShutdownStopsIt(t *testing.T) {
	f := newOrderingFixture(t)
	addProvider(t, f, &recordingProvider{collector: protocol.CollectorProcessDetector, log: f.log})

	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	assertSequence(t, "startup order", f.log.all(), []string{
		"policy.Load",
		"spool.Open",
		"identity.Resolve",
		"provider.Start:cli_shim",
		"classifierhost.Start",
		"provider.Start:egress_proxy",
		"provider.Start:loopback_broker",
		"provider.Start:process_detector",
	})
	assertSequence(t, "recorded startup steps", f.sup.Order(), StartupOrder())
	assertRow(t, f.sup.Registry, protocol.CollectorProcessDetector, protocol.StateHealthy, protocol.DetailNone)

	f.log.reset()
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := f.log.all()
	if len(got) != 7 {
		t.Fatalf("shutdown side effects = %v, want 7", got)
	}
	assertSequence(t, "fixed shutdown head", got[:2], []string{"provider.Release:loopback_broker", "provider.Stop:egress_proxy"})
	// The remaining providers stop concurrently, so their order is not fixed.
	remaining := slices.Clone(got[2:4])
	slices.Sort(remaining)
	assertSequence(t, "remaining providers", remaining, []string{"provider.Stop:cli_shim", "provider.Stop:process_detector"})
	assertSequence(t, "shutdown tail", got[4:], []string{"spool.Drain", "trustroot.Remove", "classifierhost.Stop"})
	assertRow(t, f.sup.Registry, protocol.CollectorProcessDetector, protocol.StateAbsent, protocol.DetailNone)
}

// A bundle that switches a Toggled provider off stops it, and a later bundle that switches it on
// starts it again, with no second Startup and no restart of any other provider.
func TestApplyPolicyTogglesAProviderWithoutARestart(t *testing.T) {
	f := newOrderingFixture(t)
	toggled := &toggledProvider{collector: protocol.CollectorProcessDetector, log: f.log, off: map[string]bool{"2": true}}
	addProvider(t, f, toggled)
	reg := f.sup.Registry

	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateHealthy, protocol.DetailNone)
	f.log.reset()

	reg.ApplyPolicy(policy.Bundle{Version: "2"})
	assertSequence(t, "switched off", f.log.all(), []string{"provider.Stop:process_detector"})
	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateAbsent, protocol.DetailDisabledByPolicy)

	// A bundle that keeps it off changes nothing.
	reg.ApplyPolicy(policy.Bundle{Version: "2"})
	assertSequence(t, "still off", f.log.all(), []string{"provider.Stop:process_detector"})

	reg.ApplyPolicy(policy.Bundle{Version: "3"})
	assertSequence(t, "switched on", f.log.all(), []string{"provider.Stop:process_detector", "provider.Start:process_detector"})
	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateHealthy, protocol.DetailNone)

	assertSequence(t, "supervisor steps", f.sup.Order(), StartupOrder())
	for _, c := range fixedCollectors {
		assertRow(t, reg, c, protocol.StateHealthy, protocol.DetailNone)
	}
	if starts, stops := toggled.counts(); starts != 2 || stops != 1 {
		t.Fatalf("toggled provider started %d and stopped %d times, want 2 and 1", starts, stops)
	}
}

// A provider the bundle in force switches off is not started by start_collectors and reports
// absent with disabled_by_policy; a bundle applied before startup only records the switch.
func TestStartCollectorsSkipsAProviderTheBundleInForceSwitchesOff(t *testing.T) {
	f := newOrderingFixture(t)
	toggled := &toggledProvider{collector: protocol.CollectorProcessDetector, log: f.log, off: map[string]bool{"1": true}}
	addProvider(t, f, toggled)
	reg := f.sup.Registry
	f.sup.Policy = &applyingPolicy{log: f.log, reg: reg, bundle: &policy.Bundle{Version: "1"}}

	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if starts, _ := toggled.counts(); starts != 0 {
		t.Fatalf("a provider the bundle in force switches off was started %d times", starts)
	}
	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateAbsent, protocol.DetailDisabledByPolicy)

	reg.ApplyPolicy(policy.Bundle{Version: "2"})
	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateHealthy, protocol.DetailNone)

	// After shutdown a bundle starts nothing.
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	reg.ApplyPolicy(policy.Bundle{Version: "1"})
	reg.ApplyPolicy(policy.Bundle{Version: "3"})
	if starts, stops := toggled.counts(); starts != 1 || stops != 1 {
		t.Fatalf("after shutdown the provider started %d and stopped %d times, want 1 and 1", starts, stops)
	}
}

// A provider that panics in Start during a policy toggle is absent, and the providers toggled
// with it, and the rest, are unaffected.
func TestPanicDuringAPolicyStartIsContainedToOneRow(t *testing.T) {
	f := newOrderingFixture(t)
	bad := &toggledProvider{collector: protocol.CollectorProcessDetector, log: f.log, off: map[string]bool{"2": true}, panicAfter: 1}
	good := &toggledProvider{collector: protocol.CollectorCaptureExtension, log: f.log, off: map[string]bool{"2": true}}
	addProvider(t, f, bad)
	addProvider(t, f, good)
	reg := f.sup.Registry

	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	reg.ApplyPolicy(policy.Bundle{Version: "2"})
	reg.ApplyPolicy(policy.Bundle{Version: "3"})

	assertRow(t, reg, protocol.CollectorProcessDetector, protocol.StateAbsent, protocol.DetailNone)
	assertRow(t, reg, protocol.CollectorCaptureExtension, protocol.StateHealthy, protocol.DetailNone)
	for _, c := range fixedCollectors {
		assertRow(t, reg, c, protocol.StateHealthy, protocol.DetailNone)
	}
}

// A fixed provider that is Toggled and switched off by the bundle in force is not started at its
// own step, and reports absent with disabled_by_policy. Once start_collectors has run, a bundle that
// switches it on starts it and one that switches it off stops it; the step order is unchanged.
func TestAFixedProviderTheBundleSwitchesOffWaitsForAToggle(t *testing.T) {
	log := &orderLog{}
	reg := NewRegistry(testClock, nil)
	loop := &recordingProvider{collector: protocol.CollectorLoopbackBroker, log: log}
	off := map[string]bool{"1": true, "3": true}
	shim := &toggledProvider{collector: protocol.CollectorCLIShim, log: log, off: off}
	proxy := &toggledProvider{collector: protocol.CollectorEgressProxy, log: log, off: off}
	for _, p := range []Provider{shim, proxy, loop} {
		if err := reg.Add(p); err != nil {
			t.Fatalf("Add(%s): %v", p.Name(), err)
		}
	}
	sup, err := NewSupervisor(reg, nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	sup.Policy = &applyingPolicy{log: log, reg: reg, bundle: &policy.Bundle{Version: "1"}}
	sup.Spool = &recordingSpool{log: log}
	sup.Loopback = loop

	if err := sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	assertSequence(t, "startup with both switched off", log.all(), []string{
		"policy.Load", "spool.Open", "provider.Start:loopback_broker",
	})
	assertSequence(t, "recorded startup steps", sup.Order(), StartupOrder())
	assertRow(t, reg, protocol.CollectorCLIShim, protocol.StateAbsent, protocol.DetailDisabledByPolicy)
	assertRow(t, reg, protocol.CollectorEgressProxy, protocol.StateAbsent, protocol.DetailDisabledByPolicy)

	log.reset()
	reg.ApplyPolicy(policy.Bundle{Version: "2"})
	started := log.all()
	slices.Sort(started)
	assertSequence(t, "switched on", started, []string{"provider.Start:cli_shim", "provider.Start:egress_proxy"})
	assertRow(t, reg, protocol.CollectorCLIShim, protocol.StateHealthy, protocol.DetailNone)
	assertRow(t, reg, protocol.CollectorEgressProxy, protocol.StateHealthy, protocol.DetailNone)

	log.reset()
	reg.ApplyPolicy(policy.Bundle{Version: "3"})
	stopped := log.all()
	slices.Sort(stopped)
	assertSequence(t, "switched off", stopped, []string{"provider.Stop:cli_shim", "provider.Stop:egress_proxy"})
	assertRow(t, reg, protocol.CollectorCLIShim, protocol.StateAbsent, protocol.DetailDisabledByPolicy)
	assertRow(t, reg, protocol.CollectorEgressProxy, protocol.StateAbsent, protocol.DetailDisabledByPolicy)
	assertRow(t, reg, protocol.CollectorLoopbackBroker, protocol.StateHealthy, protocol.DetailNone)

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, p := range []*toggledProvider{shim, proxy} {
		if starts, _ := p.counts(); starts != 1 {
			t.Errorf("%s started %d times, want once, by the toggle", p.collector, starts)
		}
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		if running {
			t.Errorf("%s is still running after shutdown", p.collector)
		}
	}
}
