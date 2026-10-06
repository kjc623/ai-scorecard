package spool

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func TestAppendAssignsSequenceAndPeekReturnsIt(t *testing.T) {
	sp := openTest(t, t.TempDir())

	a, err := sp.Append(testEntry(0))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if a.Seq != 0 {
		t.Fatalf("first append got seq %d, want 0", a.Seq)
	}
	if a.State != protocol.SpoolPending {
		t.Fatalf("appended entry state is %q, want pending", a.State)
	}

	got, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Peek returned %d entries, want 1", len(got))
	}
	if got[0].Seq != a.Seq || got[0].DedupKey != a.DedupKey {
		t.Fatalf("Peek returned %+v, want the appended record", got[0])
	}
	if sp.Depth() != 1 {
		t.Fatalf("depth %d, want 1", sp.Depth())
	}
}

// The payload is opaque and immutable: Peek must hand back exactly the bytes Append was
// given, before and after a reopen. The payload here is not JSON at all — the
// spool has no decoder for it, and a payload it could not parse must still round-trip.
func TestPayloadIsStoredAndReturnedByteForByte(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x00, 0xff, 0xfe, 0x01, 0x80, 0x7f, 0x00, 0xc3, 0x28, 0x0a}
	e := testEntry(7)
	e.Payload = payload

	sp := openTest(t, dir)
	appended, err := sp.Append(e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	peeked, err := sp.Peek(1)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !bytes.Equal(peeked[0].Payload, payload) {
		t.Fatalf("Peek payload %v, want %v", peeked[0].Payload, payload)
	}
	if appended.SizeBytes != int64(len(payload)) {
		t.Fatalf("SizeBytes %d, want %d", appended.SizeBytes, len(payload))
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sp2 := openTest(t, dir)
	peeked2, err := sp2.Peek(1)
	if err != nil {
		t.Fatalf("Peek after reopen: %v", err)
	}
	if !bytes.Equal(peeked2[0].Payload, payload) {
		t.Fatalf("payload after reopen %v, want %v", peeked2[0].Payload, payload)
	}
	if peeked2[0].Seq != appended.Seq {
		t.Fatalf("seq after reopen %d, want %d", peeked2[0].Seq, appended.Seq)
	}
}

func TestAppendRefusesDefectsAtTheDoor(t *testing.T) {
	sp := openTest(t, t.TempDir())

	cases := []struct {
		name   string
		mutate func(*protocol.Entry)
	}{
		{"no payload", func(e *protocol.Entry) { e.Payload = nil }},
		{"kind outside the registry", func(e *protocol.Entry) { e.Kind = protocol.Kind("process_telemetry") }},
		{"mode outside the closed set", func(e *protocol.Entry) { e.CollectionMode = protocol.CollectionMode("m9") }},
		{"no dedup key", func(e *protocol.Entry) { e.DedupKey = "" }},
		{"already in flight", func(e *protocol.Entry) { e.State = protocol.SpoolInFlight }},
		{"already delivered", func(e *protocol.Entry) { e.State = protocol.SpoolDelivered }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := testEntry(0)
			tc.mutate(&e)
			if _, err := sp.Append(e); err == nil {
				t.Fatalf("Append accepted %s", tc.name)
			} else {
				var ee *EntryError
				if !errors.As(err, &ee) {
					t.Fatalf("error is %T (%v), want *EntryError", err, err)
				}
			}
		})
	}
	if sp.Depth() != 0 {
		t.Fatalf("a refused entry changed the depth: %d", sp.Depth())
	}
}

func TestEmptyStateDefaultsToPending(t *testing.T) {
	sp := openTest(t, t.TempDir())
	e := testEntry(0)
	e.State = ""
	got, err := sp.Append(e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got.State != protocol.SpoolPending {
		t.Fatalf("state %q, want pending", got.State)
	}
}

func TestMarkInFlightIncrementsAttemptsAndIsIdempotent(t *testing.T) {
	sp := openTest(t, t.TempDir())
	appended := appendN(t, sp, 2)
	seqs := []uint64{appended[0].Seq, appended[1].Seq}

	if err := sp.MarkInFlight(seqs); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	if got := sp.Depth(); got != 2 {
		t.Fatalf("depth after MarkInFlight %d, want 2 (in-flight is still undelivered)", got)
	}
	if got, _ := sp.Peek(0); len(got) != 0 {
		t.Fatalf("Peek returned %d records after MarkInFlight, want 0", len(got))
	}

	// A second call must not double-count an attempt.
	if err := sp.MarkInFlight(seqs); err != nil {
		t.Fatalf("MarkInFlight (second): %v", err)
	}
	if err := sp.Settle(seqs[0], protocol.SpoolPending, "transport reset"); err != nil {
		t.Fatalf("Settle to pending: %v", err)
	}
	got, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Peek returned %d records, want 1 released record", len(got))
	}
	if got[0].Attempts != 1 {
		t.Fatalf("attempts %d, want 1", got[0].Attempts)
	}
	if got[0].LastError != "transport reset" {
		t.Fatalf("last error %q, want the recorded failure", got[0].LastError)
	}
}

// The delivery outcome -> spool state mapping lives in protocol.Outcome.SettleState. This
// test drives the spool through that mapping rather than re-deriving it.
func TestSettleFollowsProtocolOutcomeMapping(t *testing.T) {
	cases := []struct {
		name      string
		outcome   protocol.Outcome
		reason    protocol.ReasonCode
		wantState protocol.SpoolState
		wantRetry bool
	}{
		{"accepted", protocol.OutcomeAccepted, "", protocol.SpoolDelivered, false},
		{"duplicate", protocol.OutcomeDuplicate, "", protocol.SpoolDelivered, false},
		{"rejected", protocol.OutcomeRejected, protocol.ReasonSchemaViolation, protocol.SpoolRejected, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := openTest(t, t.TempDir())
			e, err := sp.Append(testEntry(0))
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := sp.MarkInFlight([]uint64{e.Seq}); err != nil {
				t.Fatalf("MarkInFlight: %v", err)
			}
			state, terminal := tc.outcome.SettleState()
			if state != tc.wantState || terminal == tc.wantRetry {
				t.Fatalf("Outcome.SettleState = (%q, %v)", state, terminal)
			}
			if err := sp.Settle(e.Seq, state, string(tc.reason)); err != nil {
				t.Fatalf("Settle(%q): %v", state, err)
			}
			st := sp.Extended()
			switch tc.wantState {
			case protocol.SpoolDelivered:
				if st.Depth != 0 || st.DeliveredTotal != 1 {
					t.Fatalf("want delivered depth 0 and delivered total 1, got depth %d total %d", st.Depth, st.DeliveredTotal)
				}
			case protocol.SpoolRejected:
				if st.Depth != 0 || st.RejectedTotal != 1 {
					t.Fatalf("want rejected depth 0 and rejected total 1, got depth %d total %d", st.Depth, st.RejectedTotal)
				}
			case protocol.SpoolPending:
				if st.Depth != 1 || st.Pending != 1 {
					t.Fatalf("want the record back in the queue, got depth %d pending %d", st.Depth, st.Pending)
				}
			}
		})
	}
}

func TestSettleRejectedRequiresAClosedReasonCode(t *testing.T) {
	sp := openTest(t, t.TempDir())
	e, err := sp.Append(testEntry(0))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sp.Settle(e.Seq, protocol.SpoolRejected, "because_i_said_so"); err == nil {
		t.Fatal("Settle accepted a reason outside the closed set")
	}
	if err := sp.Settle(e.Seq, protocol.SpoolRejected, string(protocol.ReasonOversize)); err != nil {
		t.Fatalf("Settle with a closed reason code: %v", err)
	}
	if err := sp.Settle(e.Seq, protocol.SpoolInFlight, ""); err == nil {
		t.Fatal("Settle accepted in_flight as a terminal state")
	}
	if err := sp.Settle(e.Seq, protocol.SpoolDropped, ""); err == nil {
		t.Fatal("Settle accepted dropped: eviction is the spool's decision, not the caller's")
	}
}

func TestReopenKeepsSequenceOrderAndDepth(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appended := appendN(t, sp, 5)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sp2 := openTest(t, dir)
	got, err := sp2.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("Peek returned %d, want 5", len(got))
	}
	for i, e := range got {
		if e.Seq != appended[i].Seq {
			t.Fatalf("record %d has seq %d, want %d", i, e.Seq, appended[i].Seq)
		}
	}
	if sp2.Depth() != 5 {
		t.Fatalf("depth %d, want 5", sp2.Depth())
	}
	// A new append after recovery continues the sequence rather than reusing it.
	next, err := sp2.Append(testEntry(99))
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if next.Seq != 5 {
		t.Fatalf("sequence after reopen is %d, want 5", next.Seq)
	}
}

// Retention expiry is counted separately from an overflow drop: different failures, different
// fixes.
func TestExpireIsCountedSeparatelyFromDrops(t *testing.T) {
	sp := openTest(t, t.TempDir())
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	fresh := testEntry(0)
	fresh.ExpiresAt = now.Add(time.Hour)
	stale := testEntry(1)
	stale.ExpiresAt = now.Add(-time.Minute)
	stale2 := testEntry(2)
	stale2.ExpiresAt = now.Add(-time.Hour)

	for _, e := range []protocol.Entry{fresh, stale, stale2} {
		if _, err := sp.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	n, err := sp.Expire(now)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 2 {
		t.Fatalf("Expire removed %d records, want 2", n)
	}
	st := sp.Extended()
	if st.ExpiredTotal != 2 {
		t.Fatalf("ExpiredTotal %d, want 2", st.ExpiredTotal)
	}
	if st.DroppedTotal != 0 {
		t.Fatalf("DroppedTotal %d, want 0: an expiry is not an overflow drop", st.DroppedTotal)
	}
	if st.Depth != 1 {
		t.Fatalf("depth %d, want 1", st.Depth)
	}
	remaining, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Seq != fresh.Seq {
		t.Fatalf("expiry removed the wrong records: %+v", remaining)
	}
	if len(sp.ExpiredBy()) != 1 || sp.ExpiredBy()[0].Count != 2 {
		t.Fatalf("expiry attribution = %+v, want one row of 2", sp.ExpiredBy())
	}
}

// Append-only: a delivered observation is never rewritten. Every byte that was on disk
// before the delivery is still on disk, at the same offset, afterwards.
func TestDeliveredRecordsAreNeverRewritten(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appended := appendN(t, sp, 3)
	segPath := segmentFiles(t, dir)[0]
	before := mustRead(t, segPath)
	beforeOffsets := frameOffsets(t, before)

	seqs := []uint64{appended[0].Seq, appended[1].Seq, appended[2].Seq}
	if err := sp.MarkInFlight(seqs); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	for _, seq := range seqs {
		if err := sp.Settle(seq, protocol.SpoolDelivered, ""); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}

	after := mustRead(t, segPath)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("delivery rewrote bytes that were already in the segment: the log is not append-only")
	}
	// The three observation frames are byte-identical, at the same offsets.
	for _, off := range beforeOffsets {
		bodyLen := int(binary.LittleEndian.Uint32(before[off+16 : off+20]))
		end := off + frameHeaderSize + bodyLen + frameTrailerSize
		if !bytes.Equal(before[off:end], after[off:end]) {
			t.Fatalf("frame at offset %d was rewritten by delivery", off)
		}
	}
	if st := sp.Extended(); st.Depth != 0 || st.DeliveredTotal != 3 {
		t.Fatalf("depth %d delivered %d, want 0/3", st.Depth, st.DeliveredTotal)
	}

	// And it is still true after a reopen.
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp2 := openTest(t, dir)
	if !bytes.Equal(mustRead(t, segPath), after) {
		t.Fatal("reopening rewrote the segment")
	}
	if st := sp2.Extended(); st.DeliveredTotal != 3 || st.Depth != 0 {
		t.Fatalf("after reopen: delivered %d depth %d, want 3/0", st.DeliveredTotal, st.Depth)
	}
}

// A drain loop driven through protocol.Store exactly as capture-core will drive it. The
// caller here holds the interface, not the concrete type, which is what makes a
// SQLite-backed implementation a drop-in replacement later.
func TestDrainCycleThroughTheStoreInterface(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appendN(t, sp, 5)

	var st protocol.Store = sp
	batch, err := st.Peek(500)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(batch) != 5 {
		t.Fatalf("Peek returned %d, want 5", len(batch))
	}
	seqs := make([]uint64, 0, len(batch))
	for _, e := range batch {
		seqs = append(seqs, e.Seq)
	}
	if err := st.MarkInFlight(seqs); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	// Three accepted, one duplicate, two terminally rejected: the outcome-to-state mapping
	// is protocol.Outcome.SettleState, not a second opinion held here.
	outcomes := []struct {
		outcome protocol.Outcome
		reason  protocol.ReasonCode
	}{
		{protocol.OutcomeAccepted, ""},
		{protocol.OutcomeDuplicate, ""},
		{protocol.OutcomeAccepted, ""},
		{protocol.OutcomeRejected, protocol.ReasonSchemaViolation},
		{protocol.OutcomeRejected, protocol.ReasonModeViolation},
	}
	for i, o := range outcomes {
		state, _ := o.outcome.SettleState()
		if err := st.Settle(seqs[i], state, string(o.reason)); err != nil {
			t.Fatalf("Settle(%s): %v", o.outcome, err)
		}
	}
	stats := st.Stats()
	if stats.Depth != 0 {
		t.Fatalf("Depth = %d after a full drain, want 0", stats.Depth)
	}
	if stats.DeliveredTotal != 3 || stats.RejectedTotal != 2 {
		t.Fatalf("delivered %d rejected %d, want 3/2", stats.DeliveredTotal, stats.RejectedTotal)
	}
	if stats.DroppedTotal != 0 {
		t.Fatalf("DroppedTotal = %d, want 0", stats.DroppedTotal)
	}
}

func TestStatsExposeDepthAndDropCounter(t *testing.T) {
	sp := openTest(t, t.TempDir())
	appendN(t, sp, 3)
	st := sp.Stats()
	if st.Depth != 3 {
		t.Fatalf("SpoolStats.Depth = %d, want 3", st.Depth)
	}
	if st.DroppedTotal != 0 {
		t.Fatalf("SpoolStats.DroppedTotal = %d, want 0", st.DroppedTotal)
	}
	if st.BoundBytes != DefaultBounds().MaxBytes {
		t.Fatalf("BoundBytes = %d, want the default %d", st.BoundBytes, DefaultBounds().MaxBytes)
	}
	if st.UsedBytes <= 0 {
		t.Fatalf("UsedBytes = %d, want the on-disk footprint", st.UsedBytes)
	}
	if st.OldestSpooledAt.IsZero() {
		t.Fatal("OldestSpooledAt is zero with three pending records")
	}
}

func TestCloseIsIdempotentAndOperationsAfterCloseFail(t *testing.T) {
	sp := openTest(t, t.TempDir())
	if _, err := sp.Append(testEntry(0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := sp.Append(testEntry(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after close: %v, want ErrClosed", err)
	}
	if _, err := sp.Peek(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Peek after close: %v, want ErrClosed", err)
	}
}
