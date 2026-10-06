// Command oidc is the lab's customer identity provider: a small OpenID Connect provider that
// control-api signs people in through, the way it signs them in through a customer's Entra or Okta.
//
// It serves one realm per lab tenant. Each realm is its own issuer, <OIDC_ISSUER>/<realm>, because
// control-api maps an issuer to exactly one tenant (as Entra has one issuer per directory). Per realm
// it offers discovery, a JWKS, an authorization-code flow with PKCE (S256) for one confidential
// client (control-api), and RS256 id_tokens carrying the claims control-api reads: email (verified),
// preferred_username, name and roles.
//
// The issuer, the token endpoint and the JWKS are reached by control-api on the lab network; the
// authorization endpoint is reached by a browser, so discovery names it under OIDC_PUBLIC_URL.
//
// It has no passwords and no consent screen: /authorize signs in the realm's account named by
// login_hint (control-api passes the work email on as one), and without a hint it shows the realm's
// accounts to choose from. The signing key is generated at every start.
//
//	OIDC_ADDR           listen address (default 0.0.0.0:8080)
//	OIDC_ISSUER         issuer base as control-api reaches it (default http://oidc:8080)
//	OIDC_PUBLIC_URL     the same server as a browser reaches it (default OIDC_ISSUER)
//	OIDC_CLIENT_ID      the one client (default sac-control)
//	OIDC_CLIENT_SECRET  its secret (required)
//	OIDC_USERS          JSON array of {"sub","name","email","role","realm"}
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr     = "0.0.0.0:8080"
	defaultIssuer   = "http://oidc:8080"
	defaultClientID = "sac-control"
	// authCodeTTL bounds an issued authorization code; a sign-in round trip takes seconds.
	authCodeTTL = 2 * time.Minute
	tokenTTL    = 15 * time.Minute
	keyBits     = 2048
)

// user is one account: the realm it signs in to and the product role it carries in `roles`.
type user struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
	Realm string `json:"realm"`
}

type config struct {
	addr         string
	issuer       string // base; a realm's issuer is issuer + "/" + realm
	publicURL    string // base of the browser-facing authorization endpoint
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
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oidc:", err)
		os.Exit(1)
	}
	s, err := newServer(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oidc:", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: cfg.addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Info("oidc listening", "addr", cfg.addr, "issuer", cfg.issuer, "public_url", cfg.publicURL, "realms", s.realms())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("oidc stopped", "error", err.Error())
		os.Exit(1)
	}
}

var realmName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func configFromEnv(getenv func(string) string) (config, error) {
	get := func(name, fallback string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	cfg := config{
		addr:         get("OIDC_ADDR", defaultAddr),
		issuer:       strings.TrimRight(get("OIDC_ISSUER", defaultIssuer), "/"),
		clientID:     get("OIDC_CLIENT_ID", defaultClientID),
		clientSecret: strings.TrimSpace(getenv("OIDC_CLIENT_SECRET")),
	}
	cfg.publicURL = strings.TrimRight(get("OIDC_PUBLIC_URL", cfg.issuer), "/")
	if cfg.clientSecret == "" {
		return config{}, errors.New("OIDC_CLIENT_SECRET is required: control-api is a confidential client")
	}
	users, err := usersFromJSON(getenv("OIDC_USERS"))
	if err != nil {
		return config{}, err
	}
	cfg.users = users
	return cfg, nil
}

// usersFromJSON parses OIDC_USERS. A provider with no accounts signs nobody in, so that is an error.
func usersFromJSON(raw string) ([]user, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var users []user
	if err := dec.Decode(&users); err != nil {
		return nil, fmt.Errorf("OIDC_USERS is not a JSON array of accounts: %w", err)
	}
	if len(users) == 0 {
		return nil, errors.New("OIDC_USERS names no accounts")
	}
	for i, u := range users {
		if u.Sub == "" || u.Email == "" || u.Role == "" {
			return nil, fmt.Errorf("OIDC_USERS[%d] needs sub, email and role", i)
		}
		if !realmName.MatchString(u.Realm) {
			return nil, fmt.Errorf("OIDC_USERS[%d]: realm %q is not a lower-case name", i, u.Realm)
		}
	}
	return users, nil
}

func newServer(cfg config, log *slog.Logger) (*server, error) {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	sum := sha256.Sum256(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	return &server{
		cfg: cfg, key: key, kid: base64.RawURLEncoding.EncodeToString(sum[:8]),
		log: log, now: time.Now, codes: map[string]authCode{},
	}, nil
}

// Handler returns the provider's routes.
func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /{realm}/.well-known/openid-configuration", s.inRealm(s.handleDiscovery))
	mux.HandleFunc("GET /{realm}/jwks", s.inRealm(s.handleJWKS))
	mux.HandleFunc("GET /{realm}/authorize", s.inRealm(s.handleAuthorize))
	mux.HandleFunc("POST /{realm}/token", s.inRealm(s.handleToken))
	return mux
}

func (s *server) realms() []string {
	var out []string
	for _, u := range s.cfg.users {
		if !slices.Contains(out, u.Realm) {
			out = append(out, u.Realm)
		}
	}
	return out
}

// inRealm resolves the {realm} path segment, answering 404 for a realm no account belongs to.
func (s *server) inRealm(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		realm := r.PathValue("realm")
		if !slices.Contains(s.realms(), realm) {
			http.NotFound(w, r)
			return
		}
		h(w, r, realm)
	}
}

func (s *server) issuer(realm string) string { return s.cfg.issuer + "/" + realm }

func (s *server) handleDiscovery(w http.ResponseWriter, _ *http.Request, realm string) {
	iss := s.issuer(realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                s.cfg.publicURL + "/" + realm + "/authorize",
		"token_endpoint":                        iss + "/token",
		"jwks_uri":                              iss + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"claims_supported":                      []string{"sub", "email", "email_verified", "name", "preferred_username", "roles", "nonce"},
	})
}

func (s *server) handleJWKS(w http.ResponseWriter, _ *http.Request, _ string) {
	e := big.NewInt(int64(s.key.PublicKey.E))
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": base64.RawURLEncoding.EncodeToString(s.key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(e.Bytes()),
	}}})
}

func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request, realm string) {
	q := r.URL.Query()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || back.Host == "" {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_request", "redirect_uri must be an absolute URL"))
		return
	}
	refuse := func(code, detail string) {
		params := back.Query()
		params.Set("error", code)
		params.Set("error_description", detail)
		if state := q.Get("state"); state != "" {
			params.Set("state", state)
		}
		back.RawQuery = params.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		refuse("unsupported_response_type", "only response_type=code is served")
		return
	case q.Get("client_id") != s.cfg.clientID:
		refuse("invalid_client", "unknown client_id")
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		refuse("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	u, ok := s.findUser(realm, q.Get("login_hint"))
	if !ok {
		s.writeChooser(w, r, realm)
		return
	}
	code := randomToken()
	s.mu.Lock()
	s.codes[code] = authCode{user: u, redirectURI: back.String(), codeChallenge: q.Get("code_challenge"),
		nonce: q.Get("nonce"), expiresAt: s.now().Add(authCodeTTL)}
	s.mu.Unlock()
	params := back.Query()
	params.Set("code", code)
	if state := q.Get("state"); state != "" {
		params.Set("state", state)
	}
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request, realm string) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_request", "the token request is not form-encoded"))
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, oauthError("unsupported_grant_type", "only authorization_code is served"))
		return
	}
	// Either client authentication form of RFC 6749 section 2.3.1; the basic form URL-encodes each
	// part before joining them.
	clientID, secret, basic := r.BasicAuth()
	if basic {
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	} else {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if clientID != s.cfg.clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.clientSecret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, oauthError("invalid_client", "client authentication failed"))
		return
	}

	s.mu.Lock()
	entry, ok := s.codes[r.PostForm.Get("code")]
	delete(s.codes, r.PostForm.Get("code"))
	s.mu.Unlock()
	switch {
	case !ok || s.now().After(entry.expiresAt) || entry.user.Realm != realm:
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_grant", "the code is unknown, used or expired"))
		return
	case r.PostForm.Get("redirect_uri") != entry.redirectURI:
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_grant", "redirect_uri differs from the authorization request"))
		return
	case !pkceMatches(entry.codeChallenge, r.PostForm.Get("code_verifier")):
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_grant", "code_verifier does not match the challenge"))
		return
	}

	now := s.now()
	base := map[string]any{"iss": s.issuer(realm), "aud": s.cfg.clientID, "sub": entry.user.Sub,
		"iat": now.Unix(), "exp": now.Add(tokenTTL).Unix()}
	idClaims := map[string]any{"email": entry.user.Email, "email_verified": true, "name": entry.user.Name,
		"preferred_username": entry.user.Email, "roles": []string{entry.user.Role}}
	for k, v := range base {
		idClaims[k] = v
	}
	if entry.nonce != "" {
		idClaims["nonce"] = entry.nonce
	}
	idToken, err1 := s.sign(idClaims)
	accessToken, err2 := s.sign(base)
	if err := errors.Join(err1, err2); err != nil {
		writeJSON(w, http.StatusInternalServerError, oauthError("server_error", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": accessToken, "id_token": idToken,
		"token_type": "Bearer", "expires_in": int(tokenTTL.Seconds())})
}

// pkceMatches is RFC 7636 S256: base64url(sha256(verifier)) == challenge.
func pkceMatches(challenge, verifier string) bool {
	if verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// sign returns an RS256 JWT over claims.
func (s *server) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *server) findUser(realm, hint string) (user, bool) {
	hint = strings.ToLower(strings.TrimSpace(hint))
	if hint == "" {
		return user{}, false
	}
	for _, u := range s.cfg.users {
		if u.Realm == realm && (strings.ToLower(u.Email) == hint || u.Sub == hint) {
			return u, true
		}
	}
	return user{}, false
}

// writeChooser lists a realm's accounts, each linking back to /authorize with its login_hint.
func (s *server) writeChooser(w http.ResponseWriter, r *http.Request, realm string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Lab sign-in</title><h1>Sign in to %s</h1><ul>", html.EscapeString(realm))
	for _, u := range s.cfg.users {
		if u.Realm != realm {
			continue
		}
		q := r.URL.Query()
		q.Set("login_hint", u.Email)
		fmt.Fprintf(w, `<li><a href="%s">%s</a> (%s)</li>`, html.EscapeString("authorize?"+q.Encode()),
			html.EscapeString(u.Email), html.EscapeString(u.Role))
	}
	fmt.Fprint(w, "</ul>")
}

func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>Lab identity provider</title><h1>Lab identity provider</h1><table><tr><th>issuer</th><th>account</th><th>role</th></tr>")
	for _, u := range s.cfg.users {
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>", html.EscapeString(s.issuer(u.Realm)),
			html.EscapeString(u.Email), html.EscapeString(u.Role))
	}
	fmt.Fprint(w, "</table>")
}

func oauthError(code, detail string) map[string]string {
	return map[string]string{"error": code, "error_description": detail}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
