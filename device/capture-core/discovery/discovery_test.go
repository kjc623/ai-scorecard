package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const testDevice = "5f0f0c1e-0000-4000-8000-0000000000d1"

// recordingPipeline mints each fact through core.BuildEnvelope, as core.Pipeline does, and keeps
// what it minted.
type recordingPipeline struct {
	mu    sync.Mutex
	facts []core.Fact
	raw   [][]byte
	fail  error
}

func (p *recordingPipeline) Record(_ context.Context, f core.Fact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	id := core.Identity{TenantID: "tenant-1", DeviceID: testDevice, UserRef: "u_console"}
	if f.Person != nil {
		id.UserRef, id.SubjectName = f.Person.UserRef, f.Person.SubjectName
	}
	raw, err := core.BuildEnvelope(core.EnvelopeInput{
		Identity:          id,
		EventID:           fmt.Sprintf("00000000-0000-4000-8000-%012d", len(p.facts)+1),
		Kind:              f.Kind,
		Route:             f.Route,
		Mode:              protocol.ModeM0,
		ToolFingerprint:   f.ToolFingerprint,
		OccurredAt:        f.OccurredAt,
		MonotonicOffsetMS: f.MonotonicOffsetMS,
		DedupKey:          f.DedupKey,
		FactFields:        f.FactFields,
	})
	if err != nil {
		return err
	}
	p.facts = append(p.facts, f)
	p.raw = append(p.raw, raw)
	return nil
}

func (p *recordingPipeline) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.facts)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	dir    state.Dir
	clock  *clock
	pipe   *recordingPipeline
	budget int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		dir:    dir,
		clock:  &clock{now: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)},
		pipe:   &recordingPipeline{},
		budget: 200,
	}
}

func (f *fixture) emitter(t *testing.T) *Emitter {
	t.Helper()
	e, err := New(Config{
		Pipeline: f.pipe,
		Dir:      f.dir,
		Clock:    f.clock.Now,
		Bundles: func() *policy.Bundle {
			return &policy.Bundle{Endpoint: policy.EndpointPolicy{DiscoveryDailyBudget: f.budget}}
		},
		DeviceID: testDevice,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *fixture) installed(appKey string) Record {
	return Record{
		Type:       protocol.DiscoveryTypeAppInstalled,
		Basis:      protocol.DetectionBasisInstalledScan,
		Route:      protocol.RouteInvScan,
		AppKey:     appKey,
		Version:    "0.48.1",
		Publisher:  "Anysphere, Inc.",
		UserRef:    UnattributedUserRef,
		OccurredAt: f.clock.Now(),
	}
}

func counters(c *core.CounterSet) map[protocol.Counter]uint64 { return c.Cumulative() }

func wantCounters(t *testing.T, c *core.CounterSet, observed, emitted, dropped, errs uint64) {
	t.Helper()
	got := counters(c)
	if got[protocol.CounterObserved] != observed || got[protocol.CounterEmitted] != emitted ||
		got[protocol.CounterDropped] != dropped || got[protocol.CounterErrors] != errs {
		t.Fatalf("counters = %v, want observed %d, emitted %d, dropped %d, errors %d", got, observed, emitted, dropped, errs)
	}
}

func TestTheSameRecordTwiceInADayIsOneEnvelope(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	r := f.installed("cursor")
	for range 2 {
		if err := e.Emit(context.Background(), c, r); err != nil {
			t.Fatal(err)
		}
	}
	later := r
	later.OccurredAt = r.OccurredAt.Add(10 * time.Hour)
	f.clock.advance(10 * time.Hour)
	if err := e.Emit(context.Background(), c, later); err != nil {
		t.Fatal(err)
	}
	if n := f.pipe.count(); n != 1 {
		t.Fatalf("%d envelopes, want 1", n)
	}
	wantCounters(t, c, 0, 1, 0, 0)
}

func TestTheEmittedFactIsTheRecord(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	r := f.installed("cursor")
	if err := e.Emit(context.Background(), c, r); err != nil {
		t.Fatal(err)
	}
	got := f.pipe.facts[0]
	want := "sha256:" + Key(testDevice, UnattributedUserRef, r)
	if got.Kind != protocol.KindDiscovery || got.Route != protocol.RouteInvScan || got.ToolFingerprint != "app:cursor" ||
		got.DedupKey != want || got.Person == nil || got.Person.UserRef != UnattributedUserRef ||
		got.AppVersion != "0.48.1" || got.Publisher != "Anysphere, Inc." || !got.OccurredAt.Equal(r.OccurredAt) {
		t.Fatalf("fact = %+v", got)
	}
	var env map[string]any
	if err := json.Unmarshal(f.pipe.raw[0], &env); err != nil {
		t.Fatal(err)
	}
	if env["user_ref"] != UnattributedUserRef || env["dedup_key"] != want || env["discovery_type"] != "app_installed" {
		t.Fatalf("envelope = %s", f.pipe.raw[0])
	}
}

// The key is the §6 tuple: each part of it tells two records apart, and nothing else does.
func TestTheKeyIsDeviceUserTypeAppVersionDestinationAndDay(t *testing.T) {
	f := newFixture(t)
	base := f.installed("cursor")
	k := Key(testDevice, "u_a", base)
	for name, other := range map[string]string{
		"device":      Key("other-device", "u_a", base),
		"user":        Key(testDevice, "u_b", base),
		"type":        Key(testDevice, "u_a", func() Record { r := base; r.Type = protocol.DiscoveryTypeAppRunning; return r }()),
		"app":         Key(testDevice, "u_a", func() Record { r := base; r.AppKey = "chatgpt_desktop"; return r }()),
		"version":     Key(testDevice, "u_a", func() Record { r := base; r.Version = "0.49.0"; return r }()),
		"destination": Key(testDevice, "u_a", func() Record { r := base; r.DestinationHost = "api.openai.com"; return r }()),
		"day":         Key(testDevice, "u_a", func() Record { r := base; r.OccurredAt = r.OccurredAt.Add(24 * time.Hour); return r }()),
	} {
		if other == k {
			t.Errorf("a different %s gives the same key", name)
		}
	}
	same := base
	same.Publisher, same.Basis, same.ModelNames = "Someone else", protocol.DetectionBasisPackageScan, []string{"m"}
	same.OccurredAt = time.Date(2026, 10, 8, 23, 59, 59, 0, time.UTC)
	if Key(testDevice, "u_a", same) != k {
		t.Error("fields outside the key, or another time on the same UTC day, change the key")
	}
	if len(k) != 64 {
		t.Errorf("key %q is not hex sha256", k)
	}
}

func TestANewDayEmitsAgain(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(24 * time.Hour)
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	if n := f.pipe.count(); n != 2 {
		t.Fatalf("%d envelopes, want 2", n)
	}
	wantCounters(t, c, 0, 2, 0, 0)

	var s seen
	raw, err := os.ReadFile(f.dir.Path(SeenFile))
	if err != nil || json.Unmarshal(raw, &s) != nil {
		t.Fatalf("seen file: %v %s", err, raw)
	}
	if s.Day != "2026-10-09" || len(s.Keys) != 1 {
		t.Fatalf("seen file holds %+v, want today's one key", s)
	}
}

func TestARestartDoesNotReEmit(t *testing.T) {
	f := newFixture(t)
	c := core.NewCounterSet(f.clock.Now())
	if err := f.emitter(t).Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Hour)
	restarted := f.emitter(t)
	if err := restarted.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	if n := f.pipe.count(); n != 1 {
		t.Fatalf("%d envelopes after a restart, want 1", n)
	}

	// A restart on a later day starts the day empty.
	f.clock.advance(24 * time.Hour)
	if err := f.emitter(t).Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	if n := f.pipe.count(); n != 2 {
		t.Fatalf("%d envelopes after a restart the next day, want 2", n)
	}
	wantCounters(t, c, 0, 2, 0, 0)
}

func TestTheRecordPastTheBudgetIsDroppedAndCounted(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	for i := range 201 {
		if err := e.Emit(context.Background(), c, f.installed(fmt.Sprintf("app_%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.pipe.count(); n != 200 {
		t.Fatalf("%d envelopes, want 200", n)
	}
	wantCounters(t, c, 0, 200, 1, 0)

	// The budget is the day's, so a restart does not refill it.
	if err := f.emitter(t).Emit(context.Background(), c, f.installed("app_new")); err != nil {
		t.Fatal(err)
	}
	wantCounters(t, c, 0, 200, 2, 0)

	// The next day has a budget again.
	f.clock.advance(24 * time.Hour)
	if err := e.Emit(context.Background(), c, f.installed("app_200")); err != nil {
		t.Fatal(err)
	}
	wantCounters(t, c, 0, 201, 2, 0)
}

func TestNoBundleLeavesNoBudget(t *testing.T) {
	f := newFixture(t)
	e, err := New(Config{Pipeline: f.pipe, Dir: f.dir, Clock: f.clock.Now, Bundles: func() *policy.Bundle { return nil }, DeviceID: testDevice})
	if err != nil {
		t.Fatal(err)
	}
	c := core.NewCounterSet(f.clock.Now())
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	if f.pipe.count() != 0 {
		t.Fatal("a record left with no bundle in force")
	}
	wantCounters(t, c, 0, 0, 1, 0)
}

func TestAStopIsNeverEmitted(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	running := Record{
		Type: protocol.DiscoveryTypeAppRunning, Basis: protocol.DetectionBasisProcessEvent, Route: protocol.RouteProcDetect,
		AppKey: "claude_desktop", Version: "1.0.0", UserRef: "u_a", Person: &core.Person{UserRef: "u_a"}, OccurredAt: f.clock.Now(),
	}
	e.Stop(context.Background(), c, running)
	if f.pipe.count() != 0 {
		t.Fatal("a stop became an envelope")
	}
	wantCounters(t, c, 1, 0, 0, 0)

	// A start, a stop and a second start the same day are one envelope.
	for _, step := range []func(){
		func() { _ = e.Emit(context.Background(), c, running) },
		func() { e.Stop(context.Background(), c, running) },
		func() { _ = e.Emit(context.Background(), c, running) },
	} {
		step()
	}
	if n := f.pipe.count(); n != 1 {
		t.Fatalf("%d envelopes, want 1", n)
	}
	wantCounters(t, c, 2, 1, 0, 0)
}

// One record of every discovery_type, with the fields that type carries, mints an envelope.
func TestEveryEmittedFactBuildsAnEnvelope(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	now := f.clock.Now()
	records := []Record{
		{Type: protocol.DiscoveryTypeAppInstalled, Basis: protocol.DetectionBasisInstalledScan, Route: protocol.RouteInvScan,
			AppKey: "claude_desktop", Version: "1.0.0", Publisher: "Anthropic, PBC", UserRef: UnattributedUserRef, OccurredAt: now},
		{Type: protocol.DiscoveryTypeAppRunning, Basis: protocol.DetectionBasisProcessEvent, Route: protocol.RouteProcDetect,
			AppKey: "claude_desktop", Version: "1.0.0", Publisher: "Anthropic, PBC",
			Person: &core.Person{UserRef: "u_a", SubjectName: "ada@contoso.com"}, OccurredAt: now},
		{Type: protocol.DiscoveryTypeCLIInstalled, Basis: protocol.DetectionBasisPackageScan, Route: protocol.RouteInvScan,
			AppKey: "claude_code", Version: "2.0.1", UserRef: "u_a", OccurredAt: now},
		{Type: protocol.DiscoveryTypeIDEExtension, Basis: protocol.DetectionBasisExtensionScan, Route: protocol.RouteInvScan,
			AppKey: "github_copilot", Version: "1.250.0", HostApp: "app:vscode", UserRef: "u_a", OccurredAt: now},
		{Type: protocol.DiscoveryTypeLocalModel, Basis: protocol.DetectionBasisModelStore, Route: protocol.RouteInvScan,
			AppKey: "ollama", Version: "0.12.3", ModelNames: []string{"llama3.1:8b", "qwen2.5-coder:7b"}, UserRef: "u_a", OccurredAt: now},
		{Type: protocol.DiscoveryTypeInferenceConnection, Basis: protocol.DetectionBasisFlowMetadata, Route: protocol.RouteNetFlow,
			AppKey: "cursor", DestinationHost: "api.openai.com", UserRef: "u_a", OccurredAt: now},
	}
	for _, r := range records {
		if err := e.Emit(context.Background(), c, r); err != nil {
			t.Fatalf("%s: %v", r.Type, err)
		}
	}
	if n := f.pipe.count(); n != len(records) {
		t.Fatalf("%d envelopes, want %d", n, len(records))
	}
	for i, raw := range f.pipe.raw {
		if err := core.ValidateEnvelopeMode(raw); err != nil {
			t.Errorf("%s: %v", records[i].Type, err)
		}
	}
	wantCounters(t, c, 0, uint64(len(records)), 0, 0)
}

func TestAFailedRecordIsCountedAndRetried(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	f.pipe.fail = errors.New("spool unavailable")
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err == nil {
		t.Fatal("a failed record reported success")
	}
	wantCounters(t, c, 0, 0, 0, 1)
	f.pipe.fail = nil
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	if n := f.pipe.count(); n != 1 {
		t.Fatalf("%d envelopes, want the retried one", n)
	}
	wantCounters(t, c, 0, 1, 0, 1)
}

func TestAnInvalidRecordIsAnError(t *testing.T) {
	f := newFixture(t)
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	noApp := f.installed("")
	wrongRoute := f.installed("cursor")
	wrongRoute.Route = protocol.RouteToolOTel
	badType := f.installed("cursor")
	badType.Type = "app_uninstalled"
	for _, r := range []Record{noApp, wrongRoute, badType} {
		if err := e.Emit(context.Background(), c, r); err == nil {
			t.Errorf("%+v was emitted", r)
		}
	}
	if f.pipe.count() != 0 {
		t.Fatal("an invalid record became an envelope")
	}
	wantCounters(t, c, 0, 0, 0, 3)
}

func TestACorruptSeenFileIsReplaced(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.dir.Path(SeenFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := f.emitter(t)
	raw, err := os.ReadFile(f.dir.Path(SeenFile))
	if err != nil || json.Unmarshal(raw, &seen{}) != nil {
		t.Fatalf("the corrupt file was not replaced: %v %s", err, raw)
	}
	c := core.NewCounterSet(f.clock.Now())
	if err := e.Emit(context.Background(), c, f.installed("cursor")); err != nil {
		t.Fatal(err)
	}
	wantCounters(t, c, 0, 1, 0, 1)

	other := core.NewCounterSet(f.clock.Now())
	if err := e.Emit(context.Background(), other, f.installed("chatgpt_desktop")); err != nil {
		t.Fatal(err)
	}
	wantCounters(t, other, 0, 1, 0, 0)
}

func TestASeenFileWithAnUnreadableDayIsReplaced(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.dir.Path(SeenFile), []byte(`{"day":"yesterday","keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := f.emitter(t)
	c := core.NewCounterSet(f.clock.Now())
	e.Stop(context.Background(), c, Record{})
	wantCounters(t, c, 1, 0, 0, 1)
}

// Collectors emit from their own goroutines: every distinct record leaves once, within the budget,
// and every attempt at a record that did not fit is dropped.
func TestConcurrentCollectors(t *testing.T) {
	f := newFixture(t)
	f.budget = 50
	e := f.emitter(t)
	sets := []*core.CounterSet{core.NewCounterSet(f.clock.Now()), core.NewCounterSet(f.clock.Now()), core.NewCounterSet(f.clock.Now())}
	var wg sync.WaitGroup
	for g := range 12 {
		wg.Go(func() {
			c := sets[g%len(sets)]
			for i := range 60 {
				r := f.installed(fmt.Sprintf("app_%03d", i))
				if err := e.Emit(context.Background(), c, r); err != nil {
					t.Error(err)
				}
				e.Stop(context.Background(), c, r)
			}
		})
	}
	wg.Wait()
	if n := f.pipe.count(); n != 50 {
		t.Fatalf("%d envelopes, want 50", n)
	}
	var emitted, dropped, observed uint64
	for _, c := range sets {
		got := counters(c)
		emitted += got[protocol.CounterEmitted]
		dropped += got[protocol.CounterDropped]
		observed += got[protocol.CounterObserved]
	}
	if emitted != 50 || dropped != 12*10 || observed != 12*60 {
		t.Fatalf("emitted %d, dropped %d, observed %d", emitted, dropped, observed)
	}
}
