package drain

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func TestBuildBatchRespectsMaxEvents(t *testing.T) {
	var entries []protocol.Entry
	for i := 0; i < protocol.MaxBatchEvents+20; i++ {
		entries = append(entries, testEnvelope(t, "evt"))
	}
	bb, err := buildBatch(entries, time.Now())
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	if len(bb.entries) != protocol.MaxBatchEvents {
		t.Fatalf("batch has %d events, want %d (the cap)", len(bb.entries), protocol.MaxBatchEvents)
	}
}

func TestBuildBatchRespectsDecompressedCap(t *testing.T) {
	// A few envelopes each just under the per-envelope cap should stop at the decompressed cap.
	// The payload is a valid JSON string, as every spooled envelope is.
	big := []byte(`"` + strings.Repeat("a", protocol.MaxEnvelopeBytes-100) + `"`)
	var entries []protocol.Entry
	for i := 0; i < 400; i++ {
		e := testEnvelope(t, "evt")
		e.Payload = append([]byte(nil), big...)
		e.SizeBytes = int64(len(big))
		entries = append(entries, e)
	}
	bb, err := buildBatch(entries, time.Now())
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	var total int64
	for _, e := range bb.entries {
		total += int64(len(e.Payload))
	}
	if total > protocol.MaxDecompressedBytes {
		t.Fatalf("decompressed batch is %d bytes, over the %d cap", total, protocol.MaxDecompressedBytes)
	}
	if len(bb.entries) >= len(entries) {
		t.Fatal("the cap did not trim the batch")
	}
}

func TestBuildBatchBodyIsGzipAndValid(t *testing.T) {
	bb, err := buildBatch([]protocol.Entry{testEnvelope(t, "evt-1"), testEnvelope(t, "evt-2")}, time.Now())
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	if len(bb.body) > protocol.MaxRequestBodyBytes {
		t.Fatalf("body is %d bytes, over the %d compressed cap", len(bb.body), protocol.MaxRequestBodyBytes)
	}
	zr, err := gzip.NewReader(bytes.NewReader(bb.body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	var batch protocol.EventBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := batch.Validate(); err != nil {
		t.Fatalf("batch does not validate: %v", err)
	}
	if batch.EventCount != 2 || len(batch.Events) != 2 {
		t.Fatalf("batch count %d events %d, want 2/2", batch.EventCount, len(batch.Events))
	}
	if bb.id != batch.BatchID {
		t.Fatal("the returned batch id does not match the encoded batch_id")
	}
}

func TestSettleMapping(t *testing.T) {
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-a")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.Append(testEnvelope(t, "evt-b")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	bb, err := buildBatch(storePeekAll(t, store), time.Now())
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	if err := store.MarkInFlight(bb.seqs); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	d := &Drainer{}
	var res Result
	resp := &protocol.EventBatchResponse{
		Results: []protocol.EventResult{
			{EventID: "evt-a", Outcome: protocol.OutcomeAccepted},
			{EventID: "evt-b", Outcome: protocol.OutcomeRejected, Reason: protocol.ReasonUnknownKind},
		},
	}
	d.settle(store, bb, resp, &res)
	if res.Delivered != 1 || res.Rejected != 1 {
		t.Fatalf("result = %+v, want 1 delivered, 1 rejected", res)
	}
	if store.state(1) != protocol.SpoolDelivered {
		t.Fatalf("entry 1 = %s, want delivered", store.state(1))
	}
	if store.state(2) != protocol.SpoolRejected || store.reason(2) != string(protocol.ReasonUnknownKind) {
		t.Fatalf("entry 2 = %s/%q, want rejected/unknown_kind", store.state(2), store.reason(2))
	}
}

func storePeekAll(t *testing.T, store protocol.Store) []protocol.Entry {
	t.Helper()
	entries, err := store.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	return entries
}
