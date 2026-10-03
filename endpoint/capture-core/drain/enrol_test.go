package drain

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// newRawDrainer builds a drainer with a manual (plain-HTTP) client and no credential, for exercising
// the enrol and token flows in isolation.
func newRawDrainer(t *testing.T, cfg Config, handler http.HandlerFunc) (*Drainer, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	d := &Drainer{
		cfg:    cfg,
		client: &client{base: strings.TrimRight(srv.URL, "/"), caPool: x509.NewCertPool(), http: srv.Client()},
		log:    nopLogger{},
		clock:  time.Now,
		state:  protocol.StateAbsent,
		stopCh: make(chan struct{}),
	}
	return d, srv
}

func TestEnrolDPoP(t *testing.T) {
	var got protocol.EnrolmentRequest
	var dpopHeader string
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "tenant-1",
		DeviceID:       "device-1",
		AgentVersion:   "test",
		MDMID:          "mdm-9",
	}, func(w http.ResponseWriter, r *http.Request) {
		dpopHeader = r.Header.Get(protocol.HeaderDPoP)
		body := gunzipBody(t, r)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode enrol request: %v", err)
		}
		resp := protocol.EnrolmentResponse{
			SchemaVersion: protocol.EnrolmentSchemaVersion,
			DeviceID:      "device-1",
			TenantID:      "tenant-1",
			Region:        "eu",
			Credential:    protocol.IssuedCredential{Mode: protocol.AuthModeDPoP, JWK: &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}},
			ServerTime:    time.Now().UTC(),
		}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	})
	c, err := d.enrol(context.Background(), HardwareIdentityHash("tenant-1", "device-1", "mdm-9"))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if got.Mode != protocol.AuthModeDPoP {
		t.Fatalf("mode = %q, want dpop", got.Mode)
	}
	if got.EnrolmentToken != "tok-123" {
		t.Fatalf("token = %q, want tok-123", got.EnrolmentToken)
	}
	if got.JWK == nil || got.JWK.Kty != "EC" {
		t.Fatalf("jwk = %+v, want an EC JWK", got.JWK)
	}
	if got.CSR != "" {
		t.Fatal("a dpop enrol carried a CSR")
	}
	if got.Device.HardwareIdentityHash == "" {
		t.Fatal("enrol carried no hardware_identity_hash")
	}
	if dpopHeader == "" {
		t.Fatal("a dpop enrol carried no DPoP proof header")
	}
	if c.Mode != protocol.AuthModeDPoP || c.JWK == nil {
		t.Fatalf("issued credential = %+v", c)
	}
	if c.PrivateKey == "" {
		t.Fatal("issued credential carries no private key")
	}
	if _, err := c.ECPrivateKey(); err != nil {
		t.Fatalf("issued key does not parse: %v", err)
	}
}

func TestEnrolX509(t *testing.T) {
	var got protocol.EnrolmentRequest
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeX509,
		EnrolmentToken: "tok-123",
		TenantID:       "tenant-1",
		DeviceID:       "device-1",
		AgentVersion:   "test",
	}, func(w http.ResponseWriter, r *http.Request) {
		body := gunzipBody(t, r)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode enrol request: %v", err)
		}
		resp := protocol.EnrolmentResponse{
			SchemaVersion: protocol.EnrolmentSchemaVersion,
			DeviceID:      "device-1",
			TenantID:      "tenant-1",
			Region:        "eu",
			Credential:    protocol.IssuedCredential{Mode: protocol.AuthModeX509, CertPEM: "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"},
			ServerTime:    time.Now().UTC(),
		}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	})
	c, err := d.enrol(context.Background(), HardwareIdentityHash("tenant-1", "device-1", ""))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if got.Mode != protocol.AuthModeX509 {
		t.Fatalf("mode = %q, want x509", got.Mode)
	}
	if got.CSR == "" {
		t.Fatal("an x509 enrol carried no CSR")
	}
	if got.JWK != nil {
		t.Fatal("an x509 enrol carried a JWK")
	}
	if c.Mode != protocol.AuthModeX509 || c.CertPEM == "" {
		t.Fatalf("issued credential = %+v", c)
	}
}

func TestEnrolFailureIsClassified(t *testing.T) {
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "tenant-1",
		DeviceID:       "device-1",
		AgentVersion:   "test",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, []byte(`{"error":{"code":"revoked_device","server_time":"2026-10-01T00:00:00Z"}}`))
	})
	_, err := d.enrol(context.Background(), HardwareIdentityHash("tenant-1", "device-1", ""))
	if err == nil {
		t.Fatal("enrol accepted a 401")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("error is %T, want *apiError", err)
	}
	if ae.status != http.StatusUnauthorized || ae.code != protocol.ReasonRevokedDevice {
		t.Fatalf("apiError = %+v, want 401/revoked_device", ae)
	}
	if ae.Retryable() {
		t.Fatal("a revoked credential is not retryable")
	}
}

func TestHardwareIdentityHash(t *testing.T) {
	withMDM := HardwareIdentityHash("tenant-1", "device-1", "mdm-9")
	fallback := HardwareIdentityHash("tenant-1", "device-1", "")
	again := HardwareIdentityHash("tenant-1", "device-1", "mdm-9")
	if withMDM != again {
		t.Fatal("HardwareIdentityHash is not deterministic")
	}
	if withMDM == fallback {
		t.Fatal("the MDM seed and the fallback seed produced the same hash; the seed must be honoured")
	}
	otherTenant := HardwareIdentityHash("tenant-2", "device-1", "mdm-9")
	if withMDM == otherTenant {
		t.Fatal("the hash is not tenant-scoped")
	}
	for _, h := range []string{withMDM, fallback} {
		if !strings.HasPrefix(h, "sha256:") {
			t.Fatalf("hash %q has no sha256: prefix", h)
		}
	}
}
