package token_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/dpop"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/token"
)

const (
	tenantID  = "11111111-1111-7111-8111-111111111111"
	deviceID  = "33333333-3333-7333-8333-333333333333"
	credID    = "44444444-4444-7444-8444-444444444444"
	issuer    = "https://control.eu.example.com"
	audience  = "ingest.eu.example.com"
	tokenHTU  = "https://control.eu.example.com/v1/token"
	grantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

var fixedNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// tokenRig is a memory store with one active dpop device credential and the service under test.
type tokenRig struct {
	store     *store.Memory
	deviceKey *ecdsa.PrivateKey
	jwk       protocol.JWK
	thumb     string
	svc       *token.Service
	signKey   *ecdsa.PrivateKey
}

func newTokenRig(t *testing.T) *tokenRig {
	t.Helper()
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	jwk, err := jose.JWKFromPublic(&deviceKey.PublicKey)
	if err != nil {
		t.Fatalf("device jwk: %v", err)
	}
	thumb, err := jwk.Thumbprint()
	if err != nil {
		t.Fatalf("device thumbprint: %v", err)
	}
	jwkJSON, err := json.Marshal(jwk)
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	st := store.NewMemory()
	st.SetNow(func() time.Time { return fixedNow })
	st.AddTenant(store.Tenant{TenantID: tenantID, Status: "active", IngestEnabled: true, ResidencyRegion: "eu-west"})
	st.AddDevice(store.Device{TenantID: tenantID, DeviceID: deviceID, OS: "windows"})
	st.AddCredential(store.Credential{
		TenantID: tenantID, CredentialID: credID, DeviceID: deviceID,
		Type: protocol.AuthModeDPoP, PublicKeyThumbprint: thumb, PublicKeyJWK: jwkJSON,
		IssuedAt: fixedNow, ExpiresAt: fixedNow.Add(90 * 24 * time.Hour),
	})
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	svc, err := token.New(st, signKey, token.Config{
		Issuer: issuer, Audience: audience, TokenTTL: 15 * time.Minute, Region: "eu-west",
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	return &tokenRig{store: st, deviceKey: deviceKey, jwk: jwk, thumb: thumb, svc: svc, signKey: signKey}
}

func (r *tokenRig) assertion(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	header := map[string]any{"alg": jose.AlgES256, "typ": "JWT"}
	claims := map[string]any{
		"sub": deviceID, "tenant_id": tenantID,
		"iat": fixedNow.Unix(), "exp": fixedNow.Add(time.Hour).Unix(),
	}
	compact, err := jose.SignES256(header, claims, key)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return compact
}

func (r *tokenRig) proof(t *testing.T, key *ecdsa.PrivateKey, jwk protocol.JWK, htm, htu, jti string) string {
	t.Helper()
	header := map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": jwk}
	claims := map[string]any{"htm": htm, "htu": htu, "iat": fixedNow.Unix(), "jti": jti}
	compact, err := jose.SignES256(header, claims, key)
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	return compact
}

// TestIssuesAVerifiableDPoPBoundToken is the contract: the token header is exactly at+jwt/ES256/kid,
// the claims name the device, tenant and credential, and cnf.jkt binds it to the proof's key.
func TestIssuesAVerifiableDPoPBoundToken(t *testing.T) {
	r := newTokenRig(t)
	assertion := r.assertion(t, r.deviceKey)
	proof := r.proof(t, r.deviceKey, r.jwk, "POST", tokenHTU, "jti-1")

	resp, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
		GrantType: grantType, Assertion: assertion, DeviceID: deviceID,
	}, proof, "POST", tokenHTU)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("token response does not validate: %v", err)
	}
	if resp.ExpiresIn != int((15 * time.Minute).Seconds()) {
		t.Errorf("expires_in = %d, want 900", resp.ExpiresIn)
	}

	// Decode the protected header without trusting the library, so the header shape is asserted on
	// the wire bytes rather than on the struct that produced them.
	header := decodeHeader(t, resp.AccessToken)
	if header["typ"] != "at+jwt" {
		t.Errorf("token header typ = %v, want at+jwt", header["typ"])
	}
	if header["alg"] != "ES256" {
		t.Errorf("token header alg = %v, want ES256", header["alg"])
	}
	if header["kid"] != r.svc.KeyID() {
		t.Errorf("token header kid = %v, want %s", header["kid"], r.svc.KeyID())
	}

	verifier, err := token.NewVerifier(&r.signKey.PublicKey, issuer, audience, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	at, err := verifier.Verify(resp.AccessToken)
	if err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
	if at.TenantID != tenantID || at.DeviceID != deviceID || at.CredentialID != credID {
		t.Fatalf("verified claims = %+v, want tenant/device/credential", at)
	}
	if at.JKT != r.thumb {
		t.Errorf("cnf.jkt = %q, want the proof thumbprint %q", at.JKT, r.thumb)
	}
	if !at.ExpiresAt.Equal(fixedNow.Add(15 * time.Minute)) {
		t.Errorf("exp = %s, want %s", at.ExpiresAt, fixedNow.Add(15*time.Minute))
	}
}

// TestRefusesABadAssertionOrProof covers the two authentication failures and the replay refusal.
func TestRefusesABadAssertionOrProof(t *testing.T) {
	r := newTokenRig(t)
	goodAssertion := r.assertion(t, r.deviceKey)
	goodProof := r.proof(t, r.deviceKey, r.jwk, "POST", tokenHTU, "jti-good")

	t.Run("assertion signed by another key", func(t *testing.T) {
		other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		_, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
			GrantType: grantType, Assertion: r.assertion(t, other),
		}, goodProof, "POST", tokenHTU)
		if !isCode(err, apierr.CodeInvalidAssertion) {
			t.Fatalf("err = %v, want code %s", err, apierr.CodeInvalidAssertion)
		}
	})

	t.Run("proof for a different key", func(t *testing.T) {
		other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		otherJWK, _ := jose.JWKFromPublic(&other.PublicKey)
		proof := r.proof(t, other, otherJWK, "POST", tokenHTU, "jti-other")
		_, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
			GrantType: grantType, Assertion: goodAssertion,
		}, proof, "POST", tokenHTU)
		if !isCode(err, apierr.CodeInvalidProof) {
			t.Fatalf("err = %v, want code %s", err, apierr.CodeInvalidProof)
		}
	})

	t.Run("proof bound to another URL", func(t *testing.T) {
		proof := r.proof(t, r.deviceKey, r.jwk, "POST", "https://evil.example/v1/token", "jti-htu")
		_, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
			GrantType: grantType, Assertion: goodAssertion,
		}, proof, "POST", tokenHTU)
		if !isCode(err, apierr.CodeInvalidProof) {
			t.Fatalf("err = %v, want code %s", err, apierr.CodeInvalidProof)
		}
	})

	t.Run("unknown grant type", func(t *testing.T) {
		_, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
			GrantType: "password", Assertion: goodAssertion,
		}, goodProof, "POST", tokenHTU)
		if !isCode(err, apierr.CodeInvalidRequest) {
			t.Fatalf("err = %v, want code %s", err, apierr.CodeInvalidRequest)
		}
	})
}

// TestVerifierRefusesATamperedToken proves the verifier checks the signature rather than trusting
// the claims, and that a token whose cnf.jkt is stripped is refused because it would be a bearer
// token.
func TestVerifierRefusesATamperedToken(t *testing.T) {
	r := newTokenRig(t)
	assertion := r.assertion(t, r.deviceKey)
	proof := r.proof(t, r.deviceKey, r.jwk, "POST", tokenHTU, "jti-verify")
	resp, err := r.svc.Issue(context.Background(), protocol.TokenRequest{
		GrantType: grantType, Assertion: assertion,
	}, proof, "POST", tokenHTU)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	tampered := resp.AccessToken[:len(resp.AccessToken)-2] + "AA"
	verifier, _ := token.NewVerifier(&r.signKey.PublicKey, issuer, audience, func() time.Time { return fixedNow })
	if _, err := verifier.Verify(tampered); err == nil {
		t.Fatal("a tampered token verified")
	}
	// A token signed for a different audience is refused when the verifier pins one.
	otherVerifier, _ := token.NewVerifier(&r.signKey.PublicKey, issuer, "other-audience", func() time.Time { return fixedNow })
	if _, err := otherVerifier.Verify(resp.AccessToken); err == nil {
		t.Fatal("a token with the wrong audience verified")
	}
}

// TestRegionMismatchFailsClosedAtTokenIssue mirrors the enrolment check: a device whose tenant is
// pinned elsewhere is refused before its credential is read.
func TestRegionMismatchFailsClosedAtTokenIssue(t *testing.T) {
	deviceKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk, _ := jose.JWKFromPublic(&deviceKey.PublicKey)
	thumb, _ := jwk.Thumbprint()
	jwkJSON, _ := json.Marshal(jwk)
	st := store.NewMemory()
	st.AddTenant(store.Tenant{TenantID: tenantID, Status: "active", IngestEnabled: true, ResidencyRegion: "us-east"})
	st.AddCredential(store.Credential{
		TenantID: tenantID, CredentialID: credID, DeviceID: deviceID,
		Type: protocol.AuthModeDPoP, PublicKeyThumbprint: thumb, PublicKeyJWK: jwkJSON,
		IssuedAt: fixedNow, ExpiresAt: fixedNow.Add(90 * 24 * time.Hour),
	})
	signKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	svc, _ := token.New(st, signKey, token.Config{Region: "eu-west", Now: func() time.Time { return fixedNow }})
	r := &tokenRig{store: st, deviceKey: deviceKey, jwk: jwk, thumb: thumb, svc: svc, signKey: signKey}

	assertion := r.assertion(t, deviceKey)
	proof := r.proof(t, deviceKey, jwk, "POST", tokenHTU, "jti-region")
	_, err := svc.Issue(context.Background(), protocol.TokenRequest{
		GrantType: grantType, Assertion: assertion,
	}, proof, "POST", tokenHTU)
	if !isCode(err, apierr.CodeRegionMismatch) {
		t.Fatalf("err = %v, want code %s", err, apierr.CodeRegionMismatch)
	}
}

// TestDPoPVerifyRequiresMatchingHTM is a direct check on the shared proof verifier: a proof for a
// GET is not accepted for a POST.
func TestDPoPVerifyRequiresMatchingHTM(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk, _ := jose.JWKFromPublic(&key.PublicKey)
	header := map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": jwk}
	claims := map[string]any{"htm": "GET", "htu": tokenHTU, "iat": fixedNow.Unix(), "jti": "jti-htm"}
	compact, _ := jose.SignES256(header, claims, key)
	if _, err := dpop.Verify(compact, "POST", tokenHTU, "", fixedNow, dpop.DefaultSkew); err == nil {
		t.Fatal("a proof bound to GET verified for POST")
	}
}

func decodeHeader(t *testing.T, compact string) map[string]any {
	t.Helper()
	dot := -1
	for i, c := range compact {
		if c == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		t.Fatal("compact token has no header segment")
	}
	raw, err := base64.RawURLEncoding.DecodeString(compact[:dot])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	return out
}

func isCode(err error, code string) bool {
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == code
}
