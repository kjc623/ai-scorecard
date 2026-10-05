// Package idptest is a pair of in-process identity providers for tests: a fake Microsoft Entra ID
// (the organizations authority, whose discovery issuer is the {tenantid} template, per-tenant
// issuers, the common keys endpoint, and per-tenant token endpoints for app-only tokens) and a fake
// generic OpenID provider. Both run the real protocol over HTTP — discovery, JWKS, PKCE-checked
// authorization codes, RS256 id_tokens, refresh — so the relying party under test cannot tell them
// from the real thing except by their hosts.
//
// The browser step is a method rather than a redirect: Authorize takes the authorize URL the relying
// party built, checks it as a provider would, and returns the code and state the browser would have
// carried back.
package idptest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Identity is who signs in at the fake provider.
type Identity struct {
	// Subject is `sub` for OIDC and `oid` for Entra.
	Subject string
	// TenantID is the Entra tid.
	TenantID          string
	Email             string
	PreferredUsername string
	Roles             []string
	Extra             map[string]any
}

type grant struct {
	who       Identity
	redirect  string
	challenge string
	nonce     string
}

// IdP is one fake provider.
type IdP struct {
	Server       *httptest.Server
	Entra        bool
	ClientID     string
	ClientSecret string
	// AuthMethods is advertised as token_endpoint_auth_methods_supported when set.
	AuthMethods []string
	// Mutate edits every id_token's claims before signing, for the negative cases.
	Mutate func(claims map[string]any)
	// SignKid, when set, is the kid written in the header instead of the published one.
	SignKid string
	// RefreshTokens makes the code grant return a refresh token; FailRefresh refuses refreshes.
	RefreshTokens bool
	FailRefresh   bool
	Now           func() time.Time

	mu      sync.Mutex
	key     *rsa.PrivateKey
	kid     string
	codes   map[string]grant
	refresh map[string]Identity
	// Counters the tests read.
	JWKSHits, CodeGrants, RefreshGrants, ClientCredentialGrants int
	LastForm                                                    url.Values
	LastBasic                                                   [2]string
}

// NewOIDC starts a generic provider whose issuer is the server's URL.
func NewOIDC(t testing.TB, clientID, clientSecret string) *IdP {
	return start(t, false, clientID, clientSecret)
}

// NewEntra starts a fake Entra. Its base URL plays https://login.microsoftonline.com.
func NewEntra(t testing.TB, clientID, clientSecret string) *IdP {
	return start(t, true, clientID, clientSecret)
}

var (
	keyOnce   sync.Mutex
	sharedKey *rsa.PrivateKey
)

// testKey is generated once per test binary: RSA generation dominates a test's run time otherwise.
func testKey(t testing.TB) *rsa.PrivateKey {
	keyOnce.Lock()
	defer keyOnce.Unlock()
	if sharedKey == nil {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("idptest: generate key: %v", err)
		}
		sharedKey = k
	}
	return sharedKey
}

func start(t testing.TB, entra bool, clientID, secret string) *IdP {
	t.Helper()
	p := &IdP{
		Entra: entra, ClientID: clientID, ClientSecret: secret, Now: time.Now,
		key: testKey(t), kid: "k1", codes: map[string]grant{}, refresh: map[string]Identity{},
	}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Server.Close)
	return p
}

// Base is the provider's base URL (Entra: the login host; OIDC: the issuer).
func (p *IdP) Base() string { return p.Server.URL }

// Issuer is the OIDC issuer, or for Entra the issuer of one tenant.
func (p *IdP) Issuer(tid string) string {
	if p.Entra {
		return p.Server.URL + "/" + tid + "/v2.0"
	}
	return p.Server.URL
}

// RotateKey replaces the signing key and its kid; the JWKS publishes only the new one.
func (p *IdP) RotateKey(t testing.TB, kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("idptest: generate key: %v", err)
	}
	p.mu.Lock()
	p.key, p.kid = k, kid
	p.mu.Unlock()
}

// Counts returns the counters under the lock.
func (p *IdP) Counts() (jwks, code, refresh, clientCredentials int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.JWKSHits, p.CodeGrants, p.RefreshGrants, p.ClientCredentialGrants
}

// Form returns the last token request's form and basic credentials.
func (p *IdP) Form() (url.Values, [2]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.LastForm, p.LastBasic
}

func (p *IdP) authorizePath() string {
	if p.Entra {
		return "/organizations/oauth2/v2.0/authorize"
	}
	return "/authorize"
}

func (p *IdP) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case !p.Entra && path == "/.well-known/openid-configuration":
		doc := map[string]any{
			"issuer": p.Server.URL, "authorization_endpoint": p.Server.URL + "/authorize",
			"token_endpoint": p.Server.URL + "/token", "jwks_uri": p.Server.URL + "/jwks",
			"response_types_supported": []string{"code"}, "id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported": []string{"S256"},
		}
		if len(p.AuthMethods) > 0 {
			doc["token_endpoint_auth_methods_supported"] = p.AuthMethods
		}
		writeJSON(w, 200, doc)
	case p.Entra && path == "/organizations/v2.0/.well-known/openid-configuration":
		writeJSON(w, 200, map[string]any{
			"issuer":                                p.Server.URL + "/{tenantid}/v2.0",
			"authorization_endpoint":                p.Server.URL + "/organizations/oauth2/v2.0/authorize",
			"token_endpoint":                        p.Server.URL + "/organizations/oauth2/v2.0/token",
			"jwks_uri":                              p.Server.URL + "/organizations/discovery/v2.0/keys",
			"response_types_supported":              []string{"code", "id_token"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case p.Entra && strings.HasSuffix(path, "/v2.0/.well-known/openid-configuration"):
		tid := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/v2.0/.well-known/openid-configuration")
		writeJSON(w, 200, map[string]any{
			"issuer":                 p.Server.URL + "/" + tid + "/v2.0",
			"authorization_endpoint": p.Server.URL + "/" + tid + "/oauth2/v2.0/authorize",
			"token_endpoint":         p.Server.URL + "/" + tid + "/oauth2/v2.0/token",
			"jwks_uri":               p.Server.URL + "/" + tid + "/discovery/v2.0/keys",
		})
	case path == "/jwks" || strings.HasSuffix(path, "/discovery/v2.0/keys"):
		p.mu.Lock()
		p.JWKSHits++
		pub, kid := p.key.PublicKey, p.kid
		p.mu.Unlock()
		writeJSON(w, 200, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	case r.Method == http.MethodPost && (path == "/token" || strings.HasSuffix(path, "/oauth2/v2.0/token")):
		p.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Authorize plays the browser and the provider's sign-in page.
func (p *IdP) Authorize(t testing.TB, authorizeURL string, who Identity) (code, state string) {
	t.Helper()
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("idptest: authorize URL: %v", err)
	}
	if u.Scheme+"://"+u.Host != p.Server.URL || u.Path != p.authorizePath() {
		t.Fatalf("idptest: authorize URL %q is not this provider's authorize endpoint", authorizeURL)
	}
	q := u.Query()
	for _, want := range []struct{ k, v string }{{"response_type", "code"}, {"client_id", p.ClientID}, {"code_challenge_method", "S256"}} {
		if q.Get(want.k) != want.v {
			t.Fatalf("idptest: authorize %s = %q, want %q", want.k, q.Get(want.k), want.v)
		}
	}
	for _, k := range []string{"redirect_uri", "state", "nonce", "code_challenge", "scope"} {
		if q.Get(k) == "" {
			t.Fatalf("idptest: authorize request has no %s", k)
		}
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Fatalf("idptest: scope %q lacks openid", q.Get("scope"))
	}
	code = randomString()
	p.mu.Lock()
	p.codes[code] = grant{who: who, redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	p.mu.Unlock()
	return code, q.Get("state")
}

func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	user, pass, hasBasic := r.BasicAuth()
	if hasBasic {
		user, _ = url.QueryUnescape(user)
		pass, _ = url.QueryUnescape(pass)
	}
	p.mu.Lock()
	p.LastForm = r.PostForm
	p.LastBasic = [2]string{user, pass}
	p.mu.Unlock()

	form := r.PostForm
	clientID := form.Get("client_id")
	if hasBasic {
		clientID = user
	}
	if clientID != p.ClientID {
		oauthError(w, 401, "invalid_client")
		return
	}
	if !p.clientAuthenticated(form, hasBasic, pass) {
		oauthError(w, 401, "invalid_client")
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		p.mu.Lock()
		g, ok := p.codes[form.Get("code")]
		delete(p.codes, form.Get("code"))
		p.CodeGrants++
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || form.Get("redirect_uri") != g.redirect || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, 400, "invalid_grant")
			return
		}
		p.issue(w, g.who, g.nonce)
	case "refresh_token":
		p.mu.Lock()
		p.RefreshGrants++
		who, ok := p.refresh[form.Get("refresh_token")]
		delete(p.refresh, form.Get("refresh_token"))
		fail := p.FailRefresh
		p.mu.Unlock()
		if !ok || fail {
			oauthError(w, 400, "invalid_grant")
			return
		}
		p.issue(w, who, "")
	case "client_credentials":
		p.mu.Lock()
		p.ClientCredentialGrants++
		p.mu.Unlock()
		tid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/oauth2/v2.0/token")
		writeJSON(w, 200, map[string]any{"access_token": "app-token-" + tid, "token_type": "Bearer", "expires_in": 3600})
	default:
		oauthError(w, 400, "unsupported_grant_type")
	}
}

func (p *IdP) clientAuthenticated(form url.Values, hasBasic bool, basicSecret string) bool {
	if form.Get("client_assertion_type") == "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" && form.Get("client_assertion") != "" {
		return p.Entra
	}
	if p.ClientSecret == "" {
		return true
	}
	if hasBasic {
		return basicSecret == p.ClientSecret
	}
	return form.Get("client_secret") == p.ClientSecret
}

func (p *IdP) issue(w http.ResponseWriter, who Identity, nonce string) {
	now := p.Now()
	claims := map[string]any{
		"aud": p.ClientID, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if p.Entra {
		claims["iss"] = p.Server.URL + "/" + who.TenantID + "/v2.0"
		claims["tid"] = who.TenantID
		claims["oid"] = who.Subject
		claims["sub"] = "pairwise-" + who.Subject
		claims["ver"] = "2.0"
	} else {
		claims["iss"] = p.Server.URL
		claims["sub"] = who.Subject
	}
	if who.Email != "" {
		claims["email"] = who.Email
	}
	if who.PreferredUsername != "" {
		claims["preferred_username"] = who.PreferredUsername
	}
	if who.Roles != nil {
		claims["roles"] = who.Roles
	}
	for k, v := range who.Extra {
		claims[k] = v
	}
	if p.Mutate != nil {
		p.Mutate(claims)
	}
	p.mu.Lock()
	key, kid := p.key, p.kid
	if p.SignKid != "" {
		kid = p.SignKid
	}
	p.mu.Unlock()
	idToken := sign(key, kid, claims)
	body := map[string]any{"id_token": idToken, "access_token": "opaque-" + randomString(), "token_type": "Bearer", "expires_in": 3600}
	if p.RefreshTokens {
		rt := "rt-" + randomString()
		p.mu.Lock()
		p.refresh[rt] = who
		p.mu.Unlock()
		body["refresh_token"] = rt
	}
	writeJSON(w, 200, body)
}

// Sign makes an RS256 JWT with the given key, for tests that forge a token outright.
func sign(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	cb, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func randomString() string {
	var b [18]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": "idptest refused the request"})
}
