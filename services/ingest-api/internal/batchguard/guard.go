// Package batchguard implements §5.3's `duplicate_batch` rule: a replay of the same batch_id
// returns `duplicate_batch` at batch level, and the device then re-sends with a fresh batch_id.
//
// §6 calls this "the only check-then-act in the design" and states that a race there is benign:
// both racers fall through to the per-event (tenant_id, event_id) constraint and both events
// report as duplicates, which is the correct answer anyway. That is what makes an in-process guard
// acceptable for a single-instance deployment.
//
// LIMITATION, stated rather than discovered: the window is per process. In a scale-out deployment
// with more than one ingest instance, a replayed batch_id that lands on a different instance is not
// detected, and the device receives per-event `duplicate` outcomes instead of the batch-level
// signal. Both are correct answers about the data (§6 says so), so this degrades the diagnosis, not
// the accounting. The guard would move to a shared store before multi-instance is relied on.
package batchguard

import (
	"sync"
	"time"
)

// Guard remembers batch ids for a replay window.
type Guard struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]time.Time
	now    func() time.Time
}

// New builds a guard. A non-positive window disables the guard, visibly.
func New(window time.Duration) *Guard {
	return &Guard{window: window, seen: map[string]time.Time{}, now: time.Now}
}

// SetNow overrides the clock for tests.
func (g *Guard) SetNow(now func() time.Time) { g.now = now }

// Check reports whether this (tenant, device, batch_id) was accepted inside the window. It does not
// record anything: the batch is only marked by Mark, after the write committed.
//
// The split matters. Marking a batch before it is written would answer `duplicate_batch` to a device
// that sent a malformed batch, fixed it and re-sent it under the same batch_id -- a batch that never
// committed would look like a replay, and the documented recovery in §5.3 ("re-send with a fresh
// batch_id") would be the only way out of a defect the device had already fixed.
//
// The cost of checking before the commit is a race, and §6 already priced it: "a race there is
// benign -- both racers fall through to the event-key constraint and both events report as
// duplicates, which is the correct answer anyway."
func (g *Guard) Check(tenantID, deviceID, batchID string) bool {
	if g.window <= 0 {
		return false
	}
	key := tenantID + "|" + deviceID + "|" + batchID
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	if at, ok := g.seen[key]; ok && now.Sub(at) <= g.window {
		return true
	}
	return false
}

// Mark records a batch as accepted. It is called after the batch has committed, so a batch that was
// refused for its shape can be re-sent unchanged.
func (g *Guard) Mark(tenantID, deviceID, batchID string) {
	if g.window <= 0 {
		return
	}
	key := tenantID + "|" + deviceID + "|" + batchID
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	// Opportunistic sweep: the map is bounded by the number of batches in the window, which is
	// 2 requests/s at the sizing model's worst case, so this is not a hot path.
	if len(g.seen) > 1024 {
		for k, at := range g.seen {
			if now.Sub(at) > g.window {
				delete(g.seen, k)
			}
		}
	}
	g.seen[key] = now
}
