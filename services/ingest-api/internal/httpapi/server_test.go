package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/batchguard"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

type harness struct {
	server *Server
	mem    *store.Memory
	// logs captures the server's structured log, so a test can assert that what must not reach the
	// device did reach the operator.
	logs *bytes.Buffer
}

func newHarness(t *testing.T, a auth.Authenticator) *harness {
	return newHarnessWithStore(t, a, nil)
}

// newHarnessWithStore builds the same server around a caller-supplied store. A nil store means the
// in-memory one; a test that needs a failure the real store cannot produce (an unclassified write
// error, for instance) passes its own wrapper.
func newHarnessWithStore(t *testing.T, a auth.Authenticator, st store.Store) *harness {
	t.Helper()
	path, err := contract.Discover(".")
	if err != nil {
		t.Fatalf("discover schema: %v", err)
	}
	schema, err := contract.Load(path)
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	routes, err := store.LoadRouteTable(filepath.Join("..", "..", "testdata", "route-fidelity.seed.json"))
	if err != nil {
		t.Fatalf("load routes: %v", err)
	}
	mem := store.NewMemory(routes)
	mem.SetPrincipal(ladder.TenantID, ladder.DeviceID, "cred-1", store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true,
		CredentialExpiry: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if st == nil {
		st = mem
	}
	svc, err := ingest.New(schema, st, ingest.DefaultConfig())
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return &harness{server: New(svc, a, batchguard.New(time.Hour), logger), mem: mem, logs: logs}
}

func staticAuth() auth.Authenticator {
	return auth.Static{P: auth.Principal{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
	}}
}

func batchBody(t *testing.T, batchID string, events ...json.RawMessage) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.EventBatch{
		SchemaVersion: "1.0",
		BatchID:       batchID,
		DeviceSentAt:  time.Date(2026, 10, 2, 14, 0, 3, 0, time.UTC),
		EventCount:    len(events),
		Events:        events,
	})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	return b
}

func post(t *testing.T, h *harness, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

// TestEndpointShapeIsTheProtocolShape asserts the exact 200 body of §5.3, field by field, because
// this is the seam the device and the verifier both read.
func TestEndpointShapeIsTheProtocolShape(t *testing.T) {
	h := newHarness(t, staticAuth())
	twoRoutes := ladder.Scenarios[0]
	rec := post(t, h, batchBody(t, "b-1", twoRoutes.Steps[0].Envelope, twoRoutes.Steps[1].Envelope), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	for _, key := range []string{"schema_version", "batch_id", "received_at", "server_time", "counts", "results"} {
		if _, ok := body[key]; !ok {
			t.Errorf("response is missing %q", key)
		}
	}
	var counts protocol.BatchCounts
	if err := json.Unmarshal(body["counts"], &counts); err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Accepted != 2 || counts.Duplicate != 0 || counts.Rejected != 0 {
		t.Errorf("counts = %+v", counts)
	}

	// The device's own validator is the strictest reader of this response; if it accepts the bytes,
	// the response is exactly the contract.
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal into protocol.EventBatchResponse: %v", err)
	}
	sent := []string{ladder.DeterministicUUID(1), ladder.DeterministicUUID(2)}
	if err := resp.Validate(sent); err != nil {
		t.Fatalf("the response does not satisfy the device's own validator: %v", err)
	}

	// Field-level spot checks on the first result: accepted carries submission_id, dedup_tier and
	// won_fields; a duplicate carries first_received_at instead of dedup_tier.
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(body["results"], &results); err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, key := range []string{"event_id", "outcome", "submission_id", "dedup_tier", "won_fields"} {
		if _, ok := results[0][key]; !ok {
			t.Errorf("an accepted result is missing %q", key)
		}
	}
	if _, ok := results[0]["reason"]; ok {
		t.Error("an accepted result must not carry a reason")
	}
}

// TestDuplicateReplayShape asserts §5.3's duplicate result: the submission id and the first
// receipt, and no dedup_tier (the field describes the observation that created the submission).
func TestDuplicateReplayShape(t *testing.T) {
	h := newHarness(t, staticAuth())
	env := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(50), Tool: "replay-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 90,
		ContentDigest: ladder.Hash('4'), DedupKey: ladder.Hash('5'),
	})

	first := post(t, h, batchBody(t, "b-first", env), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first: status %d %s", first.Code, first.Body.String())
	}
	second := post(t, h, batchBody(t, "b-second", env), nil)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d %s", second.Code, second.Body.String())
	}

	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Counts.Duplicate != 1 {
		t.Fatalf("counts = %+v, want 1 duplicate", resp.Counts)
	}
	got := resp.Results[0]
	if got.Outcome != protocol.OutcomeDuplicate {
		t.Fatalf("outcome = %q", got.Outcome)
	}
	if got.SubmissionID == "" || got.FirstReceivedAt == nil {
		t.Fatalf("a duplicate must carry submission_id and first_received_at: %+v", got)
	}
	if got.DedupTier != "" {
		t.Errorf("dedup_tier = %q, want it absent on a duplicate", got.DedupTier)
	}
}

// TestBatchThatParsesAlwaysReturns200 covers §5.3's central promise, including the case where every
// event inside the batch is rejected.
func TestBatchThatParsesAlwaysReturns200(t *testing.T) {
	h := newHarness(t, staticAuth())
	var m map[string]any
	if err := json.Unmarshal(ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(60), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m0", SizeBytes: 1, DedupKey: ladder.Hash('9'),
	}), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["content_digest"] = ladder.Hash('a') // the M0 violation
	bad, _ := json.Marshal(m)

	rec := post(t, h, batchBody(t, "b-all-rejected", bad), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when every event is rejected: %s", rec.Code, rec.Body.String())
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Counts.Rejected != 1 {
		t.Fatalf("counts = %+v", resp.Counts)
	}
	if resp.Results[0].Reason != protocol.ReasonModeViolation {
		t.Errorf("reason = %q, want mode_violation", resp.Results[0].Reason)
	}
	if err := resp.Validate([]string{ladder.DeterministicUUID(60)}); err != nil {
		t.Errorf("the device's validator rejected our rejection: %v", err)
	}
}

// TestErrorEnvelopeShape asserts §5's common error body.
func TestErrorEnvelopeShape(t *testing.T) {
	h := newHarness(t, staticAuth())
	cases := []struct {
		name       string
		body       []byte
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{"unparseable batch", []byte("{not json"), nil, 400, "schema_violation"},
		{"unknown content encoding", batchBody(t, "b-enc"), map[string]string{"Content-Encoding": "br"}, 400, "schema_violation"},
		{"event_count mismatch", []byte(`{"schema_version":"1.0","batch_id":"b","event_count":9,"events":[]}`), nil, 400, "schema_violation"},
		{"unsupported batch schema_version", []byte(`{"schema_version":"9.9","batch_id":"b","event_count":0,"events":[]}`), nil, 400, "unsupported_schema_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, c.body, c.headers)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			var env struct {
				Error struct {
					Code       string                     `json:"code"`
					Detail     map[string]json.RawMessage `json:"detail"`
					ServerTime time.Time                  `json:"server_time"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not the §5 envelope: %v (%s)", err, rec.Body.String())
			}
			if env.Error.Code != c.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, c.wantCode)
			}
			if env.Error.ServerTime.IsZero() {
				t.Error("§5: every response carries server_time")
			}
			if env.Error.Detail == nil {
				t.Error("the error envelope must carry a detail object")
			}
		})
	}
}

func TestOversizeIsRejectedAtTheBodyCap(t *testing.T) {
	h := newHarness(t, staticAuth())
	h.server.MaxCompressedBody = 128
	rec := post(t, h, batchBody(t, "b-big", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(70), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	})), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"oversize"`) {
		t.Errorf("body = %s, want the closed code oversize", rec.Body.String())
	}
}

func TestGzipRequestBodyIsAccepted(t *testing.T) {
	h := newHarness(t, staticAuth())
	raw := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(71), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	})
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(batchBody(t, "b-gzip", raw)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	rec := post(t, h, buf.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestDuplicateBatchGuard covers §5.3's batch-level idempotency: a replay of the same batch_id is
// refused with the closed code, and re-sending with a fresh batch_id gives exact per-event outcomes.
func TestDuplicateBatchGuard(t *testing.T) {
	h := newHarness(t, staticAuth())
	body := batchBody(t, "b-fixed", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(72), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))

	if rec := post(t, h, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, body, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("replay: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"duplicate_batch"`) {
		t.Errorf("body = %s, want duplicate_batch", rec.Body.String())
	}

	// The recovery §5.3 describes: a fresh batch_id gives exact per-event outcomes.
	fresh := batchBody(t, "b-fresh", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(72), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))
	rec = post(t, h, fresh, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh batch: status = %d, want 200", rec.Code)
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Counts.Duplicate != 1 {
		t.Errorf("counts = %+v, want the event to report as duplicate", resp.Counts)
	}
}

func TestAuthFailuresMapToTheirStatusAndCode(t *testing.T) {
	body := batchBody(t, "b-auth", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(73), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"no credential", auth.ErrNoCredential, 401, "revoked_device"},
		{"credential revoked", auth.ErrCredentialRevoked, 401, "revoked_device"},
		{"credential expired", auth.ErrCredentialExpired, 401, "revoked_device"},
		{"unknown tenant", auth.ErrUnknownTenant, 403, "unknown_tenant"},
		{"tenant suspended", auth.ErrTenantSuspended, 403, "unknown_tenant"},
		{"region mismatch", auth.ErrRegionMismatch, 403, "region_mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, auth.Static{Err: c.err})
			rec := post(t, h, body, nil)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Error.Code != c.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, c.wantCode)
			}
		})
	}
}

func TestMethodAndHealth(t *testing.T) {
	h := newHarness(t, staticAuth())
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/events = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// TestDevHeaderAuthenticator proves the verification-only principal path cooperates with the real
// handler, so a reviewer can POST to a locally started binary without a certificate authority.
func TestDevHeaderAuthenticator(t *testing.T) {
	h := newHarness(t, DevHeader{
		TenantHeader: "X-Dev-Tenant-Id", DeviceHeader: "X-Dev-Device-Id", CredentialID: "cred-1",
	})
	body := batchBody(t, "b-dev", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(74), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))
	rec := post(t, h, body, map[string]string{
		"X-Dev-Tenant-Id": ladder.TenantID,
		"X-Dev-Device-Id": ladder.DeviceID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without the headers: status = %d, want 401", rec.Code)
	}
}
