package flowmon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

const testDevice = "5f0f0c1e-0000-4000-8000-0000000000d2"

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

// dnsEvent is a completed query for name by pid, answered with results.
func dnsEvent(pid uint32, at time.Time, name, results string) etwsession.Event {
	return etwsession.Event{
		Provider: DNSClient.GUID,
		ID:       eventQueryCompleted,
		PID:      pid,
		Time:     at,
		Properties: map[string]string{
			"QueryName":    name,
			"QueryType":    "1",
			"QueryOptions": "140737488355328",
			"QueryStatus":  "0",
			"QueryResults": results,
		},
	}
}

// connectEvent is pid's TCP connect to daddr:443, written as the kernel writes it: in the context
// of whichever process was running.
func connectEvent(pid uint32, at time.Time, daddr string) etwsession.Event {
	id := eventConnectIPv4
	saddr := "10.0.0.5"
	if strings.Contains(daddr, ":") {
		id, saddr = eventConnectIPv6, "fe80::1"
	}
	return etwsession.Event{
		Provider: KernelNetwork.GUID,
		ID:       id,
		PID:      4,
		Time:     at,
		Properties: map[string]string{
			"PID":   strconv.FormatUint(uint64(pid), 10),
			"size":  "0",
			"daddr": daddr,
			"saddr": saddr,
			"dport": "443",
			"sport": "50123",
		},
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
}

func (r *recordingEmitter) Emit(ctx context.Context, c *core.CounterSet, rec discovery.Record) error {
	r.mu.Lock()
	r.emits = append(r.emits, rec)
	r.mu.Unlock()
	return r.e.Emit(ctx, c, rec)
}

func (r *recordingEmitter) calls() []discovery.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]discovery.Record(nil), r.emits...)
}

func testBundle() *policy.Bundle {
	b := &policy.Bundle{Catalog: []policy.CatalogApp{
		{AppKey: "openai_api", Category: "api", Signals: []policy.CatalogSignal{
			{Platform: "any", Kind: policy.SignalInferenceDomain, Value: "api.openai.com"},
		}},
		{AppKey: "anthropic_api", Category: "api", Signals: []policy.CatalogSignal{
			{Platform: "any", Kind: policy.SignalInferenceDomain, Value: "api.anthropic.com"},
		}},
		// The seed catalog gives Claude Code the API's domain too; the first app in catalog order
		// is the domain's.
		{AppKey: "claude_code", Category: "coding_agent", Signals: []policy.CatalogSignal{
			{Platform: "any", Kind: policy.SignalInferenceDomain, Value: "api.anthropic.com"},
		}},
		{AppKey: "cursor", Category: "ide", Signals: []policy.CatalogSignal{
			{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "Cursor.exe"},
		}},
	}}
	b.Endpoint.Flows.Enabled = true
	b.Endpoint.DiscoveryDailyBudget = 200
	return b
}

// fakeHost answers for the processes a test describes.
type fakeHost struct {
	mu    sync.Mutex
	procs map[uint32]hostinfo.Process
}

func (h *fakeHost) set(p hostinfo.Process) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs[p.PID] = p
}

func (h *fakeHost) process(pid uint32) (hostinfo.Process, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.procs[pid]
	if !ok {
		return hostinfo.Process{}, errors.New("no such process")
	}
	return p, nil
}

func user(sid string) *hostinfo.User {
	return &hostinfo.User{SID: sid, Account: `CONTOSO\` + sid}
}

func curl(pid uint32, u *hostinfo.User) hostinfo.Process {
	return hostinfo.Process{
		PID:       pid,
		Image:     `C:\Windows\System32\curl.exe`,
		Publisher: "Microsoft Windows",
		User:      u,
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

	mu      sync.Mutex
	failing bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		src:  newFakeSource(),
		host: &fakeHost{procs: map[uint32]hostinfo.Process{}},
		pipe: &recordingPipeline{},
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
		Clock:   func() time.Time { return testNow },
	}, h.host.process)
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

// waitEmits waits for n calls to the emitter and fails if there are more.
func (h *harness) waitEmits(n int) []discovery.Record {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d emits", n), func() bool { return len(h.emitter.calls()) >= n })
	got := h.emitter.calls()
	if len(got) != n {
		h.t.Fatalf("emitter saw %d records, want %d: %+v", len(got), n, got)
	}
	return got
}

// marker is a curl-shaped sequence by another process to api.anthropic.com. Its record, arriving
// after the events under test, shows those were handled.
func (h *harness) marker(at time.Time) {
	h.host.set(curl(999, user("S-1-5-21-9")))
	h.send(dnsEvent(999, at, "api.anthropic.com", "::ffff:160.79.104.10;"), connectEvent(999, at, "160.79.104.10"))
}

// A curl-shaped sequence, the DNS answer and then the connect from the same process, gives one
// inference_connection attributed to the process's owner, with the domain's app, the host name and
// the signer.
func TestCurlShapedSequenceGivesOneRecord(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(4242, user("S-1-5-21-1-2-3-1001")))
	h.start()

	h.send(
		dnsEvent(4242, testNow, "API.OpenAI.com.", "type:  5 api.openai.com.cdn.cloudflare.net;::ffff:162.159.140.245;::ffff:172.66.0.243;"),
		connectEvent(4242, testNow.Add(50*time.Millisecond), "172.66.0.243"),
	)
	emits := h.waitEmits(1)
	r := emits[0]
	if r.Type != protocol.DiscoveryTypeInferenceConnection || r.Basis != protocol.DetectionBasisFlowMetadata ||
		r.Route != protocol.RouteNetFlow || r.AppKey != "openai_api" || r.DestinationHost != "api.openai.com" ||
		r.Publisher != "Microsoft Windows" || r.Person == nil || r.Person.UserRef != "u_S-1-5-21-1-2-3-1001" ||
		r.UserRef != "u_S-1-5-21-1-2-3-1001" || !r.OccurredAt.Equal(testNow.Add(50*time.Millisecond)) {
		t.Fatalf("record = %+v", r)
	}
	facts := h.pipe.recorded()
	if len(facts) != 1 || facts[0].Kind != protocol.KindDiscovery || facts[0].ToolFingerprint != "app:openai_api" ||
		facts[0].Route != protocol.RouteNetFlow || facts[0].DestinationHost != "api.openai.com" ||
		facts[0].Person == nil || facts[0].Person.UserRef != "u_S-1-5-21-1-2-3-1001" {
		t.Fatalf("pipeline facts = %+v", facts)
	}
	c := h.p.Health().Counters
	if c[protocol.CounterEmitted] != 1 || c[protocol.CounterObserved] != 1 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters = %v", c)
	}
}

// A connect over IPv6 is attributed like one over IPv4.
func TestIPv6ConnectIsAttributed(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(77, user("S-1-5-21-7")))
	h.start()

	h.send(dnsEvent(77, testNow, "api.openai.com", "2606:4700:4400::ac40:9bd1;"), connectEvent(77, testNow, "2606:4700:4400::ac40:9bd1"))
	if r := h.waitEmits(1)[0]; r.AppKey != "openai_api" || r.DestinationHost != "api.openai.com" || r.UserRef != "u_S-1-5-21-7" {
		t.Fatalf("record = %+v", r)
	}
}

// A connect to an address another process resolved within ten minutes is still attributed, to the
// connecting process's owner.
func TestAddressResolvedByAnotherProcessIsAttributed(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(10, user("S-1-5-21-10")))
	h.host.set(curl(11, user("S-1-5-21-11")))
	h.start()

	h.send(
		dnsEvent(10, testNow, "api.openai.com", "::ffff:162.159.140.245;"),
		connectEvent(11, testNow.Add(9*time.Minute), "162.159.140.245"),
	)
	r := h.waitEmits(1)[0]
	if r.AppKey != "openai_api" || r.DestinationHost != "api.openai.com" || r.UserRef != "u_S-1-5-21-11" {
		t.Fatalf("record = %+v, want app:openai_api by the connecting process's owner", r)
	}
}

// The connecting process's own answer wins over another process's for the same address.
func TestOwnAnswerWinsOverAnothers(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(20, user("S-1-5-21-20")))
	h.start()

	h.send(
		dnsEvent(20, testNow, "api.anthropic.com", "::ffff:104.18.32.47;"),
		dnsEvent(21, testNow.Add(time.Second), "api.openai.com", "::ffff:104.18.32.47;"),
		connectEvent(20, testNow.Add(2*time.Second), "104.18.32.47"),
	)
	if r := h.waitEmits(1)[0]; r.AppKey != "anthropic_api" || r.DestinationHost != "api.anthropic.com" {
		t.Fatalf("record = %+v, want the connecting process's own answer", r)
	}
}

// A mapping older than ten minutes attributes nothing, from the connecting process's own answer or
// another's.
func TestExpiredMappingIsNotAttributed(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(30, user("S-1-5-21-30")))
	h.host.set(curl(31, user("S-1-5-21-31")))
	h.start()

	h.send(
		dnsEvent(30, testNow, "api.openai.com", "::ffff:162.159.140.245;"),
		connectEvent(30, testNow.Add(mappingTTL), "162.159.140.245"),
		connectEvent(31, testNow.Add(mappingTTL+time.Minute), "162.159.140.245"),
	)
	h.marker(testNow.Add(mappingTTL + time.Minute))
	if r := h.waitEmits(1)[0]; r.AppKey != "anthropic_api" {
		t.Fatalf("an expired mapping was attributed: %+v", r)
	}
}

// A name the catalog does not list as an inference domain gives nothing, and neither does a
// connect to an address no answer named.
func TestNonCatalogNameGivesNothing(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(40, user("S-1-5-21-40")))
	h.start()

	h.send(
		dnsEvent(40, testNow, "www.example.com", "::ffff:93.184.216.34;"),
		connectEvent(40, testNow, "93.184.216.34"),
		connectEvent(40, testNow, "8.8.8.8"),
		// A suffix of a catalog name is not that name.
		dnsEvent(40, testNow, "evil-api.openai.com.example.net", "::ffff:203.0.113.9;"),
		connectEvent(40, testNow, "203.0.113.9"),
	)
	h.marker(testNow)
	if r := h.waitEmits(1)[0]; r.AppKey != "anthropic_api" {
		t.Fatalf("a non-catalog destination was recorded: %+v", r)
	}
	if c := h.p.Health().Counters; c[protocol.CounterObserved] != 1 {
		t.Fatalf("counters = %v, want only the marker observed", c)
	}
}

// Two connects to the same domain by the same user on one day give one record.
func TestTwoConnectsInADayGiveOneRecord(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(50, user("S-1-5-21-50")))
	h.host.set(curl(51, user("S-1-5-21-50")))
	h.start()

	h.send(
		dnsEvent(50, testNow, "api.openai.com", "::ffff:162.159.140.245;"),
		connectEvent(50, testNow, "162.159.140.245"),
		dnsEvent(51, testNow.Add(3*time.Hour), "api.openai.com", "::ffff:172.66.0.243;"),
		connectEvent(51, testNow.Add(3*time.Hour), "172.66.0.243"),
	)
	h.waitEmits(2)
	if n := len(h.pipe.recorded()); n != 1 {
		t.Fatalf("%d envelopes for two connects in a day, want 1", n)
	}
	if c := h.p.Health().Counters; c[protocol.CounterEmitted] != 1 || c[protocol.CounterObserved] != 2 {
		t.Fatalf("counters = %v", c)
	}
}

// A process that is itself a catalog app is recorded with the domain's app, not its own.
func TestCatalogProcessCarriesTheDomainsApp(t *testing.T) {
	h := newHarness(t)
	h.host.set(hostinfo.Process{PID: 60, Image: `C:\Users\ada\AppData\Local\Programs\cursor\Cursor.exe`, Publisher: "Anysphere, Inc.", User: user("S-1-5-21-60")})
	h.start()

	h.send(dnsEvent(60, testNow, "api.anthropic.com", "::ffff:160.79.104.10;"), connectEvent(60, testNow, "160.79.104.10"))
	r := h.waitEmits(1)[0]
	if r.AppKey != "anthropic_api" || r.Publisher != "Anysphere, Inc." || r.DestinationHost != "api.anthropic.com" {
		t.Fatalf("record = %+v", r)
	}
}

// A process gone before it could be opened is still recorded, unattributed and without a signer.
func TestExitedProcessIsRecordedUnattributed(t *testing.T) {
	h := newHarness(t)
	h.start()

	h.send(dnsEvent(70, testNow, "api.openai.com", "::ffff:162.159.140.245;"), connectEvent(70, testNow, "162.159.140.245"))
	r := h.waitEmits(1)[0]
	if r.AppKey != "openai_api" || r.Person != nil || r.UserRef != discovery.UnattributedUserRef || r.Publisher != "" {
		t.Fatalf("record = %+v", r)
	}
}

// A failed query, events from another provider or of another id give nothing; a connect with no
// readable process id or address counts an error.
func TestMalformedEventsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.host.set(curl(80, user("S-1-5-21-80")))
	h.start()

	failed := dnsEvent(80, testNow, "api.openai.com", "::ffff:162.159.140.245;")
	failed.Properties["QueryStatus"] = "9003"
	other := dnsEvent(80, testNow, "api.openai.com", "::ffff:162.159.140.245;")
	other.Provider = "{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}"
	otherID := dnsEvent(80, testNow, "api.openai.com", "::ffff:162.159.140.245;")
	otherID.ID = 3006
	noPID := connectEvent(80, testNow, "162.159.140.245")
	delete(noPID.Properties, "PID")
	badAddr := connectEvent(80, testNow, "162.159.140.245")
	badAddr.Properties["daddr"] = "0xA29F8CF5"
	h.send(failed, other, otherID, connectEvent(80, testNow, "162.159.140.245"), noPID, badAddr)
	h.marker(testNow)
	if r := h.waitEmits(1)[0]; r.AppKey != "anthropic_api" {
		t.Fatalf("record = %+v", r)
	}
	if c := h.p.Health().Counters; c[protocol.CounterErrors] != 2 {
		t.Fatalf("counters = %v, want two errors for the unreadable connects", c)
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

	h.marker(testNow)
	h.waitEmits(1)
}

// Where the platform has no ETW the provider is absent with etw_session_failed and runs nothing.
func TestUnsupportedPlatformIsAbsent(t *testing.T) {
	for name, events := range map[string]func() (Source, error){
		"no source":   nil,
		"unsupported": func() (Source, error) { return nil, etwsession.ErrUnsupported },
	} {
		t.Run(name, func(t *testing.T) {
			p := New(Config{Emitter: &recordingEmitter{}, Events: events})
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

// The monitor follows endpoint.flows.enabled.
func TestEnabledFollowsTheBundle(t *testing.T) {
	p := New(Config{})
	b := testBundle()
	if !p.Enabled(b) {
		t.Fatal("flows.enabled true is not enabled")
	}
	b.Endpoint.Flows.Enabled = false
	if p.Enabled(b) || p.Enabled(nil) {
		t.Fatal("the monitor is enabled with flows.enabled false or no bundle")
	}
	if p.Name() != protocol.CollectorFlowMonitor {
		t.Fatalf("collector = %s", p.Name())
	}
}

func TestQueryAddresses(t *testing.T) {
	got := queryAddresses("type:  5 api.openai.com.cdn.cloudflare.net;::ffff:162.159.140.245;2606:4700:4400::ac40:9bd1;;junk; 172.66.0.243 ;")
	want := "[162.159.140.245 2606:4700:4400::ac40:9bd1 172.66.0.243]"
	if fmt.Sprint(got) != want {
		t.Fatalf("queryAddresses = %v, want %s", got, want)
	}
	if got := queryAddresses(""); len(got) != 0 {
		t.Fatalf("queryAddresses of nothing = %v", got)
	}
}
