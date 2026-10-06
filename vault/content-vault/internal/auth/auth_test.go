package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/auth/authtest"
)

const tenant = "7d3c6a52-0b8e-4f0e-9a51-2a4c1f6b9e01"

func verify(t *testing.T, v *auth.Verifier, tok string) (auth.Claims, error) {
	t.Helper()
	return v.Verify(context.Background(), tok)
}

func TestAPersonTokenBecomesThePerson(t *testing.T) {
	iss := authtest.New(t)
	c, err := verify(t, iss.Verifier(t), iss.Person(t, tenant, "alice@example.com", "analyst", "content_reader", "auditor"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Person()
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != tenant || p.Actor != "alice@example.com" || p.SessionID != "0123456789abcdef" {
		t.Fatalf("person = %+v", p)
	}
	if len(p.Roles) != 2 || !p.HasAnyRole("content_reader") || p.HasAnyRole("auditor") {
		t.Fatalf("roles = %v: product roles kept, unknown roles dropped", p.Roles)
	}
}

func TestAServiceTokenNamesItsService(t *testing.T) {
	iss := authtest.New(t)
	c, err := verify(t, iss.Verifier(t), iss.Service(t, "control-api"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Service != "control-api" || c.Subject != "control-api" {
		t.Fatalf("claims = %+v", c)
	}
	if _, err := c.Person(); err == nil {
		t.Fatal("a service token was accepted as a person")
	}
}

func TestIssuerAndAudienceAreExact(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)

	wrongIss := iss.Claims()
	wrongIss["iss"] = iss.URL + "/"
	wrongIss["svc"] = "control-api"
	if _, err := verify(t, v, iss.Sign(t, wrongIss)); err == nil {
		t.Error("a token from another issuer verified")
	}

	for name, aud := range map[string]any{"another audience": "sac-query", "no audience": nil, "array without vault": []string{"sac-query", "sac-control"}} {
		c := iss.Claims()
		c["svc"] = "control-api"
		if aud == nil {
			delete(c, "aud")
		} else {
			c["aud"] = aud
		}
		if _, err := verify(t, v, iss.Sign(t, c)); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestTheSignatureIsChecked(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	forged := authtest.SignWith(t, other, "test-key-1", iss.Claims())
	if _, err := verify(t, v, forged); err == nil {
		t.Error("a token signed by another key under the issuer's kid verified")
	}
	unknownKid := authtest.SignWith(t, other, "someone-else", iss.Claims())
	if _, err := verify(t, v, unknownKid); err == nil {
		t.Error("a token naming an unpublished kid verified")
	}

	good := iss.Service(t, "control-api")
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + strings.TrimRight(parts[1], "A") + "B." + parts[2]
	if _, err := verify(t, v, tampered); err == nil {
		t.Error("a token with an edited payload verified")
	}
}

func TestOnlyES256IsAccepted(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
		(&jose.SignerOptions{}).WithType("at+jwt").WithHeader(jose.HeaderKey("kid"), "test-key-1"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := jwt.Signed(signer).Claims(iss.Claims()).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify(t, v, hs); err == nil {
		t.Error("an HS256 token verified")
	}
	none := "eyJhbGciOiJub25lIiwidHlwIjoiYXQrand0In0." + strings.Split(hs, ".")[1] + "."
	if _, err := verify(t, v, none); err == nil {
		t.Error("an unsigned token verified")
	}
}

func TestLifetimeIsEnforced(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)
	now := time.Now()
	cases := map[string]func(map[string]any){
		"expired":          func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() },
		"no exp":           func(c map[string]any) { delete(c, "exp") },
		"no iat":           func(c map[string]any) { delete(c, "iat") },
		"issued in future": func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() },
		"older than 10m": func(c map[string]any) {
			c["iat"] = now.Add(-12 * time.Minute).Unix()
			c["exp"] = now.Add(time.Hour).Unix()
		},
		"not yet valid (nb)": func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() },
	}
	for name, edit := range cases {
		c := iss.Claims()
		c["svc"] = "control-api"
		edit(c)
		if _, err := verify(t, v, iss.Sign(t, c)); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestAPersonNeedsTenantActorAndRole(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)
	cases := map[string]func(map[string]any){
		"no tenant":        func(c map[string]any) { delete(c, "sac_tenant") },
		"tenant not uuid":  func(c map[string]any) { c["sac_tenant"] = "acme" },
		"no actor":         func(c map[string]any) { delete(c, "actor") },
		"control in actor": func(c map[string]any) { c["actor"] = "alice\nbob" },
		"no product role":  func(c map[string]any) { c["roles"] = []string{"auditor"} },
		"service claim":    func(c map[string]any) { c["svc"] = "control-api" },
	}
	for name, edit := range cases {
		c := iss.Claims()
		c["sub"] = "conn:alice"
		c["sac_tenant"] = tenant
		c["actor"] = "alice"
		c["roles"] = []string{"analyst"}
		edit(c)
		claims, err := verify(t, v, iss.Sign(t, c))
		if err != nil {
			t.Errorf("%s: the token itself should verify: %v", name, err)
			continue
		}
		if _, err := claims.Person(); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: Person() = %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestTheTenantIsNormalised(t *testing.T) {
	iss := authtest.New(t)
	c, err := verify(t, iss.Verifier(t), iss.Person(t, strings.ToUpper(tenant), "alice", "viewer"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.Person(); err != nil || p.TenantID != tenant {
		t.Fatalf("Person = %+v, %v", p, err)
	}
}

func TestTheJWKSIsFetchedOnceAndCached(t *testing.T) {
	iss := authtest.New(t)
	v := iss.Verifier(t)
	for range 3 {
		if _, err := verify(t, v, iss.Service(t, "control-api")); err != nil {
			t.Fatal(err)
		}
	}
	if n := iss.Fetches.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times for three tokens under one kid, want 1", n)
	}
}

func TestAnOversizedBearerIsNotParsed(t *testing.T) {
	iss := authtest.New(t)
	if _, err := verify(t, iss.Verifier(t), strings.Repeat("a", 9<<10)); err == nil {
		t.Fatal("an oversized bearer verified")
	}
	if iss.Fetches.Load() != 0 {
		t.Fatal("an oversized bearer caused a JWKS fetch")
	}
}

func TestBearer(t *testing.T) {
	for header, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", header)
		if got, err := auth.Bearer(r); err != nil || got != want {
			t.Errorf("Bearer(%q) = %q, %v", header, got, err)
		}
	}
	for _, header := range []string{"", "Basic abc", "Bearer", "Bearer   ", "abc"} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, err := auth.Bearer(r); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Bearer(%q) = %v, want ErrUnauthenticated", header, err)
		}
	}
}

func TestNewVerifierNeedsAnIssuer(t *testing.T) {
	if _, err := auth.NewVerifier(auth.Config{}); err == nil {
		t.Fatal("a verifier without an issuer was built")
	}
	v, err := auth.NewVerifier(auth.Config{Issuer: "https://control-api.internal/"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Audience != "sac-vault" || v.JWKSURL != "https://control-api.internal/.well-known/jwks.json" {
		t.Fatalf("defaults = %q, %q", v.Audience, v.JWKSURL)
	}
}
