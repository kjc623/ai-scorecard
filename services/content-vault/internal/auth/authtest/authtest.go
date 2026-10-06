// Package authtest is a product token issuer for tests. It signs ES256 tokens the way control-api
// does and serves its JWKS from an httptest server. Only tests import it.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
)

// Issuer signs tokens and publishes the public key.
type Issuer struct {
	// URL is both the issuer (iss) and the base of the JWKS address.
	URL string
	key *ecdsa.PrivateKey
	kid string
	// Fetches counts JWKS requests.
	Fetches atomic.Int64
}

// New starts an issuer that is closed when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key, kid: "test-key-1"}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: iss.kid, Algorithm: string(jose.ES256), Use: "sig"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		iss.Fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	return iss
}

// Verifier returns a verifier for this issuer with the default audience.
func (i *Issuer) Verifier(t testing.TB) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(auth.Config{Issuer: i.URL})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Sign signs claims as they are, with this issuer's key and kid.
func (i *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	return SignWith(t, i.key, i.kid, claims)
}

// SignWith signs claims with any P-256 key and kid, for tokens the issuer did not mint.
func SignWith(t testing.TB, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("at+jwt").WithHeader(jose.HeaderKey("kid"), kid))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Claims returns the standard claims of a fresh token for this issuer and the vault's audience.
func (i *Issuer) Claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": i.URL,
		"aud": []string{"sac-query", auth.DefaultAudience, "sac-control"},
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"jti": "test",
	}
}

// Person mints a person's access token.
func (i *Issuer) Person(t testing.TB, tenantID, actor string, roles ...string) string {
	t.Helper()
	c := i.Claims()
	c["sub"] = "conn:" + actor
	c["sac_tenant"] = tenantID
	c["actor"] = actor
	c["roles"] = roles
	c["idp"] = "entra"
	c["sid"] = "0123456789abcdef"
	return i.Sign(t, c)
}

// Service mints a service token for svc.
func (i *Issuer) Service(t testing.TB, svc string) string {
	t.Helper()
	c := i.Claims()
	c["aud"] = auth.DefaultAudience
	c["sub"] = svc
	c["svc"] = svc
	return i.Sign(t, c)
}
