package main

import (
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
)

const testUsers = `[
  {"sub":"reader@lab.test","name":"Rhea Reader","email":"reader@lab.test","role":"content_reader","realm":"lab"},
  {"sub":"admin@sample.test","name":"Ada Admin","email":"admin@sample.test","role":"admin","realm":"sample"}
]`

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, err := configFromEnv(env{
		"OIDC_ISSUER":        "http://oidc.test",
		"OIDC_PUBLIC_URL":    "http://127.0.0.1:8790",
		"OIDC_CLIENT_SECRET": "lab-secret",
		"OIDC_USERS":         testUsers,
	}.get)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

type env map[string]string

func (e env) get(k string) string { return e[k] }

func getJSON(t *testing.T, address string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(address)
	if err != nil {
		t.Fatalf("GET %s: %v", address, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize runs /authorize for one account and returns the code from the redirect.
func authorize(t *testing.T, ts *httptest.Server, realm, hint, verifier string) string {
	t.Helper()
	res, err := noRedirect.Get(ts.URL + "/" + realm + "/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {"sac-control"}, "redirect_uri": {"http://dash/callback"},
		"state": {"st"}, "nonce": {"no"}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"}, "login_hint": {hint},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("authorize answered %d, want 302", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Query().Get("state") != "st" || loc.Query().Get("code") == "" {
		t.Fatalf("authorize redirect is wrong: %s", loc)
	}
	return loc.Query().Get("code")
}

func exchange(t *testing.T, ts *httptest.Server, realm string, form url.Values, basicUser, basicPass string) (int, map[string]any) {
	t.Helper()
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", "http://dash/callback")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/"+realm+"/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" {
		req.SetBasicAuth(url.QueryEscape(basicUser), url.QueryEscape(basicPass))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func claims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return out
}

func TestEachRealmIsItsOwnIssuer(t *testing.T) {
	ts := testServer(t)
	for _, realm := range []string{"lab", "sample"} {
		status, d := getJSON(t, ts.URL+"/"+realm+"/.well-known/openid-configuration")
		if status != http.StatusOK || d["issuer"] != "http://oidc.test/"+realm ||
			d["token_endpoint"] != "http://oidc.test/"+realm+"/token" ||
			d["jwks_uri"] != "http://oidc.test/"+realm+"/jwks" {
			t.Fatalf("%s discovery is wrong: %d %v", realm, status, d)
		}
		// The browser reaches the authorization endpoint at the public address.
		if d["authorization_endpoint"] != "http://127.0.0.1:8790/"+realm+"/authorize" {
			t.Fatalf("%s authorization endpoint is %v", realm, d["authorization_endpoint"])
		}
		_, jwks := getJSON(t, ts.URL+"/"+realm+"/jwks")
		if keys, _ := jwks["keys"].([]any); len(keys) != 1 {
			t.Fatalf("%s JWKS: %v", realm, jwks)
		}
	}
	if status, _ := getJSON(t, ts.URL+"/other/.well-known/openid-configuration"); status != http.StatusNotFound {
		t.Fatalf("an unknown realm answered %d, want 404", status)
	}
}

func TestCodeFlowIssuesTheRealmsIDToken(t *testing.T) {
	ts := testServer(t)
	verifier := "a-verifier-that-is-long-enough-for-pkce"

	code := authorize(t, ts, "lab", "Reader@Lab.test", verifier)
	status, body := exchange(t, ts, "lab", url.Values{"code": {code}, "code_verifier": {"wrong"},
		"client_id": {"sac-control"}, "client_secret": {"lab-secret"}}, "", "")
	if status != http.StatusBadRequest {
		t.Fatalf("a wrong verifier answered %d, want 400", status)
	}
	// A code is single use, including after a refused exchange.
	status, _ = exchange(t, ts, "lab", url.Values{"code": {code}, "code_verifier": {verifier},
		"client_id": {"sac-control"}, "client_secret": {"lab-secret"}}, "", "")
	if status != http.StatusBadRequest {
		t.Fatalf("a used code answered %d, want 400", status)
	}

	code = authorize(t, ts, "lab", "reader@lab.test", verifier)
	status, body = exchange(t, ts, "lab", url.Values{"code": {code}, "code_verifier": {verifier}}, "sac-control", "lab-secret")
	if status != http.StatusOK {
		t.Fatalf("token answered %d: %v", status, body)
	}
	c := claims(t, body["id_token"].(string))
	roles, _ := c["roles"].([]any)
	if c["iss"] != "http://oidc.test/lab" || c["aud"] != "sac-control" || c["nonce"] != "no" ||
		c["email"] != "reader@lab.test" || c["email_verified"] != true || len(roles) != 1 || roles[0] != "content_reader" {
		t.Fatalf("id_token claims are wrong: %v", c)
	}

	// A code from one realm does not redeem at another.
	code = authorize(t, ts, "sample", "admin@sample.test", verifier)
	if status, _ := exchange(t, ts, "lab", url.Values{"code": {code}, "code_verifier": {verifier}}, "sac-control", "lab-secret"); status != http.StatusBadRequest {
		t.Fatalf("a sample code redeemed at lab answered %d, want 400", status)
	}
}

func TestClientAuthentication(t *testing.T) {
	ts := testServer(t)
	verifier := "another-verifier-long-enough-for-pkce"
	for name, c := range map[string]struct {
		form       url.Values
		user, pass string
		want       int
	}{
		"post":         {url.Values{"client_id": {"sac-control"}, "client_secret": {"lab-secret"}}, "", "", http.StatusOK},
		"basic":        {url.Values{}, "sac-control", "lab-secret", http.StatusOK},
		"wrong secret": {url.Values{"client_id": {"sac-control"}, "client_secret": {"nope"}}, "", "", http.StatusUnauthorized},
		"no secret":    {url.Values{"client_id": {"sac-control"}}, "", "", http.StatusUnauthorized},
		"other client": {url.Values{}, "someone-else", "lab-secret", http.StatusUnauthorized},
	} {
		c.form.Set("code", authorize(t, ts, "sample", "admin@sample.test", verifier))
		c.form.Set("code_verifier", verifier)
		if status, body := exchange(t, ts, "sample", c.form, c.user, c.pass); status != c.want {
			t.Fatalf("%s: token answered %d (%v), want %d", name, status, body, c.want)
		}
	}
}

func TestAuthorizeWithoutAHintShowsTheRealmsAccounts(t *testing.T) {
	ts := testServer(t)
	res, err := noRedirect.Get(ts.URL + "/lab/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {"sac-control"}, "redirect_uri": {"http://dash/callback"},
		"code_challenge": {"x"}, "code_challenge_method": {"S256"},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "reader@lab.test") ||
		strings.Contains(string(page), "admin@sample.test") {
		t.Fatalf("the chooser must list this realm's accounts only: %d %s", res.StatusCode, page)
	}
}

func TestConfigRefusesAnIncompleteDirectory(t *testing.T) {
	for name, e := range map[string]env{
		"no secret":   {"OIDC_USERS": testUsers},
		"no users":    {"OIDC_CLIENT_SECRET": "s", "OIDC_USERS": "[]"},
		"not json":    {"OIDC_CLIENT_SECRET": "s", "OIDC_USERS": "nope"},
		"no realm":    {"OIDC_CLIENT_SECRET": "s", "OIDC_USERS": `[{"sub":"a","email":"a@b.c","role":"viewer"}]`},
		"extra field": {"OIDC_CLIENT_SECRET": "s", "OIDC_USERS": `[{"sub":"a","email":"a@b.c","role":"viewer","realm":"lab","tenant":"x"}]`},
	} {
		if _, err := configFromEnv(e.get); err == nil {
			t.Fatalf("%s: the configuration must be refused", name)
		}
	}
}
