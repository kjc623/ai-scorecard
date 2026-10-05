// Command oidc is the local OpenID Connect stand-in the device-auth lab runs so tasks 11+
// can exercise a real sign-in without a customer identity provider.
//
// It is deliberately a stand-in, not a second authentication path, and it lives in localdev/
// for the same reason the edge and contentlab do: nothing a service imports, no service has
// an "if lab" branch. What it does provide is the real protocol shape the product is built
// against:
//
//   - discovery at /.well-known/openid-configuration, a JWKS at /jwks, and RS256-signed
//     id_token and access_token JWTs, so query-api verifies a signed token rather than
//     trusting a header;
//   - an authorization-code flow with PKCE (S256), so the dashboard holds a session and the
//     browser never sees a tenant or a role it could choose;
//   - a configurable directory of users, each with a shadow tenant and an app role, so the
//     lab can sign in as a viewer, an analyst, a content reader or an admin.
//
// What it does NOT do, and must not be mistaken for: it has no passwords, no consent screen
// and no refresh tokens; it signs with a key generated fresh at every start (so a restart
// invalidates every session), and it accepts any loopback redirect_uri. It is a lab fixture.
//
// The two claims the product reads are `roles` (an array of app-role names) and `sac_tenant`
// (the shadow tenant UUID). A real Entra ID tenant supplies the first through app roles and
// the second through a claims-mapping policy or a directory extension; the owner's hand-off
// names that configuration.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr     = "0.0.0.0:8080"
	defaultIssuer   = "http://oidc:8080"
	defaultAudience = "sac-query-api"
	defaultClientID = "sac-dashboard"
	// authCodeTTL bounds an issued authorization code. A sign-in round trip is seconds.
	authCodeTTL = 2 * time.Minute
	// accessTokenTTL and idTokenTTL are short on purpose: human revocation is near-real-time
	// (docs/06 §4.1) and a lab session should not outlive the browser run that created it.
	accessTokenTTL = 15 * time.Minute
	idTokenTTL     = 15 * time.Minute
	// keyBits for the RS256 signing key, matching what Entra ID presents.
	keyBits = 2048
)

// user is one directory entry: an account, the role it carries and the shadow tenant it
// belongs to. `sub` is the actor id the audit trail records.
type user struct {
	Sub    string `json:"sub"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Tenant string `json:"tenant"`
}

// config is the process's whole state, read from flags and environment.
type config struct {
	addr         string
	issuer       string
	audience     string
	clientID     string
	clientSecret string
	users        []user
}

// authCode is a single-use authorization code with the PKCE challenge bound to it.
type authCode struct {
	user          user
	redirectURI   string
	codeChallenge string
	nonce         string
	expiresAt     time.Time
}

// server is the lab issuer. The signing key is generated at start; the code store is in
// memory because the lab has one process and no shared state.
type server struct {
	cfg config
	key *rsa.PrivateKey
	kid string
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	codes map[string]authCode
}

func main() {
	var (
		addr         = flag.String("addr", envOr("OIDC_ADDR", defaultAddr), "listen address")
		issuer       = flag.String("issuer", os.Getenv("OIDC_ISSUER"), "public issuer base URL")
		audience     = flag.String("audience", envOr("OIDC_API_AUDIENCE", defaultAudience), "access-token audience (query-api)")
		clientID     = flag.String("client-id", envOr("OIDC_CLIENT_ID", defaultClientID), "dashboard client id")
		clientSecret = flag.String("client-secret", os.Getenv("OIDC_CLIENT_SECRET"), "client secret (empty = public client with PKCE)")
	)
	flag.Parse()

	if *issuer == "" {
		*issuer = defaultIssuer
	}

	users, err := usersFromEnv(os.Getenv("OIDC_USERS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "oidc: %v\n", err)
		os.Exit(1)
	}
	if len(users) == 0 {
		fmt.Fprintln(os.Stderr, "oidc: OIDC_USERS names no users; set it, for example "+
			`'[{"sub":"viewer@lab.test","name":"Viewer","role":"viewer","tenant":"11111111-1111-1111-1111-111111111111"}]'`)
		os.Exit(1)
	}

	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oidc: cannot generate a signing key: %v\n", err)
		os.Exit(1)
	}

	s := &server{
		cfg: config{
			addr: *addr, issuer: strings.TrimRight(*issuer, "/"),
			audience: *audience, clientID: *clientID, clientSecret: *clientSecret, users: users,
		},
		key: key, kid: kidOf(&key.PublicKey), log: slog.Default(), now: time.Now,
		codes: map[string]authCode{},
	}

	httpServer := &http.Server{
		Addr:              s.cfg.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.log.Info("oidc: listening", "addr", s.cfg.addr, "issuer", s.cfg.issuer,
		"audience", s.cfg.audience, "client_id", s.cfg.clientID, "users", len(s.cfg.users))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Error("oidc: server stopped", "error", err)
		os.Exit(1)
	}
}

// Handler returns the stand-in's routes. It is a method so a test can drive the real surface
// without a listener.
func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /jwks", s.handleJWKS)
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

func kidOf(pub *rsa.PublicKey) string {
	sum := sha256.Sum256(x509.MarshalPKCS1PublicKey(pub))
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// usersFromEnv parses OIDC_USERS, a JSON array of user objects. An error is fatal rather
// than defaulted: a lab issuer with no directory would sign in nobody.
func usersFromEnv(raw string) ([]user, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var in []user
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("OIDC_USERS is not a JSON array of users: %w", err)
	}
	for i, u := range in {
		if u.Sub == "" || u.Tenant == "" || u.Role == "" {
			return nil, fmt.Errorf("OIDC_USERS[%d] needs sub, role and tenant", i)
		}
	}
	return in, nil
}

// ---------------------------------------------------------------------------------------
// Discovery and keys
// ---------------------------------------------------------------------------------------

func (s *server) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.cfg.issuer,
		"authorization_endpoint":                s.cfg.issuer + "/authorize",
		"token_endpoint":                        s.cfg.issuer + "/token",
		"jwks_uri":                              s.cfg.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code"},
	})
}

func (s *server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	e := big.NewInt(int64(s.key.PublicKey.E))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
			"n": base64.RawURLEncoding.EncodeToString(s.key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(e.Bytes()),
		}},
	})
}

// ---------------------------------------------------------------------------------------
// Authorization code flow
// ---------------------------------------------------------------------------------------

// handleAuthorize issues a code for login_hint's user. There is no password and no consent
// screen: the lab picks the account by login_hint. Without one it renders the directory so
// a person can choose, which is what a human sees when they open the issuer directly.
func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "redirect_uri is required"})
		return
	}
	if q.Get("response_type") != "code" {
		s.redirectError(w, r, redirectURI, "unsupported_response_type", "only response_type=code is served")
		return
	}
	if q.Get("client_id") != s.cfg.clientID {
		s.redirectError(w, r, redirectURI, "invalid_client", "unknown client_id")
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		s.redirectError(w, r, redirectURI, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}

	hint := strings.TrimSpace(q.Get("login_hint"))
	u, ok := s.findUser(hint)
	if !ok {
		// A directory page rather than a JSON error: the only way to reach here in the lab is
		// a person opening the issuer in a browser, and a list is more useful than a code.
		s.writeUsersPage(w, r)
		return
	}

	code := randomToken(24)
	s.mu.Lock()
	s.codes[code] = authCode{
		user: u, redirectURI: redirectURI,
		codeChallenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
		expiresAt: s.now().Add(authCodeTTL),
	}
	s.mu.Unlock()

	back, err := url.Parse(redirectURI)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "redirect_uri is not a URL"})
		return
	}
	params := back.Query()
	params.Set("code", code)
	if state := q.Get("state"); state != "" {
		params.Set("state", state)
	}
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// handleToken exchanges a code (whose PKCE verifier the caller proves) for an access token
// for the API audience and an id token for the client.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the token request is not form-encoded")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		s.writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is served")
		return
	}
	if s.cfg.clientSecret != "" && r.PostForm.Get("client_secret") != s.cfg.clientSecret {
		s.writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_secret does not match")
		return
	}

	code := r.PostForm.Get("code")
	s.mu.Lock()
	entry, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // single-use, redeemed or expired
	}
	s.mu.Unlock()
	if !ok || s.now().After(entry.expiresAt) {
		s.writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
		return
	}
	if r.PostForm.Get("redirect_uri") != entry.redirectURI {
		s.writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the one the code was issued for")
		return
	}
	if !pkceMatches(entry.codeChallenge, r.PostForm.Get("code_verifier")) {
		s.writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the challenge")
		return
	}

	now := s.now()
	access, err := s.signJWT(map[string]any{
		"iss": s.cfg.issuer, "aud": s.cfg.audience, "sub": entry.user.Sub,
		"email": entry.user.Email, "name": entry.user.Name,
		"roles": []string{entry.user.Role}, "sac_tenant": entry.user.Tenant,
		"iat": now.Unix(), "exp": now.Add(accessTokenTTL).Unix(),
	})
	if err != nil {
		s.writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	idClaims := map[string]any{
		"iss": s.cfg.issuer, "aud": s.cfg.clientID, "sub": entry.user.Sub,
		"email": entry.user.Email, "name": entry.user.Name,
		"roles": []string{entry.user.Role}, "sac_tenant": entry.user.Tenant,
		"iat": now.Unix(), "exp": now.Add(idTokenTTL).Unix(),
	}
	if entry.nonce != "" {
		idClaims["nonce"] = entry.nonce
	}
	id, err := s.signJWT(idClaims)
	if err != nil {
		s.writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "id_token": id,
		"token_type": "Bearer", "expires_in": int(accessTokenTTL.Seconds()),
	})
}

// pkceMatches is RFC 7636 S256: base64url(sha256(verifier)) == challenge.
func pkceMatches(challenge, verifier string) bool {
	if verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// signJWT signs an RS256 JWT with the process's key. The three parts are base64url without
// padding, which is what every verifier expects.
func (s *server) signJWT(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *server) findUser(hint string) (user, bool) {
	hint = strings.ToLower(strings.TrimSpace(hint))
	for _, u := range s.cfg.users {
		if u.Sub == hint || strings.ToLower(u.Email) == hint || strings.EqualFold(u.Name, hint) {
			return u, true
		}
	}
	// No hint: fall through to the directory page. The stand-in must not sign anyone in by
	// default — a real provider shows a login form, and an unauthenticated browser must not end up
	// with a session. observe.mjs always names an account.
	return user{}, false
}

// ---------------------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "issuer": s.cfg.issuer, "users": len(s.cfg.users)})
}

// handleIndex is the directory page, so an operator can see who the stand-in can sign in.
func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	s.writeUsersPage(w, nil)
}

func (s *server) writeUsersPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>lab identity provider</title>")
	fmt.Fprintf(w, "<h1>%s</h1><p>This is the lab OpenID Connect stand-in. It has no passwords; a sign-in names an account with <code>login_hint</code>.</p>", s.cfg.issuer)
	fmt.Fprint(w, "<table><tr><th>sub</th><th>role</th><th>tenant</th></tr>")
	for _, u := range s.cfg.users {
		if r != nil {
			q := r.URL.Query()
			q.Set("login_hint", u.Sub)
			// Keep the original request's redirect/state when rendering the chooser.
			link := "/authorize?" + q.Encode()
			fmt.Fprintf(w, "<tr><td><a href=%q>%s</a></td><td>%s</td><td>%s</td></tr>", link, u.Sub, u.Role, u.Tenant)
			continue
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>", u.Sub, u.Role, u.Tenant)
	}
	fmt.Fprint(w, "</table>")
}

func (s *server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, code, detail string) {
	back, err := url.Parse(redirectURI)
	if err != nil {
		s.writeOAuthError(w, http.StatusBadRequest, code, detail)
		return
	}
	params := back.Query()
	params.Set("error", code)
	params.Set("error_description", detail)
	if state := r.URL.Query().Get("state"); state != "" {
		params.Set("state", state)
	}
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (s *server) writeOAuthError(w http.ResponseWriter, status int, code, detail string) {
	s.writeJSON(w, status, map[string]string{"error": code, "error_description": detail})
}

func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // a lab issuer that cannot read randomness cannot mint a code
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
