package drain

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
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
		"schema_version": "1.0", "event_id": eventID, "tenant_id": "tenant-1",
		"device_id": "device-1", "user_ref": "u", "tool_fingerprint": "t",
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

// newTestDrainer wires a drainer to an httptest peer over plain HTTP (loopback), so the drain-loop
// logic is tested without the TLS handshake; the TLS path is exercised by the selftest.
func newTestDrainer(t *testing.T, store StoreFunc, handler http.HandlerFunc) (*Drainer, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := &Drainer{
		cfg: Config{
			Endpoint:      srv.URL,
			AuthMode:      protocol.AuthModeDPoP,
			TenantID:      "tenant-1",
			DeviceID:      "device-1",
			AgentVersion:  "test",
			BackoffBase:   time.Millisecond,
			BackoffCap:    50 * time.Millisecond,
			DrainInterval: time.Millisecond,
		},
		store:   store,
		creds:   nil,
		client:  &client{base: strings.TrimRight(srv.URL, "/"), caPool: x509.NewCertPool(), http: srv.Client()},
		log:     nopLogger{},
		clock:   time.Now,
		backoff: Backoff{Base: time.Millisecond, Cap: 50 * time.Millisecond},
		state:   protocol.StateAbsent,
		stopCh:  make(chan struct{}),
	}
	// Pre-seed a credential so Drain does not attempt enrolment.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	c := &credential.Credential{
		Mode:       protocol.AuthModeDPoP,
		DeviceID:   "device-1",
		TenantID:   "tenant-1",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		JWK:        &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
	}
	if err := d.setCredential(c); err != nil {
		t.Fatalf("setCredential: %v", err)
	}
	// Pre-seed a valid token so the drain loop does not fetch one from the (events-only) peer.
	d.token = "test-token"
	d.tokenExp = time.Now().Add(time.Hour)
	return d, srv
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

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------------------------
// The drain loop.

func TestDrainDeliversAndSettles(t *testing.T) {
	store := newMemStore()
	for i := 0; i < 3; i++ {
		if _, err := store.Append(testEnvelope(t, "evt-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, acceptedResponse(t, r))
	})
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
}

func TestDrainRejectsTerminalReason(t *testing.T) {
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		body := gunzipBody(t, r)
		var batch protocol.EventBatch
		_ = json.Unmarshal(body, &batch)
		results := []protocol.EventResult{{EventID: "evt-x", Outcome: protocol.OutcomeRejected, Reason: protocol.ReasonModeViolation}}
		resp := protocol.EventBatchResponse{
			SchemaVersion: "1.0", BatchID: batch.BatchID, ReceivedAt: time.Now().UTC(), ServerTime: time.Now().UTC(),
			Counts: protocol.BatchCounts{Rejected: 1}, Results: results,
		}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	})
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", res.Rejected)
	}
	if store.state(1) != protocol.SpoolRejected {
		t.Fatalf("entry state = %s, want rejected", store.state(1))
	}
	if store.reason(1) != string(protocol.ReasonModeViolation) {
		t.Fatalf("reason = %q, want mode_violation", store.reason(1))
	}
}

func TestDrainRetriesRetryableStatus(t *testing.T) {
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-r")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var mu sync.Mutex
	attempts := 0
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			writeJSON(t, w, http.StatusServiceUnavailable, []byte(`{"error":{"code":"schema_violation","server_time":"2026-10-01T00:00:00Z"}}`))
			return
		}
		writeJSON(t, w, http.StatusOK, acceptedResponse(t, r))
	})
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1 after retry", res.Delivered)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry after 503)", attempts)
	}
}

func TestDrainDuplicateBatchUsesFreshID(t *testing.T) {
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-d")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		body := gunzipBody(t, r)
		var batch protocol.EventBatch
		_ = json.Unmarshal(body, &batch)
		mu.Lock()
		seen[batch.BatchID] = true
		first := len(seen) == 1
		mu.Unlock()
		if first {
			writeJSON(t, w, http.StatusConflict, []byte(`{"error":{"code":"duplicate_batch","server_time":"2026-10-01T00:00:00Z"}}`))
			return
		}
		writeJSON(t, w, http.StatusOK, acceptedResponseForBatch(t, batch))
	})
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1", res.Delivered)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("saw %d distinct batch ids, want 2 (a fresh id on duplicate_batch)", len(seen))
	}
}

func TestDrainTerminalRetainsSpool(t *testing.T) {
	store := newMemStore()
	if _, err := store.Append(testEnvelope(t, "evt-t")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, []byte(`{"error":{"code":"revoked_device","server_time":"2026-10-01T00:00:00Z"}}`))
	})
	if _, err := d.Drain(context.Background(), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// The spool retains the record (back to pending), not discarded (§13).
	if store.state(1) != protocol.SpoolPending {
		t.Fatalf("entry state = %s, want pending (retained)", store.state(1))
	}
	if st := d.Status(); st.State != protocol.StateDegraded || st.Detail != protocol.DetailUpstreamFailure {
		t.Fatalf("status = %+v, want degraded/upstream_failure", st)
	}
}

func TestDrainEmptyIsNoop(t *testing.T) {
	store := newMemStore()
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the drain should not call the peer when the spool is empty")
	})
	res, err := d.Drain(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !res.Empty {
		t.Fatal("Empty = false for an empty spool")
	}
	if res.Delivered != 0 || res.Rejected != 0 {
		t.Fatalf("empty spool produced %+v", res)
	}
}

func TestDrainRejectsOversizeEnvelope(t *testing.T) {
	store := newMemStore()
	e := testEnvelope(t, "evt-big")
	e.Payload = bytes.Repeat([]byte("x"), protocol.MaxEnvelopeBytes+1)
	e.SizeBytes = int64(len(e.Payload))
	if _, err := store.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an oversize envelope must not be sent")
	})
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", res.Rejected)
	}
	if store.state(1) != protocol.SpoolRejected {
		t.Fatalf("state = %s, want rejected", store.state(1))
	}
}
