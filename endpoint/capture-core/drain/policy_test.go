package drain

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

const testEnvelopeJSON = `{"key_id":"policy-key-1","algorithm":"ed25519","payload":{"version":"3","seed_hosts":["a<b"]},"signature":"c2ln"}`

// policyResponseBody is a 200 body as control-api serves it: protocol.PolicyResponse with the signed
// envelope's bytes inside it, untouched.
func policyResponseBody(envelope string) []byte {
	return []byte(`{"schema_version":"1.0","bundle_version":"3","signed_bundle":` + envelope + `,"upgrade_required":false,"server_time":"2026-10-05T09:00:00Z"}`)
}

// policyDrainer is an enrolled drainer whose edge answers GET /v1/policy with fn.
func policyDrainer(t *testing.T, fn http.HandlerFunc) *Drainer {
	t.Helper()
	e := newFakeEdge(t)
	e.mux.HandleFunc("/v1/policy", fn)
	return newTestDrainer(t, e, nil, e.issued(24*time.Hour))
}

func TestFetchPolicyServedWithETagAndCadence(t *testing.T) {
	var gotMethod, gotINM, gotCert string
	d := policyDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotINM = r.Method, r.Header.Get(protocol.HeaderIfNoneMatch)
		if len(r.TLS.PeerCertificates) > 0 {
			gotCert = r.TLS.PeerCertificates[0].Subject.CommonName
		}
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
	if gotCert != testDevice {
		t.Fatalf("client certificate %q, want the device leaf", gotCert)
	}
	if got.Status != PolicyServed || string(got.Envelope) != testEnvelopeJSON || got.ETag != `"3"` || got.NextPoll != 10*time.Minute {
		t.Fatalf("fetch = %+v", got)
	}
}

func TestFetchPolicyNotModifiedKeepsTheETag(t *testing.T) {
	d := policyDrainer(t, func(w http.ResponseWriter, r *http.Request) {
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
	d := policyDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, []byte(`{"error":{"code":"no_policy_bundle"}}`))
	})
	got, err := d.FetchPolicy(context.Background(), "")
	if err != nil || got.Status != PolicyNone {
		t.Fatalf("404 with the error envelope = %+v, %v; want PolicyNone", got, err)
	}

	// The same status from an edge that does not serve the route says nothing about the tenant's
	// policy, so it must not take the device to M0.
	d = policyDrainer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if got, err := d.FetchPolicy(context.Background(), ""); err == nil || got.Status == PolicyNone {
		t.Fatalf("a bare 404 = %+v, %v; want an error", got, err)
	}
}

func TestFetchPolicyRefusalCarriesRetryAfter(t *testing.T) {
	d := policyDrainer(t, func(w http.ResponseWriter, r *http.Request) {
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

// The signature covers the envelope's bytes as signed, so extraction hands them over unchanged
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
