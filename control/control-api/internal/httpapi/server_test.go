package httpapi_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/httpapi"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/signer"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/token"
)

const (
	tenantID = "11111111-1111-7111-8111-111111111111"
	deviceID = "33333333-3333-7333-8333-333333333333"
	credID   = "44444444-4444-7444-8444-444444444444"
)

var fixedNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// harness wires the two services and the store behind an httptest server.
type harness struct {
	srv       *httptest.Server
	store     *store.Memory
	deviceKey *ecdsa.PrivateKey
	jwk       protocol.JWK
	token     *enrolToken
}

type enrolToken struct {
	plaintext string
	tenant    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	st.SetNow(func() time.Time { return fixedNow })
	st.AddTenant(store.Tenant{TenantID: tenantID, Status: "active", IngestEnabled: true, ResidencyRegion: "eu-west"})

	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	jwk, err := jose.JWKFromPublic(&deviceKey.PublicKey)
	if err != nil {
		t.Fatalf("device jwk: %v", err)
	}
	thumb, _ := jwk.Thumbprint()
	jwkJSON, _ := json.Marshal(jwk)
	st.AddDevice(store.Device{TenantID: tenantID, DeviceID: deviceID, OS: "windows"})
	st.AddCredential(store.Credential{
		TenantID: tenantID, CredentialID: credID, DeviceID: deviceID,
		Type: protocol.AuthModeDPoP, PublicKeyThumbprint: thumb, PublicKeyJWK: jwkJSON,
		IssuedAt: fixedNow, ExpiresAt: fixedNow.Add(90 * 24 * time.Hour),
	})

	ca, err := signer.NewLocalCA(nil, nil, 90*24*time.Hour, []string{"ingest.eu.example.com"})
	if err != nil {
		t.Fatalf("NewLocalCA: %v", err)
	}
	enrolSvc, err := enrol.New(st, ca, enrol.Config{Region: "eu-west", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	signKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tokenSvc, err := token.New(st, signKey, token.Config{
		Issuer: "https://control.eu.example.com", Audience: "ingest", Region: "eu-west",
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	verifier, _ := token.NewVerifier(&signKey.PublicKey, "https://control.eu.example.com", "ingest", func() time.Time { return fixedNow })
	server := httpapi.New(enrolSvc, tokenSvc, verifier, st, nil)
	server.Now = func() time.Time { return fixedNow }
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	h := &harness{srv: ts, store: st, deviceKey: deviceKey, jwk: jwk}
	h.token = h.addToken(t)
	return h
}

func (h *harness) addToken(t *testing.T) *enrolToken {
	t.Helper()
	plaintext, err := enrol.MintEnrolmentToken(tenantID)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	h.store.AddEnrolmentToken(store.EnrolmentToken{
		TenantID: tenantID, TokenHash: enrol.HashEnrolmentToken(plaintext),
		IssuedAt: fixedNow, ExpiresAt: fixedNow.Add(time.Hour),
	})
	return &enrolToken{plaintext: plaintext, tenant: tenantID}
}

func postJSON(t *testing.T, url string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, buf.Bytes()
}

func makeCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		t.Fatalf("CSR: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// TestEnrolEndToEnd drives the transport: a valid x509 enrolment returns 200 with a credential, and a
// reused token returns a §5 error envelope with a code and server_time.
func TestEnrolEndToEnd(t *testing.T) {
	h := newHarness(t)
	body := protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: h.token.plaintext,
		Mode:           protocol.AuthModeX509,
		CSR:            makeCSR(t),
		Device:         protocol.DeviceInfo{OS: "windows", AgentVersion: "1.0", HardwareIdentityHash: "hw-http"},
	}
	resp, raw := postJSON(t, h.srv.URL+"/v1/enrol", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out protocol.EnrolmentResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.DeviceID == "" || out.Credential.CertPEM == "" {
		t.Fatalf("response = %+v, want a device id and a certificate", out)
	}

	resp, raw = postJSON(t, h.srv.URL+"/v1/enrol", body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused token status = %d, want 401; body = %s", resp.StatusCode, raw)
	}
	var errEnv struct {
		Error struct {
			Code       string    `json:"code"`
			ServerTime time.Time `json:"server_time"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &errEnv); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if errEnv.Error.Code == "" || errEnv.Error.ServerTime.IsZero() {
		t.Fatalf("error envelope = %s, want a code and server_time", raw)
	}
}

// TestTokenEndToEnd drives the token transport: a valid assertion and proof return a DPoP token, and
// a bad proof returns 401 in the common envelope.
func TestTokenEndToEnd(t *testing.T) {
	h := newHarness(t)
	htu := h.srv.URL + "/v1/token"

	header := map[string]any{"alg": jose.AlgES256, "typ": "JWT"}
	assertionClaims := map[string]any{
		"sub": deviceID, "tenant_id": tenantID,
		"iat": fixedNow.Unix(), "exp": fixedNow.Add(time.Hour).Unix(),
	}
	assertion, err := jose.SignES256(header, assertionClaims, h.deviceKey)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	proofHeader := map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": h.jwk}
	proofClaims := map[string]any{"htm": "POST", "htu": htu, "iat": fixedNow.Unix(), "jti": "jti-http"}
	proof, _ := jose.SignES256(proofHeader, proofClaims, h.deviceKey)

	body := protocol.TokenRequest{GrantType: token.GrantTypeJWTBearer, Assertion: assertion}
	resp, raw := postJSON(t, htu, body, map[string]string{protocol.HeaderDPoP: proof})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out protocol.TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("token response does not validate: %v", err)
	}

	// A proof bound to the wrong URL is refused.
	badProof, _ := jose.SignES256(proofHeader, map[string]any{"htm": "POST", "htu": "https://evil/v1/token", "iat": fixedNow.Unix(), "jti": "jti-http-bad"}, h.deviceKey)
	resp, raw = postJSON(t, htu, body, map[string]string{protocol.HeaderDPoP: badProof})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad proof status = %d, want 401; body = %s", resp.StatusCode, raw)
	}
}

// TestReenrolmentWithAccessToken drives the dpop re-enrolment path: the current credential is
// presented as the access token plus a proof, not as a bootstrap token.
func TestReenrolmentWithAccessToken(t *testing.T) {
	h := newHarness(t)
	htu := h.srv.URL + "/v1/token"
	header := map[string]any{"alg": jose.AlgES256, "typ": "JWT"}
	assertion, _ := jose.SignES256(header, map[string]any{
		"sub": deviceID, "tenant_id": tenantID,
		"iat": fixedNow.Unix(), "exp": fixedNow.Add(time.Hour).Unix(),
	}, h.deviceKey)
	proof, _ := jose.SignES256(
		map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": h.jwk},
		map[string]any{"htm": "POST", "htu": htu, "iat": fixedNow.Unix(), "jti": "jti-reenrol"},
		h.deviceKey)
	_, raw := postJSON(t, htu, protocol.TokenRequest{GrantType: token.GrantTypeJWTBearer, Assertion: assertion},
		map[string]string{protocol.HeaderDPoP: proof})
	var tok protocol.TokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("decode token: %v", err)
	}

	enrolHTU := h.srv.URL + "/v1/enrol"
	// The one proof header authenticates the *current* credential: it is bound to the access token
	// through `ath` and keyed by the registered device key, which is also the key a re-enrolment
	// must re-register.
	sum := sha256.Sum256([]byte(tok.AccessToken))
	ath := base64.RawURLEncoding.EncodeToString(sum[:])
	enrolProof, _ := jose.SignES256(
		map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": h.jwk},
		map[string]any{"htm": "POST", "htu": enrolHTU, "iat": fixedNow.Unix(), "jti": "jti-enrol-proof", "ath": ath},
		h.deviceKey)
	resp, raw := postJSON(t, enrolHTU, protocol.EnrolmentRequest{
		SchemaVersion: protocol.EnrolmentSchemaVersion,
		Mode:          protocol.AuthModeDPoP,
		JWK:           &h.jwk,
		Device:        protocol.DeviceInfo{OS: "windows", HardwareIdentityHash: "hw-http"},
	}, map[string]string{
		protocol.HeaderAuthorization: "DPoP " + tok.AccessToken,
		protocol.HeaderDPoP:          enrolProof,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-enrolment status = %d, want 200; body = %s", resp.StatusCode, raw)
	}
	var out protocol.EnrolmentResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Reenrolled || out.DeviceID != deviceID {
		t.Fatalf("re-enrolment = %+v, want reenrolled on the existing device %s", out, deviceID)
	}
}
