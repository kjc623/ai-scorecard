// Package procmon is the process_detector collector. It watches process start and stop in real time,
// matches each process to the app catalog by its executable's name, and reports the apps that run
// to the discovery emitter, which turns the first start per app, user and UTC day into an
// app_running record carrying the image's version and signer.
package procmon

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/etwsession"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// catalogPlatform is the catalog platform whose executables processes are matched against.
	catalogPlatform = "windows"
	sessionName     = "process"

	// The Kernel-Process events the monitor reads: ProcessStart and ProcessStop.
	eventStart uint16 = 1
	eventStop  uint16 = 2

	// reopenInterval is how long after a failed or ended session it is opened again;
	// reconcileInterval how often the running set is checked against the process list, which
	// catches a stop or a start the session lost.
	reopenInterval    = time.Minute
	reconcileInterval = 10 * time.Minute
)

// KernelProcess is the Microsoft-Windows-Kernel-Process provider at the informational level with
// WINEVENT_KEYWORD_PROCESS, limited to ProcessStart and ProcessStop.
var KernelProcess = etwsession.Provider{
	GUID:     "{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}",
	Level:    4,
	Keywords: 0x10,
	EventIDs: []uint16{eventStart, eventStop},
}

// Source delivers process start and stop events; *etwsession.Session is one. Events is closed when
// the source stops delivering.
type Source interface {
	Events() <-chan etwsession.Event
	Close()
}

// KernelEvents opens the real-time session over KernelProcess. It returns etwsession.ErrUnsupported
// where the platform has no ETW.
func KernelEvents() (Source, error) {
	s, err := etwsession.Open(sessionName, []etwsession.Provider{KernelProcess})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Emitter is the part of discovery.Emitter the monitor reports to.
type Emitter interface {
	Emit(ctx context.Context, collector *core.CounterSet, r discovery.Record) error
	Stop(ctx context.Context, collector *core.CounterSet, r discovery.Record)
}

// Config is what the monitor is built with.
type Config struct {
	Emitter Emitter
	// Events opens the source of process events. nil, or a source that returns
	// etwsession.ErrUnsupported, leaves the monitor absent.
	Events func() (Source, error)
	// Bundles returns the bundle in force, whose catalog processes are matched against.
	Bundles func() *policy.Bundle
	// Person names whom an account's processes are attributed to.
	Person func(hostinfo.User) core.Person
	Log    core.Logger
	Clock  func() time.Time
}

// host is what the monitor reads about the machine's processes.
type host struct {
	// snapshot lists the running processes.
	snapshot func() ([]proc, error)
	process  func(pid uint32) (hostinfo.Process, error)
	// version is an image's file version, empty when it has none.
	version func(image string) string
}

var system = host{snapshot: snapshot, process: hostinfo.ProcessInfo, version: fileVersion}

// proc is a process as an event or the process list names it.
type proc struct {
	pid, parent uint32
	// base is the image's file name.
	base string
	at   time.Time
}

// Provider is the process_detector collector.
type Provider struct {
	cfg       Config
	host      host
	counters  *core.CounterSet
	reopen    time.Duration
	reconcile time.Duration

	// life serialises Start and Stop.
	life sync.Mutex
	stop chan struct{}
	done chan struct{}

	mu          sync.Mutex
	running     bool
	unsupported bool
	failed      bool
	startedAt   time.Time
	lastSuccess time.Time
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns the process monitor.
func New(cfg Config) *Provider { return newProvider(cfg, system) }

func newProvider(cfg Config, h host) *Provider {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	if cfg.Bundles == nil {
		cfg.Bundles = func() *policy.Bundle { return nil }
	}
	now := cfg.Clock()
	return &Provider{
		cfg:       cfg,
		host:      h,
		counters:  core.NewCounterSet(now),
		reopen:    reopenInterval,
		reconcile: reconcileInterval,
		startedAt: now,
	}
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return protocol.CollectorProcessDetector }

// Enabled implements core.Toggled: endpoint.processes.enabled.
func (p *Provider) Enabled(b *policy.Bundle) bool { return b != nil && b.Endpoint.Processes.Enabled }

// ApplyPolicy implements core.Provider. The catalog is read from the bundle in force at each
// event, so there is nothing to apply.
func (p *Provider) ApplyPolicy(policy.Bundle) error { return nil }

// Start opens the event source and watches it. A source that cannot be opened leaves the monitor
// running and degraded, and it is opened again later; where the platform has none the monitor is
// absent.
func (p *Provider) Start(context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	if p.stop != nil {
		return nil
	}
	src, err := p.open()
	p.mu.Lock()
	p.running = true
	p.startedAt = p.cfg.Clock()
	p.lastSuccess = time.Time{}
	p.unsupported = errors.Is(err, etwsession.ErrUnsupported)
	p.failed = err != nil
	p.mu.Unlock()
	if p.unsupported {
		return nil
	}
	if err != nil {
		p.cfg.Log.Printf("procmon: the process event session could not be opened: %v", err)
	}
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go p.run(src, p.stop, p.done)
	return nil
}

// Stop closes the event source and forgets the running set.
func (p *Provider) Stop(ctx context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	if p.stop == nil {
		return nil
	}
	stop, done := p.stop, p.done
	p.stop, p.done = nil, nil
	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Health implements core.Provider: healthy while the session delivers events.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	running, unsupported, failed := p.running, p.unsupported, p.failed
	since, last := p.startedAt, p.lastSuccess
	p.mu.Unlock()
	switch {
	case unsupported:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailETWSessionFailed, since, last)
	case !running:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, since, last)
	case failed:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailETWSessionFailed, since, last)
	}
	return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, since, last)
}

func (p *Provider) open() (Source, error) {
	if p.cfg.Events == nil {
		return nil, etwsession.ErrUnsupported
	}
	return p.cfg.Events()
}

// delivering records that the source is open and delivering.
func (p *Provider) delivering() {
	now := p.cfg.Clock()
	p.mu.Lock()
	p.failed = false
	p.lastSuccess = now
	p.mu.Unlock()
}

func (p *Provider) fail() {
	p.mu.Lock()
	p.failed = true
	p.mu.Unlock()
}

// run handles src's events until stop closes, opening the source again whenever it fails.
func (p *Provider) run(src Source, stop, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &monitor{p: p, ctx: ctx, running: map[uint32]*instance{}}
	defer func() {
		if src != nil {
			src.Close()
		}
	}()

	var events <-chan etwsession.Event
	var retry <-chan time.Time
	opened := func() {
		events = src.Events()
		p.delivering()
		m.reconcileWith()
	}
	if src != nil {
		opened()
	} else {
		retry = time.After(p.reopen)
	}
	tick := time.NewTicker(p.reconcile)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-events:
			if !ok {
				src.Close()
				src, events = nil, nil
				p.fail()
				p.cfg.Log.Printf("procmon: the process event session stopped delivering events")
				retry = time.After(p.reopen)
				continue
			}
			p.delivering()
			m.handle(ev)
		case <-retry:
			retry = nil
			s, err := p.open()
			if err != nil {
				p.fail()
				retry = time.After(p.reopen)
				continue
			}
			src = s
			opened()
			p.cfg.Log.Printf("procmon: the process event session is open again")
		case <-tick.C:
			if src != nil {
				m.reconcileWith()
			}
		}
	}
}

// instance is one running app: the process the monitor saw start and the processes of the same
// app it started, such as an Electron app's renderers. It stops when the last of them exits.
type instance struct {
	record  discovery.Record
	root    uint32
	members int
}

// monitor is the running set, owned by run's goroutine.
type monitor struct {
	p       *Provider
	ctx     context.Context
	running map[uint32]*instance
}

func (m *monitor) handle(ev etwsession.Event) {
	if !strings.EqualFold(ev.Provider, KernelProcess.GUID) {
		return
	}
	pid, ok := uintProperty(ev, "ProcessID")
	if !ok {
		m.p.counters.Add(protocol.CounterErrors)
		return
	}
	switch ev.ID {
	case eventStart:
		parent, _ := uintProperty(ev, "ParentProcessID")
		at := ev.Time
		if at.IsZero() {
			at = m.p.cfg.Clock()
		}
		m.started(proc{pid: pid, parent: parent, base: baseName(ev.Properties["ImageName"]), at: at})
	case eventStop:
		m.stopped(pid)
	}
}

// started handles a process that started. A catalog app's process whose parent is a running
// process of the same app joins that instance; any other becomes a new instance, reported to the
// emitter.
func (m *monitor) started(pr proc) {
	if _, ok := m.running[pr.pid]; ok {
		return
	}
	keys := m.p.cfg.Bundles().AppByExe(catalogPlatform, pr.base)
	if len(keys) == 0 {
		return
	}
	if inst, ok := m.running[pr.parent]; ok && pr.parent != pr.pid && slices.Contains(keys, inst.record.AppKey) {
		inst.members++
		m.running[pr.pid] = inst
		return
	}
	m.p.counters.Add(protocol.CounterObserved)
	r := m.describe(pr, keys)
	m.running[pr.pid] = &instance{record: r, root: pr.pid, members: 1}
	m.p.cfg.Log.Printf("procmon: app:%s started (pid %d)", r.AppKey, pr.pid)
	if err := m.p.cfg.Emitter.Emit(m.ctx, m.p.counters, r); err != nil {
		m.p.cfg.Log.Printf("procmon: app:%s (pid %d) was not recorded: %v", r.AppKey, pr.pid, err)
	}
}

// stopped handles a process that exited. The instance stops with its last process.
func (m *monitor) stopped(pid uint32) {
	inst, ok := m.running[pid]
	if !ok {
		return
	}
	delete(m.running, pid)
	inst.members--
	if inst.members > 0 {
		return
	}
	m.p.cfg.Log.Printf("procmon: app:%s stopped (pid %d)", inst.record.AppKey, inst.root)
	m.p.cfg.Emitter.Stop(m.ctx, m.p.counters, inst.record)
}

// describe is the app_running record of a started process: its app, image version, signer and
// owner. A process that has already exited, or cannot be opened, is recorded with what the event
// named and no person.
func (m *monitor) describe(pr proc, keys []string) discovery.Record {
	r := discovery.Record{
		Type:       protocol.DiscoveryTypeAppRunning,
		Basis:      protocol.DetectionBasisProcessEvent,
		Route:      protocol.RouteProcDetect,
		UserRef:    discovery.UnattributedUserRef,
		OccurredAt: pr.at,
	}
	if info, err := m.p.host.process(pr.pid); err == nil {
		r.Publisher = info.Publisher
		if info.Image != "" {
			r.Version = m.p.host.version(info.Image)
		}
		if info.User != nil && m.p.cfg.Person != nil {
			person := m.p.cfg.Person(*info.User)
			r.Person, r.UserRef = &person, person.UserRef
		}
	}
	r.AppKey = m.appOf(keys, r.Publisher)
	return r
}

// appOf picks the app among those sharing the executable's name: the first whose catalog
// publisher is the observed one, else the first in catalog order. A publisher the catalog does not
// list for the app is still reported as observed, so an impostor binary is visible.
func (m *monitor) appOf(keys []string, publisher string) string {
	if len(keys) > 1 && publisher != "" {
		for _, k := range m.p.cfg.Bundles().AppByPublisher(catalogPlatform, publisher) {
			if slices.Contains(keys, k) {
				return k
			}
		}
	}
	return keys[0]
}

// reconcileWith checks the running set against the process list: a process gone from it stops,
// and a catalog app's process not yet known starts, parents before their children.
func (m *monitor) reconcileWith() {
	procs, err := m.p.host.snapshot()
	if err != nil {
		m.p.counters.Add(protocol.CounterErrors)
		m.p.cfg.Log.Printf("procmon: listing the running processes: %v", err)
		return
	}
	now := m.p.cfg.Clock()
	alive := make(map[uint32]struct{}, len(procs))
	pending := map[uint32]proc{}
	b := m.p.cfg.Bundles()
	for _, pr := range procs {
		alive[pr.pid] = struct{}{}
		if _, known := m.running[pr.pid]; !known && len(b.AppByExe(catalogPlatform, pr.base)) > 0 {
			pr.at = now
			pending[pr.pid] = pr
		}
	}
	for pid := range m.running {
		if _, ok := alive[pid]; !ok {
			m.stopped(pid)
		}
	}
	for len(pending) > 0 {
		ready := make([]proc, 0, len(pending))
		for pid, pr := range pending {
			if _, parentPending := pending[pr.parent]; !parentPending || pr.parent == pid {
				ready = append(ready, pr)
			}
		}
		if len(ready) == 0 {
			// The parent links form a cycle (PIDs reused): take them in any order.
			for _, pr := range pending {
				ready = append(ready, pr)
			}
		}
		slices.SortFunc(ready, func(a, b proc) int { return cmp.Compare(a.pid, b.pid) })
		for _, pr := range ready {
			delete(pending, pr.pid)
			m.started(pr)
		}
	}
}

// uintProperty reads an integer payload field, which the event decoder formats in decimal or, for
// some fields, as 0x-prefixed hex.
func uintProperty(ev etwsession.Event, name string) (uint32, bool) {
	s, ok := ev.Properties[name]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// baseName is an image path's file name. The event names the image by its kernel path
// (\Device\HarddiskVolume3\...\app.exe); the process list by its file name alone.
func baseName(image string) string {
	return image[strings.LastIndexAny(image, `\/`)+1:]
}
