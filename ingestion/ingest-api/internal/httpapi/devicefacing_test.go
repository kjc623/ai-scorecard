package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Tests for the device-facing surface's one rule about internal detail: a device is an untrusted
// reader, so nothing derived from a driver, a socket or a schema loader may appear in a response.
//
// `invalid input syntax for type uuid: "sha256:aaa…"` reached a device once, from mapStoreError's
// default branch. The fix is at one call site; these tests are the class — they assert the behaviour
// from outside, through the real handler, so the next such branch fails here rather than on a
// customer's device.

// failingStore is the in-memory store with one method replaced, so a test can produce a failure the
// real store cannot: an error the service cannot classify.
type failingStore struct {
	store.Store
	err error
}

func (f failingStore) WriteBatch(context.Context, store.BatchWrite) (store.BatchResult, error) {
	return store.BatchResult{}, f.err
}

// markerText is what must never be sent. It is shaped like the real leak: a driver's message naming a
// value that came from the request.
const markerText = `store: principal status: ERROR: invalid input syntax for type uuid: "sha256:SECRET-MARKER"`

func TestInternalErrorTextNeverReachesTheDevice(t *testing.T) {
	// The wrapper needs a real route table: the service refuses to serve without one, which is itself
	// a property worth keeping (a deployment whose ref.route_fidelity is empty accepts nothing).
	routes, err := store.LoadRouteTable(filepath.Join("..", "..", "testdata", "route-fidelity.seed.json"))
	if err != nil {
		t.Fatalf("load routes: %v", err)
	}
	h := newHarnessWithStore(t, staticAuth(), failingStore{
		Store: store.NewMemory(routes),
		err:   errors.New(markerText),
	})

	body := batchBody(t, "leak-1", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(1), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))
	rec := post(t, h, body, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a write failure is retryable)", rec.Code)
	}
	got := rec.Body.String()
	for _, forbidden := range []string{"SECRET-MARKER", "invalid input syntax", "uuid", "store:", "ERROR"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the response carries %q, which is internal detail:\n%s", forbidden, got)
		}
	}

	// The contract's machine-readable surface is unaffected: a closed code the device acts on.
	var env struct {
		Error struct {
			Code       string                     `json:"code"`
			Detail     map[string]json.RawMessage `json:"detail"`
			ServerTime string                     `json:"server_time"`
			Message    string                     `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the error envelope is malformed: %v (%s)", err, got)
	}
	if !protocol.ReasonCode(env.Error.Code).Valid() {
		t.Errorf("code %q is outside §7's closed set", env.Error.Code)
	}
	if env.Error.ServerTime == "" {
		t.Error("§5: every response carries server_time")
	}

	// And the operator still gets everything: the alternative to leaking is logging, not losing.
	if !strings.Contains(h.logs.String(), "SECRET-MARKER") {
		t.Errorf("the internal failure was not logged, so it was lost rather than kept:\n%s", h.logs.String())
	}
}

// TestRejectionDetailNeverEchoesTheOffendingValue covers the other half of the same rule: §7 requires
// a pointer and the violated constraint's shape, never the value.
func TestRejectionDetailNeverEchoesTheOffendingValue(t *testing.T) {
	h := newHarness(t, staticAuth())
	var m map[string]any
	if err := json.Unmarshal(ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(2), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	const marker = "PROMPT-CONTENT-MARKER"
	m["source"] = marker // an enum violation whose value must not come back
	raw, _ := json.Marshal(m)

	rec := post(t, h, batchBody(t, "leak-2", raw), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a per-event rejection", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Errorf("the rejection echoed the offending value:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/events/0/source") {
		t.Errorf("the rejection does not point at the field:\n%s", rec.Body.String())
	}
}

// TestMalformedDevIdentityIsA401NotADatabaseError is the second fix at the level a person hits it:
// a typo in a header is a credential problem, so it is refused where the identity is established.
func TestMalformedDevIdentityIsA401NotADatabaseError(t *testing.T) {
	h := newHarness(t, DevHeader{
		TenantHeader: "X-Dev-Tenant-Id", DeviceHeader: "X-Dev-Device-Id", CredentialID: "cred-1",
	})
	body := batchBody(t, "dev-1", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(3), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))

	cases := []struct {
		name   string
		tenant string
		device string
	}{
		{"device is a digest", ladder.TenantID, ladder.Hash('c')},
		{"tenant is a digest", ladder.Hash('d'), ladder.DeviceID},
		{"both are not uuids", "tenant-1", "device-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, body, map[string]string{
				"X-Dev-Tenant-Id": c.tenant,
				"X-Dev-Device-Id": c.device,
			})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: a malformed identity is an authentication failure, not a write failure", rec.Code)
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Error.Code != string(protocol.ReasonRevokedDevice) {
				t.Errorf("code = %q, want %q (the closed set has one 401 code)", env.Error.Code, protocol.ReasonRevokedDevice)
			}
			// Nothing was read or written: the refusal happens before the body is even parsed.
			if n := h.mem.ObservationCount(); n != 0 {
				t.Errorf("observations = %d, want 0", n)
			}
			if !strings.Contains(rec.Body.String(), "uuid") {
				t.Errorf("the message should say what is wrong with the identity:\n%s", rec.Body.String())
			}
		})
	}

	t.Run("a well-formed identity still works", func(t *testing.T) {
		rec := post(t, h, body, map[string]string{
			"X-Dev-Tenant-Id": ladder.TenantID, "X-Dev-Device-Id": ladder.DeviceID,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestUnclassifiedAuthFailureIsLoggedNotSent covers writeAuthError's default branch, the same class
// one branch over.
func TestUnclassifiedAuthFailureIsLoggedNotSent(t *testing.T) {
	authErr := errors.New("auth: credential status: dial tcp 10.0.0.5:5432: connect: connection refused")
	h := newHarness(t, auth.Static{Err: authErr})
	body := batchBody(t, "auth-1", ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(4), Tool: "t", OccurredAt: "2026-10-02T14:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 1,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	}))
	rec := post(t, h, body, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	for _, forbidden := range []string{"dial tcp", "10.0.0.5", "connection refused"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("the response carries %q:\n%s", forbidden, rec.Body.String())
		}
	}
	if !strings.Contains(h.logs.String(), "dial tcp") {
		t.Errorf("the internal failure was not logged:\n%s", h.logs.String())
	}
}
