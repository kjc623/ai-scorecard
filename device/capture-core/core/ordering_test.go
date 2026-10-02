package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// orderLog records the literal sequence of side effects. Every assertion below compares full
// slices, never sets: the ordering *is* the contract, so "these all happened" is not the
// property being tested.
type orderLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *orderLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

func assertSequence(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, " > ") != strings.Join(want, " > ") {
		t.Fatalf("%s\ngot:  %v\nwant: %v", what, got, want)
	}
}

type recordingProvider struct {
	route    protocol.Route
	log      *orderLog
	startErr error
	addr     string
	released bool
	mu       sync.Mutex
	started  bool
}

func (p *recordingProvider) Name() protocol.Route { return p.route }

func (p *recordingProvider) Start(context.Context) error {
	p.log.add("provider.Start:" + string(p.route))
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	return p.startErr
}

func (p *recordingProvider) Stop(context.Context) error {
	p.log.add("provider.Stop:" + string(p.route))
	return nil
}

func (p *recordingProvider) Health() Health {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return Health{State: protocol.StateAbsent, Counters: map[protocol.Counter]uint64{}}
	}
	return Health{State: protocol.StateHealthy, Counters: map[protocol.Counter]uint64{}}
}

func (p *recordingProvider) ApplyPolicy(policy.Bundle) error { return nil }

// ListenAddr is what the supervisor uses to point the system proxy at the proxy. A provider
// that cannot say where it listens is never pointed at.
func (p *recordingProvider) ListenAddr() string { return p.addr }

// Release is `proxy.loopback`'s §3.5 step 2: the port goes first, before anything else.
func (p *recordingProvider) Release(context.Context) error {
	p.log.add("provider.Release:" + string(p.route))
	p.mu.Lock()
	p.released = true
	p.mu.Unlock()
	return nil
}

type recordingPolicy struct {
	log *orderLog
	err error
}

func (p *recordingPolicy) Load(context.Context) error {
	p.log.add("policy.Load")
	return p.err
}

type recordingSpool struct {
	log     *orderLog
	openErr error
	open    bool
	drained bool
	depth   int
	dropped int
}

func (s *recordingSpool) Open(context.Context) error {
	s.log.add("spool.Open")
	if s.openErr != nil {
		return s.openErr
	}
	s.open = true
	return nil
}

func (s *recordingSpool) Stats() protocol.SpoolStats {
	return protocol.SpoolStats{Depth: s.depth, DroppedTotal: uint64(s.dropped)}
}

func (s *recordingSpool) Drain(context.Context, time.Time) (DrainResult, error) {
	s.log.add("spool.Drain")
	s.drained = true
	return DrainResult{Delivered: 3}, nil
}

func (s *recordingSpool) Close() error { return nil }

type recordingClassifierHost struct {
	log      *orderLog
	startErr error
}

func (c *recordingClassifierHost) Start(context.Context) error {
	c.log.add("classifierhost.Start")
	return c.startErr
}

func (c *recordingClassifierHost) Stop(context.Context) error {
	c.log.add("classifierhost.Stop")
	return nil
}

type recordingSystemProxy struct {
	log     *orderLog
	addr    string
	eff     bool
	mu      sync.Mutex
	pointed bool
}

func (s *recordingSystemProxy) PointAt(_ context.Context, addr string) error {
	s.log.add("systemproxy.PointAt:" + addr)
	s.mu.Lock()
	s.pointed = true
	s.addr = addr
	s.mu.Unlock()
	return nil
}

func (s *recordingSystemProxy) Restore(context.Context) error {
	s.log.add("systemproxy.Restore")
	s.mu.Lock()
	s.pointed = false
	s.mu.Unlock()
	return nil
}

func (s *recordingSystemProxy) Effective(context.Context) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr, s.eff && s.pointed
}

type recordingTrustRoot struct {
	log *orderLog
}

func (t *recordingTrustRoot) Remove(context.Context) error {
	t.log.add("trustroot.Remove")
	return nil
}

type orderingFixture struct {
	log      *orderLog
	sup      *Supervisor
	reg      *Registry
	spool    *recordingSpool
	proxy    *recordingSystemProxy
	tls      *recordingProvider
	loopback *recordingProvider
	detect   *recordingProvider
	shim     *recordingProvider
	host     *recordingClassifierHost
	trust    *recordingTrustRoot
}

func newOrderingFixture(t *testing.T, tlsStartErr error) *orderingFixture {
	t.Helper()
	log := &orderLog{}
	reg := NewRegistry(testClock, nil)
	detect := &recordingProvider{route: protocol.RouteProcDetect, log: log}
	shim := &recordingProvider{route: protocol.RouteCLIShim, log: log}
	tlsP := &recordingProvider{route: protocol.RouteProxyTLS, log: log, startErr: tlsStartErr, addr: "127.0.0.1:53535"}
	loop := &recordingProvider{route: protocol.RouteProxyLoopback, log: log}
	for _, p := range []Provider{detect, shim, tlsP, loop} {
		if err := reg.Add(p); err != nil {
			t.Fatalf("Add(%s): %v", p.Name(), err)
		}
	}
	sup, err := NewSupervisor(reg, nil, testClock)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	spool := &recordingSpool{log: log}
	proxy := &recordingSystemProxy{log: log, eff: true}
	host := &recordingClassifierHost{log: log}
	trust := &recordingTrustRoot{log: log}
	sup.Policy = &recordingPolicy{log: log}
	sup.Spool = spool
	sup.SystemProxy = proxy
	sup.ClassifierHost = host
	sup.TrustRoot = trust
	sup.Loopback = loop
	sup.RemoveTrustRoot = true
	return &orderingFixture{
		log: log, sup: sup, reg: reg, spool: spool, proxy: proxy, tls: tlsP,
		loopback: loop, detect: detect, shim: shim, host: host, trust: trust,
	}
}

// TestOrdering_3_5_StartupSequenceIsLiteral asserts §3.5's startup column as a literal sequence.
// The rules it encodes, row by row:
//
//	rows 1-2: bundle verified, then the spool opens — before any provider starts
//	row 3:    proc.detect starts first
//	row 4:    cli.shim
//	row 5:    classifier-host (idle)
//	row 6:    proxy.tls, and the system proxy is pointed at it only once it is listening
//	row 7:    proxy.loopback LAST, after its upstream preflight
func TestOrdering_3_5_StartupSequenceIsLiteral(t *testing.T) {
	f := newOrderingFixture(t, nil)
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	// The side effects, in the literal order §3.5's startup column requires. The supervisor's own
	// step records are asserted against StartupOrder() in
	// TestOrdering_RecordedStepsAreOrderedAndComplete, so every row of the column is checked.
	want := []string{
		"policy.Load",                         // 1 load bundle, verify signature
		"spool.Open",                          // 2 open and unlock the spool, before any provider
		"provider.Start:proc.detect",          // 3 proc.detect first
		"provider.Start:cli.shim",             // 4 cli.shim
		"classifierhost.Start",                // 5 classifier-host, idle
		"provider.Start:proxy.tls",            // 6 proxy.tls ...
		"systemproxy.PointAt:127.0.0.1:53535", // ... and the system proxy only once it listens
		"provider.Start:proxy.loopback",       // 7 the broker binds LAST
	}
	assertSequence(t, "§3.5 startup order", f.log.all(), want)
	if len(f.sup.Order()) != len(StartupOrder()) {
		t.Fatalf("supervisor recorded %d steps, declared order has %d", len(f.sup.Order()), len(StartupOrder()))
	}
}

// TestOrdering_3_5_ShutdownSequenceIsLiteral asserts §3.5's shutdown column, including the one
// interpretation it forces: "provider.Stop all; proxy stops enforcing first" (row 1) and
// "RELEASE THE LOOPBACK PORT before anything else" (row 2) cannot both be literally first, so
// the loopback release goes first of all — E14 is the path whose failure breaks the user — and
// the proxy stops enforcing immediately after. proc.detect, which starts first, stops last.
func TestOrdering_3_5_ShutdownSequenceIsLiteral(t *testing.T) {
	f := newOrderingFixture(t, nil)
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	f.log.mu.Lock()
	f.log.steps = nil
	f.log.mu.Unlock()

	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{
		"provider.Release:proxy.loopback", // 2 release the loopback port before anything else
		"provider.Stop:proxy.tls",         // 1 stop enforcing first
		"provider.Stop:cli.shim",          // 1 remaining providers, proc.detect (started first) last
		"provider.Stop:proc.detect",
		"spool.Drain",         // 3 bounded drain
		"systemproxy.Restore", // 4 restore the system proxy to its pre-install value
		"trustroot.Remove",    // 5 remove the trusted root (kill switch or uninstall)
		"classifierhost.Stop", // 6 stop classifier-host, then exit
	}
	assertSequence(t, "§3.5 shutdown order", f.log.all(), want)

	// The declared orders are data, so a reordering of the code is caught by the two assertions
	// above and a reordering of the contract is a visible edit here.
	if strings.Join(StartupOrder(), ",") == strings.Join(ShutdownOrder(), ",") {
		t.Fatal("startup and shutdown orders are identical, which cannot be right")
	}
	if StartupOrder()[len(StartupOrder())-1] != StepStartProxyLoopback {
		t.Fatal("§3.5: the broker must bind last on startup")
	}
	if ShutdownOrder()[0] != StepReleaseLoopback {
		t.Fatal("§3.5: the loopback port must be released first on shutdown")
	}
}

// §3.5 row 6: "The system proxy points at the proxy only once it is listening". A proxy that
// failed to start must not be pointed at, or every request on the machine becomes a connection
// failure.
func TestOrdering_3_5_SystemProxyIsNotPointedAtAFailedProxy(t *testing.T) {
	f := newOrderingFixture(t, errors.New("bind failed"))
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	for _, step := range f.log.all() {
		if strings.HasPrefix(step, "systemproxy.PointAt") {
			t.Fatalf("§3.5 row 6: the system proxy was pointed at a proxy that is not listening: %v", f.log.all())
		}
	}
	if f.proxy.pointed {
		t.Fatal("system proxy was pointed at a failed proxy")
	}
}

// §3.5 row 2: "The spool opens before any provider starts — a provider with nowhere to write
// must either drop silently (forbidden, C22) or block (forbidden, brief §6)". So a failed spool
// open stops startup rather than starting providers that cannot record anything.
func TestOrdering_3_5_SpoolOpenFailureStopsStartup(t *testing.T) {
	f := newOrderingFixture(t, nil)
	f.spool.openErr = errors.New("spool key unreadable")
	err := f.sup.Startup(context.Background())
	if err == nil {
		t.Fatal("§3.5 row 2: startup continued with no spool")
	}
	for _, step := range f.log.all() {
		if strings.HasPrefix(step, "provider.Start:") {
			t.Fatalf("§3.5 row 2: a provider started before the spool was available: %v", f.log.all())
		}
	}
}

// §3.5 crash policy: a crash is a restart with backoff, and a crash loop stops the loop rather
// than retrying into a state where the user's network is intermittently broken.
func TestCrashLoopGuard_3_5_StopsTheLoopAndReportsTheCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	g := NewCrashLoopGuard(3, time.Minute, clock)
	for i := 0; i < 3; i++ {
		if !g.ShouldRestart("panic") {
			t.Fatalf("restart %d refused before the threshold", i+1)
		}
		now = now.Add(time.Second)
	}
	if g.ShouldRestart("panic") {
		t.Fatal("§3.5: the crash loop was not stopped at the threshold")
	}
	if !g.Stopped() {
		t.Fatal("guard did not record the loop as stopped")
	}
	if g.LoopCount() != 4 {
		t.Fatalf("loop count = %d, want 4 (the restarts plus the one that ended the loop)", g.LoopCount())
	}
	if g.ShouldRestart("panic") {
		t.Fatal("a stopped guard restarted again; recovery must require an operator-visible event")
	}
	if g.Backoff(time.Second) > 32*time.Second {
		t.Fatalf("backoff = %v, want a bounded value", g.Backoff(time.Second))
	}
	// Crashes outside the window do not count toward the loop.
	now = now.Add(2 * time.Hour)
	g2 := NewCrashLoopGuard(2, time.Minute, clock)
	if !g2.ShouldRestart("panic") {
		t.Fatal("first crash refused")
	}
	now = now.Add(2 * time.Minute)
	if !g2.ShouldRestart("panic") {
		t.Fatal("a crash outside the window counted toward the loop")
	}
}

// The supervisor's recorded order must be re-readable: a caller (or a test) can assert what
// happened, which is what makes the ordering auditable rather than documentary.
func TestOrdering_RecordedStepsAreOrderedAndComplete(t *testing.T) {
	f := newOrderingFixture(t, nil)
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	startup := f.sup.Order()
	if len(startup) != len(StartupOrder()) {
		t.Fatalf("recorded %v, want one entry per declared startup step %v", startup, StartupOrder())
	}
	want := strings.Join(StartupOrder(), ",")
	if strings.Join(startup, ",") != want {
		t.Fatalf("recorded supervisor steps = %v, want %v", startup, StartupOrder())
	}
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	shutdown := f.sup.Order()
	// Order() returns everything performed in this supervisor's lifetime, startup then shutdown.
	if len(shutdown) != len(StartupOrder())+len(ShutdownOrder()) {
		t.Fatalf("recorded %d steps, want %d", len(shutdown), len(StartupOrder())+len(ShutdownOrder()))
	}
	tail := shutdown[len(StartupOrder()):]
	if strings.Join(tail, ",") != strings.Join(ShutdownOrder(), ",") {
		t.Fatalf("recorded shutdown steps = %v, want %v", tail, ShutdownOrder())
	}
}

var _ = fmt.Sprintf // keep fmt for failure messages added later
