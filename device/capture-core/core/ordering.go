package core

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Startup steps, in order. The supervisor records every step it performs, and the tests assert
// the recorded sequence against the declared one instead of trusting the order of statements in
// a function.
const (
	StepLoadBundle          = "load_bundle_verify_signature"
	StepOpenSpool           = "open_and_unlock_spool"
	StepResolveIdentity     = "resolve_device_identity"
	StepStartCLIShim        = "start_cli.shim"
	StepStartClassifierHost = "start_classifier_host"
	StepStartProxyTLS       = "start_proxy.tls"
	StepStartProxyLoopback  = "start_proxy.loopback"
	StepStartCollectors     = "start_collectors"
)

// Shutdown steps, in order.
const (
	StepReleaseLoopback    = "release_loopback_port"
	StepStopProxyTLS       = "stop_proxy.tls_stop_enforcing"
	StepStopProviders      = "stop_remaining_providers"
	StepDrainSpool         = "drain_spool_bounded"
	StepRemoveTrustRoot    = "remove_trusted_root"
	StepStopClassifierHost = "stop_classifier_host"
)

// StartupOrder is the startup sequence, as data.
func StartupOrder() []string {
	return []string{
		StepLoadBundle, StepOpenSpool, StepResolveIdentity, StepStartCLIShim,
		StepStartClassifierHost, StepStartProxyTLS, StepStartProxyLoopback, StepStartCollectors,
	}
}

// ShutdownOrder is the shutdown sequence, as data. The loopback broker's port is released first
// of all, because a held port whose owner has stopped breaks the user's tool rather than losing
// data; the proxy stops enforcing immediately after.
func ShutdownOrder() []string {
	return []string{
		StepReleaseLoopback, StepStopProxyTLS, StepStopProviders, StepDrainSpool,
		StepRemoveTrustRoot, StepStopClassifierHost,
	}
}

// PolicyLoader loads and verifies a policy bundle at startup. Failure is not fatal: the previous
// bundle stays in force, or the device runs at M0 with none, and keeps collecting what it is
// permitted to collect.
type PolicyLoader interface {
	Load(ctx context.Context) error
}

// DrainResult is what a bounded drain achieved, for the log.
type DrainResult struct {
	Delivered int
	Dropped   int
	Err       error
}

// SpoolController is the supervisor's view of the spool. Open must succeed before any provider
// starts: a provider with nowhere to write would have to drop silently or block, so it must not
// start at all.
type SpoolController interface {
	Open(ctx context.Context) error
	Stats() protocol.SpoolStats
	Drain(ctx context.Context, deadline time.Time) (DrainResult, error)
	Close() error
}

// IdentityResolver installs the envelope identity before any provider starts: the sealed
// credential when there is one, else a bounded enrolment. Its failure leaves the identity
// unresolved, which makes the pipeline refuse to mint rather than stamping a placeholder.
type IdentityResolver interface {
	Resolve(ctx context.Context) error
}

// ClassifierHostController supervises the classifier child process. A hung or crashed host
// degrades classification and never aborts startup.
type ClassifierHostController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// TrustRoot removes the per-device CA from the platform trust store. Removal is as reliable as
// installation: a customer who removes the agent must not be left with a trusted root it
// installed.
type TrustRoot interface {
	Remove(ctx context.Context) error
}

// Releaser is implemented by a provider whose shutdown must run before anything else:
// proxy.loopback, whose held port breaks the user's tool if it outlives the broker.
type Releaser interface {
	Release(ctx context.Context) error
}

// Supervisor runs the startup and shutdown sequences. It owns the sequence; the registry owns
// the bookkeeping.
type Supervisor struct {
	Registry       *Registry
	Policy         PolicyLoader
	Spool          SpoolController
	Identity       IdentityResolver
	ClassifierHost ClassifierHostController
	TrustRoot      TrustRoot

	// Loopback is the broker provider. It is started last of the fixed providers and released first.
	Loopback Provider

	// RemoveTrustRoot removes the per-device CA at shutdown.
	RemoveTrustRoot bool

	// DrainDeadline bounds the shutdown drain.
	DrainDeadline time.Duration

	Log   Logger
	Clock func() time.Time

	mu    sync.Mutex
	steps []string
}

// NewSupervisor returns a supervisor. Registry is required; the rest may be nil in a test that
// exercises part of the sequence, and a nil component's step is recorded but does nothing.
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

// Startup runs the startup sequence. Its one hard dependency is structural: the spool opens
// before any provider starts.
func (s *Supervisor) Startup(ctx context.Context) error {
	s.mu.Lock()
	s.steps = nil
	s.mu.Unlock()

	s.record(StepLoadBundle)
	if s.Policy != nil {
		if err := s.Policy.Load(ctx); err != nil {
			// Not fatal: failing startup here would turn a control-plane outage into a blind
			// endpoint.
			s.Log.Printf("core: policy load failed; retaining the previous bundle or M0: %v", err)
		}
	}

	s.record(StepOpenSpool)
	if s.Spool != nil {
		if err := s.Spool.Open(ctx); err != nil {
			return fmt.Errorf("core: spool did not open, so no provider may start: %w", err)
		}
	}

	// The identity is resolved after the spool opens (the sealed credential unseals under the
	// spool key) and before any provider starts. A failure is not fatal: the pipeline refuses to
	// mint until enrolment succeeds in the background.
	s.record(StepResolveIdentity)
	if s.Identity != nil {
		if err := s.Identity.Resolve(ctx); err != nil {
			s.Log.Printf("core: identity unresolved; providers start but will refuse to mint until enrolment succeeds: %v", err)
		}
	}

	// cli.shim writes files and environment only; it holds no ports.
	s.record(StepStartCLIShim)
	s.startCollector(ctx, protocol.CollectorCLIShim)

	// The classifier host idles resident so its spawn cost stays off the interactive path.
	s.record(StepStartClassifierHost)
	if s.ClassifierHost != nil {
		if err := s.ClassifierHost.Start(ctx); err != nil {
			s.Log.Printf("core: classifier host failed to start; classification degrades to rules-only: %v", err)
		}
	}

	s.record(StepStartProxyTLS)
	s.startCollector(ctx, protocol.CollectorEgressProxy)

	// proxy.loopback last of the fixed providers. The broker's own Start runs the upstream
	// preflight and stays released when it fails, so refusing to start is its safe behaviour
	// rather than an error.
	s.record(StepStartProxyLoopback)
	s.startCollector(ctx, protocol.CollectorLoopbackBroker)

	// Every other provider, concurrently, unless the bundle in force switches it off. From here a
	// policy change starts or stops a Toggled provider.
	s.record(StepStartCollectors)
	for _, res := range s.Registry.StartCollectors(ctx, fixedCollectors...) {
		if res.Err != nil {
			s.Log.Printf("core: provider %s did not start: %v", res.Collector, res.Err)
		}
	}
	return nil
}

// fixedCollectors are the providers the startup and shutdown sequences place by name.
var fixedCollectors = []protocol.Collector{
	protocol.CollectorCLIShim, protocol.CollectorEgressProxy, protocol.CollectorLoopbackBroker,
}

// startCollector starts a fixed provider at its step, unless it is Toggled and the bundle in force
// switches it off; then a policy toggle starts it once start_collectors has run.
func (s *Supervisor) startCollector(ctx context.Context, c protocol.Collector) bool {
	if _, ok := s.Registry.Provider(c); !ok {
		return false
	}
	if !s.Registry.Enabled(c) {
		s.Log.Printf("core: provider %s not started: the bundle in force switches it off", c)
		return false
	}
	res := s.Registry.StartCollector(ctx, c)
	if res.Err != nil {
		s.Log.Printf("core: provider %s did not start: %v", c, res.Err)
		return false
	}
	return true
}

// Shutdown runs the shutdown sequence. It never returns a provider error, and a panic in one
// step is contained so the steps after it still run.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Printf("core: a provider panicked during shutdown; the shutdown sequence continues: %v", rec)
		}
	}()

	s.record(StepReleaseLoopback)
	s.safeStep("loopback release", func() {
		if s.Loopback != nil && !isNilInterface(s.Loopback) {
			if r, ok := s.Loopback.(Releaser); ok {
				if err := r.Release(ctx); err != nil {
					s.Log.Printf("core: loopback release reported an error; the registry will report it tampered: %v", err)
				}
			} else if _, ok := s.Registry.Provider(protocol.CollectorLoopbackBroker); ok {
				s.Registry.StopCollector(ctx, protocol.CollectorLoopbackBroker)
			}
		}
	})

	s.record(StepStopProxyTLS)
	s.safeStep("proxy.tls stop", func() {
		if _, ok := s.Registry.Provider(protocol.CollectorEgressProxy); ok {
			s.Registry.StopCollector(ctx, protocol.CollectorEgressProxy)
		}
	})

	s.record(StepStopProviders)
	s.safeStep("remaining providers stop", func() {
		// cli.shim and every provider start_collectors started, concurrently. Policy toggles end
		// here, so a bundle that arrives now starts nothing.
		s.Registry.StopExcept(ctx, protocol.CollectorEgressProxy, protocol.CollectorLoopbackBroker)
	})

	s.record(StepDrainSpool)
	s.safeStep("spool drain", func() {
		if s.Spool != nil {
			deadline := s.Clock().Add(s.DrainDeadline)
			if res, err := s.Spool.Drain(ctx, deadline); err != nil {
				s.Log.Printf("core: spool drain ended with %d delivered, %d dropped: %v", res.Delivered, res.Dropped, err)
			}
			if stats := s.Spool.Stats(); stats.Depth > 0 {
				s.Log.Printf("core: shutdown with %d undelivered events still spooled; they are delivered after the next start", stats.Depth)
			}
		}
	})

	s.record(StepRemoveTrustRoot)
	if s.RemoveTrustRoot && s.TrustRoot != nil {
		s.safeStep("trusted root removal", func() {
			if err := s.TrustRoot.Remove(ctx); err != nil {
				s.Log.Printf("core: could not remove the trusted root: %v", err)
			}
		})
	}

	s.record(StepStopClassifierHost)
	s.safeStep("classifier host stop", func() {
		if s.ClassifierHost != nil {
			if err := s.ClassifierHost.Stop(ctx); err != nil {
				s.Log.Printf("core: classifier host did not stop cleanly: %v", err)
			}
		}
	})
	return nil
}

// safeStep runs one shutdown step and contains a panic inside it, so a defect in one step cannot
// cancel the loopback release, the proxy stop or the spool drain.
func (s *Supervisor) safeStep(name string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Printf("core: panic during %s; the shutdown sequence continues: %v", name, rec)
		}
	}()
	fn()
}

// isNilInterface reports whether an interface value holds a typed nil pointer: a typed nil is not
// equal to nil, and calling a method on it would panic the shutdown path.
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
