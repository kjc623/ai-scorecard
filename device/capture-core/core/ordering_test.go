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

// orderLog records the literal sequence of side effects. Every assertion compares full slices,
// never sets: the ordering is the property under test.
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

func (l *orderLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = nil
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
	// panicOnRelease simulates a provider whose release path has a defect: the supervisor must
	// contain it rather than let it abort the shutdown sequence.
	panicOnRelease bool
	mu             sync.Mutex
	started        bool
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

func (p *recordingProvider) Release(context.Context) error {
	if p.panicOnRelease {
		panic("provider release defect (simulated)")
	}
	p.log.add("provider.Release:" + string(p.route))
	return nil
}

type recordingPolicy struct{ log *orderLog }

func (p *recordingPolicy) Load(context.Context) error {
	p.log.add("policy.Load")
	return nil
}

type recordingSpool struct {
	log     *orderLog
	openErr error
	drained bool
}

func (s *recordingSpool) Open(context.Context) error {
	s.log.add("spool.Open")
	return s.openErr
}

func (s *recordingSpool) Stats() protocol.SpoolStats { return protocol.SpoolStats{} }

func (s *recordingSpool) Drain(context.Context, time.Time) (DrainResult, error) {
	s.log.add("spool.Drain")
	s.drained = true
	return DrainResult{Delivered: 3}, nil
}

func (s *recordingSpool) Close() error { return nil }

type recordingIdentity struct{ log *orderLog }

func (r *recordingIdentity) Resolve(context.Context) error {
	r.log.add("identity.Resolve")
	return nil
}

type recordingClassifierHost struct{ log *orderLog }

func (c *recordingClassifierHost) Start(context.Context) error {
	c.log.add("classifierhost.Start")
	return nil
}

func (c *recordingClassifierHost) Stop(context.Context) error {
	c.log.add("classifierhost.Stop")
	return nil
}

type recordingTrustRoot struct{ log *orderLog }

func (t *recordingTrustRoot) Remove(context.Context) error {
	t.log.add("trustroot.Remove")
	return nil
}

type orderingFixture struct {
	log      *orderLog
	sup      *Supervisor
	spool    *recordingSpool
	loopback *recordingProvider
}

func newOrderingFixture(t *testing.T) *orderingFixture {
	t.Helper()
	log := &orderLog{}
	reg := NewRegistry(testClock, nil)
	loop := &recordingProvider{route: protocol.RouteProxyLoopback, log: log}
	for _, p := range []Provider{
		&recordingProvider{route: protocol.RouteCLIShim, log: log},
		&recordingProvider{route: protocol.RouteProxyTLS, log: log},
		loop,
	} {
		if err := reg.Add(p); err != nil {
			t.Fatalf("Add(%s): %v", p.Name(), err)
		}
	}
	sup, err := NewSupervisor(reg, nil, testClock)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	spool := &recordingSpool{log: log}
	sup.Policy = &recordingPolicy{log: log}
	sup.Spool = spool
	sup.Identity = &recordingIdentity{log: log}
	sup.ClassifierHost = &recordingClassifierHost{log: log}
	sup.TrustRoot = &recordingTrustRoot{log: log}
	sup.Loopback = loop
	sup.RemoveTrustRoot = true
	return &orderingFixture{log: log, sup: sup, spool: spool, loopback: loop}
}

// The startup side effects happen in the declared order: the bundle is verified and the spool
// opens before any provider starts, the identity is resolved before anything can mint, and the
// loopback broker binds last.
func TestStartupSequenceIsLiteral(t *testing.T) {
	f := newOrderingFixture(t)
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	assertSequence(t, "startup order", f.log.all(), []string{
		"policy.Load",
		"spool.Open",
		"identity.Resolve",
		"provider.Start:cli.shim",
		"classifierhost.Start",
		"provider.Start:proxy.tls",
		"provider.Start:proxy.loopback",
	})
	assertSequence(t, "recorded startup steps", f.sup.Order(), StartupOrder())
}

// The loopback port is released before anything else, the proxy stops enforcing next, and the
// classifier host stops last.
func TestShutdownSequenceIsLiteral(t *testing.T) {
	f := newOrderingFixture(t)
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	f.log.reset()
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	assertSequence(t, "shutdown order", f.log.all(), []string{
		"provider.Release:proxy.loopback",
		"provider.Stop:proxy.tls",
		"provider.Stop:cli.shim",
		"spool.Drain",
		"trustroot.Remove",
		"classifierhost.Stop",
	})
	assertSequence(t, "recorded shutdown steps", f.sup.Order()[len(StartupOrder()):], ShutdownOrder())
}

// A failed spool open stops startup rather than starting providers that cannot record anything.
func TestSpoolOpenFailureStopsStartup(t *testing.T) {
	f := newOrderingFixture(t)
	f.spool.openErr = errors.New("spool key unreadable")
	if err := f.sup.Startup(context.Background()); err == nil {
		t.Fatal("startup continued with no spool")
	}
	for _, step := range f.log.all() {
		if strings.HasPrefix(step, "provider.Start:") {
			t.Fatalf("a provider started before the spool was available: %v", f.log.all())
		}
	}
}

// A typed-nil provider is not nil: a composition that leaves a nil pointer in the Loopback field
// must not turn shutdown into a panic before the remaining steps run.
func TestShutdownWithATypedNilLoopbackCompletes(t *testing.T) {
	f := newOrderingFixture(t)
	var nilBroker *typedNilBroker
	f.sup.Loopback = nilBroker
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with a typed-nil loopback: %v", err)
	}
	steps := f.sup.Order()
	if steps[len(steps)-1] != StepStopClassifierHost {
		t.Fatalf("shutdown ended at %q, want %q", steps[len(steps)-1], StepStopClassifierHost)
	}
}

// A provider that panics while being released does not abort the shutdown sequence.
func TestShutdownContainsAPanickingProvider(t *testing.T) {
	f := newOrderingFixture(t)
	f.loopback.panicOnRelease = true
	if err := f.sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if err := f.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with a panicking provider: %v", err)
	}
	steps := f.sup.Order()
	if len(steps) == 0 || steps[len(steps)-1] != StepStopClassifierHost {
		t.Fatalf("shutdown stopped early: %v", steps)
	}
	if !f.spool.drained {
		t.Fatal("the spool was not drained because a provider panicked")
	}
}

// typedNilBroker is a nil pointer whose type implements Releaser, so assigning it to an interface
// produces the typed-nil case.
type typedNilBroker struct{}

func (*typedNilBroker) Name() protocol.Route            { return protocol.RouteProxyLoopback }
func (*typedNilBroker) Start(context.Context) error     { return nil }
func (*typedNilBroker) Stop(context.Context) error      { return nil }
func (*typedNilBroker) Health() Health                  { return Health{} }
func (*typedNilBroker) ApplyPolicy(policy.Bundle) error { return nil }
func (*typedNilBroker) Release(context.Context) error   { return nil }

var _ Provider = (*typedNilBroker)(nil)
var _ Releaser = (*typedNilBroker)(nil)
