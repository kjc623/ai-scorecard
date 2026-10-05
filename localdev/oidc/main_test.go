package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testTenant = "11111111-1111-4111-8111-111111111111"

// testServer builds the stand-in over a fresh key, the same way main does.
func testServer(t *testing.T) *server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &server{
		cfg: config{
			issuer: "http://oidc.test", audience: "sac-query-api", clientID: "sac-dashboard",
			users: []user{{Sub: "reader@lab.test", Name: "Rhea Reader", Email: "reader@lab.test", Role: "content_reader", Tenant: testTenant}},
		},
		key: key, kid: kidOf(&key.PublicKey), log: slog.Default(), now: time.Now,
		codes: map[string]authCode{},
	}
}

func getJSON(t *testing.T, ts *httptest.Server, path string) map[string]any {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s is not JSON: %v", path, err)
	}
	return out
}

// TestDiscoveryAndJWKS: a client discovers the endpoints and finds one RS256 key, which is what
// control-api, the relying party, fetches to verify an id_token.
func TestDiscoveryAndJWKS(t *testing.T) {
	ts := httptest.NewServer(testServer(t).Handler())
	defer ts.Close()

	d := getJSON(t, ts, "/.well-known/openid-configuration")
	if d["issuer"] != "http://oidc.test" || d["jwks_uri"] != "http://oidc.test/jwks" {
		t.Fatalf("discovery is wrong: %v", d)
	}
	jwks := getJSON(t, ts, "/jwks")
	keys, _ := jwks["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected one key, got %v", jwks)
	}
	key, _ := keys[0].(map[string]any)
	if key["kty"] != "RSA" || key["alg"] != "RS256" {
		t.Fatalf("key is not an RS256 RSA key: %v", key)
	}
}

// TestAuthorizeWithoutHintShowsTheDirectory: the stand-in must not sign anyone in by default, or
// an unauthenticated browser would end up with a session.
func TestAuthorizeWithoutHintShowsTheDirectory(t *testing.T) {
	ts := httptest.NewServer(testServer(t).Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u := ts.URL + "/authorize?response_type=code&client_id=sac-dashboard&redirect_uri=" + url.QueryEscape("http://dash/callback") + "&code_challenge=x&code_challenge_method=S256"
	res, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("no-hint authorize returned %d, want 200 directory", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "reader@lab.test") {
		t.Fatal("the directory page did not list the user")
	}
}

// TestAuthorizationCodeFlowWithPKCE is the real flow: a code comes back with the state, the
// exchange proves the verifier, wrong verifiers are refused, and the id_token carries the tenant,
// the role and the nonce.
func TestAuthorizationCodeFlowWithPKCE(t *testing.T) {
	ts := httptest.NewServer(testServer(t).Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	verifier := base64.RawURLEncoding.EncodeToString([]byte("a-verifier-long-enough-for-the-test"))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authURL := ts.URL + "/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {"sac-dashboard"}, "redirect_uri": {"http://dash/callback"},
		"state": {"st-1"}, "nonce": {"no-1"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"login_hint": {"reader@lab.test"},
	}.Encode()
	res, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("authorize returned %d, want 302", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" || loc.Query().Get("state") != "st-1" {
		t.Fatalf("authorize redirect is wrong: %s", res.Header.Get("Location"))
	}

	// A wrong verifier is refused before a token is minted.
	bad, err := http.PostForm(ts.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://dash/callback"}, "client_id": {"sac-dashboard"}, "code_verifier": {"wrong"}})
	if err != nil {
		t.Fatalf("token (bad): %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong verifier returned %d, want 400", bad.StatusCode)
	}

	// The code was single-use, so re-authorize for the good exchange.
	res2, _ := client.Get(authURL)
	res2.Body.Close()
	loc2, _ := url.Parse(res2.Header.Get("Location"))
	good, err := http.PostForm(ts.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc2.Query().Get("code")}, "redirect_uri": {"http://dash/callback"}, "client_id": {"sac-dashboard"}, "code_verifier": {verifier}})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer good.Body.Close()
	if good.StatusCode != http.StatusOK {
		t.Fatalf("token returned %d", good.StatusCode)
	}
	var tokens map[string]any
	json.NewDecoder(good.Body).Decode(&tokens)
	idToken, _ := tokens["id_token"].(string)
	accessToken, _ := tokens["access_token"].(string)
	if idToken == "" || accessToken == "" {
		t.Fatalf("no tokens: %v", tokens)
	}
	claims := decodePayload(t, idToken)
	if claims["sac_tenant"] != testTenant || claims["nonce"] != "no-1" {
		t.Fatalf("id_token claims wrong: %v", claims)
	}
	roles, _ := claims["roles"].([]any)
	if len(roles) != 1 || roles[0] != "content_reader" {
		t.Fatalf("id_token roles wrong: %v", claims["roles"])
	}
	access := decodePayload(t, accessToken)
	if access["aud"] != "sac-query-api" {
		t.Fatalf("access token audience wrong: %v", access["aud"])
	}
}

func decodePayload(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %s", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload is not base64url: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return out
}

// TestConfidentialClient: as an upstream provider the stand-in has one confidential client,
// control-api. Discovery says how to authenticate, both RFC 6749 §2.3.1 forms work, a wrong secret
// or another client id is refused, and the id_token carries what the relying party takes the
// actor from.
func TestConfidentialClient(t *testing.T) {
	s := testServer(t)
	s.cfg.clientID, s.cfg.clientSecret = "sac-control", "lab-secret"
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	d := getJSON(t, ts, "/.well-known/openid-configuration")
	methods, _ := d["token_endpoint_auth_methods_supported"].([]any)
	if len(methods) != 2 || methods[0] != "client_secret_post" || methods[1] != "client_secret_basic" {
		t.Fatalf("discovery must offer both secret forms, got %v", d["token_endpoint_auth_methods_supported"])
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	verifier := base64.RawURLEncoding.EncodeToString([]byte("another-verifier-long-enough-for-it"))
	sum := sha256.Sum256([]byte(verifier))
	code := func() string {
		t.Helper()
		res, err := client.Get(ts.URL + "/authorize?" + url.Values{
			"response_type": {"code"}, "client_id": {"sac-control"}, "redirect_uri": {"http://control/callback"},
			"nonce": {"n"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
			"code_challenge_method": {"S256"}, "login_hint": {"reader@lab.test"},
		}.Encode())
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		res.Body.Close()
		loc, _ := url.Parse(res.Header.Get("Location"))
		return loc.Query().Get("code")
	}
	exchange := func(form url.Values, user, pass string) *http.Response {
		t.Helper()
		form.Set("grant_type", "authorization_code")
		form.Set("redirect_uri", "http://control/callback")
		form.Set("code_verifier", verifier)
		form.Set("code", code())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if user != "" {
			req.SetBasicAuth(url.QueryEscape(user), url.QueryEscape(pass))
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		return res
	}

	for name, c := range map[string]struct {
		form       url.Values
		user, pass string
		want       int
	}{
		"post":           {url.Values{"client_id": {"sac-control"}, "client_secret": {"lab-secret"}}, "", "", http.StatusOK},
		"basic":          {url.Values{}, "sac-control", "lab-secret", http.StatusOK},
		"wrong secret":   {url.Values{"client_id": {"sac-control"}, "client_secret": {"nope"}}, "", "", http.StatusUnauthorized},
		"no secret":      {url.Values{"client_id": {"sac-control"}}, "", "", http.StatusUnauthorized},
		"another client": {url.Values{}, "sac-dashboard", "lab-secret", http.StatusUnauthorized},
	} {
		res := exchange(c.form, c.user, c.pass)
		var body map[string]any
		json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Fatalf("%s: token returned %d (%v), want %d", name, res.StatusCode, body, c.want)
		}
		if c.want != http.StatusOK {
			continue
		}
		claims := decodePayload(t, body["id_token"].(string))
		if claims["aud"] != "sac-control" || claims["email"] != "reader@lab.test" ||
			claims["email_verified"] != true || claims["preferred_username"] != "reader@lab.test" {
			t.Fatalf("%s: id_token claims wrong: %v", name, claims)
		}
	}
}

// TestUsersFromEnvRejectsAnIncompleteDirectory so a misconfigured lab fails at start.
func TestUsersFromEnvRejectsAnIncompleteDirectory(t *testing.T) {
	if _, err := usersFromEnv(`[{"sub":"a","role":"viewer"}]`); err == nil {
		t.Fatal("a user with no tenant must be refused")
	}
	if _, err := usersFromEnv(`not json`); err == nil {
		t.Fatal("non-JSON must be refused")
	}
	users, err := usersFromEnv(`[{"sub":"a","role":"viewer","tenant":"11111111-1111-4111-8111-111111111111"}]`)
	if err != nil || len(users) != 1 {
		t.Fatalf("a complete user must parse: %v %v", users, err)
	}
}
