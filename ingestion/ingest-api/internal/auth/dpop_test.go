package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	testIssuer   = "https://control.example.com"
	testAudience = "ingest-api"
	testDPoPURL  = "https://ingest.example.com/v1/events"
)

// testDPoPNow is the fixed instant every DPoP fixture's clock reads, so token exp, proof iat and the
// replay window are deterministic.
var testDPoPNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// signer mints compact ES256 JWS tokens and proofs over arbitrary header/claims maps, so a test can
// vary exactly one field and assert the verifier notices.
type signer struct{ key *ecdsa.PrivateKey }

func (s signer) jws(t *testing.T, header, claims any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signing))
	r, sInt, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sInt.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func paddedCoordinate(v *big.Int) string {
	out := make([]byte, 32)
	v.FillBytes(out)
	return base64.RawURLEncoding.EncodeToString(out)
}

func publicJWK(pub *ecdsa.PublicKey) protocol.JWK {
	return protocol.JWK{Kty: "EC", Crv: "P-256", X: paddedCoordinate(pub.X), Y: paddedCoordinate(pub.Y)}
}

func jwkMap(j protocol.JWK) map[string]any {
	return map[string]any{"kty": j.Kty, "crv": j.Crv, "x": j.X, "y": j.Y}
}

func athFor(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type dpopFixture struct {
	t          *testing.T
	now        time.Time
	mem        *store.Memory
	tokenKey   *ecdsa.PrivateKey
	deviceKey  *ecdsa.PrivateKey
	jkt        string
	tenant     string
	device     string
	credential string
	proofSeq   int
}

func newDPoPFixture(t *testing.T) *dpopFixture {
	t.Helper()
	tokenKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("token key: %v", err)
	}
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	jkt, err := publicJWK(&deviceKey.PublicKey).Thumbprint()
	if err != nil {
		t.Fatalf("device jwk thumbprint: %v", err)
	}
	f := &dpopFixture{
		t: t, now: testDPoPNow,
		tokenKey: tokenKey, deviceKey: deviceKey, jkt: jkt,
		tenant:     "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		device:     "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		credential: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	f.mem = store.NewMemory(nil)
	f.mem.SetNow(func() time.Time { return f.now })
	f.mem.SetPrincipal(f.tenant, f.device, f.credential, store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true, CredentialType: "dpop",
		PublicKeyThumbprint: jkt, CredentialExpiry: f.now.Add(time.Hour),
	})
	return f
}

func (f *dpopFixture) authenticator() *DPoPAuthenticator {
	return &DPoPAuthenticator{
		Store: f.mem, TokenPublicKey: &f.tokenKey.PublicKey,
		Issuer: testIssuer, Audience: testAudience, Region: "eu-west",
		Now: func() time.Time { return f.now },
	}
}

func (f *dpopFixture) token(mutate func(map[string]any)) string {
	claims := map[string]any{
		"iss": testIssuer, "aud": testAudience, "sub": f.device,
		"tenant_id": f.tenant, "device_id": f.device, "credential_id": f.credential,
		"cnf": map[string]any{"jkt": f.jkt},
		"iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(),
		"jti": "token-1",
	}
	if mutate != nil {
		mutate(claims)
	}
	return signer{f.tokenKey}.jws(f.t, map[string]any{"alg": "ES256", "typ": "at+jwt", "kid": "k1"}, claims)
}

func (f *dpopFixture) newRequest(accessToken string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/events?x=1&y=2", nil)
	req.Host = "ingest.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set(protocol.HeaderAuthorization, "DPoP "+accessToken)
	return req
}

func (f *dpopFixture) proof(req *http.Request, accessToken string, key *ecdsa.PrivateKey, jwk map[string]any, mutate func(map[string]any)) string {
	if key == nil {
		key = f.deviceKey
	}
	if jwk == nil {
		jwk = jwkMap(publicJWK(&f.deviceKey.PublicKey))
	}
	f.proofSeq++
	claims := map[string]any{
		"htm": req.Method, "htu": testDPoPURL,
		"iat": f.now.Unix(), "jti": fmt.Sprintf("proof-%d", f.proofSeq),
		"ath": athFor(accessToken),
	}
	if mutate != nil {
		mutate(claims)
	}
	return signer{key}.jws(f.t, map[string]any{"typ": "dpop+jwt", "alg": "ES256", "jwk": jwk}, claims)
}

// readyRequest is a token and a matching proof, with optional mutations on either half.
func (f *dpopFixture) readyRequest(tokenMutate, proofMutate func(map[string]any)) *http.Request {
	accessToken := f.token(tokenMutate)
	req := f.newRequest(accessToken)
	req.Header.Set(protocol.HeaderDPoP, f.proof(req, accessToken, nil, nil, proofMutate))
	return req
}

func (f *dpopFixture) authenticate(req *http.Request) (Principal, error) {
	return f.authenticator().Authenticate(context.Background(), req)
}

func TestDPoPAuthenticatorAcceptsACorrectProof(t *testing.T) {
	f := newDPoPFixture(t)
	got, err := f.authenticate(f.readyRequest(nil, nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.TenantID != f.tenant || got.DeviceID != f.device || got.CredentialID != f.credential {
		t.Errorf("principal = %s/%s/%s, want the token's claims", got.TenantID, got.DeviceID, got.CredentialID)
	}
	if got.NotAfter.IsZero() {
		t.Error("the principal carries no credential expiry")
	}
}

func TestDPoPAuthenticatorRefusesABadProof(t *testing.T) {
	cases := []struct {
		name   string
		key    *ecdsa.PrivateKey
		jwk    map[string]any
		mutate func(map[string]any)
	}{
		{name: "wrong htm", mutate: func(c map[string]any) { c["htm"] = http.MethodGet }},
		{name: "wrong htu", mutate: func(c map[string]any) { c["htu"] = "https://evil.example.com/v1/events" }},
		{name: "wrong ath", mutate: func(c map[string]any) { c["ath"] = athFor("a different access token") }},
		{name: "missing jti", mutate: func(c map[string]any) { c["jti"] = "" }},
		{name: "stale iat", mutate: func(c map[string]any) { c["iat"] = testDPoPNow.Add(-10 * time.Minute).Unix() }},
		{name: "private jwk", jwk: map[string]any{
			"kty": "EC", "crv": "P-256", "x": "AA", "y": "AA", "d": "private-material",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDPoPFixture(t)
			accessToken := f.token(nil)
			req := f.newRequest(accessToken)
			req.Header.Set(protocol.HeaderDPoP, f.proof(req, accessToken, c.key, c.jwk, c.mutate))
			if _, err := f.authenticate(req); !errors.Is(err, ErrBadProof) {
				t.Fatalf("err = %v, want ErrBadProof", err)
			}
		})
	}
}

// TestDPoPAuthenticatorRefusesAProofKeyThatIsNotTheBoundKey signs a well-formed proof with a key
// whose thumbprint is not the token's cnf.jkt.
func TestDPoPAuthenticatorRefusesAProofKeyThatIsNotTheBoundKey(t *testing.T) {
	f := newDPoPFixture(t)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("other key: %v", err)
	}
	accessToken := f.token(nil)
	req := f.newRequest(accessToken)
	req.Header.Set(protocol.HeaderDPoP, f.proof(req, accessToken, other, jwkMap(publicJWK(&other.PublicKey)), nil))
	if _, err := f.authenticate(req); !errors.Is(err, ErrBadProof) {
		t.Fatalf("err = %v, want ErrBadProof for a mismatched proof key", err)
	}
}

func TestDPoPAuthenticatorRefusesABadToken(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://elsewhere.example.com" }},
		{"wrong audience", func(c map[string]any) { c["aud"] = "another-service" }},
		{"expired", func(c map[string]any) { c["exp"] = time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC).Unix() }},
		{"missing cnf", func(c map[string]any) { delete(c, "cnf") }},
		{"empty cnf jkt", func(c map[string]any) { c["cnf"] = map[string]any{"jkt": ""} }},
		{"sub disagrees with device_id", func(c map[string]any) { c["sub"] = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" }},
		{"tenant is not a uuid", func(c map[string]any) { c["tenant_id"] = "not-a-uuid" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDPoPFixture(t)
			if _, err := f.authenticate(f.readyRequest(c.mutate, nil)); !errors.Is(err, ErrBadAccessToken) {
				t.Fatalf("err = %v, want ErrBadAccessToken", err)
			}
		})
	}
}

// TestDPoPAuthenticatorRefusesABadTokenHeader covers the shared token format's protected header.
func TestDPoPAuthenticatorRefusesABadTokenHeader(t *testing.T) {
	f := newDPoPFixture(t)
	claims := map[string]any{
		"iss": testIssuer, "aud": testAudience, "sub": f.device,
		"tenant_id": f.tenant, "device_id": f.device, "credential_id": f.credential,
		"cnf": map[string]any{"jkt": f.jkt},
		"iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(), "jti": "t",
	}
	for _, typ := range []string{"JWT", "dpop+jwt", ""} {
		token := signer{f.tokenKey}.jws(t, map[string]any{"alg": "ES256", "typ": typ}, claims)
		req := f.newRequest(token)
		req.Header.Set(protocol.HeaderDPoP, f.proof(req, token, nil, nil, nil))
		if _, err := f.authenticate(req); !errors.Is(err, ErrBadAccessToken) {
			t.Fatalf("typ %q: err = %v, want ErrBadAccessToken", typ, err)
		}
	}
}

func TestDPoPAuthenticatorRefusesAReplayedJTI(t *testing.T) {
	f := newDPoPFixture(t)
	accessToken := f.token(nil)
	req := f.newRequest(accessToken)
	req.Header.Set(protocol.HeaderDPoP, f.proof(req, accessToken, nil, nil, nil))

	if _, err := f.authenticate(req); err != nil {
		t.Fatalf("first presentation: %v", err)
	}
	if _, err := f.authenticate(req); !errors.Is(err, ErrReplay) {
		t.Fatalf("second presentation: err = %v, want ErrReplay", err)
	}
}

func TestDPoPAuthenticatorRefusesAThumbprintOrTypeMismatch(t *testing.T) {
	t.Run("stored thumbprint differs", func(t *testing.T) {
		f := newDPoPFixture(t)
		// A new fixture re-registers the same credential with a wrong binding.
		f.mem.SetPrincipal(f.tenant, f.device, f.credential, store.PrincipalStatus{
			TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
			DeviceKnown: true, CredentialKnown: true, CredentialType: "dpop",
			PublicKeyThumbprint: "not-the-device-key", CredentialExpiry: f.now.Add(time.Hour),
		})
		if _, err := f.authenticate(f.readyRequest(nil, nil)); !errors.Is(err, ErrThumbprintMismatch) {
			t.Fatalf("err = %v, want ErrThumbprintMismatch", err)
		}
	})

	t.Run("stored credential type is x509", func(t *testing.T) {
		f := newDPoPFixture(t)
		f.mem.SetPrincipal(f.tenant, f.device, f.credential, store.PrincipalStatus{
			TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
			DeviceKnown: true, CredentialKnown: true, CredentialType: "x509",
			PublicKeyThumbprint: f.jkt, CredentialExpiry: f.now.Add(time.Hour),
		})
		if _, err := f.authenticate(f.readyRequest(nil, nil)); !errors.Is(err, ErrCredentialTypeMismatch) {
			t.Fatalf("err = %v, want ErrCredentialTypeMismatch", err)
		}
	})
}

func TestDPoPAuthenticatorRefusesAMissingProofHeader(t *testing.T) {
	f := newDPoPFixture(t)
	req := f.newRequest(f.token(nil)) // Authorization is present, DPoP is not
	if _, err := f.authenticate(req); !errors.Is(err, ErrBadProof) {
		t.Fatalf("err = %v, want ErrBadProof", err)
	}
}

func TestDPoPAuthenticatorPresents(t *testing.T) {
	f := newDPoPFixture(t)
	a := f.authenticator()

	if !a.Presents(f.newRequest("a-token")) {
		t.Error("Presents said no to Authorization: DPoP")
	}
	none := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	if a.Presents(none) {
		t.Error("Presents said yes to a request with no Authorization header")
	}
	bearer := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	bearer.Header.Set(protocol.HeaderAuthorization, "Bearer a-token")
	if a.Presents(bearer) {
		t.Error("Presents said yes to a bearer token")
	}
}
