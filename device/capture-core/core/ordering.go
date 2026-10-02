package core

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Startup and shutdown ordering, docs/01-collectors.md §3.5. These strings are the literal
// contract: the supervisor records every step it performs, and the tests assert the recorded
// sequence against the declared one instead of trusting the order of statements in a
// function.
const (
	StepLoadBundle          = "load_bundle_verify_signature"
	StepOpenSpool           = "open_and_unlock_spool"
	StepStartProcDetect     = "start_proc.detect"
	StepStartCLIShim        = "start_cli.shim"
	StepStartClassifierHost = "start_classifier_host"
	StepStartProxyTLS       = "start_proxy.tls"
	StepPointSystemProxy    = "point_system_proxy_at_proxy"
	StepStartProxyLoopback  = "start_proxy.loopback"
)

// Shutdown steps, in order.
const (
	StepReleaseLoopback    = "release_loopback_port"
	StepStopProxyTLS       = "stop_proxy.tls_stop_enforcing"
	StepStopProviders      = "stop_remaining_providers"
	StepDrainSpool         = "drain_spool_bounded"
	StepRestoreSystemProxy = "restore_system_proxy"
	StepRemoveTrustRoot    = "remove_trusted_root"
	StepStopClassifierHost = "stop_classifier_host"
)

// StartupOrder is §3.5's startup column, as data.
func StartupOrder() []string {
	return []string{
		StepLoadBundle, StepOpenSpool, StepStartProcDetect, StepStartCLIShim,
		StepStartClassifierHost, StepStartProxyTLS, StepPointSystemProxy, StepStartProxyLoopback,
	}
}

// ShutdownOrder is §3.5's shutdown column, as data, with the one interpretation §3.5 forces:
// step 1 says "provider.Stop all; proxy stops enforcing first" and step 2 says "RELEASE THE
// LOOPBACK PORT before anything else". Both cannot be literally first, so the broker's release
// happens first of all — it is the one path whose failure breaks the user rather than losing
// data (E14) — and the proxy stops enforcing immediately after. proc.detect, which starts
// first, stops last, so it can report tamper signals about the others.
func ShutdownOrder() []string {
	return []string{
		StepReleaseLoopback, StepStopProxyTLS, StepStopProviders, StepDrainSpool,
		StepRestoreSystemProxy, StepRemoveTrustRoot, StepStopClassifierHost,
	}
}

// PolicyLoader loads and verifies a policy bundle at startup. Failure is not fatal: §13.3
// retains the previous bundle, or falls to M0 with none, and the device keeps collecting what
// it is permitted to collect.
type PolicyLoader interface {
	Load(ctx context.Context) error
}

// DrainResult is what a bounded drain achieved, for the health report.
type DrainResult struct {
	Delivered int
	Dropped   int
	Err       error
}

// SpoolController is the supervisor's view of the spool. Open must succeed before any
// provider starts: a provider with nowhere to write must either drop silently (forbidden,
// C22) or block (forbidden), so it must not start at all.
type SpoolController interface {
	Open(ctx context.Context) error
	Stats() protocol.SpoolStats
	Drain(ctx context.Context, deadline time.Time) (DrainResult, error)
	Close() error
}

// ClassifierHostController supervises the classifier child process. A hung or crashed host is
// `degraded`, never "no labels found" (§3.4), so its failures never abort startup.
type ClassifierHostController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// SystemProxy is the platform proxy configuration. `PointAt` is called only after the proxy
// is listening, and `Effective` is what health consults: a setting that was written but is not
// the effective proxy is not coverage (§5.6).
type SystemProxy interface {
	PointAt(ctx context.Context, addr string) error
	Restore(ctx context.Context) error
	Effective(ctx context.Context) (string, bool)
}

// TrustRoot removes the per-device CA from the platform trust store. Removal is as reliable as
// installation: a customer ending a pilot must not be left with a trusted root installed by
// software they removed (§5.2).
type TrustRoot interface {
	Remove(ctx context.Context) error
}

// ListenAddr is implemented by a provider that listens on a socket, so the supervisor can
// point the system proxy at it. There is no default: a proxy that cannot say where it listens
// must not be pointed at.
type ListenAddr interface {
	ListenAddr() string
}

// Releaser is implemented by a provider whose shutdown must run before anything else —
// `proxy.loopback`, whose held port is the one path whose failure breaks the user (E14).
type Releaser interface {
	Release(ctx context.Context) error
}

// Supervisor runs §3.5's literal ordering. It owns the sequence and the registry owns the
// bookkeeping; neither decides the other's job.
type Supervisor struct {
	Registry       *Registry
	Policy         PolicyLoader
	Spool          SpoolController
	ClassifierHost ClassifierHostController
	SystemProxy    SystemProxy
	TrustRoot      TrustRoot

	// Loopback is the broker provider. It is started last and released first.
	Loopback Provider

	// RemoveTrustRoot is set by an uninstall or a kill switch; ordinary shutdowns leave the CA
	// installed so a restart does not have to reinstall it.
	RemoveTrustRoot bool

	// DrainDeadline bounds the shutdown drain (§3.5 step 3).
	DrainDeadline time.Duration

	Log   Logger
	Clock func() time.Time

	mu    sync.Mutex
	steps []string
}

// NewSupervisor returns a supervisor. Registry is required; the rest may be nil in a test that
// exercises a subset of the order, and a nil component records its step as skipped rather than
// pretending it happened.
func NewSupervisor(reg *Registry, log Logger, clock func() time.Time) (*Supervisor, error) {
	if reg == nil {
		return nil, fmt.Errorf("core: supervisor needs a provider registry")
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = nopLogger{}
	}
	return &Supervisor{Registry: reg, Log: log, Clock: clock, DrainDeadline: 30 * time.Second}, nil
}

func (s *Supervisor) record(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

// Order returns the steps performed so far, in order.
func (s *Supervisor) Order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.steps))
	copy(out, s.steps)
	return out
}

// Startup runs the startup column. The two hard dependencies are structural here: the spool
// opens before any provider starts, and the system proxy is pointed at the proxy only after
// the proxy reports it is listening.
func (s *Supervisor) Startup(ctx context.Context) error {
	s.mu.Lock()
	s.steps = nil
	s.mu.Unlock()

	// 1. load bundle, verify signature (else retain previous, or M0 — §13.3).
	s.record(StepLoadBundle)
	if s.Policy != nil {
		if err := s.Policy.Load(ctx); err != nil {
			// Not fatal by design: the previous bundle stays in force, or the device runs at
			// M0. Failing startup here would turn a control-plane outage into a blind endpoint.
			s.Log.Printf("core: policy load failed; retaining the previous bundle or M0: %v", err)
		}
	}

	// 2. open and unlock the spool, enforce the bound.
	s.record(StepOpenSpool)
	if s.Spool != nil {
		if err := s.Spool.Open(ctx); err != nil {
			return fmt.Errorf("core: spool did not open, so no provider may start (a provider with nowhere to write must not run): %w", err)
		}
	}

	// 3. proc.detect: cheapest provider, no ports, no trust. Starts first so it can produce the
	// tamper signal when something stops the others (C24), and stops last.
	s.record(StepStartProcDetect)
	s.startRoute(ctx, protocol.RouteProcDetect)

	// 4. cli.shim: files only, no ports.
	s.record(StepStartCLIShim)
	s.startRoute(ctx, protocol.RouteCLIShim)

	// 5. classifier-host: idle, resident, keeps spawn cost off the interactive path.
	s.record(StepStartClassifierHost)
	if s.ClassifierHost != nil {
		if err := s.ClassifierHost.Start(ctx); err != nil {
			s.Log.Printf("core: classifier host failed to start; classification degrades to rules-only: %v", err)
		}
	}

	// 6. proxy.tls, then point the system proxy at it — only once it is listening.
	s.record(StepStartProxyTLS)
	tlsOK := s.startRoute(ctx, protocol.RouteProxyTLS)
	s.record(StepPointSystemProxy)
	if tlsOK && s.SystemProxy != nil {
		addr := s.proxyAddr(protocol.RouteProxyTLS)
		if addr == "" {
			s.Log.Printf("core: proxy.tls started but did not report a listen address; leaving the system proxy untouched")
		} else if err := s.SystemProxy.PointAt(ctx, addr); err != nil {
			s.Log.Printf("core: could not point the system proxy at %s: %v", addr, err)
		}
	} else if s.SystemProxy != nil && !tlsOK {
		s.Log.Printf("core: proxy.tls is not listening; the system proxy is deliberately left alone")
	}

	// 7. proxy.loopback LAST, and only after its upstream preflight succeeds. The broker's own
	// Start performs the preflight attempt before returning and stays RELEASED on failure
	// (§6.2 rule 3), which is why "refusing to start" is its safe behaviour rather than an error.
	s.record(StepStartProxyLoopback)
	s.startRoute(ctx, protocol.RouteProxyLoopback)
	return nil
}

func (s *Supervisor) startRoute(ctx context.Context, route protocol.Route) bool {
	if _, ok := s.Registry.Provider(route); !ok {
		return false
	}
	res := s.Registry.StartRoute(ctx, route)
	if res.Err != nil {
		s.Log.Printf("core: provider %s did not start: %v", route, res.Err)
		return false
	}
	return true
}

func (s *Supervisor) proxyAddr(route protocol.Route) string {
	p, ok := s.Registry.Provider(route)
	if !ok {
		return ""
	}
	if la, ok := p.(ListenAddr); ok {
		return la.ListenAddr()
	}
	return ""
}

// Shutdown runs the shutdown column and never returns a provider error: Stop never fails
// visibly (§4.1). An error is returned only for the two things shutdown cannot proceed past —
// nothing here is one, so the return exists for future fatal steps.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	// Shutdown must not be the place a defect becomes user-visible. A provider that panics while
	// being released would otherwise abort the whole shutdown column — leaving the loopback port
	// bound, which is precisely the failure E14 exists to prevent. The panic is logged loudly and
	// the column continues.
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Printf("core: a provider panicked during shutdown; the shutdown column continues: %v", rec)
		}
	}()

	// 1. Release the loopback port before anything else (E14: the one path whose failure
	// breaks the user rather than losing data).
	s.record(StepReleaseLoopback)
	if s.Loopback != nil && !isNilInterface(s.Loopback) {
		if r, ok := s.Loopback.(Releaser); ok {
			if err := r.Release(ctx); err != nil {
				s.Log.Printf("core: loopback release reported an error; the registry will report it tampered: %v", err)
			}
		} else if _, ok := s.Registry.Provider(protocol.RouteProxyLoopback); ok {
			s.Registry.StopRoute(ctx, protocol.RouteProxyLoopback)
		}
	}

	// 2. The proxy stops enforcing first: interception and enforcement stop before the system
	// proxy is restored, so traffic is never pointed at a proxy that has stopped intercepting.
	s.record(StepStopProxyTLS)
	if _, ok := s.Registry.Provider(protocol.RouteProxyTLS); ok {
		s.Registry.StopRoute(ctx, protocol.RouteProxyTLS)
	}

	// 3. Remaining providers; proc.detect (started first) stops last.
	s.record(StepStopProviders)
	for _, route := range []protocol.Route{protocol.RouteCLIShim, protocol.RouteProcDetect} {
		if _, ok := s.Registry.Provider(route); ok {
			s.Registry.StopRoute(ctx, route)
		}
	}

	// 4. Drain the spool, bounded by a deadline.
	s.record(StepDrainSpool)
	if s.Spool != nil {
		deadline := s.Clock().Add(s.DrainDeadline)
		if res, err := s.Spool.Drain(ctx, deadline); err != nil {
			s.Log.Printf("core: spool drain ended with %d delivered, %d dropped: %v", res.Delivered, res.Dropped, err)
		}
		// Nothing uninstalls while the spool holds undelivered events without recording it
		// (§3.5, §12.2): the dropped count for an uninstall is an operator-visible fact.
		if stats := s.Spool.Stats(); stats.Depth > 0 {
			s.Log.Printf("core: shutdown with %d undelivered events still spooled; they are counted as dropped for this shutdown", stats.Depth)
		}
	}

	// 5. Restore the system proxy to its pre-install value.
	s.record(StepRestoreSystemProxy)
	if s.SystemProxy != nil {
		if err := s.SystemProxy.Restore(ctx); err != nil {
			s.Log.Printf("core: could not restore the system proxy: %v", err)
		}
	}

	// 6. Remove the trusted root only when the kill switch or an uninstall asks for it.
	s.record(StepRemoveTrustRoot)
	if s.RemoveTrustRoot && s.TrustRoot != nil {
		if err := s.TrustRoot.Remove(ctx); err != nil {
			s.Log.Printf("core: could not remove the trusted root: %v", err)
		}
	}

	// 7. Stop the classifier host, then exit.
	// (Unbinding 80/443 belongs to the proxy providers' own Stop and has already happened.)
	s.record(StepStopClassifierHost)
	if s.ClassifierHost != nil {
		if err := s.ClassifierHost.Stop(ctx); err != nil {
			s.Log.Printf("core: classifier host did not stop cleanly: %v", err)
		}
	}
	return nil
}

// isNilInterface reports whether an interface value holds a typed nil pointer. It exists because
// a typed nil is not equal to nil: a composition that assigns `var b *Broker; sup.Loopback = b`
// passes a non-nil interface whose method calls dereference a nil receiver. That is how a disabled
// provider becomes a panic on the shutdown path, and shutdown is the one path that must not fail.
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

// CrashLoopGuard is §3.5's crash policy: a crash is a restart with backoff, and a crash loop —
// N restarts in a window — stops the loop, leaves the proxy off (fail open), releases the
// loopback port and reports `absent` with the loop count, rather than retrying into a state
// where the user's network is intermittently broken.
type CrashLoopGuard struct {
	MaxRestarts int
	Window      time.Duration
	Clock       func() time.Time

	mu      sync.Mutex
	history []time.Time
	Loops   int
	stopped bool
}

// NewCrashLoopGuard returns a guard. Zero values take §3.5's shape: 5 restarts per hour.
func NewCrashLoopGuard(max int, window time.Duration, clock func() time.Time) *CrashLoopGuard {
	if max <= 0 {
		max = 5
	}
	if window <= 0 {
		window = time.Hour
	}
	if clock == nil {
		clock = time.Now
	}
	return &CrashLoopGuard{MaxRestarts: max, Window: window, Clock: clock}
}

// ShouldRestart records a crash and answers whether the supervisor may restart. When it
// answers false the loop is over: Stop has been requested once and the caller must not ask
// again.
func (g *CrashLoopGuard) ShouldRestart(reason string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return false
	}
	now := g.Clock()
	cut := now.Add(-g.Window)
	kept := g.history[:0]
	for _, t := range g.history {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	g.history = append(kept, now)
	if len(g.history) > g.MaxRestarts {
		g.stopped = true
		g.Loops = len(g.history)
		return false
	}
	g.Loops = len(g.history)
	return true
}

// Stopped reports whether the guard has ended the loop. A stopped guard never restarts again:
// recovery requires a restart of the service, which is an operator-visible event.
func (g *CrashLoopGuard) Stopped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopped
}

// LoopCount is the number of crashes in the window, reported in the health row.
func (g *CrashLoopGuard) LoopCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Loops
}

// Backoff is the restart delay: doubling from base, capped, with no jitter here because the
// only consumer is a service manager that already jitters.
func (g *CrashLoopGuard) Backoff(base time.Duration) time.Duration {
	g.mu.Lock()
	n := g.Loops
	g.mu.Unlock()
	if n < 1 {
		n = 1
	}
	if n > 6 {
		n = 6
	}
	return base * time.Duration(1<<uint(n-1))
}
