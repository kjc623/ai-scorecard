package session_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/shadow-ai-capture/control-api/internal/session"
	"github.com/shadow-ai-capture/control-api/internal/session/sessiontest"
)

const (
	tenant = "aaaaaaaa-0000-4000-8000-000000000001"
	conn   = "cccccccc-0000-4000-8000-00000000000c"
	iss    = "http://control-api:8080"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func newKey(t *testing.T) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pemOf(t *testing.T, k *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func principal() session.Principal {
	return session.Principal{Tenant: tenant, Actor: "alice@contoso.example", Roles: []string{"admin", "viewer", "viewer", "root"},
		IdP: "entra", Subject: conn + ":oid", SessionID: "0123456789abcdef"}
}

func newIssuer(t *testing.T, c *clock, keys *session.KeySet) *session.Issuer {
	is, err := session.NewIssuer(keys, session.IssuerConfig{Issuer: iss + "/", Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return is
}

// unverified splits a token into its one protected header and its claims, without verifying it.
func unverified(t *testing.T, tok string) (jose.Header, map[string]json.RawMessage) {
	t.Helper()
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]json.RawMessage
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		t.Fatal(err)
	}
	return parsed.Headers[0], claims
}

func TestMintedTokenShapeAndVerify(t *testing.T) {
	c := &clock{t: time.Now().UTC()}
	keys, err := session.ParseKeys(pemOf(t, newKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	is := newIssuer(t, c, keys)
	tok, exp, err := is.Mint(principal())
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(c.t); d > session.TokenTTL || d < session.TokenTTL-time.Second || session.TokenTTL > session.MaxTokenTTL {
		t.Fatalf("exp is %s away", exp.Sub(c.t))
	}
	h, claims := unverified(t, tok)
	if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != "at+jwt" {
		t.Fatalf("typ = %q", typ)
	}
	if h.KeyID != keys.SigningKeyID() {
		t.Fatalf("kid = %q", h.KeyID)
	}
	var aud []string
	_ = json.Unmarshal(claims["aud"], &aud)
	if strings.Join(aud, ",") != "sac-query,sac-vault,sac-control" {
		t.Fatalf("aud = %v", aud)
	}
	for _, claim := range []string{"iss", "sub", "sac_tenant", "actor", "roles", "idp", "sid", "iat", "exp", "jti"} {
		if _, ok := claims[claim]; !ok {
			t.Fatalf("token lacks %s", claim)
		}
	}
	var gotIss string
	_ = json.Unmarshal(claims["iss"], &gotIss)
	if gotIss != iss {
		t.Fatalf("iss = %q (the trailing slash must not survive)", gotIss)
	}
	v := session.NewVerifier(is)
	p, err := v.Verify(tok, session.AudienceVault)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tenant != tenant || strings.Join(p.Roles, ",") != "viewer,admin" || p.SessionID != "0123456789abcdef" || p.Subject != conn+":oid" {
		t.Fatalf("principal = %+v", p)
	}
}

func TestServiceToken(t *testing.T) {
	c := &clock{t: time.Now().UTC()}
	keys, _ := session.NewKeySet(newKey(t))
	is := newIssuer(t, c, keys)
	tok, err := is.ServiceToken(session.AudienceVault, "control-api")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := unverified(t, tok)
	if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != "at+jwt" || h.KeyID != keys.SigningKeyID() {
		t.Fatalf("header = %+v", h)
	}
	// A verifier holds only the published JWKS.
	jwks := keys.JWKS()
	pub := jwks.Key(h.KeyID)
	if len(pub) != 1 {
		t.Fatalf("the signing kid is not in the JWKS")
	}
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var registered jwt.Claims
	var raw map[string]json.RawMessage
	if err := parsed.Claims(pub[0].Key, &registered, &raw); err != nil {
		t.Fatalf("does not verify against the JWKS: %v", err)
	}
	if err := registered.ValidateWithLeeway(jwt.Expected{Issuer: iss, AnyAudience: jwt.Audience{"sac-vault"}, Subject: "control-api", Time: c.t}, 0); err != nil {
		t.Fatal(err)
	}
	if string(raw["aud"]) != `"sac-vault"` || string(raw["svc"]) != `"control-api"` {
		t.Fatalf("aud = %s, svc = %s", raw["aud"], raw["svc"])
	}
	if life := registered.Expiry.Time().Sub(registered.IssuedAt.Time()); life <= 0 || life > 5*time.Minute {
		t.Fatalf("life = %s", life)
	}
	for _, claim := range []string{"sac_tenant", "actor", "roles"} {
		if _, ok := raw[claim]; ok {
			t.Fatalf("a service token carries %s", claim)
		}
	}
	if _, err := session.NewVerifier(is).Verify(tok, session.AudienceVault); !errors.Is(err, session.ErrInvalidToken) {
		t.Fatalf("a service token passed as a product token: %v", err)
	}
	if _, err := is.ServiceToken("", "control-api"); err == nil {
		t.Fatal("a service token without an audience was minted")
	}
}

func TestVerifyRefusals(t *testing.T) {
	c := &clock{t: time.Now().UTC()}
	key := newKey(t)
	keys, _ := session.NewKeySet(key)
	is := newIssuer(t, c, keys)
	v := session.NewVerifier(is)
	tok, _, err := is.Mint(principal())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := session.NewKeySet(newKey(t))
	foreign, _, _ := newIssuer(t, c, other).Mint(principal())
	otherIss, _ := session.NewIssuer(keys, session.IssuerConfig{Issuer: "http://elsewhere:8080", Now: c.Now})
	wrongIss, _, _ := otherIss.Mint(principal())
	wrongTyp, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: keys.SigningKeyID()}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	otherTyped, _ := jwt.Signed(wrongTyp).Claims(map[string]any{"iss": iss}).Serialize()
	parts := strings.Split(tok, ".")
	hs256 := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"at+jwt","kid":"`+keys.SigningKeyID()+`"}`)) + "." + parts[1] + "." + parts[2]
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"at+jwt"}`)) + "." + parts[1] + "."
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+iss+`","aud":["sac-query"],"sac_tenant":"`+tenant+`","actor":"x","roles":["admin"],"idp":"entra","sub":"s","exp":9999999999}`)) + "." + parts[2]

	cases := map[string]struct {
		token, aud string
	}{
		"wrong audience":   {tok, "sac-other"},
		"unknown key":      {foreign, session.AudienceQuery},
		"wrong issuer":     {wrongIss, session.AudienceQuery},
		"another typ":      {otherTyped, session.AudienceQuery},
		"hs256 alg":        {hs256, session.AudienceQuery},
		"none alg":         {none, session.AudienceQuery},
		"tampered payload": {tampered, session.AudienceQuery},
		"garbage":          {"a.b", session.AudienceQuery},
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(cs.token, cs.aud); !errors.Is(err, session.ErrInvalidToken) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("expiry with leeway", func(t *testing.T) {
		c.t = c.t.Add(session.TokenTTL + 30*time.Second)
		if _, err := v.Verify(tok, session.AudienceQuery); err != nil {
			t.Fatalf("inside the leeway: %v", err)
		}
		c.t = c.t.Add(31 * time.Second)
		if _, err := v.Verify(tok, session.AudienceQuery); !errors.Is(err, session.ErrInvalidToken) {
			t.Fatalf("past the leeway: %v", err)
		}
	})
}

func TestMintRefusesAnIncompletePrincipal(t *testing.T) {
	keys, _ := session.NewKeySet(newKey(t))
	is := newIssuer(t, &clock{t: time.Now()}, keys)
	for name, mut := range map[string]func(*session.Principal){
		"no tenant": func(p *session.Principal) { p.Tenant = "" },
		"no role":   func(p *session.Principal) { p.Roles = []string{"root"} },
		"no actor":  func(p *session.Principal) { p.Actor = " " },
		"bad idp":   func(p *session.Principal) { p.IdP = "saml" },
	} {
		p := principal()
		mut(&p)
		if _, _, err := is.Mint(p); err == nil {
			t.Fatalf("%s: minted", name)
		}
	}
}

func TestRotationPublishesBothKeys(t *testing.T) {
	c := &clock{t: time.Now().UTC()}
	oldKey, newKeyV := newKey(t), newKey(t)
	oldKeys, _ := session.NewKeySet(oldKey)
	minted, _, err := newIssuer(t, c, oldKeys).Mint(principal())
	if err != nil {
		t.Fatal(err)
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(&oldKey.PublicKey)
	file := append(pemOf(t, newKeyV), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})...)
	rotated, err := session.ParseKeys(file)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.SigningKeyID() == oldKeys.SigningKeyID() {
		t.Fatal("the first private key in the file must sign")
	}
	raw, _ := json.Marshal(rotated.JWKS())
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Keys) != 2 {
		t.Fatalf("JWKS lists %d keys, want 2 (%v)", len(doc.Keys), err)
	}
	if _, err := session.NewVerifier(newIssuer(t, c, rotated)).Verify(minted, session.AudienceQuery); err != nil {
		t.Fatalf("a token under the previous key no longer verifies: %v", err)
	}
	if _, err := session.ParseKeys(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})); err == nil {
		t.Fatal("a file with no private key was accepted")
	}
}

// TestOpenSSLKeyFile reads what `openssl ecparam -name prime256v1 -genkey` writes: the curve's OID
// block ahead of the key.
func TestOpenSSLKeyFile(t *testing.T) {
	prime256v1 := []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
	file := append(pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: prime256v1}), pemOf(t, newKey(t))...)
	if _, err := session.ParseKeys(file); err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	if _, err := session.ParseKeys(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}})); err == nil {
		t.Fatal("an unexpected block was accepted")
	}
}

func TestWellKnown(t *testing.T) {
	keys, _ := session.NewKeySet(newKey(t))
	is := newIssuer(t, &clock{t: time.Now()}, keys)
	h := is.WellKnownHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, session.PathJWKS, nil))
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil || rec.Code != 200 || len(jwks.Keys) != 1 {
		t.Fatalf("jwks: %d %s", rec.Code, rec.Body)
	}
	k := jwks.Keys[0]
	if k["kty"] != "EC" || k["crv"] != "P-256" || k["alg"] != "ES256" || k["use"] != "sig" || k["kid"] != keys.SigningKeyID() || k["d"] != "" {
		t.Fatalf("jwk = %v", k)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, session.PathDiscovery, nil))
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc["issuer"] != iss || doc["jwks_uri"] != iss+session.PathJWKS {
		t.Fatalf("discovery = %v", doc)
	}
}

func TestManagerBounds(t *testing.T) {
	c := &clock{t: time.Now().UTC()}
	st := sessiontest.NewStore()
	m, err := session.NewManager(st, session.ManagerConfig{MaxAge: 24 * time.Hour, Idle: 3 * time.Hour, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, rec, err := m.Create(ctx, session.Record{TenantID: tenant, ConnectionID: conn, Subject: "s", Actor: "a", Roles: []string{"viewer"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExpiresAt.Sub(rec.CreatedAt) != session.MaxAge {
		t.Fatalf("a 24h max age was not clamped to %s", session.MaxAge)
	}
	stored, _ := st.ByHash(ctx, session.HashID(id))
	if strings.Contains(string(stored.Hash), id) || len(stored.Hash) != 32 {
		t.Fatal("the session id itself must never be stored")
	}
	c.t = c.t.Add(session.IdleTimeout)
	if _, err := m.Resume(ctx, id); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("a 3h idle was not clamped to %s: %v", session.IdleTimeout, err)
	}
	if _, err := m.Resume(ctx, "unknown"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("unknown: %v", err)
	}
	if _, _, err := m.Create(ctx, session.Record{TenantID: "not-a-uuid", ConnectionID: conn, Subject: "s", Actor: "a"}); err == nil {
		t.Fatal("a session without a tenant uuid was created")
	}
}
