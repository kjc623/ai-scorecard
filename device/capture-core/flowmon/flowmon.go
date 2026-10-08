// Package flowmon is the flow_monitor collector. It reads the operating system's DNS answers and TCP
// connect events in real time, never a payload, and when a process connects to an address it
// resolved from an inference domain in the app catalog it reports an inference_connection to the
// discovery emitter: that app, the host name, and the process's signer and owner. Connections to
// any other destination are not recorded.
package flowmon

import (
	"context"
	"errors"
	"net/netip"
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
	sessionName = "flow"

	// The DNS-Client event the monitor reads: a query completed, with its answers.
	eventQueryCompleted uint16 = 3008
	// The Kernel-Network events: a TCP connection attempted, over IPv4 and over IPv6.
	eventConnectIPv4 uint16 = 12
	eventConnectIPv6 uint16 = 28

	// mappingTTL is how long an answer attributes the connections to its addresses.
	mappingTTL = 10 * time.Minute
	// reopenInterval is how long after a failed or ended session it is opened again.
	reopenInterval = time.Minute
)

// DNSClient is the Microsoft-Windows-DNS-Client provider, limited to the query-completed event,
// which the client writes in the context of the process that asked.
var DNSClient = etwsession.Provider{
	GUID:     "{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}",
	Level:    4,
	EventIDs: []uint16{eventQueryCompleted},
}

// KernelNetwork is the Microsoft-Windows-Kernel-Network provider with
// KERNEL_NETWORK_KEYWORD_IPV4 and KERNEL_NETWORK_KEYWORD_IPV6, limited to the TCP connect events.
var KernelNetwork = etwsession.Provider{
	GUID:     "{7DD42A49-5329-4832-8DFD-43D979153A88}",
	Level:    4,
	Keywords: 0x10 | 0x20,
	EventIDs: []uint16{eventConnectIPv4, eventConnectIPv6},
}

// Source delivers DNS and connect events; *etwsession.Session is one. Events is closed when the
// source stops delivering.
type Source interface {
	Events() <-chan etwsession.Event
	Close()
}

// NetworkEvents opens the real-time session over DNSClient and KernelNetwork. It returns
// etwsession.ErrUnsupported where the platform has no ETW.
func NetworkEvents() (Source, error) {
	s, err := etwsession.Open(sessionName, []etwsession.Provider{DNSClient, KernelNetwork})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Emitter is the part of discovery.Emitter the monitor reports to.
type Emitter interface {
	Emit(ctx context.Context, collector *core.CounterSet, r discovery.Record) error
}

// Config is what the monitor is built with.
type Config struct {
	Emitter Emitter
	// Events opens the source of DNS and connect events. nil, or a source that returns
	// etwsession.ErrUnsupported, leaves the monitor absent.
	Events func() (Source, error)
	// Bundles returns the bundle in force, whose catalog inference domains the DNS answers are
	// matched against.
	Bundles func() *policy.Bundle
	// Person names whom an account's processes are attributed to.
	Person func(hostinfo.User) core.Person
	Log    core.Logger
	Clock  func() time.Time
}

// Provider is the flow_monitor collector.
type Provider struct {
	cfg      Config
	process  func(pid uint32) (hostinfo.Process, error)
	counters *core.CounterSet
	reopen   time.Duration

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

// New returns the flow monitor.
func New(cfg Config) *Provider { return newProvider(cfg, hostinfo.ProcessInfo) }

func newProvider(cfg Config, process func(uint32) (hostinfo.Process, error)) *Provider {
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
		process:   process,
		counters:  core.NewCounterSet(now),
		reopen:    reopenInterval,
		startedAt: now,
	}
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return protocol.CollectorFlowMonitor }

// Enabled implements core.Toggled: endpoint.flows.enabled.
func (p *Provider) Enabled(b *policy.Bundle) bool { return b != nil && b.Endpoint.Flows.Enabled }

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
		p.cfg.Log.Printf("flowmon: the network event session could not be opened: %v", err)
	}
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go p.run(src, p.stop, p.done)
	return nil
}

// Stop closes the event source and forgets the DNS answers.
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
	m := &monitor{p: p, ctx: ctx, byPID: map[uint32]map[netip.Addr]answer{}, byAddr: map[netip.Addr]answer{}}
	defer func() {
		if src != nil {
			src.Close()
		}
	}()

	var events <-chan etwsession.Event
	var retry <-chan time.Time
	if src != nil {
		events = src.Events()
		p.delivering()
	} else {
		retry = time.After(p.reopen)
	}
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-events:
			if !ok {
				src.Close()
				src, events = nil, nil
				p.fail()
				p.cfg.Log.Printf("flowmon: the network event session stopped delivering events")
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
			src, events = s, s.Events()
			p.delivering()
			p.cfg.Log.Printf("flowmon: the network event session is open again")
		}
	}
}

// answer is a catalog inference domain a DNS query resolved to an address, and when.
type answer struct {
	host string
	at   time.Time
}

// monitor holds the DNS answers, owned by run's goroutine. Only names the catalog lists as
// inference domains are kept.
type monitor struct {
	p   *Provider
	ctx context.Context
	// byPID is each process's answers by address; byAddr the latest answer for an address from
	// any process.
	byPID  map[uint32]map[netip.Addr]answer
	byAddr map[netip.Addr]answer
	// pruned is when expired answers were last removed.
	pruned time.Time
}

func (m *monitor) handle(ev etwsession.Event) {
	at := ev.Time
	if at.IsZero() {
		at = m.p.cfg.Clock()
	}
	switch {
	case strings.EqualFold(ev.Provider, DNSClient.GUID) && ev.ID == eventQueryCompleted:
		m.resolved(ev, at)
	case strings.EqualFold(ev.Provider, KernelNetwork.GUID) && (ev.ID == eventConnectIPv4 || ev.ID == eventConnectIPv6):
		m.connected(ev, at)
	}
}

// resolved keeps the addresses a completed query for a catalog inference domain answered, under
// the process that asked.
func (m *monitor) resolved(ev etwsession.Event, at time.Time) {
	m.prune(at)
	if status := strings.TrimSpace(ev.Properties["QueryStatus"]); status != "" && status != "0" {
		return
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(ev.Properties["QueryName"]), "."))
	if host == "" || len(m.p.cfg.Bundles().AppByDomain(host)) == 0 {
		return
	}
	addrs := queryAddresses(ev.Properties["QueryResults"])
	if len(addrs) == 0 {
		return
	}
	own := m.byPID[ev.PID]
	if own == nil {
		own = map[netip.Addr]answer{}
		m.byPID[ev.PID] = own
	}
	a := answer{host: host, at: at}
	for _, addr := range addrs {
		own[addr] = a
		m.byAddr[addr] = a
	}
}

// connected attributes a TCP connect to the catalog domain its destination was resolved from: by
// the connecting process if it resolved it, else by any process, within mappingTTL.
func (m *monitor) connected(ev etwsession.Event, at time.Time) {
	pid, ok := uintProperty(ev, "PID")
	daddr, err := netip.ParseAddr(strings.TrimSpace(ev.Properties["daddr"]))
	if !ok || err != nil {
		m.p.counters.Add(protocol.CounterErrors)
		return
	}
	dest := daddr.WithZone("").Unmap()
	a, ok := m.lookup(pid, dest, at)
	if !ok {
		return
	}
	keys := m.p.cfg.Bundles().AppByDomain(a.host)
	if len(keys) == 0 {
		return
	}
	m.p.counters.Add(protocol.CounterObserved)
	r := discovery.Record{
		Type:            protocol.DiscoveryTypeInferenceConnection,
		Basis:           protocol.DetectionBasisFlowMetadata,
		Route:           protocol.RouteNetFlow,
		AppKey:          keys[0],
		DestinationHost: a.host,
		UserRef:         discovery.UnattributedUserRef,
		OccurredAt:      at,
	}
	if info, err := m.p.process(pid); err == nil {
		r.Publisher = info.Publisher
		if info.User != nil && m.p.cfg.Person != nil {
			person := m.p.cfg.Person(*info.User)
			r.Person, r.UserRef = &person, person.UserRef
		}
	}
	if err := m.p.cfg.Emitter.Emit(m.ctx, m.p.counters, r); err != nil {
		m.p.cfg.Log.Printf("flowmon: a connection to app:%s (pid %d) was not recorded: %v", r.AppKey, pid, err)
	}
}

func (m *monitor) lookup(pid uint32, addr netip.Addr, at time.Time) (answer, bool) {
	fresh := func(a answer) bool { return at.Sub(a.at) < mappingTTL }
	if a, ok := m.byPID[pid][addr]; ok && fresh(a) {
		return a, true
	}
	if a, ok := m.byAddr[addr]; ok && fresh(a) {
		return a, true
	}
	return answer{}, false
}

// prune removes expired answers, at most once a minute.
func (m *monitor) prune(now time.Time) {
	if now.Sub(m.pruned) < time.Minute {
		return
	}
	m.pruned = now
	for pid, own := range m.byPID {
		for addr, a := range own {
			if now.Sub(a.at) >= mappingTTL {
				delete(own, addr)
			}
		}
		if len(own) == 0 {
			delete(m.byPID, pid)
		}
	}
	for addr, a := range m.byAddr {
		if now.Sub(a.at) >= mappingTTL {
			delete(m.byAddr, addr)
		}
	}
}

// queryAddresses reads the addresses in a QueryResults field: answers separated by semicolons, an
// address record as its address (IPv4 as an IPv4-mapped IPv6 address) and any other record as
// "type: <n> <data>", which is skipped.
func queryAddresses(results string) []netip.Addr {
	var out []netip.Addr
	for _, s := range strings.Split(results, ";") {
		addr, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		out = append(out, addr.WithZone("").Unmap())
	}
	return out
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
