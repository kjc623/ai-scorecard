// Package core is the endpoint agent's spine: the provider contract (docs/01-collectors.md
// §4.1), the health and counter contract (§4.2, §4.3), the mode resolution and the content
// gate (§11), and the startup/shutdown ordering (§3.5).
//
// Two properties are structural rather than documentary here:
//
//   - A provider cannot report success it did not observe. Health is assembled from a
//     positive observation, and the registry overrides any row that contradicts its own
//     bookkeeping (a provider that failed to start cannot surface as healthy).
//   - The M0 content gate is not a check someone has to remember: content is reached only
//     through a reader the pipeline consults *after* the mode is resolved, and the gate
//     refuses to read when the mode does not permit it.
package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Health is the per-provider health row of §4.1. `Detail` is protocol.Detail — the closed
// vocabulary the health channel carries as `error_code` — rather than the bare `string` the
// document sketches, because the closed set is what lets a coverage report group by cause
// without parsing prose.
//
// WindowDelta and WindowSince are §4.3's second half: counters are cumulative since process
// start *plus* a windowed delta, because "1,204 dropped since Tuesday" is actionable and
// "1,204 dropped since install" is not. protocol.HealthReport carries one counter map, so
// the wire encoding puts cumulative counters there and the window is exposed separately (see
// the endpoint report's open items).
type Health struct {
	State       protocol.CollectorState
	Detail      protocol.Detail
	LastSuccess time.Time // zero means "never" — the field C23 requires
	Counters    map[protocol.Counter]uint64
	WindowDelta map[protocol.Counter]uint64
	WindowSince time.Time
	Since       time.Time
}

// Healthy is the constructor for the one state that asserts a positive observation.
func Healthy(detail protocol.Detail, since, lastSuccess time.Time, counters *CounterSet) Health {
	return buildHealth(protocol.StateHealthy, detail, since, lastSuccess, counters)
}

// Degraded names a capability that is not working while the provider is still in the path.
func Degraded(detail protocol.Detail, since, lastSuccess time.Time, counters *CounterSet) Health {
	return buildHealth(protocol.StateDegraded, detail, since, lastSuccess, counters)
}

// Absent is "not running, not in the path, or crashed out".
func Absent(detail protocol.Detail, since time.Time, counters *CounterSet) Health {
	return buildHealth(protocol.StateAbsent, detail, since, time.Time{}, counters)
}

// Tampered is the external-facing state: something outside the provider changed it. It is
// the only state that raises a security finding rather than an operations one.
func Tampered(detail protocol.Detail, since, lastSuccess time.Time, counters *CounterSet) Health {
	return buildHealth(protocol.StateTampered, detail, since, lastSuccess, counters)
}

func buildHealth(state protocol.CollectorState, detail protocol.Detail, since, lastSuccess time.Time, counters *CounterSet) Health {
	h := Health{
		State:       state,
		Detail:      detail,
		Since:       since,
		LastSuccess: lastSuccess,
		Counters:    map[protocol.Counter]uint64{},
		WindowDelta: map[protocol.Counter]uint64{},
	}
	for _, c := range protocol.AllCounters {
		h.Counters[c] = 0
		h.WindowDelta[c] = 0
	}
	if counters != nil {
		h.Counters = counters.Cumulative()
		h.WindowDelta, h.WindowSince = counters.Window()
	}
	return h
}

// Validate rejects a health row outside the closed vocabularies. A provider that reports an
// unknown state or counter is a defect upstream, and the row is refused rather than stored
// with a name the reporting layer does not know.
func (h Health) Validate() error {
	switch h.State {
	case protocol.StateHealthy, protocol.StateDegraded, protocol.StateAbsent, protocol.StateTampered:
	default:
		return fmt.Errorf("core: health state %q outside the closed set", h.State)
	}
	for k := range h.Counters {
		if !knownCounter(k) {
			return fmt.Errorf("core: health carries counter %q outside the closed seven", k)
		}
	}
	for k := range h.WindowDelta {
		if !knownCounter(k) {
			return fmt.Errorf("core: health window carries counter %q outside the closed seven", k)
		}
	}
	return nil
}

// Report renders the row for POST /v1/health. The collector name is the route name, so a
// provider cannot invent a coverage path the reporting layer does not know (§4.3).
func (h Health) Report(deviceID, version string) protocol.HealthReport {
	rep := protocol.NewHealthReport(deviceID, "", version, h.Since)
	rep.State = h.State
	rep.Detail = h.Detail
	if !h.LastSuccess.IsZero() {
		t := h.LastSuccess
		rep.LastSuccess = &t
	}
	for k, v := range h.Counters {
		rep.Counters[k] = v
	}
	return rep
}

func knownCounter(k protocol.Counter) bool {
	for _, c := range protocol.AllCounters {
		if c == k {
			return true
		}
	}
	return false
}

// CounterSet is §4.3's bounded counter set: a fixed, small, named set with a closed
// vocabulary, cumulative since process start plus a windowed delta, carried on the health
// channel only — never as events.
//
// A name outside the closed seven cannot be added: it increments `errors` and is recorded
// (bounded) for the report, because a provider inventing a counter name is exactly the
// high-cardinality leak the closed set exists to prevent.
type CounterSet struct {
	mu        sync.Mutex
	since     time.Time
	window    time.Time
	cum       map[protocol.Counter]uint64
	delta     map[protocol.Counter]uint64
	invalid   []string
	lastReset time.Time
}

// NewCounterSet starts a cumulative set and an open window at t.
func NewCounterSet(t time.Time) *CounterSet {
	c := &CounterSet{
		since:     t,
		window:    t,
		lastReset: t,
		cum:       map[protocol.Counter]uint64{},
		delta:     map[protocol.Counter]uint64{},
	}
	for _, k := range protocol.AllCounters {
		c.cum[k] = 0
		c.delta[k] = 0
	}
	return c
}

// Incr adds n to a counter. An unknown name never becomes a counter: it is refused and
// counted as an error, because the set is closed (A15).
func (c *CounterSet) Incr(k protocol.Counter, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !knownCounter(k) {
		c.cum[protocol.CounterErrors]++
		c.delta[protocol.CounterErrors]++
		if len(c.invalid) < 8 {
			c.invalid = append(c.invalid, string(k))
		}
		return
	}
	c.cum[k] += n
	c.delta[k] += n
}

// Add is Incr by one.
func (c *CounterSet) Add(k protocol.Counter) { c.Incr(k, 1) }

// Cumulative returns a copy of the cumulative counters.
func (c *CounterSet) Cumulative() map[protocol.Counter]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[protocol.Counter]uint64, len(c.cum))
	for k, v := range c.cum {
		out[k] = v
	}
	return out
}

// Window returns the windowed delta and the instant the window opened.
func (c *CounterSet) Window() (map[protocol.Counter]uint64, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[protocol.Counter]uint64, len(c.delta))
	for k, v := range c.delta {
		out[k] = v
	}
	return out, c.window
}

// RollWindow closes the current window and opens a new one. The cumulative totals are
// untouched: a roll is a reporting boundary, not a reset of history.
func (c *CounterSet) RollWindow(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.delta {
		c.delta[k] = 0
	}
	c.window = t
	c.lastReset = t
}

// InvalidNames returns the counter names that were refused, for diagnosis. It is bounded so
// a defective provider cannot turn the refusal path into a memory leak of its own.
func (c *CounterSet) InvalidNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.invalid))
	copy(out, c.invalid)
	return out
}

// Since returns the instant the cumulative counters started.
func (c *CounterSet) Since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.since
}

// Snapshot renders health from this counter set. Providers call exactly one of these so the
// counters and the state can never drift apart.
func (c *CounterSet) Snapshot(state protocol.CollectorState, detail protocol.Detail, since, lastSuccess time.Time) Health {
	return buildHealth(state, detail, since, lastSuccess, c)
}

// Provider is §4.1's common provider contract, one provider per coverage row.
//
// The four rules the contract encodes: Start is transactional (ready, or everything it took
// released — there is no half-started state, because a half-started proxy is worse than a
// stopped one); Stop never fails visibly (the error is logged and the provider reported
// tampered, since interference is evidence, and shutdown must not deadlock on it); Health
// never says healthy when the provider is not in the path; and no provider may fail into a
// state that reports success — state derives from a positive observation, never from the
// absence of errors (C25).
type Provider interface {
	Name() protocol.Route
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Health() Health
	ApplyPolicy(b policy.Bundle) error
}
