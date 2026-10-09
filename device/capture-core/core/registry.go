package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// Registry owns the provider set, keyed by collector. It starts providers concurrently (one
// failure degrades one coverage row and nothing else) and is the single place that decides what
// a provider's health row is allowed to say.
//
// The registry never restarts a provider. A crash-loop policy is a supervisor decision, and
// keeping it out of the registry is what makes "which provider is running" one question with
// one answer. The only start or stop the registry makes on its own is a policy toggle: a
// Toggled provider that a new bundle switches on or off.
type Registry struct {
	mu        sync.Mutex
	order     []protocol.Collector
	providers map[protocol.Collector]Provider
	rows      map[protocol.Collector]*rowState
	clock     func() time.Time
	log       Logger

	// lifecycle serialises policy toggles with the supervisor's collector start and its
	// shutdown stop, so a bundle that arrives during shutdown cannot start a provider again.
	lifecycle sync.Mutex
	// toggling is true from the supervisor's collector start until shutdown stops the providers.
	// Outside that window a policy switch is recorded but starts and stops nothing: before it the
	// spool may not be open yet, and after it the service is stopping.
	toggling bool
}

type rowState struct {
	provider  Provider
	startErr  error
	stopErr   error
	started   bool
	stopped   bool
	disabled  bool // the bundle last applied (or none) switches this Toggled provider off
	startedAt time.Time
	stoppedAt time.Time
	lastPanic string
}

func (row *rowState) running() bool { return row.started && !row.stopped }

// NewRegistry returns an empty registry.
func NewRegistry(clock func() time.Time, log Logger) *Registry {
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = nopLogger{}
	}
	return &Registry{
		providers: map[protocol.Collector]Provider{},
		rows:      map[protocol.Collector]*rowState{},
		clock:     clock,
		log:       log,
	}
}

// Add registers a provider. Two providers for one collector would be two claims to one coverage
// row, so the second is refused rather than silently winning.
func (r *Registry) Add(p Provider) error {
	if p == nil {
		return errors.New("core: nil provider")
	}
	c := p.Name()
	if !c.Valid() {
		return fmt.Errorf("core: provider names collector %q outside the closed vocabulary", c)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[c]; dup {
		return fmt.Errorf("core: two providers claim collector %q; one provider owns one coverage row", c)
	}
	row := &rowState{provider: p}
	// Until a bundle is applied, a Toggled provider follows what it says with none in force.
	if t, ok := p.(Toggled); ok {
		row.disabled = !t.Enabled(nil)
	}
	r.providers[c] = p
	r.rows[c] = row
	r.order = append(r.order, c)
	return nil
}

// Collectors lists the registered collectors in registration order.
func (r *Registry) Collectors() []protocol.Collector {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.Collector, len(r.order))
	copy(out, r.order)
	return out
}

// Provider returns a registered provider.
func (r *Registry) Provider(c protocol.Collector) (Provider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[c]
	return p, ok
}

// Enabled reports whether the bundle last applied (or none) leaves a collector on. Only a Toggled
// provider can be switched off.
func (r *Registry) Enabled(c protocol.Collector) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[c]
	return ok && !row.disabled
}

// StartResult is one provider's start outcome. It is returned for every provider, so the
// caller can report a row even for the ones that failed.
type StartResult struct {
	Collector protocol.Collector
	Err       error
	Panicked  string
	Duration  time.Duration
}

// StartAll starts every registered provider concurrently and waits for all of them. One
// failure does not stop the rest: a provider failure degrades one coverage row and nothing
// else.
//
// A panic inside a provider's Start is recovered and converted into that provider's failure,
// because a provider that can panic the registry can degrade every row at once.
func (r *Registry) StartAll(ctx context.Context) []StartResult {
	return r.startConcurrently(ctx, r.Collectors())
}

// StartCollectors starts, concurrently, every registered provider that is not in skip, is not
// running, and is enabled by the bundle last applied. From then until StopExcept, a policy
// change starts or stops a Toggled provider.
func (r *Registry) StartCollectors(ctx context.Context, skip ...protocol.Collector) []StartResult {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	var todo []protocol.Collector
	for _, c := range r.order {
		row := r.rows[c]
		if slices.Contains(skip, c) || row.running() || row.disabled {
			continue
		}
		todo = append(todo, c)
	}
	r.toggling = true
	r.mu.Unlock()
	return r.startConcurrently(ctx, todo)
}

func (r *Registry) startConcurrently(ctx context.Context, cs []protocol.Collector) []StartResult {
	results := make([]StartResult, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c protocol.Collector) {
			defer wg.Done()
			results[i] = r.startOne(ctx, c)
		}(i, c)
	}
	wg.Wait()
	return results
}

// StartCollector starts one provider, for the ordering dependencies that forbid a blanket
// concurrent start (the loopback broker binds last).
func (r *Registry) StartCollector(ctx context.Context, c protocol.Collector) StartResult {
	return r.startOne(ctx, c)
}

func (r *Registry) startOne(ctx context.Context, c protocol.Collector) (res StartResult) {
	r.mu.Lock()
	row, ok := r.rows[c]
	r.mu.Unlock()
	res.Collector = c
	if !ok {
		res.Err = fmt.Errorf("core: collector %q is not registered", c)
		return res
	}
	start := r.clock()
	defer func() {
		if rec := recover(); rec != nil {
			res.Panicked = fmt.Sprint(rec)
			res.Err = fmt.Errorf("core: provider %s panicked during Start: %v", c, rec)
			r.recordStart(c, res.Err, res.Panicked)
		}
		res.Duration = r.clock().Sub(start)
	}()
	err := row.provider.Start(ctx)
	r.recordStart(c, err, "")
	res.Err = err
	return res
}

func (r *Registry) recordStart(c protocol.Collector, err error, panicked string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[c]
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
	r.log.Printf("core: provider %s failed to start: %v", c, err)
}

// StopResult is one provider's stop outcome.
type StopResult struct {
	Collector protocol.Collector
	Err       error
	Panicked  string
}

// StopAll stops every provider concurrently. It never returns an error to the caller: Stop
// never fails visibly, the error is logged and the provider reported `tampered`, because
// interference is evidence and shutdown must not deadlock on it.
func (r *Registry) StopAll(ctx context.Context) []StopResult {
	return r.StopExcept(ctx)
}

// StopExcept stops, concurrently, every provider not in skip that the registry has not already
// stopped, and ends policy toggling. Shutdown uses it after the steps that stop providers in a
// fixed order.
func (r *Registry) StopExcept(ctx context.Context, skip ...protocol.Collector) []StopResult {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	r.toggling = false
	var todo []protocol.Collector
	for _, c := range r.order {
		if slices.Contains(skip, c) || r.rows[c].stopped {
			continue
		}
		todo = append(todo, c)
	}
	r.mu.Unlock()

	results := make([]StopResult, len(todo))
	var wg sync.WaitGroup
	for i, c := range todo {
		wg.Add(1)
		go func(i int, c protocol.Collector) {
			defer wg.Done()
			results[i] = r.StopCollector(ctx, c)
		}(i, c)
	}
	wg.Wait()
	return results
}

// StopCollector stops one provider and returns what happened, for logging. The caller decides
// the order; the registry decides the bookkeeping.
func (r *Registry) StopCollector(ctx context.Context, c protocol.Collector) (res StopResult) {
	r.mu.Lock()
	row, ok := r.rows[c]
	r.mu.Unlock()
	res.Collector = c
	if !ok {
		res.Err = fmt.Errorf("core: collector %q is not registered", c)
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
			res.Err = fmt.Errorf("core: provider %s panicked during Stop: %v", c, rec)
		}
		if res.Err != nil {
			r.mu.Lock()
			row.stopErr = res.Err
			r.mu.Unlock()
			r.log.Printf("core: provider %s failed to stop; reporting tampered: %v", c, res.Err)
		}
	}()
	res.Err = row.provider.Stop(ctx)
	return res
}

// ApplyResults reports per-provider policy application. A provider that cannot apply a diff
// keeps its previous behaviour and reports it; the registry does not restart it (applying
// policy is a diff, never a restart).
type ApplyResult struct {
	Collector protocol.Collector
	Err       error
}

// ApplyPolicy fans a verified bundle out to every provider, then starts each Toggled provider
// the bundle switches on and stops each one it switches off, concurrently; no other provider is
// touched. Before the supervisor starts the collectors, and once shutdown has stopped them, the
// switch is only recorded.
func (r *Registry) ApplyPolicy(b policy.Bundle) []ApplyResult {
	cs := r.Collectors()
	out := make([]ApplyResult, 0, len(cs))
	for _, c := range cs {
		p, _ := r.Provider(c)
		out = append(out, ApplyResult{Collector: c, Err: p.ApplyPolicy(b)})
	}

	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	var start, stop []protocol.Collector
	r.mu.Lock()
	for _, c := range r.order {
		row := r.rows[c]
		t, ok := row.provider.(Toggled)
		if !ok {
			continue
		}
		row.disabled = !t.Enabled(&b)
		switch {
		case !r.toggling:
		case !row.disabled && !row.running():
			start = append(start, c)
		case row.disabled && row.running():
			stop = append(stop, c)
		}
	}
	r.mu.Unlock()

	// A toggle is not bound to the poll that delivered the bundle: the provider outlives it.
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, c := range start {
		wg.Add(1)
		go func(c protocol.Collector) {
			defer wg.Done()
			if res := r.startOne(ctx, c); res.Err == nil {
				r.log.Printf("core: provider %s started: the bundle in force switches it on", c)
			}
		}(c)
	}
	for _, c := range stop {
		wg.Add(1)
		go func(c protocol.Collector) {
			defer wg.Done()
			if res := r.StopCollector(ctx, c); res.Err == nil {
				r.log.Printf("core: provider %s stopped: the bundle in force switches it off", c)
			}
		}(c)
	}
	wg.Wait()
	return out
}

// Health returns one row per provider, in registration order, with the registry's
// bookkeeping applied. The overrides are the point:
//
//   - a provider that never started, or failed to start, is `absent` regardless of what it
//     claims — no provider may fail into a state that reports success;
//   - a provider whose Stop failed is `tampered` (interference is evidence);
//   - a provider that was stopped can never be `healthy` again;
//   - a provider the bundle switches off, and that is not running, is `absent` with
//     `disabled_by_policy`: its Stop was requested, so it is not `tampered`;
//   - a row whose state or counters are outside the closed vocabularies is reported as
//     `absent` (state) or has the unknown names dropped (counters), and the defect is logged.
func (r *Registry) Health() []Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Health, 0, len(r.order))
	for _, c := range r.order {
		out = append(out, r.rowHealthLocked(c))
	}
	return out
}

// HealthFor returns one provider's row.
func (r *Registry) HealthFor(c protocol.Collector) (Health, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[c]; !ok {
		return Health{}, false
	}
	return r.rowHealthLocked(c), true
}

func (r *Registry) rowHealthLocked(c protocol.Collector) Health {
	row := r.rows[c]
	h := row.provider.Health()

	// Sanitise first: a provider cannot smuggle an unknown name onto the health channel.
	if err := h.Validate(); err != nil {
		r.log.Printf("core: provider %s reported health outside the closed vocabularies: %v", c, err)
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
	case row.disabled && !row.running():
		h.State = protocol.StateAbsent
		h.Detail = protocol.DetailDisabledByPolicy
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

// Reports renders every row for the health channel. A row's collector is its provider's Name.
//
// A row is never dropped for a detail the closed vocabulary does not yet contain: losing a
// `tampered` signal because its cause name is unknown to the reporting layer would be worse
// than sending a cause the server records as unknown, and the cause is what an operator needs
// to attribute a coverage cliff. The validation error is returned so the caller logs it, and
// the row still goes.
func (r *Registry) Reports(deviceID, version string) ([]protocol.HealthReport, []error) {
	cs := r.Collectors()
	rows := r.Health()
	reports := make([]protocol.HealthReport, 0, len(rows))
	var errs []error
	for i, h := range rows {
		rep := h.Report(deviceID, version)
		rep.Collector = string(cs[i])
		if err := rep.Validate(); err != nil {
			errs = append(errs, err)
			r.log.Printf("core: health row for %s is not fully valid; sending it anyway: %v", cs[i], err)
		}
		reports = append(reports, rep)
	}
	return reports, errs
}

// RowsByName is a test and reporting convenience: the health rows keyed by collector name.
func (r *Registry) RowsByName() map[string]Health {
	cs := r.Collectors()
	out := map[string]Health{}
	for i, h := range r.Health() {
		out[string(cs[i])] = h
	}
	return out
}

// SortedCollectors returns the registered collectors sorted, for deterministic reports.
func (r *Registry) SortedCollectors() []protocol.Collector {
	out := r.Collectors()
	slices.Sort(out)
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
