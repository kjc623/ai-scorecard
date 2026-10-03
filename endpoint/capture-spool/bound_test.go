package spool

import (
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// The bound test the acceptance criterion names: at the bound the oldest undelivered
// observations are evicted and the drop counter increases by exactly the number evicted.
func TestBoundEvictsOldestExactlyAndCounts(t *testing.T) {
	sp := openTest(t, t.TempDir(), func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 5}
	})
	appendN(t, sp, 20)

	st := sp.Extended()
	if st.DroppedTotal != 15 {
		t.Fatalf("DroppedTotal = %d, want exactly 15 evicted records", st.DroppedTotal)
	}
	if st.Depth != 5 {
		t.Fatalf("Depth = %d, want 5", st.Depth)
	}
	if st.Pending != 5 || st.InFlight != 0 {
		t.Fatalf("pending %d in-flight %d, want 5/0", st.Pending, st.InFlight)
	}
	if st.OverBoundTotal != 0 {
		t.Fatalf("OverBoundTotal = %d, want 0: eviction kept up with the bound", st.OverBoundTotal)
	}

	remaining, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(remaining) != 5 {
		t.Fatalf("Peek returned %d records, want 5", len(remaining))
	}
	for i, e := range remaining {
		want := uint64(15 + i)
		if e.Seq != want {
			t.Fatalf("surviving record %d has seq %d, want %d: drop-oldest must keep the newest", i, e.Seq, want)
		}
	}

	// Attribution: §12.2 wants "we lost N prompt events from the proxy route", not a bare
	// number.
	attr := sp.DroppedBy()
	if len(attr) != 1 {
		t.Fatalf("DroppedBy = %+v, want one (kind, route) row", attr)
	}
	if attr[0].Kind != string(protocol.KindPrompt) || attr[0].Route != string(protocol.RouteProxyTLS) || attr[0].Count != 15 {
		t.Fatalf("DroppedBy = %+v, want prompt/proxy.tls x15", attr[0])
	}
}

// An in-flight record is never evicted: §12.2 evicts the oldest *pending* rows first, and
// never in-flight or already-sent ones.
func TestBoundNeverEvictsInFlightRecords(t *testing.T) {
	sp := openTest(t, t.TempDir(), func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 4}
	})
	appended := appendN(t, sp, 4)
	if err := sp.MarkInFlight([]uint64{appended[0].Seq, appended[1].Seq}); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	// Two more appends: each must evict the oldest *pending* record (seq 2, then seq 3), and
	// the two in-flight records must survive both evictions.
	for i := 0; i < 2; i++ {
		if _, err := sp.Append(testEntry(100 + i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	st := sp.Extended()
	if st.InFlight != 2 {
		t.Fatalf("InFlight = %d, want 2: an in-flight record was evicted", st.InFlight)
	}
	if st.Pending != 2 {
		t.Fatalf("Pending = %d, want 2", st.Pending)
	}
	if st.DroppedTotal != 2 {
		t.Fatalf("DroppedTotal = %d, want 2", st.DroppedTotal)
	}
	// The in-flight records are still there and still settleable.
	if err := sp.Settle(appended[0].Seq, protocol.SpoolDelivered, ""); err != nil {
		t.Fatalf("Settle in-flight record: %v", err)
	}
	if st := sp.Extended(); st.DeliveredTotal != 1 {
		t.Fatalf("DeliveredTotal = %d, want 1: the in-flight record was evicted and could not be settled", st.DeliveredTotal)
	}
}

// When the bound cannot be met by evicting pending records — everything retained is in
// flight — the observation is still accepted and the overage is counted, rather than the new
// observation being discarded or an in-flight record being evicted (§12.2).
func TestOverBoundIsCountedNotSilent(t *testing.T) {
	sp := openTest(t, t.TempDir(), func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 2}
	})
	appended := appendN(t, sp, 2)
	if err := sp.MarkInFlight([]uint64{appended[0].Seq, appended[1].Seq}); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	// One append over the bound with nothing evictable: the new observation must be kept,
	// the overage counted, and neither in-flight record touched.
	fresh, err := sp.Append(testEntry(100))
	if err != nil {
		t.Fatalf("Append over the bound: %v", err)
	}

	st := sp.Extended()
	if st.DroppedTotal != 0 {
		t.Fatalf("DroppedTotal = %d, want 0: nothing was evictable", st.DroppedTotal)
	}
	if st.OverBoundTotal != 1 {
		t.Fatalf("OverBoundTotal = %d, want 1: an over-bound append must be visible", st.OverBoundTotal)
	}
	if st.Depth != 3 || st.InFlight != 2 || st.Pending != 1 {
		t.Fatalf("depth %d (in-flight %d, pending %d), want 3/2/1: the device must keep observing while it cannot store",
			st.Depth, st.InFlight, st.Pending)
	}
	kept, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(kept) != 1 || kept[0].Seq != fresh.Seq {
		t.Fatalf("the over-bound observation was not kept: %+v", kept)
	}
	// A subsequent append can evict it, and it does so as the oldest *pending* record.
	if _, err := sp.Append(testEntry(101)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if st := sp.Extended(); st.DroppedTotal != 1 || st.InFlight != 2 {
		t.Fatalf("after the next append: dropped %d in-flight %d, want 1/2", st.DroppedTotal, st.InFlight)
	}
}

// The counter is monotonic and survives both the deletion that produces it and a reopen.
func TestDropCounterIsMonotonicAcrossReopenAndReclamation(t *testing.T) {
	dir := t.TempDir()
	bounds := func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 3}
		c.SegmentBytes = 512 // force many small segments, so reclamation actually runs
	}
	sp := openTest(t, dir, bounds)
	appendN(t, sp, 200)

	before := sp.Extended()
	if before.DroppedTotal != 197 {
		t.Fatalf("DroppedTotal = %d, want 197", before.DroppedTotal)
	}
	if before.Watermark == 0 {
		t.Fatalf("no segment was folded into the counter file, so reclamation never ran (watermark 0, segments %d)", before.Segments)
	}
	if before.Segments > 8 {
		t.Fatalf("%d segments retained at a 3-record bound: reclamation is not freeing space", before.Segments)
	}
	if before.UsedBytes > 32<<10 {
		t.Fatalf("UsedBytes = %d at a 3-record bound: reclamation is not freeing space", before.UsedBytes)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sp2 := openTest(t, dir, bounds)
	after := sp2.Extended()
	if after.DroppedTotal != before.DroppedTotal {
		t.Fatalf("DroppedTotal changed across reopen: %d -> %d (a segment's tombstones were counted twice or lost)",
			before.DroppedTotal, after.DroppedTotal)
	}
	if after.Depth != before.Depth {
		t.Fatalf("Depth changed across reopen: %d -> %d", before.Depth, after.Depth)
	}
	// Attribution survives reclamation too.
	attr := sp2.DroppedBy()
	if len(attr) != 1 || attr[0].Count != 197 {
		t.Fatalf("DroppedBy after reopen = %+v, want prompt/proxy.tls x197", attr)
	}

	// Reopened at a 3-record bound with 3 retained, every append now evicts exactly one
	// record: 5 appends, 5 more drops, and the total only ever grows.
	appendN(t, sp2, 5)
	if got := sp2.DroppedTotal(); got != 202 {
		t.Fatalf("DroppedTotal = %d after 5 more appends at a full 3-record bound, want 202", got)
	}
	if got := sp2.Depth(); got != 3 {
		t.Fatalf("Depth = %d after 5 more appends, want 3", got)
	}
}

// The byte bound is enforced on write and frees space by reclaiming whole dead segments.
func TestByteBoundIsEnforcedOnWrite(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir, func(c *Config) {
		c.Bounds = Bounds{MaxBytes: 4096}
		c.SegmentBytes = 1024
	})
	appendN(t, sp, 300)

	st := sp.Extended()
	if st.DroppedTotal == 0 {
		t.Fatal("no drops at a 4 KB bound with 300 records: the byte bound is not enforced")
	}
	if st.Depth >= 300 {
		t.Fatalf("Depth = %d: nothing was evicted", st.Depth)
	}
	// The spool may exceed the byte bound by at most one segment (the active one) plus the
	// tombstones that were just written; the tombstones are accounted for in the projection.
	if limit := st.Bounds.MaxBytes + 1024 + 4096; st.UsedBytes > limit {
		t.Fatalf("UsedBytes = %d exceeds the byte bound %d plus one segment", st.UsedBytes, st.Bounds.MaxBytes)
	}
}
