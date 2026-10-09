package procmon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/etwsession"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const testDevice = "5f0f0c1e-0000-4000-8000-0000000000d1"

var testNow = time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)

// fakeSource is an event source a test writes events into.
type fakeSource struct {
	events chan etwsession.Event
	once   sync.Once
	closed chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{events: make(chan etwsession.Event, 64), closed: make(chan struct{})}
}

func (s *fakeSource) Events() <-chan etwsession.Event { return s.events }
func (s *fakeSource) Close()                          { s.once.Do(func() { close(s.closed) }) }

// end closes the event channel, as a session stopped from outside does.
func (s *fakeSource) end() { close(s.events) }

func startEvent(pid, parent uint32, image string) etwsession.Event {
	return etwsession.Event{
		Provider: KernelProcess.GUID,
		ID:       eventStart,
		Time:     testNow,
		Properties: map[string]string{
			"ProcessID":       strconv.FormatUint(uint64(pid), 10),
			"ParentProcessID": strconv.FormatUint(uint64(parent), 10),
			"ImageName":       `\Device\HarddiskVolume3\Users\ada\AppData\Local\AnthropicClaude\` + image,
		},
	}
}

func stopEvent(pid uint32) etwsession.Event {
	return etwsession.Event{
		Provider:   KernelProcess.GUID,
		ID:         eventStop,
		Time:       testNow,
		Properties: map[string]string{"ProcessID": strconv.FormatUint(uint64(pid), 10)},
	}
}

// recordingPipeline mints each fact through core.BuildEnvelope, as core.Pipeline does.
type recordingPipeline struct {
	mu    sync.Mutex
	facts []core.Fact
}

func (p *recordingPipeline) Record(_ context.Context, f core.Fact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := core.Identity{TenantID: "tenant-1", DeviceID: testDevice, UserRef: "u_console"}
	if f.Person != nil {
		id.UserRef = f.Person.UserRef
	}
	if _, err := core.BuildEnvelope(core.EnvelopeInput{
		Identity:        id,
		EventID:         fmt.Sprintf("00000000-0000-4000-8000-%012d", len(p.facts)+1),
		Kind:            f.Kind,
		Route:           f.Route,
		Mode:            protocol.ModeM0,
		ToolFingerprint: f.ToolFingerprint,
		OccurredAt:      f.OccurredAt,
		DedupKey:        f.DedupKey,
		FactFields:      f.FactFields,
	}); err != nil {
		return err
	}
	p.facts = append(p.facts, f)
	return nil
}

func (p *recordingPipeline) recorded() []core.Fact {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Fact(nil), p.facts...)
}

// recordingEmitter passes calls to a real discovery.Emitter and keeps them.
type recordingEmitter struct {
	e *discovery.Emitter

	mu    sync.Mutex
	emits []discovery.Record
	stops []discovery.Record
}

func (r *recordingEmitter) Emit(ctx context.Context, c *core.CounterSet, rec discovery.Record) error {
	r.mu.Lock()
	r.emits = append(r.emits, rec)
	r.mu.Unlock()
	return r.e.Emit(ctx, c, rec)
}

func (r *recordingEmitter) Stop(ctx context.Context, c *core.CounterSet, rec discovery.Record) {
	r.mu.Lock()
	r.stops = append(r.stops, rec)
	r.mu.Unlock()
	r.e.Stop(ctx, c, rec)
}

func (r *recordingEmitter) calls() (emits, stops []discovery.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]discovery.Record(nil), r.emits...), append([]discovery.Record(nil), r.stops...)
}

// lockedLog keeps the provider's log lines.
type lockedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *lockedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func testBundle() *policy.Bundle {
	b := &policy.Bundle{Catalog: []policy.CatalogApp{
		{AppKey: "claude_desktop", Category: "chat_assistant", Signals: []policy.CatalogSignal{
			{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "claude.exe"},
			{Platform: "windows", Kind: policy.SignalPublisher, Value: "Anthropic, PBC"},
		}},
		{AppKey: "claude_code", Category: "coding_agent", Signals: []policy.CatalogSignal{
			{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "claude.exe"},
			{Platform: "windows", Kind: policy.SignalPublisher, Value: "Anthropic Code Signing"},
		}},
		{AppKey: "cursor", Category: "ide", Signals: []policy.CatalogSignal{
			{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "Cursor.exe"},
			{Platform: "windows", Kind: policy.SignalPublisher, Value: "Anysphere"},
		}},
	}}
	b.Endpoint.Processes.Enabled = true
	b.Endpoint.DiscoveryDailyBudget = 200
	return b
}

// fakeHost answers for the processes a test describes.
type fakeHost struct {
	mu      sync.Mutex
	procs   map[uint32]hostinfo.Process
	running []proc
}

func (h *fakeHost) set(p hostinfo.Process) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs[p.PID] = p
}

func (h *fakeHost) host() host {
	return host{
		snapshot: func() ([]proc, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return append([]proc(nil), h.running...), nil
		},
		process: func(pid uint32) (hostinfo.Process, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			p, ok := h.procs[pid]
			if !ok {
				return hostinfo.Process{}, errors.New("no such process")
			}
			return p, nil
		},
		version: func(image string) string { return "0.14.10.0" },
	}
}

func ada() *hostinfo.User { return &hostinfo.User{SID: "S-1-5-21-1-2-3-1001", Account: `CONTOSO\ada`} }

func claudeProcess(pid uint32, publisher string) hostinfo.Process {
	return hostinfo.Process{
		PID:       pid,
		Image:     `C:\Users\ada\AppData\Local\AnthropicClaude\app-0.14.10\claude.exe`,
		Publisher: publisher,
		User:      ada(),
		Started:   testNow,
	}
}

type harness struct {
	t       *testing.T
	p       *Provider
	src     *fakeSource
	host    *fakeHost
	pipe    *recordingPipeline
	emitter *recordingEmitter
	log     *lockedLog

	mu      sync.Mutex
	failing bool
}

func newHarness(t *testing.T, running ...proc) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		src:  newFakeSource(),
		host: &fakeHost{procs: map[uint32]hostinfo.Process{}, running: running},
		pipe: &recordingPipeline{},
		log:  &lockedLog{},
	}
	bundle := testBundle()
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := discovery.New(discovery.Config{
		Pipeline: h.pipe,
		Dir:      dir,
		Clock:    func() time.Time { return testNow },
		Bundles:  func() *policy.Bundle { return bundle },
		DeviceID: testDevice,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.emitter = &recordingEmitter{e: e}
	h.p = newProvider(Config{
		Emitter: h.emitter,
		Events:  h.open,
		Bundles: func() *policy.Bundle { return bundle },
		Person:  func(u hostinfo.User) core.Person { return core.Person{UserRef: "u_" + u.SID} },
		Log:     h.log,
		Clock:   func() time.Time { return testNow },
	}, h.host.host())
	return h
}

// open hands out the harness's source, or fails while the harness is failing.
func (h *harness) open() (Source, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failing {
		return nil, errors.New("access denied")
	}
	return h.src, nil
}

// fail makes the next opens fail, and replaces the source the opens that succeed hand out.
func (h *harness) fail(failing bool, src *fakeSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failing = failing
	if src != nil {
		h.src = src
	}
}

func (h *harness) source() *fakeSource {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.src
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.p.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = h.p.Stop(context.Background()) })
}

func (h *harness) send(evs ...etwsession.Event) {
	src := h.source()
	for _, ev := range evs {
		src.events <- ev
	}
}

// waitFor polls cond until it holds or five seconds pass.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) waitCalls(emits, stops int) ([]discovery.Record, []discovery.Record) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d emits and %d stops", emits, stops), func() bool {
		e, s := h.emitter.calls()
		return len(e) >= emits && len(s) >= stops
	})
	e, s := h.emitter.calls()
	if len(e) != emits || len(s) != stops {
		h.t.Fatalf("emitter saw %d emits and %d stops, want %d and %d", len(e), len(s), emits, stops)
	}
	return e, s
}

// The start and then the stop of a catalog app give one record and one stop, and the service log
// names the app and PID once for each.
func TestCatalogAppStartThenStop(t *testing.T) {
	h := newHarness(t)
	h.host.set(claudeProcess(4242, "Anthropic, PBC"))
	h.start()

	h.send(startEvent(4242, 100, "claude.exe"))
	emits, _ := h.waitCalls(1, 0)
	r := emits[0]
	if r.Type != protocol.DiscoveryTypeAppRunning || r.Basis != protocol.DetectionBasisProcessEvent ||
		r.Route != protocol.RouteProcDetect || r.AppKey != "claude_desktop" || r.Version != "0.14.10.0" ||
		r.Publisher != "Anthropic, PBC" || r.Person == nil || r.Person.UserRef != "u_S-1-5-21-1-2-3-1001" ||
		!r.OccurredAt.Equal(testNow) {
		t.Fatalf("record = %+v", r)
	}
	facts := h.pipe.recorded()
	if len(facts) != 1 || facts[0].Kind != protocol.KindDiscovery || facts[0].ToolFingerprint != "app:claude_desktop" ||
		facts[0].Route != protocol.RouteProcDetect || facts[0].DiscoveryType != protocol.DiscoveryTypeAppRunning {
		t.Fatalf("pipeline facts = %+v", facts)
	}

	h.send(stopEvent(4242))
	_, stops := h.waitCalls(1, 1)
	if stops[0].AppKey != "claude_desktop" {
		t.Fatalf("stop = %+v", stops[0])
	}
	if n := len(h.pipe.recorded()); n != 1 {
		t.Fatalf("the stop recorded an envelope: %d facts", n)
	}
	want := []string{"procmon: app:claude_desktop started (pid 4242)", "procmon: app:claude_desktop stopped (pid 4242)"}
	if got := h.log.all(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("log = %q, want %q", got, want)
	}
	c := h.p.Health().Counters
	if c[protocol.CounterEmitted] != 1 || c[protocol.CounterObserved] != 2 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters = %v", c)
	}
}

// A process the catalog does not list gives nothing: no record, no stop, no log line.
func TestNonCatalogAppGivesNothing(t *testing.T) {
	h := newHarness(t)
	h.host.set(hostinfo.Process{PID: 7, Image: `C:\Windows\notepad.exe`, Publisher: "Microsoft Windows", User: ada()})
	h.host.set(claudeProcess(8, "Anthropic, PBC"))
	h.start()

	h.send(startEvent(7, 1, "notepad.exe"), stopEvent(7))
	// A catalog app's start after them shows they were handled.
	h.send(startEvent(8, 1, "claude.exe"))
	emits, _ := h.waitCalls(1, 0)
	if emits[0].AppKey != "claude_desktop" {
		t.Fatalf("the only record is %+v", emits[0])
	}
	for _, l := range h.log.all() {
		if l != "procmon: app:claude_desktop started (pid 8)" {
			t.Fatalf("unexpected log line %q", l)
		}
	}
}

// Two starts of the same app by the same user on one day give one record.
func TestTwoStartsInADayGiveOneRecord(t *testing.T) {
	h := newHarness(t)
	h.host.set(claudeProcess(10, "Anthropic, PBC"))
	h.host.set(claudeProcess(11, "Anthropic, PBC"))
	h.start()

	h.send(startEvent(10, 1, "claude.exe"), stopEvent(10), startEvent(11, 1, "claude.exe"))
	h.waitCalls(2, 1)
	if n := len(h.pipe.recorded()); n != 1 {
		t.Fatalf("%d envelopes for two starts in a day, want 1", n)
	}
	if c := h.p.Health().Counters; c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v", c)
	}
}

// A binary whose signer is not the catalog's publisher for the app is still recorded, with the
// signer observed, so an impostor is visible.
func TestMismatchedPublisherIsEmitted(t *testing.T) {
	h := newHarness(t)
	h.host.set(hostinfo.Process{PID: 66, Image: `C:\Users\ada\Downloads\Cursor.exe`, Publisher: "Totally Legit Software", User: ada()})
	h.start()

	h.send(startEvent(66, 1, "Cursor.exe"))
	emits, _ := h.waitCalls(1, 0)
	if emits[0].AppKey != "cursor" || emits[0].Publisher != "Totally Legit Software" {
		t.Fatalf("record = %+v", emits[0])
	}
	facts := h.pipe.recorded()
	if len(facts) != 1 || facts[0].Publisher != "Totally Legit Software" {
		t.Fatalf("pipeline facts = %+v", facts)
	}
}

// When two apps share an executable's name, the signer decides between them.
func TestSharedExecutableIsResolvedByPublisher(t *testing.T) {
	h := newHarness(t)
	h.host.set(claudeProcess(20, "Anthropic Code Signing"))
	h.start()

	h.send(startEvent(20, 1, "claude.exe"))
	emits, _ := h.waitCalls(1, 0)
	if emits[0].AppKey != "claude_code" {
		t.Fatalf("record = %+v, want app claude_code", emits[0])
	}
}

// An app's child processes of the same executable are one running app: one record, and one stop
// when the last of them exits.
func TestChildProcessesAreOneInstance(t *testing.T) {
	h := newHarness(t)
	for _, pid := range []uint32{30, 31, 32} {
		h.host.set(claudeProcess(pid, "Anthropic, PBC"))
	}
	h.start()

	h.send(startEvent(30, 1, "claude.exe"), startEvent(31, 30, "claude.exe"), startEvent(32, 31, "claude.exe"))
	h.send(stopEvent(30), stopEvent(32))
	h.waitCalls(1, 0)
	h.send(stopEvent(31))
	h.waitCalls(1, 1)
	want := []string{"procmon: app:claude_desktop started (pid 30)", "procmon: app:claude_desktop stopped (pid 30)"}
	if got := h.log.all(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

// Apps already running when the monitor starts are seen from the process list, parents before
// their children.
func TestAppsAlreadyRunningAreSeenAtStart(t *testing.T) {
	h := newHarness(t,
		proc{pid: 41, parent: 40, base: "claude.exe"},
		proc{pid: 40, parent: 4, base: "claude.exe"},
		proc{pid: 50, parent: 4, base: "explorer.exe"},
	)
	h.host.set(claudeProcess(40, "Anthropic, PBC"))
	h.host.set(claudeProcess(41, "Anthropic, PBC"))
	h.start()

	emits, _ := h.waitCalls(1, 0)
	if emits[0].AppKey != "claude_desktop" || !emits[0].OccurredAt.Equal(testNow) {
		t.Fatalf("record = %+v", emits[0])
	}
	// The event for a process the list already showed is not a second start.
	h.send(startEvent(40, 4, "claude.exe"), stopEvent(41), stopEvent(40))
	h.waitCalls(1, 1)
	if got := h.log.all(); len(got) != 2 || got[0] != "procmon: app:claude_desktop started (pid 40)" {
		t.Fatalf("log = %q", got)
	}
}

// A process gone before it could be opened is still recorded, unattributed and without a
// version or signer.
func TestExitedProcessIsRecordedUnattributed(t *testing.T) {
	h := newHarness(t)
	h.start()

	h.send(startEvent(90, 1, "claude.exe"))
	emits, _ := h.waitCalls(1, 0)
	r := emits[0]
	if r.AppKey != "claude_desktop" || r.Person != nil || r.UserRef != discovery.UnattributedUserRef || r.Version != "" || r.Publisher != "" {
		t.Fatalf("record = %+v", r)
	}
}

// Events from another provider, or with no process id, are not process starts.
func TestMalformedEventsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.host.set(claudeProcess(5, "Anthropic, PBC"))
	h.start()

	other := startEvent(5, 1, "claude.exe")
	other.Provider = "{EDD08927-9CC4-4E65-B970-C2560FB5C289}"
	broken := startEvent(5, 1, "claude.exe")
	delete(broken.Properties, "ProcessID")
	h.send(other, broken, startEvent(5, 1, "claude.exe"))
	h.waitCalls(1, 0)
	if c := h.p.Health().Counters; c[protocol.CounterErrors] != 1 {
		t.Fatalf("counters = %v, want one error for the event with no process id", c)
	}
}

// The provider is degraded with etw_session_failed while the source cannot be opened or has
// stopped delivering, healthy while it delivers, and the source is opened again after a failure.
func TestHealthFollowsTheSession(t *testing.T) {
	h := newHarness(t)
	h.p.reopen = 10 * time.Millisecond
	degraded := func() bool {
		got := h.p.Health()
		return got.State == protocol.StateDegraded && got.Detail == protocol.DetailETWSessionFailed
	}
	healthy := func() bool { return h.p.Health().State == protocol.StateHealthy }

	h.fail(true, nil)
	h.start()
	if !degraded() {
		t.Fatalf("health = %+v, want degraded/etw_session_failed after a failed open", h.p.Health())
	}
	h.fail(false, nil)
	h.waitFor("the session to open on a retry", healthy)

	first := h.source()
	h.fail(true, newFakeSource())
	first.end()
	h.waitFor("the provider to report the session lost", degraded)
	select {
	case <-first.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the source that stopped delivering was not closed")
	}
	h.fail(false, nil)
	h.waitFor("the session to open again", healthy)

	h.host.set(claudeProcess(3, "Anthropic, PBC"))
	h.send(startEvent(3, 1, "claude.exe"))
	h.waitCalls(1, 0)
}

// Where the platform has no ETW the provider is absent with etw_session_failed and runs nothing.
func TestUnsupportedPlatformIsAbsent(t *testing.T) {
	for name, events := range map[string]func() (Source, error){
		"no source":   nil,
		"unsupported": func() (Source, error) { return nil, etwsession.ErrUnsupported },
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(Config{Emitter: &recordingEmitter{}, Events: events}, (&fakeHost{}).host())
			if err := p.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := p.Health(); got.State != protocol.StateAbsent || got.Detail != protocol.DetailETWSessionFailed {
				t.Fatalf("health = %s/%s, want absent/etw_session_failed", got.State, got.Detail)
			}
			if err := p.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Stop closes the source, and the provider is absent until it starts again.
func TestStopClosesTheSource(t *testing.T) {
	h := newHarness(t)
	h.start()
	if err := h.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.src.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not close the source")
	}
	if got := h.p.Health(); got.State != protocol.StateAbsent || got.Detail != protocol.DetailNone {
		t.Fatalf("health after Stop = %s/%s", got.State, got.Detail)
	}
}

// LastRunning is now while an app runs, the time its last instance stopped once none does, and
// zero for an app never seen; stopping the monitor counts what still runs as stopped then.
func TestLastRunningFollowsTheApp(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	now := testNow
	h.p.cfg.Clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		return now
	}
	h.host.set(hostinfo.Process{PID: 9, Image: `C:\Program Files\cursor\Cursor.exe`, Publisher: "Anysphere", User: ada()})
	h.start()
	if got := h.p.LastRunning("cursor"); !got.IsZero() {
		t.Fatalf("LastRunning before any start = %v", got)
	}

	h.send(startEvent(9, 1, "Cursor.exe"))
	h.waitCalls(1, 0)
	want := advance(time.Hour)
	if got := h.p.LastRunning("cursor"); !got.Equal(want) {
		t.Fatalf("LastRunning while running = %v, want now (%v)", got, want)
	}
	h.send(stopEvent(9))
	h.waitCalls(1, 1)
	stopped := testNow.Add(time.Hour)
	advance(time.Hour)
	if got := h.p.LastRunning("cursor"); !got.Equal(stopped) {
		t.Fatalf("LastRunning after the stop = %v, want %v", got, stopped)
	}

	h.send(startEvent(10, 1, "Cursor.exe"))
	h.waitCalls(2, 1)
	stopped = advance(time.Minute)
	if err := h.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	advance(time.Hour)
	if got := h.p.LastRunning("cursor"); !got.Equal(stopped) {
		t.Fatalf("LastRunning after the monitor stopped = %v, want %v", got, stopped)
	}
	if got := h.p.LastRunning("claude_code"); !got.IsZero() {
		t.Fatalf("LastRunning of an app never seen = %v", got)
	}
}

// The monitor follows endpoint.processes.enabled.
func TestEnabledFollowsTheBundle(t *testing.T) {
	p := New(Config{})
	b := testBundle()
	if !p.Enabled(b) {
		t.Fatal("processes.enabled true is not enabled")
	}
	b.Endpoint.Processes.Enabled = false
	if p.Enabled(b) || p.Enabled(nil) {
		t.Fatal("the monitor is enabled with processes.enabled false or no bundle")
	}
	if p.Name() != protocol.CollectorProcessDetector {
		t.Fatalf("collector = %s", p.Name())
	}
}

func TestBaseNameAndProperties(t *testing.T) {
	for in, want := range map[string]string{
		`\Device\HarddiskVolume3\Program Files\cursor\Cursor.exe`: "Cursor.exe",
		`C:\Windows\notepad.exe`:                                  "notepad.exe",
		`claude.exe`:                                              "claude.exe",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]uint32{"4242": 4242, "0x10A2": 0x10a2, " 7 ": 7} {
		got, ok := uintProperty(etwsession.Event{Properties: map[string]string{"ProcessID": in}}, "ProcessID")
		if !ok || got != want {
			t.Errorf("uintProperty(%q) = %d, %v, want %d", in, got, ok, want)
		}
	}
	if _, ok := uintProperty(etwsession.Event{Properties: map[string]string{"ProcessID": "-1"}}, "ProcessID"); ok {
		t.Error("a negative process id parsed")
	}
}
