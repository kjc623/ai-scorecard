package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Logger is the narrow logging seam the registry uses for the two facts that must not be
// lost: a Stop that failed (which is reported as tampering) and a provider that reported
// something outside the closed vocabularies.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Registry owns the provider set. It starts providers concurrently (§4.1: one failure
// degrades one coverage row and nothing else) and is the single place that decides what a
// provider's health row is allowed to say.
//
// The registry never restarts a provider. A crash-loop policy is a supervisor decision
// (§3.5), and keeping it out of the registry is what makes "which provider is running" one
// question with one answer.
type Registry struct {
	mu        sync.Mutex
	order     []protocol.Route
	providers map[protocol.Route]Provider
	rows      map[protocol.Route]*rowState
	clock     func() time.Time
	log       Logger
}

type rowState struct {
	provider  Provider
	startErr  error
	stopErr   error
	started   bool
	stopped   bool
	startedAt time.Time
	stoppedAt time.Time
	lastPanic string
}

// NewRegistry returns an empty registry.
func NewRegistry(clock func() time.Time, log Logger) *Registry {
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = nopLogger{}
	}
	return &Registry{
		providers: map[protocol.Route]Provider{},
		rows:      map[protocol.Route]*rowState{},
		clock:     clock,
		log:       log,
	}
}

// Add registers a provider. Two providers on one route would be two claims to one coverage
// row, so the second is refused rather than silently winning.
func (r *Registry) Add(p Provider) error {
	if p == nil {
		return errors.New("core: nil provider")
	}
	route := p.Name()
	if !route.Valid() {
		return fmt.Errorf("core: provider names route %q outside the closed vocabulary", route)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[route]; dup {
		return fmt.Errorf("core: two providers claim route %q; one provider owns one coverage row", route)
	}
	r.providers[route] = p
	r.rows[route] = &rowState{provider: p}
	r.order = append(r.order, route)
	return nil
}

// Routes lists the registered routes in registration order.
func (r *Registry) Routes() []protocol.Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.Route, len(r.order))
	copy(out, r.order)
	return out
}

// Provider returns a registered provider.
func (r *Registry) Provider(route protocol.Route) (Provider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[route]
	return p, ok
}

// StartResult is one provider's start outcome. It is returned for every provider, so the
// caller can report a row even for the ones that failed.
type StartResult struct {
	Route    protocol.Route
	Err      error
	Panicked string
	Duration time.Duration
}

// StartAll starts every registered provider concurrently and waits for all of them. One
// failure does not stop the rest: a provider failure degrades one coverage row and nothing
// else (master §3, Alternative B).
//
// A panic inside a provider's Start is recovered and converted into that provider's failure,
// because a provider that can panic the registry can degrade every row at once.
func (r *Registry) StartAll(ctx context.Context) []StartResult {
	r.mu.Lock()
	routes := append([]protocol.Route(nil), r.order...)
	r.mu.Unlock()

	results := make([]StartResult, len(routes))
	var wg sync.WaitGroup
	for i, route := range routes {
		wg.Add(1)
		go func(i int, route protocol.Route) {
			defer wg.Done()
			results[i] = r.startOne(ctx, route)
		}(i, route)
	}
	wg.Wait()
	return results
}

// StartRoute starts one provider, for the §3.5 ordering dependencies that forbid a blanket
// concurrent start (the broker binds last; the system proxy is pointed at the proxy only
// once it is listening).
func (r *Registry) StartRoute(ctx context.Context, route protocol.Route) StartResult {
	return r.startOne(ctx, route)
}

func (r *Registry) startOne(ctx context.Context, route protocol.Route) (res StartResult) {
	r.mu.Lock()
	row, ok := r.rows[route]
	r.mu.Unlock()
	res.Route = route
	if !ok {
		res.Err = fmt.Errorf("core: route %q is not registered", route)
		return res
	}
	start := r.clock()
	defer func() {
		if rec := recover(); rec != nil {
			res.Panicked = fmt.Sprint(rec)
			res.Err = fmt.Errorf("core: provider %s panicked during Start: %v", route, rec)
			r.recordStart(route, res.Err, res.Panicked)
		}
		res.Duration = r.clock().Sub(start)
	}()
	err := row.provider.Start(ctx)
	r.recordStart(route, err, "")
	res.Err = err
	return res
}

func (r *Registry) recordStart(route protocol.Route, err error, panicked string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[route]
	if row == nil {
		return
	}
	row.startedAt = r.clock()
	row.startErr = err
	row.lastPanic = panicked
	if err == nil {
		row.started = true
		row.stopped = false
		return
	}
	// A failed Start is not "started"; the provider is out of the path until a successful
	// Start says otherwise.
	row.started = false
	r.log.Printf("core: provider %s failed to start: %v", route, err)
}

// StopResult is one provider's stop outcome.
type StopResult struct {
	Route    protocol.Route
	Err      error
	Panicked string
}

// StopAll stops every provider concurrently. It never returns an error to the caller: Stop
// never fails visibly, the error is logged and the provider reported `tampered`, because
// interference is evidence and shutdown must not deadlock on it (§4.1).
func (r *Registry) StopAll(ctx context.Context) []StopResult {
	r.mu.Lock()
	routes := append([]protocol.Route(nil), r.order...)
	r.mu.Unlock()

	results := make([]StopResult, len(routes))
	var wg sync.WaitGroup
	for i, route := range routes {
		wg.Add(1)
		go func(i int, route protocol.Route) {
			defer wg.Done()
			results[i] = r.StopRoute(ctx, route)
		}(i, route)
	}
	wg.Wait()
	return results
}

// StopRoute stops one provider and returns what happened, for logging. The caller decides
// the order; the registry decides the bookkeeping.
func (r *Registry) StopRoute(ctx context.Context, route protocol.Route) (res StopResult) {
	r.mu.Lock()
	row, ok := r.rows[route]
	r.mu.Unlock()
	res.Route = route
	if !ok {
		res.Err = fmt.Errorf("core: route %q is not registered", route)
		return res
	}
	// Mark it stopped *before* the call: Health must never say healthy after Stop, even if
	// Stop blocks or panics.
	r.mu.Lock()
	row.stopped = true
	row.stoppedAt = r.clock()
	r.mu.Unlock()

	defer func() {
		if rec := recover(); rec != nil {
			res.Panicked = fmt.Sprint(rec)
			res.Err = fmt.Errorf("core: provider %s panicked during Stop: %v", route, rec)
		}
		if res.Err != nil {
			r.mu.Lock()
			row.stopErr = res.Err
			r.mu.Unlock()
			r.log.Printf("core: provider %s failed to stop; reporting tampered: %v", route, res.Err)
		}
	}()
	res.Err = row.provider.Stop(ctx)
	return res
}

// ApplyResults reports per-provider policy application. A provider that cannot apply a diff
// keeps its previous behaviour and reports it; the registry does not restart it (§4.1: apply
// is a diff, never a restart).
type ApplyResult struct {
	Route protocol.Route
	Err   error
}

// ApplyPolicy fans a verified bundle out to every provider.
func (r *Registry) ApplyPolicy(b policy.Bundle) []ApplyResult {
	r.mu.Lock()
	routes := append([]protocol.Route(nil), r.order...)
	r.mu.Unlock()
	out := make([]ApplyResult, 0, len(routes))
	for _, route := range routes {
		r.mu.Lock()
		row := r.rows[route]
		r.mu.Unlock()
		out = append(out, ApplyResult{Route: route, Err: row.provider.ApplyPolicy(b)})
	}
	return out
}

// Health returns one row per provider, in registration order, with the registry's
// bookkeeping applied. The overrides are the point:
//
//   - a provider that never started, or failed to start, is `absent` regardless of what it
//     claims — no provider may fail into a state that reports success (C25);
//   - a provider whose Stop failed is `tampered` (interference is evidence);
//   - a provider that was stopped can never be `healthy` again;
//   - a row whose state or counters are outside the closed vocabularies is reported as
//     `absent` (state) or has the unknown names dropped (counters), and the defect is logged.
func (r *Registry) Health() []Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Health, 0, len(r.order))
	for _, route := range r.order {
		out = append(out, r.rowHealthLocked(route))
	}
	return out
}

// HealthFor returns one provider's row.
func (r *Registry) HealthFor(route protocol.Route) (Health, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[route]; !ok {
		return Health{}, false
	}
	return r.rowHealthLocked(route), true
}

func (r *Registry) rowHealthLocked(route protocol.Route) Health {
	row := r.rows[route]
	h := row.provider.Health()

	// Sanitise first: a provider cannot smuggle an unknown name onto the health channel.
	if err := h.Validate(); err != nil {
		r.log.Printf("core: provider %s reported health outside the closed vocabularies: %v", route, err)
		h = sanitise(h)
	}
	if h.Counters == nil {
		h.Counters = map[protocol.Counter]uint64{}
	}
	if h.WindowDelta == nil {
		h.WindowDelta = map[protocol.Counter]uint64{}
	}
	if h.Since.IsZero() {
		h.Since = row.startedAt
	}

	switch {
	case row.stopErr != nil:
		h.State = protocol.StateTampered
		if h.Detail == protocol.DetailNone {
			h.Detail = protocol.Detail("stop_failed")
		}
	case row.stopped:
		if h.State == protocol.StateHealthy {
			h.State = protocol.StateAbsent
		}
		if h.State != protocol.StateTampered {
			h.State = protocol.StateAbsent
		}
	case !row.started:
		// Never started, or a failed Start: out of the path, and it cannot say otherwise.
		h.State = protocol.StateAbsent
	}
	return h
}

func sanitise(h Health) Health {
	switch h.State {
	case protocol.StateHealthy, protocol.StateDegraded, protocol.StateAbsent, protocol.StateTampered:
	default:
		h.State = protocol.StateAbsent
	}
	drop := func(m map[protocol.Counter]uint64) map[protocol.Counter]uint64 {
		out := make(map[protocol.Counter]uint64, len(protocol.AllCounters))
		for _, k := range protocol.AllCounters {
			out[k] = 0
		}
		for k, v := range m {
			if knownCounter(k) {
				out[k] = v
				continue
			}
			out[protocol.CounterErrors]++
		}
		return out
	}
	h.Counters = drop(h.Counters)
	h.WindowDelta = drop(h.WindowDelta)
	if h.Detail == "" {
		h.Detail = protocol.Detail("invalid_health_report")
	}
	return h
}

// Reports renders every row for the health channel. A row that still fails protocol
// validation after sanitising is skipped rather than sent with a name the reporting layer
// does not know; the caller gets the error so it can be logged.
func (r *Registry) Reports(deviceID, version string) ([]protocol.HealthReport, []error) {
	rows := r.Health()
	reports := make([]protocol.HealthReport, 0, len(rows))
	var errs []error
	for i, h := range rows {
		route := r.Routes()[i]
		rep := h.Report(deviceID, version)
		rep.Collector = string(route)
		if err := rep.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		reports = append(reports, rep)
	}
	return reports, errs
}

// RowsByName is a test and reporting convenience: the health rows keyed by collector name.
func (r *Registry) RowsByName() map[string]Health {
	out := map[string]Health{}
	for i, h := range r.Health() {
		out[string(r.Routes()[i])] = h
	}
	return out
}

// SortedRoutes returns the registered routes sorted, for deterministic reports.
func (r *Registry) SortedRoutes() []protocol.Route {
	out := r.Routes()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResourceStack is the small helper that makes "Start is transactional" the default rather
// than a rule to remember: a provider registers each acquisition with its release, and a
// failed Start runs the releases in reverse order.
//
// It is deliberately not a Provider; it is the thing a Provider uses so its Start can return
// an error knowing it left nothing behind.
type ResourceStack struct {
	mu       sync.Mutex
	releases []func()
}

// On registers a release to run if the start does not complete.
func (s *ResourceStack) On(release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, release)
}

// Commit marks the start successful: the releases are dropped and will not run. Commit is
// idempotent, and a stack that has neither committed nor rolled back still releases, so a
// caller that returns early by mistake cannot leak a resource.
func (s *ResourceStack) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = nil
}

// Rollback runs every release in reverse acquisition order and clears the stack. A release
// that panics does not prevent the others from running: a half-released start is the state
// the contract forbids.
func (s *ResourceStack) Rollback() {
	s.mu.Lock()
	releases := s.releases
	s.releases = nil
	s.mu.Unlock()
	for i := len(releases) - 1; i >= 0; i-- {
		func(f func()) {
			defer func() { _ = recover() }()
			f()
		}(releases[i])
	}
}

// FailedStart runs the stack's releases and returns err, so a provider's Start can be one
// line: `return stack.Failed(err)`.
func (s *ResourceStack) Failed(err error) error {
	s.Rollback()
	return err
}
