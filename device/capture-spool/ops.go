package spool

import (
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Peek returns up to n pending observations in sequence order, oldest first. It reads the
// payload back from the segment, so the bytes returned are exactly the bytes that were
// appended. It never changes state and never drops anything: the bound is
// enforced on write and nowhere else.
func (s *Spool) Peek(n int) ([]protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var out []protocol.Entry
	for _, seq := range s.order {
		m := s.entries[seq]
		if m == nil || m.state != protocol.SpoolPending {
			continue
		}
		e, err := s.readEntry(m)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out, nil
}

// readEntry reads one record's frame back and rebuilds the protocol record.
func (s *Spool) readEntry(m *entryMeta) (protocol.Entry, error) {
	seg := s.segmentByID(m.segID)
	if seg == nil {
		return protocol.Entry{}, &CorruptError{
			Reason: fmt.Sprintf("record %d is indexed in missing segment %d", m.seq, m.segID),
		}
	}
	f, err := seg.readHandle()
	if err != nil {
		return protocol.Entry{}, err
	}
	h, pt, _, err := readFrameAt(f, seg.path, m.off, s.aead)
	if err != nil {
		return protocol.Entry{}, err
	}
	if h.Type != frameData {
		return protocol.Entry{}, &CorruptError{Path: seg.path, Offset: m.off, Reason: "index points at a control frame"}
	}
	meta, payload, err := decodeDataFrame(pt)
	if err != nil {
		return protocol.Entry{}, &CorruptError{Path: seg.path, Offset: m.off, Reason: err.Error()}
	}
	if meta.Seq != m.seq {
		return protocol.Entry{}, &CorruptError{Path: seg.path, Offset: m.off, Reason: fmt.Sprintf("frame carries record %d, index says %d", meta.Seq, m.seq)}
	}
	return m.toEntry(payload), nil
}

// MarkInFlight hands pending observations to a delivery attempt. Attempts is incremented and
// the transition is durable before this returns, so a process killed mid-send leaves the
// records in flight on disk — and Open returns them to pending, because in-flight is not a
// delivery.
//
// Sequences that are unknown, already in flight, or already terminal are ignored: MarkInFlight
// is idempotent, and a caller retrying after a crash must not have to reconcile first.
func (s *Spool) MarkInFlight(seqs []uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWritable(); err != nil {
		return err
	}
	now := unixNano(s.cfg.Now())
	var batch []transition
	for _, seq := range seqs {
		m := s.entries[seq]
		if m == nil || m.state != protocol.SpoolPending {
			continue
		}
		batch = append(batch, transition{Op: opClaim, Seq: seq, Attempts: m.attempts + 1, At: now})
	}
	if len(batch) == 0 {
		return nil
	}
	_, err := s.appendControlChunked(batch)
	return err
}

// Settle records the terminal outcome of a delivery attempt. The three accepted states map
// exactly onto protocol.Outcome.SettleState, which is the single place that mapping lives:
//
//	delivered  the ingest API acknowledged the event; it is never sent again
//	rejected   a terminal rejection, with a reason code from the closed set
//	pending    a retryable failure: the record goes back to the queue with its last error
//
// A sequence whose record has already been reclaimed is a no-op, so a drain that settles a
// batch after a crash cannot fail on a record that is already gone.
func (s *Spool) Settle(seq uint64, state protocol.SpoolState, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWritable(); err != nil {
		return err
	}

	var t transition
	switch state {
	case protocol.SpoolDelivered:
		if reason != "" {
			return &EntryError{Seq: seq, Reason: fmt.Sprintf("a delivered record carries no reason code, got %q", reason)}
		}
		t = transition{Op: opSettle, Seq: seq, State: string(protocol.SpoolDelivered)}
	case protocol.SpoolRejected:
		if !protocol.ReasonCode(reason).Valid() {
			return &EntryError{Seq: seq, Reason: fmt.Sprintf("rejection reason %q is outside the closed set", reason)}
		}
		t = transition{Op: opSettle, Seq: seq, State: string(protocol.SpoolRejected), Reason: reason}
	case protocol.SpoolPending:
		t = transition{Op: opRelease, Seq: seq, LastError: reason}
	default:
		return &EntryError{Seq: seq, Reason: fmt.Sprintf("Settle accepts delivered, rejected or pending, got %q", state)}
	}

	m := s.entries[seq]
	switch {
	case m == nil:
		// Already reclaimed, or never existed. Settling twice is not an error.
		return nil
	case m.state == state:
		// Already settled, or already back in the queue.
		return nil
	case !indexable(m.state):
		// Evicted, or terminally rejected: a delivery outcome cannot resurrect it.
		return nil
	}
	t.At = unixNano(s.cfg.Now())
	_, err := s.appendControlChunked([]transition{t})
	return err
}

// Expire applies device-side retention: a record past its ExpiresAt is dropped whether or not
// it was delivered, because retention is a property of the observation (protocol.Entry.ExpiresAt)
// and not of the queue.
//
// Expiries are counted separately from overflow drops. They are different failures with
// different fixes, so ExtendedStats carries both.
func (s *Spool) Expire(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWritable(); err != nil {
		return 0, err
	}
	var batch []transition
	for _, seq := range s.order {
		m := s.entries[seq]
		if m == nil || !indexable(m.state) || m.expiresAt.IsZero() || m.expiresAt.After(now) {
			continue
		}
		batch = append(batch, transition{
			Op:    opExpire,
			Seq:   seq,
			Kind:  string(m.kind),
			Route: string(m.route),
			Cause: causeExpiry,
			At:    unixNano(now),
		})
	}
	if len(batch) == 0 {
		return 0, nil
	}
	n, err := s.appendControlChunked(batch)
	if err != nil {
		return n, err
	}
	s.reclaim()
	return n, nil
}

// appendControlChunked writes tombstones in chunks and returns how many were applied.
func (s *Spool) appendControlChunked(batch []transition) (int, error) {
	applied := 0
	for start := 0; start < len(batch); start += maxTransitionsPerFrame {
		end := start + maxTransitionsPerFrame
		if end > len(batch) {
			end = len(batch)
		}
		if err := s.appendControlLocked(controlRecord{Transitions: batch[start:end]}); err != nil {
			return applied, err
		}
		applied += end - start
	}
	return applied, nil
}

// --- accessors for the health report -----------------------------------------------------

// Depth is `spool_depth`: the undelivered records the device is holding.
func (s *Spool) Depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending + s.inFlight
}

// DroppedTotal is `spool_dropped_total`: a monotonic count of observations evicted by the
// bound, never silent, and never reset by the deletion that produced it.
func (s *Spool) DroppedTotal() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped, _, _, _ := s.totalsLocked()
	return dropped
}

// ExpiredTotal is the retention-expiry count, kept separate from DroppedTotal.
func (s *Spool) ExpiredTotal() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, expired, _, _ := s.totalsLocked()
	return expired
}

// Capacity reports the configured bound.
func (s *Spool) Capacity() Bounds {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Bounds
}

// DropAttribution is what was lost, by kind and route, so an operator sees "we lost 800
// prompt events from the proxy route" rather than a bare number.
type DropAttribution struct {
	Kind  string `json:"kind"`
	Route string `json:"route"`
	Count uint64 `json:"count"`
}

// DroppedBy attributes overflow drops.
func (s *Spool) DroppedBy() []DropAttribution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attributionLocked(s.foldedDrops, true)
}

// ExpiredBy attributes retention expiries.
func (s *Spool) ExpiredBy() []DropAttribution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attributionLocked(s.foldedExpiries, false)
}

func (s *Spool) attributionLocked(folded map[attrKey]uint64, drops bool) []DropAttribution {
	merged := map[attrKey]uint64{}
	mergeAttr(merged, folded)
	for _, seg := range s.segments {
		if seg.id <= s.persisted.SegmentWatermark {
			continue // already folded into the map above
		}
		if drops {
			mergeAttr(merged, seg.counters.drops)
		} else {
			mergeAttr(merged, seg.counters.expiries)
		}
	}
	out := make([]DropAttribution, 0, len(merged))
	for k, v := range merged {
		out = append(out, DropAttribution{Kind: k.kind, Route: k.route, Count: v})
	}
	sortAttribution(out)
	return out
}

// Stats is the health-report view. It never blocks, never does I/O, and never drops anything.
func (s *Spool) Stats() protocol.SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked()
}

func (s *Spool) statsLocked() protocol.SpoolStats {
	dropped, _, delivered, rejected := s.totalsLocked()
	return protocol.SpoolStats{
		Depth:           s.pending + s.inFlight,
		DroppedTotal:    dropped,
		RejectedTotal:   rejected,
		DeliveredTotal:  delivered,
		OldestSpooledAt: s.oldestPendingLocked(),
		BoundBytes:      s.cfg.Bounds.MaxBytes,
		UsedBytes:       s.diskBytes,
	}
}

// ExtendedStats is everything the health report and the coverage report need beyond
// protocol.SpoolStats: the separate expiry count, the drop attribution, and what recovery
// had to do.
//
// A windowed delta is deliberately not computed here: every counter is cumulative since process
// start, and the reporter derives deltas from successive reports; a delta computed here would be
// a second, divergent definition of the same number.
type ExtendedStats struct {
	protocol.SpoolStats

	Pending          int
	InFlight         int
	TerminalRetained int
	Segments         int

	// ExpiredTotal and OverBoundTotal are the two facts protocol.SpoolStats cannot carry:
	// a retention expiry is not an overflow drop, and an over-bound append is a
	// record that was accepted while the spool could not free space.
	ExpiredTotal   uint64
	OverBoundTotal uint64

	Bounds    Bounds
	Recovery  Recovery
	Watermark uint64

	DroppedBy []DropAttribution
	ExpiredBy []DropAttribution
}

// Extended returns the full statistics.
func (s *Spool) Extended() ExtendedStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped, expired, delivered, rejected := s.totalsLocked()
	st := ExtendedStats{
		SpoolStats: protocol.SpoolStats{
			Depth:           s.pending + s.inFlight,
			DroppedTotal:    dropped,
			RejectedTotal:   rejected,
			DeliveredTotal:  delivered,
			OldestSpooledAt: s.oldestPendingLocked(),
			BoundBytes:      s.cfg.Bounds.MaxBytes,
			UsedBytes:       s.diskBytes,
		},
		Pending:          s.pending,
		InFlight:         s.inFlight,
		TerminalRetained: s.terminal,
		Segments:         len(s.segments),
		ExpiredTotal:     expired,
		OverBoundTotal:   s.overBound,
		Bounds:           s.cfg.Bounds,
		Recovery:         s.recovery,
		Watermark:        s.persisted.SegmentWatermark,
	}
	st.DroppedBy = s.attributionLocked(s.foldedDrops, true)
	st.ExpiredBy = s.attributionLocked(s.foldedExpiries, false)
	return st
}

// totalsLocked is the monotonic totals: what has been folded into the counter file, plus the
// tombstones still on disk in segments that exist. A segment's tombstones are counted from
// exactly one of the two, which is what makes the totals exact across a crash.
//
// Segments at or below the watermark are excluded: their counters have already been folded
// in, and their file may still be present if the process died between the fold and the
// unlink. That window is small, reachable, and tested with a real killed process
// (TestCrashBetweenTheCounterFoldAndTheUnlink).
func (s *Spool) totalsLocked() (dropped, expired, delivered, rejected uint64) {
	dropped = s.persisted.DroppedTotal
	expired = s.persisted.ExpiredTotal
	delivered = s.persisted.DeliveredTotal
	rejected = s.persisted.RejectedTotal
	for _, seg := range s.segments {
		if seg.id <= s.persisted.SegmentWatermark {
			continue
		}
		dropped += seg.counters.dropped
		expired += seg.counters.expired
		delivered += seg.counters.delivered
		rejected += seg.counters.rejected
	}
	return
}

func (s *Spool) oldestPendingLocked() time.Time {
	for _, seq := range s.order {
		m := s.entries[seq]
		if m != nil && m.state == protocol.SpoolPending {
			return m.appendedAt
		}
	}
	return time.Time{}
}

// Close flushes, persists the counters and releases the writer lock. It is idempotent.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.active != nil && s.active.file != nil {
		note(s.active.file.Sync())
	}
	if s.poisoned == nil {
		note(s.saveCounters())
	}
	for _, seg := range s.segments {
		seg.close()
	}
	note(s.lock.release())
	return firstErr
}

// ForgetSegmentLocked drops a reclaimed segment's index rows. Only terminal records can be in
// a reclaimed segment — reclaim refuses a segment with any undelivered record — so this
// cannot forget something that still had to be delivered.
func (s *Spool) forgetSegment(seg *segment) {
	forgotten := 0
	for seq, m := range s.entries {
		if m.segID != seg.id {
			continue
		}
		if indexable(m.state) {
			continue
		}
		delete(s.entries, seq)
		s.terminal--
		forgotten++
	}
	if forgotten == 0 {
		return
	}
	kept := s.order[:0]
	for _, seq := range s.order {
		if _, ok := s.entries[seq]; ok {
			kept = append(kept, seq)
		}
	}
	s.order = kept
}

func sortAttribution(a []DropAttribution) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0; j-- {
			if a[j].Kind < a[j-1].Kind || (a[j].Kind == a[j-1].Kind && a[j].Route < a[j-1].Route) {
				a[j], a[j-1] = a[j-1], a[j]
				continue
			}
			break
		}
	}
}
