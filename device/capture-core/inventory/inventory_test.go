package inventory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ticks stands in for time.After: it records each wait the loop asks for and fires when the test
// says.
type ticks struct {
	mu    sync.Mutex
	waits []time.Duration
	ch    chan time.Time
}

func newTicks() *ticks { return &ticks{ch: make(chan time.Time)} }

func (k *ticks) after(d time.Duration) <-chan time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.waits = append(k.waits, d)
	return k.ch
}

func (k *ticks) tick(t *testing.T) {
	t.Helper()
	select {
	case k.ch <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("the scan loop is not waiting")
	}
}

// waitFor waits until the loop has asked for n waits, which it does after each scan.
func (k *ticks) waitFor(t *testing.T, n int) []time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		k.mu.Lock()
		w := append([]time.Duration(nil), k.waits...)
		k.mu.Unlock()
		if len(w) >= n {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop asked for %d waits, want %d", len(w), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// countingScanner returns one record per scan, or errors when told to.
type countingScanner struct {
	mu    sync.Mutex
	scans int
	fail  []error
}

func (s *countingScanner) Scan(context.Context, *policy.Bundle) ([]discovery.Record, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans++
	return []discovery.Record{{Type: protocol.DiscoveryTypeAppInstalled, Basis: protocol.DetectionBasisInstalledScan,
		AppKey: "cursor", UserRef: discovery.UnattributedUserRef}}, s.fail
}

func (s *countingScanner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scans
}

func (s *countingScanner) failWith(errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = errs
}

type fakeEmitter struct {
	mu   sync.Mutex
	recs []discovery.Record
}

func (e *fakeEmitter) Emit(_ context.Context, c *core.CounterSet, r discovery.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recs = append(e.recs, r)
	c.Add(protocol.CounterEmitted)
	return nil
}

func bundle(enabled bool, interval int) policy.Bundle {
	return policy.Bundle{Endpoint: policy.EndpointPolicy{Inventory: policy.EndpointInventory{Enabled: enabled, IntervalMinutes: interval}}}
}

func TestProviderIsSwitchedByTheBundle(t *testing.T) {
	p := New(Config{Scanners: []Scanner{&countingScanner{}}, Emitter: &fakeEmitter{}})
	if p.Name() != protocol.CollectorInventoryScanner {
		t.Fatalf("Name = %s", p.Name())
	}
	on, off := bundle(true, 360), bundle(false, 360)
	if p.Enabled(nil) || p.Enabled(&off) || !p.Enabled(&on) {
		t.Fatal("Enabled does not follow endpoint.inventory.enabled")
	}
}

// The provider scans at start, then on the interval; an interval change applies from the next
// wait on. Each record leaves on inv.scan with the scan's time.
func TestProviderScansAtStartAndOnTheInterval(t *testing.T) {
	scanner := &countingScanner{}
	em := &fakeEmitter{}
	ticks := newTicks()
	now := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	p := New(Config{Scanners: []Scanner{scanner}, Emitter: em, Clock: func() time.Time { return now }, After: ticks.after})
	if err := p.ApplyPolicy(bundle(true, 60)); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scanner.count() != 1 {
		t.Fatalf("%d scans at start, want 1", scanner.count())
	}
	if h := p.Health(); h.State != protocol.StateHealthy || !h.LastSuccess.Equal(now) {
		t.Fatalf("health after a complete scan = %s %s, last success %v", h.State, h.Detail, h.LastSuccess)
	}
	ticks.waitFor(t, 1)
	if err := p.ApplyPolicy(bundle(true, 15)); err != nil {
		t.Fatal(err)
	}
	ticks.tick(t)
	waits := ticks.waitFor(t, 2)
	if scanner.count() != 2 {
		t.Fatalf("%d scans after one tick, want 2", scanner.count())
	}
	if waits[0] != 60*time.Minute || waits[1] != 15*time.Minute {
		t.Fatalf("waits = %v, want 1h then 15m", waits)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	for _, r := range em.recs {
		if r.Route != protocol.RouteInvScan || !r.OccurredAt.Equal(now) {
			t.Fatalf("record %+v is not on inv.scan at the scan's time", r)
		}
	}
	if h := p.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after Stop = %s", h.State)
	}
}

// A scan that could not read everything is degraded with enumeration_partial and counts an error
// per read; the next complete scan is healthy again.
func TestProviderPartialScanIsDegraded(t *testing.T) {
	scanner := &countingScanner{}
	scanner.failWith(errors.New("reading the user S-1-5-21-1 uninstall entries: access denied"), errors.New("reading HKEY_USERS: access denied"))
	ticks := newTicks()
	p := New(Config{Scanners: []Scanner{scanner}, Emitter: &fakeEmitter{}, After: ticks.after})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	h := p.Health()
	if h.State != protocol.StateDegraded || h.Detail != protocol.DetailEnumerationPartial || !h.LastSuccess.IsZero() {
		t.Fatalf("health after a partial scan = %s %s %v", h.State, h.Detail, h.LastSuccess)
	}
	if h.Counters[protocol.CounterErrors] != 2 || h.Counters[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want 2 errors and the record still emitted", h.Counters)
	}
	scanner.failWith()
	ticks.waitFor(t, 1)
	ticks.tick(t)
	ticks.waitFor(t, 2)
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health after a complete scan = %s %s", h.State, h.Detail)
	}
}

// Where the platform has no scanner the provider is absent with tool_version_unsupported, running
// or not.
func TestProviderWithoutScannersIsUnsupported(t *testing.T) {
	p := New(Config{Emitter: &fakeEmitter{}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := p.Health()
	if h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolVersionUnsupported {
		t.Fatalf("health = %s %s", h.State, h.Detail)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Under the registry, a bundle that switches the scanner off stops it (absent,
// disabled_by_policy) and one that switches it on starts it again with a fresh scan, without a
// restart of anything else.
func TestProviderTogglesThroughTheRegistry(t *testing.T) {
	scanner := &countingScanner{}
	ticks := newTicks()
	p := New(Config{Scanners: []Scanner{scanner}, Emitter: &fakeEmitter{}, After: ticks.after})
	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}
	reg.ApplyPolicy(bundle(true, 360))
	for _, res := range reg.StartCollectors(context.Background()) {
		if res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	defer reg.StopAll(context.Background())
	if h, _ := reg.HealthFor(protocol.CollectorInventoryScanner); h.State != protocol.StateHealthy {
		t.Fatalf("health after start = %s %s", h.State, h.Detail)
	}
	reg.ApplyPolicy(bundle(false, 360))
	if h, _ := reg.HealthFor(protocol.CollectorInventoryScanner); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("health switched off = %s %s", h.State, h.Detail)
	}
	reg.ApplyPolicy(bundle(true, 360))
	if h, _ := reg.HealthFor(protocol.CollectorInventoryScanner); h.State != protocol.StateHealthy {
		t.Fatalf("health switched on again = %s %s", h.State, h.Detail)
	}
	if scanner.count() != 2 {
		t.Fatalf("%d scans, want one per start", scanner.count())
	}
}

// fixedScanner returns the same records at every scan.
type fixedScanner []discovery.Record

func (s fixedScanner) Scan(context.Context, *policy.Bundle) ([]discovery.Record, []error) {
	return s, nil
}

// Installed reports what the last scan found, with the lowest release among several installs and
// an unknown version only when no record carried one; before a scan and after Stop it reports
// nothing.
func TestInstalledReadsTheLastScan(t *testing.T) {
	recs := fixedScanner{
		{AppKey: "claude_code", Version: "2.1.295"},
		{AppKey: "claude_code", Version: "2.1.40"},
		{AppKey: "claude_code", Version: ""},
		{AppKey: "codex", Version: ""},
		{AppKey: "cursor", Version: "custom build"},
		{AppKey: "cursor", Version: "1.7.52"},
	}
	ticks := newTicks()
	p := New(Config{Scanners: []Scanner{recs}, Emitter: &fakeEmitter{}, After: ticks.after})
	if _, ok := p.Installed("claude_code"); ok {
		t.Fatal("Installed reports an app before any scan")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for app, want := range map[string]string{"claude_code": "2.1.40", "codex": "", "cursor": "1.7.52"} {
		if v, ok := p.Installed(app); !ok || v != want {
			t.Errorf("Installed(%s) = %q, %v; want %q", app, v, ok, want)
		}
	}
	if _, ok := p.Installed("copilot_cli"); ok {
		t.Error("Installed reports an app the scan did not find")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Installed("claude_code"); ok {
		t.Error("Installed reports the last scan after Stop")
	}
}
