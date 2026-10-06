package drain

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// ---------------------------------------------------------------------------------------------
// memStore is a minimal in-memory protocol.Store for the drain loop, keeping only the state
// transitions the drainer performs.

type memEntry struct {
	entry    protocol.Entry
	state    protocol.SpoolState
	attempts int
	reason   string
}

type memStore struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]*memEntry
	order   []uint64
}

func newMemStore() *memStore {
	return &memStore{entries: map[uint64]*memEntry{}}
}

func (s *memStore) Append(e protocol.Entry) (protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	e.Seq = s.next
	e.State = protocol.SpoolPending
	s.entries[s.next] = &memEntry{entry: e, state: protocol.SpoolPending}
	s.order = append(s.order, s.next)
	return e, nil
}

func (s *memStore) Peek(n int) ([]protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.Entry
	for _, seq := range s.order {
		m := s.entries[seq]
		if m.state == protocol.SpoolPending {
			out = append(out, m.entry)
			if n > 0 && len(out) >= n {
				break
			}
		}
	}
	return out, nil
}

func (s *memStore) MarkInFlight(seqs []uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seq := range seqs {
		if m := s.entries[seq]; m != nil && m.state == protocol.SpoolPending {
			m.state = protocol.SpoolInFlight
			m.attempts++
		}
	}
	return nil
}

func (s *memStore) Settle(seq uint64, state protocol.SpoolState, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.entries[seq]
	if m == nil {
		return nil
	}
	switch state {
	case protocol.SpoolDelivered, protocol.SpoolRejected:
		if m.state == protocol.SpoolPending || m.state == protocol.SpoolInFlight {
			m.state = state
			m.reason = reason
		}
	case protocol.SpoolPending:
		if m.state == protocol.SpoolInFlight {
			m.state = protocol.SpoolPending
			m.reason = reason
		}
	}
	return nil
}

func (s *memStore) Stats() protocol.SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	depth := 0
	for _, m := range s.entries {
		if m.state == protocol.SpoolPending || m.state == protocol.SpoolInFlight {
			depth++
		}
	}
	return protocol.SpoolStats{Depth: depth}
}

func (s *memStore) Close() error { return nil }

func (s *memStore) state(seq uint64) protocol.SpoolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.entries[seq]; m != nil {
		return m.state
	}
	return ""
}

func (s *memStore) reason(seq uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.entries[seq]; m != nil {
		return m.reason
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// Shared helpers.

func testEnvelope(t *testing.T, eventID string) protocol.Entry {
	t.Helper()
	env := map[string]any{
		"schema_version": "1.0", "event_id": eventID, "tenant_id": testTenant,
		"device_id": testDevice, "user_ref": "u", "tool_fingerprint": "t",
		"direction": "egress", "kind": "prompt", "occurred_at": time.Now().UTC().Format(time.RFC3339),
		"monotonic_offset_ms": 1, "source": "ext.web_request", "collection_mode": "m1",
		"size_bytes": 10, "policy_decision": map[string]any{"rule_id": "r", "action": "logged", "decided_locally": true},
		"dedup_key": "sha256:abc", "content_digest": "sha256:abc", "labels": []any{},
		"classifier_version": "v1", "confidence": "high",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return protocol.Entry{
		Kind:      protocol.KindPrompt,
		Route:     protocol.RouteExtWebRequest,
		DedupKey:  "sha256:abc",
		Payload:   raw,
		SizeBytes: int64(len(raw)),
	}
}

// acceptedResponseForBatch returns a 200 events response accepting every event in the batch.
func acceptedResponseForBatch(t *testing.T, batch protocol.EventBatch) []byte {
	t.Helper()
	results := make([]protocol.EventResult, len(batch.Events))
	for i, ev := range batch.Events {
		var env struct {
			EventID string `json:"event_id"`
		}
		_ = json.Unmarshal(ev, &env)
		results[i] = protocol.EventResult{EventID: env.EventID, Outcome: protocol.OutcomeAccepted}
	}
	resp := protocol.EventBatchResponse{
		SchemaVersion: "1.0",
		BatchID:       batch.BatchID,
		ReceivedAt:    time.Now().UTC(),
		ServerTime:    time.Now().UTC(),
		Counts:        protocol.BatchCounts{Accepted: len(results)},
		Results:       results,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return out
}

// acceptedResponse reads the request batch and answers 200 accepting every event.
func acceptedResponse(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body := gunzipBody(t, r)
	var batch protocol.EventBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	return acceptedResponseForBatch(t, batch)
}

func gunzipBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		defer gz.Close()
		rd = gz
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// eventsHandler serves /v1/events with fn and records the client certificate presented.
func eventsHandler(e *fakeEdge, fn http.HandlerFunc) func() []string {
	var mu sync.Mutex
	presented := []string{}
	e.mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			presented = append(presented, r.TLS.PeerCertificates[0].Subject.CommonName)
		} else {
			presented = append(presented, "")
		}
		mu.Unlock()
		fn(w, r)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), presented...)
	}
}

func storeOf(s *memStore) StoreFunc { return func() (protocol.Store, error) { return s, nil } }

func TestDrainDeliversOverMutualTLSAndSettles(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	for _, id := range []string{"evt-a", "evt-b", "evt-c"} {
		if _, err := store.Append(testEnvelope(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	presented := eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, acceptedResponse(t, r))
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Delivered != 3 {
		t.Fatalf("Delivered = %d, want 3", res.Delivered)
	}
	for i := 1; i <= 3; i++ {
		if store.state(uint64(i)) != protocol.SpoolDelivered {
			t.Fatalf("entry %d state = %s, want delivered", i, store.state(uint64(i)))
		}
	}
	if got := presented(); len(got) == 0 || got[0] != testDevice {
		t.Fatalf("client certificates presented = %v, want the device leaf", got)
	}
	if st := d.Status(); st.State != protocol.StateHealthy {
		t.Fatalf("status = %+v, want healthy after a delivery", st)
	}
}

func TestDrainSettlesATerminalRejection(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-x")); err != nil {
		t.Fatal(err)
	}
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		var batch protocol.EventBatch
		_ = json.Unmarshal(gunzipBody(t, r), &batch)
		raw, _ := json.Marshal(protocol.EventBatchResponse{
			SchemaVersion: "1.0", BatchID: batch.BatchID, ReceivedAt: time.Now().UTC(), ServerTime: time.Now().UTC(),
			Counts:  protocol.BatchCounts{Rejected: 1},
			Results: []protocol.EventResult{{EventID: "evt-x", Outcome: protocol.OutcomeRejected, Reason: protocol.ReasonModeViolation}},
		})
		writeJSON(t, w, http.StatusOK, raw)
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Rejected != 1 || store.state(1) != protocol.SpoolRejected || store.reason(1) != string(protocol.ReasonModeViolation) {
		t.Fatalf("result %+v, entry %s/%q; want one rejected with mode_violation", res, store.state(1), store.reason(1))
	}
}

func TestDrainRetriesAnUnavailableEdge(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-r")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	attempts := 0
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			writeJSON(t, w, http.StatusServiceUnavailable, []byte(`{"error":{"code":"schema_violation"}}`))
			return
		}
		writeJSON(t, w, http.StatusOK, acceptedResponse(t, r))
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if res.Delivered != 1 || attempts != 2 {
		t.Fatalf("delivered %d after %d attempts, want 1 after 2", res.Delivered, attempts)
	}
}

func TestDrainRetainsTheSpoolWhenTheEdgeRefusesTheBatch(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-t")); err != nil {
		t.Fatal(err)
	}
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, []byte(`{"error":{"code":"revoked_device"}}`))
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	if _, err := d.Drain(context.Background(), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if store.state(1) != protocol.SpoolPending {
		t.Fatalf("entry state = %s, want pending (retained)", store.state(1))
	}
	if st := d.Status(); st.State != protocol.StateDegraded || st.Detail != protocol.DetailUpstreamFailure {
		t.Fatalf("status = %+v, want degraded/upstream_failure", st)
	}
}

// A 200 whose body is not a valid events response degrades the drain and retains the record.
func TestDrainMalformedSuccessBodyDegrades(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-mal")); err != nil {
		t.Fatal(err)
	}
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, []byte("this is not json"))
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	if _, err := d.Drain(context.Background(), time.Now().Add(250*time.Millisecond)); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if st := d.Status(); st.State != protocol.StateDegraded || st.Detail != protocol.DetailUpstreamFailure {
		t.Fatalf("status = %+v, want degraded/upstream_failure", st)
	}
	if store.state(1) != protocol.SpoolPending {
		t.Fatalf("entry state = %s, want pending (retained)", store.state(1))
	}
}

func TestDrainOfAnEmptySpoolSendsNothing(t *testing.T) {
	e := newFakeEdge(t)
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the drain called the edge with an empty spool")
	})
	d := newTestDrainer(t, e, storeOf(newMemStore()), e.issued(24*time.Hour))
	res, err := d.Drain(context.Background(), time.Now().Add(time.Second))
	if err != nil || !res.Empty || res.Delivered != 0 {
		t.Fatalf("empty spool: %+v, %v", res, err)
	}
}

func TestDrainRejectsAnOversizeEnvelopeWithoutSendingIt(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	big := testEnvelope(t, "evt-big")
	big.Payload = bytes.Repeat([]byte("x"), protocol.MaxEnvelopeBytes+1)
	if _, err := store.Append(big); err != nil {
		t.Fatal(err)
	}
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an oversize envelope was sent")
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil || res.Rejected != 1 || store.state(1) != protocol.SpoolRejected || store.reason(1) != string(protocol.ReasonOversize) {
		t.Fatalf("result %+v err %v, entry %s/%q; want rejected oversize", res, err, store.state(1), store.reason(1))
	}
}

// A record minted under another identity than the credential is settled with a visible reason and
// never sent: the write path would reject its tenant or device.
func TestDrainQuarantinesARecordMintedUnderAnotherIdentity(t *testing.T) {
	e := newFakeEdge(t)
	store := newMemStore()
	stale := testEnvelope(t, "evt-stale")
	stale.Payload = bytes.Replace(stale.Payload, []byte(testDevice), []byte("someone-else"), 1)
	if _, err := store.Append(stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(testEnvelope(t, "evt-ok")); err != nil {
		t.Fatal(err)
	}
	eventsHandler(e, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, acceptedResponse(t, r))
	})
	d := newTestDrainer(t, e, storeOf(store), e.issued(24*time.Hour))
	if _, err := d.Drain(context.Background(), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if store.state(1) != protocol.SpoolRejected || store.reason(1) != reasonStaleIdentity {
		t.Fatalf("stale entry = %s/%q, want rejected/%s", store.state(1), store.reason(1), reasonStaleIdentity)
	}
	if store.state(2) != protocol.SpoolDelivered {
		t.Fatalf("current entry = %s, want delivered", store.state(2))
	}
}
