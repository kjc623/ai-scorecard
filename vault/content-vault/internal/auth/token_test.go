package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/testrig"
)

// The product access token, verified by the vault itself (contract §2). Each test names one way a
// token can be wrong and asserts the refusal names that way, so a token refused for an incidental
// reason does not pass as coverage. query-api's test/auth.test.mjs holds the same cases.

func mustRefuse(t *testing.T, v *auth.TokenVerifier, token, want string) {
	t.Helper()
	_, err := v.Verify(context.Background(), token)
	if err == nil {
		t.Fatalf("token accepted; want a refusal mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not mention %q", err, want)
	}
}

func mustAccept(t *testing.T, v *auth.TokenVerifier, token string) auth.TokenClaims {
	t.Helper()
	c, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("token refused: %v", err)
	}
	return c
}

func TestAProductTokenBecomesTheSession(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	c := mustAccept(t, v, iss.Mint(t, nil, testrig.MintOptions{}))
	if c.TenantID != testrig.TenantID || c.Actor != "reader@lab.test" || c.Subject != testrig.IssuerSubject {
		t.Fatalf("claims %+v", c)
	}
	if len(c.Roles) != 1 || c.Roles[0] != "content_reader" {
		t.Fatalf("roles %v", c.Roles)
	}
	if c.SessionID != "0a1b2c3d4e5f6071" || c.IdP != "oidc" {
		t.Fatalf("sid %q idp %q", c.SessionID, c.IdP)
	}
	mustAccept(t, v, iss.Mint(t, map[string]any{"actor": "second@lab.test"}, testrig.MintOptions{}))
	if n := iss.Fetches(); n != 1 {
		t.Fatalf("the JWKS was fetched %d times for two tokens, want 1", n)
	}
	if v.Audience != "sac-vault" || v.JWKSURL != iss.URL+"/.well-known/jwks.json" {
		t.Fatalf("defaults: audience %q jwks %q", v.Audience, v.JWKSURL)
	}
}

func TestIssuerAndAudienceAreExact(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	mustRefuse(t, v, iss.Mint(t, map[string]any{"iss": iss.URL + "/"}, testrig.MintOptions{}), "iss")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"aud": []string{"sac-query", "sac-control"}}, testrig.MintOptions{}), "aud")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"aud": "sac-query"}, testrig.MintOptions{}), "aud")
	mustAccept(t, v, iss.Mint(t, map[string]any{"aud": "sac-vault"}, testrig.MintOptions{}))
}

func TestOnlyES256IsAccepted(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	unsigned := iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"alg": "none"}})
	mustRefuse(t, v, unsigned, "compact JWS")
	mustRefuse(t, v, unsigned+"AAAA", `alg "none"`)
	// HS256 keyed with the published public key: the classic alg-confusion forgery.
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"alg": "HS256"}, HMAC: []byte(iss.PublishedX())}), `alg "HS256"`)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"alg": "RS256"}, RSA: rsaKey}), `alg "RS256"`)
	if n := iss.Fetches(); n != 0 {
		t.Fatalf("a token with the wrong alg caused %d JWKS fetches, want 0", n)
	}
}

func TestTheHeaderMustBeTheIssuers(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"typ": "JWT"}}), "typ")
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"typ": testrig.Absent}}), "typ")
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"kid": testrig.Absent}}), "kid")
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"crit": []string{"x-ext"}, "x-ext": 1}}), "crit")
	mustAccept(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"typ": "application/AT+JWT"}}))
}

func TestTheSignatureIsChecked(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	good := iss.Mint(t, nil, testrig.MintOptions{})
	parts := strings.Split(good, ".")
	other := iss.Mint(t, map[string]any{"sac_tenant": testrig.OtherTenantID}, testrig.MintOptions{})
	mustRefuse(t, v, parts[0]+"."+strings.Split(other, ".")[1]+"."+parts[2], "signature does not verify")
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Key: testrig.ForeignKey(t)}), "signature does not verify")
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{DER: true}), "64-byte")
	// An embedded jwk is never read: the key comes only from the JWKS.
	foreign := testrig.ForeignKey(t)
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Key: foreign, Header: map[string]any{"jwk": map[string]any{"kty": "EC"}}}), "signature does not verify")
	// Padded or non-canonical base64url is not the issuer's encoding.
	mustRefuse(t, v, parts[0]+"=."+parts[1]+"."+parts[2], "base64url")
}

func TestAnOversizedBearerIsNotParsed(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	mustRefuse(t, v, iss.Mint(t, map[string]any{"actor": strings.Repeat("a", auth.MaxTokenBytes)}, testrig.MintOptions{}), "longer than")
	mustRefuse(t, v, "not-a-jwt", "compact JWS")
	if n := iss.Fetches(); n != 0 {
		t.Fatalf("%d JWKS fetches for tokens that never reached a key", n)
	}
}

func TestTimeClaimsHoldWithinTheLeeway(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	now := time.Now().Unix()
	mustRefuse(t, v, iss.Mint(t, map[string]any{"iat": now - 400, "exp": now - 120}, testrig.MintOptions{}), "expired")
	mustAccept(t, v, iss.Mint(t, map[string]any{"iat": now - 300, "exp": now - 30}, testrig.MintOptions{}))
	mustRefuse(t, v, iss.Mint(t, map[string]any{"exp": testrig.Absent}, testrig.MintOptions{}), "no exp")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"nbf": now + 300}, testrig.MintOptions{}), "nbf")
	mustAccept(t, v, iss.Mint(t, map[string]any{"nbf": now + 30}, testrig.MintOptions{}))
	// The ten-minute lifetime holds whatever exp says.
	mustRefuse(t, v, iss.Mint(t, map[string]any{"iat": now - 700, "exp": now + 3600}, testrig.MintOptions{}), "ten-minute")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"iat": now + 600}, testrig.MintOptions{}), "future")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"iat": testrig.Absent}, testrig.MintOptions{}), "no iat")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"exp": "9999999999"}, testrig.MintOptions{}), "malformed")
}

func TestAnUnknownKidRefetchesOnceThenRefuses(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	v.MinRefetch = -1 // no rate limit, so each step's single refetch is visible
	mustAccept(t, v, iss.Mint(t, nil, testrig.MintOptions{}))

	rotated := iss.Publish(t, "k2")
	mustAccept(t, v, iss.Mint(t, nil, testrig.MintOptions{Key: rotated, Header: map[string]any{"kid": "k2"}}))
	if n := iss.Fetches(); n != 2 {
		t.Fatalf("a rotation took %d fetches, want 2", n)
	}
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"kid": "nobody"}}), "not in the issuer's JWKS")
	if n := iss.Fetches(); n != 3 {
		t.Fatalf("a forged kid took %d fetches in total, want 3 (one refetch, then refused)", n)
	}
}

func TestKidMissRefetchesAreRateLimited(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	clock := time.Now()
	v.Now = func() time.Time { return clock }
	tokenAt := func(kid string) string {
		return iss.Mint(t, map[string]any{"iat": clock.Unix(), "exp": clock.Unix() + 300}, testrig.MintOptions{Header: map[string]any{"kid": kid}})
	}
	mustAccept(t, v, tokenAt("k1"))
	for i := 0; i < 5; i++ {
		mustRefuse(t, v, tokenAt("forged"), "not in the issuer's JWKS")
	}
	if n := iss.Fetches(); n != 1 {
		t.Fatalf("five forged kids inside the cooldown caused %d fetches, want 1", n)
	}
	clock = clock.Add(31 * time.Second)
	mustRefuse(t, v, tokenAt("forged"), "not in the issuer's JWKS")
	if n := iss.Fetches(); n != 2 {
		t.Fatalf("after the cooldown a forged kid caused %d fetches in total, want 2", n)
	}
}

func TestAStaleJWKSKeepsVerifyingThroughAnOutageButNotForEver(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	clock := time.Now()
	v.Now = func() time.Time { return clock }
	tokenAt := func() string {
		return iss.Mint(t, map[string]any{"iat": clock.Unix(), "exp": clock.Unix() + 300}, testrig.MintOptions{})
	}
	mustAccept(t, v, tokenAt())
	iss.SetJWKSStatus(http.StatusServiceUnavailable)
	clock = clock.Add(11 * time.Minute) // past the cache TTL: a refresh is attempted and fails
	mustAccept(t, v, tokenAt())
	clock = clock.Add(2 * time.Hour) // past MaxStale: the cached keys are no longer trusted
	mustRefuse(t, v, tokenAt(), "could not be loaded")
}

func TestOnlyUsableP256SigningKeysAreRead(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	v.MinRefetch = -1
	// An encryption key, an RSA key and an off-curve point under the kid a token names: none is
	// usable, so the token is refused rather than verified against any of them.
	iss.PublishJWK(map[string]any{"kty": "EC", "crv": "P-256", "kid": "bad", "use": "enc", "x": "AA", "y": "AA"})
	iss.PublishJWK(map[string]any{"kty": "RSA", "kid": "bad", "n": "AQAB", "e": "AQAB"})
	iss.PublishJWK(map[string]any{"kty": "EC", "crv": "P-256", "kid": "bad",
		"x": strings.Repeat("A", 43), "y": strings.Repeat("A", 43)})
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{Header: map[string]any{"kid": "bad"}}), "not in the issuer's JWKS")
	mustAccept(t, v, iss.Mint(t, nil, testrig.MintOptions{}))
}

func TestTheJWKSIsReadFromExactlyItsURL(t *testing.T) {
	iss := testrig.NewIssuer(t)
	redirect := httptest.NewServer(http.RedirectHandler(iss.URL+"/.well-known/jwks.json", http.StatusFound))
	defer redirect.Close()
	v := auth.NewTokenVerifier(iss.URL, "", redirect.URL)
	mustRefuse(t, v, iss.Mint(t, nil, testrig.MintOptions{}), "status 302")
}

func TestRolesAreTheProductSet(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	c := mustAccept(t, v, iss.Mint(t, map[string]any{"roles": []any{"analyst", "superuser", "dev", 7, "analyst"}}, testrig.MintOptions{}))
	if len(c.Roles) != 1 || c.Roles[0] != "analyst" {
		t.Fatalf("roles %v, want [analyst]: unknown dropped, duplicates collapsed", c.Roles)
	}
	mustRefuse(t, v, iss.Mint(t, map[string]any{"roles": []string{"superuser"}}, testrig.MintOptions{}), "no product role")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"roles": []string{}}, testrig.MintOptions{}), "no product role")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"roles": testrig.Absent}, testrig.MintOptions{}), "no product role")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"roles": "admin"}, testrig.MintOptions{}), "no product role")
}

func TestTenantActorAndSubjectAreRequired(t *testing.T) {
	iss := testrig.NewIssuer(t)
	v := iss.Verifier()
	mustRefuse(t, v, iss.Mint(t, map[string]any{"sac_tenant": "not-a-uuid"}, testrig.MintOptions{}), "sac_tenant")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"sac_tenant": testrig.Absent, "tid": testrig.TenantID}, testrig.MintOptions{}), "sac_tenant")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"actor": testrig.Absent, "preferred_username": "x@lab.test"}, testrig.MintOptions{}), "actor")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"actor": "evil\r\nX-Sac-Tenant: other"}, testrig.MintOptions{}), "actor")
	mustRefuse(t, v, iss.Mint(t, map[string]any{"sub": testrig.Absent}, testrig.MintOptions{}), "sub")
	c := mustAccept(t, v, iss.Mint(t, map[string]any{"sid": "not hex!", "sac_tenant": strings.ToUpper(testrig.TenantID)}, testrig.MintOptions{}))
	if c.SessionID != "" || c.TenantID != testrig.TenantID {
		t.Fatalf("sid %q tenant %q: a malformed sid is dropped, the tenant lower-cased", c.SessionID, c.TenantID)
	}
}

// ---------------------------------------------------------------------------------------
// TokenAuthenticator: the token is the person, and headers that disagree are refused
// ---------------------------------------------------------------------------------------

func tokenAuth(iss *testrig.Issuer) *auth.TokenAuthenticator {
	return &auth.TokenAuthenticator{Headers: auth.NewHeaderAuthenticator("query-api", "control-api", "ops"), Verifier: iss.Verifier()}
}

func request(h http.Header) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/content/retrieval", nil)
	r.Header = h
	return r
}

func TestATokenIsThePersonAndAgreeingHeadersPass(t *testing.T) {
	iss := testrig.NewIssuer(t)
	a := tokenAuth(iss)
	h := testrig.BearerHeaders(iss.Mint(t, map[string]any{"roles": []string{"analyst", "content_reader"}}, testrig.MintOptions{}))
	h.Set("X-Sac-Tenant", strings.ToUpper(testrig.TenantID))
	h.Set("X-Sac-Subject", "reader@lab.test")
	h.Set("X-Sac-Roles", testrig.JoinRoles("content_reader", "analyst"))
	p, err := a.Authenticate(request(h))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.Session || p.TenantID != testrig.TenantID || p.Subject != "reader@lab.test" || p.SessionID != "0a1b2c3d4e5f6071" || !p.HasAnyRole("content_reader") {
		t.Fatalf("principal %+v", p)
	}
	if !a.SessionRequired() {
		t.Fatal("a token authenticator must require a session on human routes")
	}
}

func TestHeadersThatDisagreeWithTheTokenAreRefusedNotMerged(t *testing.T) {
	iss := testrig.NewIssuer(t)
	a := tokenAuth(iss)
	token := iss.Mint(t, map[string]any{"roles": []string{"analyst"}}, testrig.MintOptions{})
	for name, set := range map[string][2]string{
		"another tenant":  {"X-Sac-Tenant", testrig.OtherTenantID},
		"another person":  {"X-Sac-Subject", "someone.else@lab.test"},
		"a wider role":    {"X-Sac-Roles", testrig.JoinRoles("analyst", "content_reader")},
		"a different one": {"X-Sac-Roles", "content_reader"},
	} {
		h := testrig.BearerHeaders(token)
		h.Set(set[0], set[1])
		if _, err := a.Authenticate(request(h)); !errors.Is(err, auth.ErrPrincipalConflict) {
			t.Errorf("%s: err %v, want ErrPrincipalConflict", name, err)
		}
	}
}

func TestUnderAnIssuerRolesComeOnlyFromATokenAndServicesStillWork(t *testing.T) {
	iss := testrig.NewIssuer(t)
	a := tokenAuth(iss)

	// A service acting for no person (control-api on the device path) authenticates as before.
	h := http.Header{}
	h.Set("X-Sac-Service", "control-api")
	h.Set("X-Sac-Subject", "control-api")
	h.Set("X-Sac-Tenant", testrig.TenantID)
	p, err := a.Authenticate(request(h))
	if err != nil || p.Session || len(p.Roles) != 0 {
		t.Fatalf("service caller: principal %+v err %v", p, err)
	}

	// The same headers naming roles, with no token, are refused: a role is a session's.
	h.Set("X-Sac-Roles", "content_reader")
	if _, err := a.Authenticate(request(h)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("header roles without a token: err %v, want ErrUnauthenticated", err)
	}

	for name, mutate := range map[string]func(http.Header){
		"not a bearer":          func(h http.Header) { h.Set("Authorization", "Basic dXNlcjpwYXNz") },
		"an unlisted service":   func(h http.Header) { h.Set("X-Sac-Service", "capture-extension") },
		"no service":            func(h http.Header) { h.Del("X-Sac-Service") },
		"an unverifiable token": func(h http.Header) { h.Set("Authorization", "Bearer a.b.c") },
	} {
		h := testrig.BearerHeaders(iss.Mint(t, nil, testrig.MintOptions{}))
		mutate(h)
		if _, err := a.Authenticate(request(h)); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: err %v, want ErrUnauthenticated", name, err)
		}
	}
}
