package drain

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

type memKeys struct{}

func (memKeys) Key() ([]byte, error) { return make([]byte, 32), nil }
func (memKeys) Sealed() bool         { return false }

func newTestCredentialStore(t *testing.T) *credential.Store {
	t.Helper()
	s, err := credential.Open(filepath.Join(t.TempDir(), "credential.sealed"), memKeys{})
	if err != nil {
		t.Fatalf("credential.Open: %v", err)
	}
	return s
}

const testEnvelopeJSON = `{"key_id":"policy-key-1","algorithm":"ed25519","payload":{"version":"3","seed_hosts":["a<b"]},"signature":"c2ln"}`

// policyResponseBody is a 200 body as control-api serves it: protocol.PolicyResponse with the signed
// envelope's bytes inside it, untouched.
func policyResponseBody(envelope string) []byte {
	return []byte(`{"schema_version":"1.0","bundle_version":"3","signed_bundle":` + envelope + `,"upgrade_required":false,"server_time":"2026-10-05T09:00:00Z"}`)
}

func TestFetchPolicyServedWithETagAndCadence(t *testing.T) {
	var gotMethod, gotINM, gotAuth, gotProof string
	d, _ := newTestDrainer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotMethod, gotINM = r.Method, r.Header.Get(protocol.HeaderIfNoneMatch)
		gotAuth, gotProof = r.Header.Get(protocol.HeaderAuthorization), r.Header.Get(protocol.HeaderDPoP)
		w.Header().Set(protocol.HeaderETag, `"3"`)
		w.Header().Set("Cache-Control", "private, max-age=600")
		writeJSON(t, w, http.StatusOK, policyResponseBody(testEnvelopeJSON))
	})
	got, err := d.FetchPolicy(context.Background(), `"2"`)
	if err != nil {
		t.Fatalf("FetchPolicy: %v", err)
	}
	if gotMethod != http.MethodGet || gotINM != `"2"` {
		t.Fatalf("request = %s If-None-Match %q", gotMethod, gotINM)
	}
	if gotAuth != "DPoP test-token" || gotProof == "" {
		t.Fatalf("auth = %q proof present %v; the policy GET must use the device credential like /v1/health", gotAuth, gotProof != "")
	}
	if got.Status != PolicyServed || string(got.Envelope) != testEnvelopeJSON || got.ETag != `"3"` || got.NextPoll != 10*time.Minute {
		t.Fatalf("fetch = %+v", got)
	}
}

func TestFetchPolicyNotModifiedKeepsTheETag(t *testing.T) {
	d, _ := newTestDrainer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	got, err := d.FetchPolicy(context.Background(), `"3"`)
	if err != nil {
		t.Fatalf("FetchPolicy: %v", err)
	}
	if got.Status != PolicyNotModified || got.ETag != `"3"` || got.Envelope != nil {
		t.Fatalf("fetch = %+v", got)
	}
}

func TestFetchPolicyNoBundleNeedsTheErrorEnvelope(t *testing.T) {
	d, _ := newTestDrainer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, []byte(`{"error":{"code":"no_policy_bundle","server_time":"2026-10-05T00:00:00Z"}}`))
	})
	got, err := d.FetchPolicy(context.Background(), "")
	if err != nil || got.Status != PolicyNone {
		t.Fatalf("404 with the error envelope = %+v, %v; want PolicyNone", got, err)
	}

	// The same status from a server (or an edge) that does not serve the route says nothing about
	// the tenant's policy, so it must not take the device to M0.
	d, _ = newTestDrainer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	if got, err := d.FetchPolicy(context.Background(), ""); err == nil || got.Status == PolicyNone {
		t.Fatalf("a bare 404 = %+v, %v; want an error", got, err)
	}
}

func TestFetchPolicyRefusalCarriesRetryAfter(t *testing.T) {
	d, _ := newTestDrainer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusServiceUnavailable, []byte(`{"error":{"code":"unavailable","retry_after_s":120}}`))
	})
	got, err := d.FetchPolicy(context.Background(), "")
	if err == nil {
		t.Fatal("a 503 was not an error")
	}
	if got.NextPoll != 2*time.Minute || got.Status != 0 {
		t.Fatalf("fetch = %+v, want only the server's retry cadence", got)
	}
}

// The signature covers the envelope's bytes as signed, so extraction must hand them over unchanged
// (the fixture carries an escaped '<', which a re-encoding would alter).
func TestSignedEnvelopeKeepsTheSignedBytes(t *testing.T) {
	if env := signedEnvelope(policyResponseBody(testEnvelopeJSON)); string(env) != testEnvelopeJSON {
		t.Fatalf("signed_bundle = %s, want the envelope byte for byte", env)
	}
	if env := signedEnvelope([]byte(testEnvelopeJSON)); string(env) != testEnvelopeJSON {
		t.Fatalf("a bare signed envelope was changed: %s", env)
	}
	if env := signedEnvelope([]byte("not json")); string(env) != "not json" {
		t.Fatal("an unrecognised body was not passed through for the verifier to name")
	}
}
