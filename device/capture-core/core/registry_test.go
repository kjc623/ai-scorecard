package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeProvider is a provider that can be told to lie in every way the contract forbids, so
// the registry's overrides are tested rather than assumed.
type fakeProvider struct {
	collector  protocol.Collector
	startErr   error
	stopErr    error
	panicStart bool
	panicStop  bool
	health     Health
	mu         sync.Mutex
	starts     int
	stops      int
	applied    []string
	applyErr   error
}

func (f *fakeProvider) Name() protocol.Collector { return f.collector }

func (f *fakeProvider) Start(context.Context) error {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	if f.panicStart {
		panic("provider exploded during Start")
	}
	return f.startErr
}

func (f *fakeProvider) Stop(context.Context) error {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
	if f.panicStop {
		panic("provider exploded during Stop")
	}
	return f.stopErr
}

func (f *fakeProvider) Health() Health { return f.health }

func (f *fakeProvider) ApplyPolicy(b policy.Bundle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, b.Version)
	return f.applyErr
}

func healthyRow() Health {
	return Health{
		State:    protocol.StateHealthy,
		Detail:   protocol.DetailNone,
		Since:    time.Unix(0, 0),
		Counters: map[protocol.Counter]uint64{protocol.CounterObserved: 3},
	}
}

func testClock() time.Time { return time.Unix(1_700_000_000, 0) }

func newTestRegistry(t *testing.T, ps ...Provider) *Registry {
	t.Helper()
	r := NewRegistry(testClock, nil)
	for _, p := range ps {
		if err := r.Add(p); err != nil {
			t.Fatalf("Add(%s): %v", p.Name(), err)
		}
	}
	return r
}

func TestRegistryRejectsDuplicateCollector(t *testing.T) {
	r := newTestRegistry(t)
	a := &fakeProvider{collector: protocol.CollectorEgressProxy, health: healthyRow()}
	b := &fakeProvider{collector: protocol.CollectorEgressProxy, health: healthyRow()}
	if err := r.Add(a); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := r.Add(b); err == nil {
		t.Fatal("second provider for one collector was accepted; one provider owns one coverage row")
	}
}

func TestRegistryStartFailureDegradesOneRowOnly(t *testing.T) {
	bad := &fakeProvider{collector: protocol.CollectorEgressProxy, startErr: errors.New("bind failed")}
	good := &fakeProvider{collector: protocol.CollectorProcessDetector, health: healthyRow()}
	other := &fakeProvider{collector: protocol.CollectorCLIShim, health: healthyRow()}
	r := newTestRegistry(t, bad, good, other)

	results := r.StartAll(context.Background())
	if len(results) != 3 {
		t.Fatalf("expected 3 start results, got %d", len(results))
	}
	rows := r.RowsByName()
	if got := rows[string(protocol.CollectorEgressProxy)].State; got != protocol.StateAbsent {
		t.Errorf("failed provider state = %q, want absent", got)
	}
	for _, c := range []protocol.Collector{protocol.CollectorProcessDetector, protocol.CollectorCLIShim} {
		if got := rows[string(c)].State; got != protocol.StateHealthy {
			t.Errorf("provider %s state = %q, want healthy (one failure must not degrade other rows)", c, got)
		}
	}
}

// A provider that claims healthy after a failed Start must not surface as healthy: "no
// provider may fail into a state that reports success".
func TestRegistryNeverReportsHealthyAfterFailedStart(t *testing.T) {
	p := &fakeProvider{collector: protocol.CollectorLoopbackBroker, startErr: errors.New("preflight refused"), health: healthyRow()}
	r := newTestRegistry(t, p)
	_ = r.StartAll(context.Background())
	h, ok := r.HealthFor(protocol.CollectorLoopbackBroker)
	if !ok {
		t.Fatal("no health row for a registered provider")
	}
	if h.State != protocol.StateAbsent {
		t.Fatalf("state = %q, want absent: a provider that failed to start reported %q", h.State, protocol.StateHealthy)
	}
	reports, errs := r.Reports("device-1", "v1")
	if len(errs) != 0 {
		t.Fatalf("reports rejected: %v", errs)
	}
	if len(reports) != 1 || reports[0].State != protocol.StateAbsent {
		t.Fatalf("report = %+v, want one absent row", reports)
	}
	if reports[0].Collector != string(protocol.CollectorLoopbackBroker) {
		t.Fatalf("collector name = %q, want the provider's collector code", reports[0].Collector)
	}
}

func TestRegistryStopFailureIsTamperedNotAVisibleError(t *testing.T) {
	p := &fakeProvider{collector: protocol.CollectorEgressProxy, stopErr: errors.New("port still bound"), health: healthyRow()}
	r := newTestRegistry(t, p)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	_ = r.StartAll(context.Background())

	results := r.StopAll(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected one stop result, got %d", len(results))
	}
	// Stop never fails visibly: the outcome is reported, not returned as a shutdown error.
	if results[0].Err == nil {
		t.Fatal("the stop error was swallowed entirely; it must be reported so the row can be tampered")
	}
	h, _ := r.HealthFor(protocol.CollectorEgressProxy)
	if h.State != protocol.StateTampered {
		t.Fatalf("state after failed Stop = %q, want tampered (interference is evidence)", h.State)
	}
}

func TestRegistryNeverHealthyAfterStop(t *testing.T) {
	p := &fakeProvider{collector: protocol.CollectorCLIShim, health: healthyRow()}
	r := newTestRegistry(t, p)
	_ = r.StartAll(context.Background())
	if h, _ := r.HealthFor(protocol.CollectorCLIShim); h.State != protocol.StateHealthy {
		t.Fatalf("precondition: state = %q, want healthy", h.State)
	}
	r.StopCollector(context.Background(), protocol.CollectorCLIShim)
	h, _ := r.HealthFor(protocol.CollectorCLIShim)
	if h.State == protocol.StateHealthy {
		t.Fatal("provider reported healthy after Stop")
	}
}

func TestRegistryRecoversPanicsIntoOneRow(t *testing.T) {
	bad := &fakeProvider{collector: protocol.CollectorEgressProxy, panicStart: true}
	good := &fakeProvider{collector: protocol.CollectorProcessDetector, health: healthyRow()}
	r := newTestRegistry(t, bad, good)
	results := r.StartAll(context.Background())
	var panicked int
	for _, res := range results {
		if res.Panicked != "" {
			panicked++
		}
	}
	if panicked != 1 {
		t.Fatalf("recovered %d panics, want 1", panicked)
	}
	rows := r.RowsByName()
	if rows[string(protocol.CollectorEgressProxy)].State != protocol.StateAbsent {
		t.Fatal("a provider that panicked during Start reported something other than absent")
	}
	if rows[string(protocol.CollectorProcessDetector)].State != protocol.StateHealthy {
		t.Fatal("a panic in one provider degraded another row")
	}
}

func TestRegistrySanitisesInventedCounters(t *testing.T) {
	p := &fakeProvider{collector: protocol.CollectorCaptureExtension, health: Health{
		State: protocol.StateHealthy,
		Counters: map[protocol.Counter]uint64{
			protocol.CounterObserved: 1,
			"per_tool_requests":      99, // not in the closed seven
		},
	}}
	r := newTestRegistry(t, p)
	_ = r.StartAll(context.Background())
	h, _ := r.HealthFor(protocol.CollectorCaptureExtension)
	if _, ok := h.Counters["per_tool_requests"]; ok {
		t.Fatal("an invented counter name reached the health channel")
	}
	if h.Counters[protocol.CounterErrors] != 1 {
		t.Fatalf("errors counter = %d, want 1 for the refused counter name", h.Counters[protocol.CounterErrors])
	}
}

func TestRegistrySanitisesUnknownState(t *testing.T) {
	p := &fakeProvider{collector: protocol.CollectorProcessDetector, health: Health{State: "fine", Counters: map[protocol.Counter]uint64{}}}
	r := newTestRegistry(t, p)
	_ = r.StartAll(context.Background())
	h, _ := r.HealthFor(protocol.CollectorProcessDetector)
	if h.State != protocol.StateAbsent {
		t.Fatalf("state = %q, want absent for an out-of-vocabulary state", h.State)
	}
}

func TestCounterSetClosedSevenAndWindow(t *testing.T) {
	c := NewCounterSet(testClock())
	c.Add(protocol.CounterObserved)
	c.Incr(protocol.CounterEmitted, 2)
	c.Incr(protocol.Counter("invented_by_a_provider"), 5)

	cum := c.Cumulative()
	if cum[protocol.CounterObserved] != 1 || cum[protocol.CounterEmitted] != 2 {
		t.Fatalf("cumulative = %v", cum)
	}
	if _, ok := cum[protocol.Counter("invented_by_a_provider")]; ok {
		t.Fatal("an invented counter became a first-class counter")
	}
	if cum[protocol.CounterErrors] != 1 {
		t.Fatalf("errors = %d, want 1", cum[protocol.CounterErrors])
	}
	if names := c.InvalidNames(); len(names) != 1 || names[0] != "invented_by_a_provider" {
		t.Fatalf("invalid names = %v, want the refused name recorded", names)
	}
	win, since := c.Window()
	if win[protocol.CounterObserved] != 1 || !since.Equal(testClock()) {
		t.Fatalf("window = %v since %v", win, since)
	}
	c.RollWindow(testClock().Add(time.Minute))
	win, _ = c.Window()
	if win[protocol.CounterObserved] != 0 {
		t.Fatalf("window delta after roll = %d, want 0", win[protocol.CounterObserved])
	}
	if c.Cumulative()[protocol.CounterObserved] != 1 {
		t.Fatal("rolling the window reset the cumulative total; a roll is a reporting boundary, not a reset")
	}
}

func TestHealthValidateAndReport(t *testing.T) {
	h := healthyRow()
	if err := h.Validate(); err != nil {
		t.Fatalf("healthy row rejected: %v", err)
	}
	bad := Health{State: protocol.StateHealthy, Counters: map[protocol.Counter]uint64{"nope": 1}}
	if err := bad.Validate(); err == nil {
		t.Fatal("a row with an out-of-vocabulary counter validated")
	}
	rep := h.Report("dev-1", "bundle-7")
	rep.Collector = string(protocol.CollectorEgressProxy)
	if err := rep.Validate(); err != nil {
		t.Fatalf("protocol rejected the report: %v", err)
	}
	if len(rep.Counters) != len(protocol.AllCounters) {
		t.Fatalf("report carries %d counters, want the complete closed set of %d", len(rep.Counters), len(protocol.AllCounters))
	}
}

func TestApplyPolicyReachesEveryProviderAndNeverRestarts(t *testing.T) {
	a := &fakeProvider{collector: protocol.CollectorEgressProxy, health: healthyRow()}
	b := &fakeProvider{collector: protocol.CollectorProcessDetector, health: healthyRow()}
	r := newTestRegistry(t, a, b)
	_ = r.StartAll(context.Background())
	before := a.starts
	results := r.ApplyPolicy(policy.Bundle{Version: "42"})
	if len(results) != 2 {
		t.Fatalf("apply results = %d, want 2", len(results))
	}
	for _, res := range results {
		if res.Err != nil {
			t.Fatalf("apply for %s failed: %v", res.Collector, res.Err)
		}
	}
	if a.starts != before {
		t.Fatal("ApplyPolicy restarted a provider; policy application is a diff, never a restart")
	}
	if len(a.applied) != 1 || a.applied[0] != "42" {
		t.Fatalf("provider saw %v, want the bundle version", a.applied)
	}
}

func TestResourceStackRollsBackInReverseOnFailedStart(t *testing.T) {
	var mu sync.Mutex
	var order []string
	stack := &ResourceStack{}
	stack.On(func() { mu.Lock(); order = append(order, "release-a"); mu.Unlock() })
	stack.On(func() { mu.Lock(); order = append(order, "release-b"); mu.Unlock() })

	err := stack.Failed(errors.New("second acquisition failed"))
	if err == nil {
		t.Fatal("Failed swallowed the error")
	}
	got := strings.Join(order, ",")
	if got != "release-b,release-a" {
		t.Fatalf("release order = %q, want reverse acquisition order", got)
	}
	stack.On(func() { mu.Lock(); order = append(order, "release-c"); mu.Unlock() })
	stack.Commit()
	stack.Rollback() // commit dropped the release; a committed start must not be torn down
	if strings.Contains(strings.Join(order, ","), "release-c") {
		t.Fatal("commit did not drop the pending releases")
	}
}

func TestCounterSetSnapshotCarriesWindowAndCumulative(t *testing.T) {
	c := NewCounterSet(testClock())
	c.Add(protocol.CounterDropped)
	c.RollWindow(testClock().Add(time.Minute))
	c.Add(protocol.CounterDropped)
	h := c.Snapshot(protocol.StateDegraded, protocol.DetailSpoolUnwritable, testClock(), time.Time{})
	if h.Counters[protocol.CounterDropped] != 2 {
		t.Fatalf("cumulative dropped = %d, want 2", h.Counters[protocol.CounterDropped])
	}
	if h.WindowDelta[protocol.CounterDropped] != 1 {
		t.Fatalf("window dropped = %d, want 1", h.WindowDelta[protocol.CounterDropped])
	}
	if h.LastSuccess.IsZero() != true {
		t.Fatal("a zero LastSuccess must stay zero: zero means \"never\"")
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("snapshot did not validate: %v", err)
	}
}
