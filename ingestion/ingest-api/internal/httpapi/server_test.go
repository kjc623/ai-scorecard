package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	tenant = "22222222-2222-4222-8222-222222222222"
	device = "33333333-3333-4333-8333-333333333333"
)

var serverTime = time.Date(2026, 10, 5, 12, 0, 1, 0, time.UTC)

type fixedAuth struct {
	p   auth.Principal
	err error
}

func (a fixedAuth) Authenticate(context.Context, *http.Request) (auth.Principal, error) {
	return a.p, a.err
}

type fakeStore struct{ err error }

func (fakeStore) Routes(context.Context) ([]string, error) { return []string{"ext.web_request"}, nil }

func (f fakeStore) WriteBatch(_ context.Context, w store.BatchWrite) ([]store.EventOutcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []store.EventOutcome
	for _, ev := range w.Accepted {
		out = append(out, store.EventOutcome{Index: ev.Index, EventID: ev.EventID, Outcome: store.OutcomeInserted,
			SubmissionID: "44444444-4444-4444-8444-444444444444", WonFields: true})
	}
	return out, nil
}

type harness struct {
	server *Server
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, a Authenticator, st fakeStore) harness {
	t.Helper()
	v, err := contract.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return harness{
		server: &Server{
			Service: ingest.New(v, st),
			Auth:    a,
			Ready:   func(context.Context) error { return nil },
			Logger:  slog.New(slog.NewJSONHandler(logs, nil)),
			Now:     func() time.Time { return serverTime },
		},
		logs: logs,
	}
}

func device1() fixedAuth {
	return fixedAuth{p: auth.Principal{TenantID: tenant, DeviceID: device, CredentialID: "55555555-5555-4555-8555-555555555555"}}
}

func batchBody(t *testing.T) []byte {
	t.Helper()
	event := map[string]any{
		"schema_version": "1.0", "event_id": "11111111-1111-4111-8111-111111111111",
		"tenant_id": tenant, "device_id": device, "user_ref": "user-1", "tool_fingerprint": "tool-1",
		"direction": "egress", "kind": "prompt", "occurred_at": "2026-10-05T12:00:00Z", "monotonic_offset_ms": 1,
		"source": "ext.web_request", "collection_mode": "m0", "size_bytes": 10,
		"policy_decision": map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":       "sha256:" + strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(event)
	body, err := json.Marshal(protocol.EventBatch{
		SchemaVersion: "1.0", BatchID: "batch-1", DeviceSentAt: serverTime, EventCount: 1, Events: []json.RawMessage{raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (h harness) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, r)
	return rec
}

func post(body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not the error envelope: %v\n%s", err, rec.Body.String())
	}
	if env.Error.ServerTime.IsZero() {
		t.Error("every error carries server_time")
	}
	return env.Error
}

func TestBatchIsAcceptedInTheProtocolShape(t *testing.T) {
	h := newHarness(t, device1(), fakeStore{})
	rec := h.do(post(batchBody(t)))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if err := resp.Validate([]string{"11111111-1111-4111-8111-111111111111"}); err != nil {
		t.Fatalf("the device would refuse this response: %v", err)
	}
	if !resp.ReceivedAt.Equal(serverTime) || resp.Counts.Accepted != 1 {
		t.Errorf("response = %+v", resp)
	}
}

func TestGzipBodyIsAccepted(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(batchBody(t))
	_ = zw.Close()
	r := post(buf.Bytes())
	r.Header.Set("Content-Encoding", "gzip")
	if rec := newHarness(t, device1(), fakeStore{}).do(r); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTransportRefusals(t *testing.T) {
	h := newHarness(t, device1(), fakeStore{})
	cases := map[string]struct {
		req    func() *http.Request
		status int
		code   protocol.ReasonCode
	}{
		"GET": {func() *http.Request { return httptest.NewRequest(http.MethodGet, "/v1/events", nil) }, 405, protocol.ReasonSchemaViolation},
		"body over the cap": {func() *http.Request {
			return post(bytes.Repeat([]byte("x"), protocol.MaxRequestBodyBytes+1))
		}, 413, protocol.ReasonOversize},
		"unsupported encoding": {func() *http.Request {
			r := post(batchBody(t))
			r.Header.Set("Content-Encoding", "br")
			return r
		}, 400, protocol.ReasonSchemaViolation},
		"corrupt gzip": {func() *http.Request {
			r := post([]byte("not gzip"))
			r.Header.Set("Content-Encoding", "gzip")
			return r
		}, 413, protocol.ReasonOversize},
		"malformed JSON": {func() *http.Request { return post([]byte(`{"batch_id":`)) }, 400, protocol.ReasonSchemaViolation},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.do(c.req())
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}
			if got := errorOf(t, rec).Code; got != c.code {
				t.Errorf("code %s, want %s", got, c.code)
			}
			if c.status == 405 && rec.Header().Get("Allow") != http.MethodPost {
				t.Error("405 must carry Allow: POST")
			}
		})
	}
}

func TestAuthenticationFailures(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   protocol.ReasonCode
	}{
		{auth.ErrNoCredential, 401, protocol.ReasonRevokedDevice},
		{fmt.Errorf("%w: wrong CA", auth.ErrBadCredential), 401, protocol.ReasonRevokedDevice},
		{store.ErrCredentialUnknown, 401, protocol.ReasonRevokedDevice},
		{store.ErrCredentialRevoked, 401, protocol.ReasonRevokedDevice},
		{store.ErrCredentialExpired, 401, protocol.ReasonRevokedDevice},
		{store.ErrDeviceRevoked, 401, protocol.ReasonRevokedDevice},
		{store.ErrUnknownTenant, 403, protocol.ReasonUnknownTenant},
		{store.ErrTenantSuspended, 403, protocol.ReasonUnknownTenant},
		{store.ErrRegionMismatch, 403, protocol.ReasonRegionMismatch},
		{errors.New("dial tcp 10.0.0.4:5432: connection refused"), 503, protocol.ReasonSchemaViolation},
	}
	for _, c := range cases {
		t.Run(c.err.Error(), func(t *testing.T) {
			h := newHarness(t, fixedAuth{err: c.err}, fakeStore{})
			rec := h.do(post(batchBody(t)))
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d", rec.Code, c.status)
			}
			if got := errorOf(t, rec).Code; got != c.code {
				t.Errorf("code %s, want %s", got, c.code)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.4") || strings.Contains(rec.Body.String(), "wrong CA") {
				t.Errorf("the response carries internal detail: %s", rec.Body.String())
			}
		})
	}
}

func TestInternalFailureIsLoggedNotSent(t *testing.T) {
	const internal = `invalid input syntax for type uuid: "sha256:aaa"`
	h := newHarness(t, device1(), fakeStore{err: errors.New(internal)})
	rec := h.do(post(batchBody(t)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if body := errorOf(t, rec); body.RetryAfterS == 0 || strings.Contains(body.Message, "syntax") {
		t.Errorf("error body = %+v", body)
	}
	if !strings.Contains(h.logs.String(), "invalid input syntax") {
		t.Errorf("the cause must reach the log, got: %s", h.logs.String())
	}
}

func TestProbes(t *testing.T) {
	h := newHarness(t, device1(), fakeStore{})
	if rec := h.do(httptest.NewRequest(http.MethodGet, "/healthz", nil)); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := h.do(httptest.NewRequest(http.MethodGet, "/readyz", nil)); rec.Code != http.StatusOK {
		t.Errorf("/readyz = %d while the database answers", rec.Code)
	}
	h.server.Ready = func(context.Context) error { return errors.New("dial tcp db.internal:5432: timeout") }
	rec := h.do(httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("/readyz = %d %s, want 503 without the dependency's detail", rec.Code, rec.Body.String())
	}
}
