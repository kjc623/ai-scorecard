package entraapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	clientID = "00000000-aaaa-4bbb-8ccc-000000000001"
	tid      = "11111111-2222-4333-8444-555555555555"
)

// tokenEndpoint is a fake Microsoft token endpoint for the client-credentials grant. check inspects
// each request's form and answers with an OAuth error code, or "" to issue a token.
type tokenEndpoint struct {
	srv   *httptest.Server
	mu    sync.Mutex
	forms []url.Values
	paths []string
	check func(path string, form url.Values) string
}

func newTokenEndpoint(t *testing.T, check func(string, url.Values) string) *tokenEndpoint {
	e := &tokenEndpoint{check: check}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		e.mu.Lock()
		e.forms = append(e.forms, r.PostForm)
		e.paths = append(e.paths, r.URL.Path)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code := e.check(r.URL.Path, r.PostForm); code != "" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code,
				"error_description": "AADSTS7000215: Invalid client secret provided. Trace ID: x"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "graph-token", "token_type": "Bearer", "expires_in": 3599})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *tokenEndpoint) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.forms)
}

func TestSecretCredential(t *testing.T) {
	ep := newTokenEndpoint(t, func(path string, f url.Values) string {
		if path != "/"+tid+"/oauth2/v2.0/token" || f.Get("grant_type") != "client_credentials" ||
			f.Get("client_id") != clientID || f.Get("scope") != GraphScope || f.Get("client_secret") != "s3cret" {
			return "invalid_request"
		}
		return ""
	})
	app, err := New(Config{ClientID: clientID, ClientSecret: "s3cret", LoginBaseURL: ep.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tok, err := app.Token(context.Background(), strings.ToUpper(tid), GraphScope)
		if err != nil || tok != "graph-token" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	if ep.calls() != 1 {
		t.Fatalf("token endpoint called %d times; a live token must be cached", ep.calls())
	}
	if app.Credential() != "client-secret" {
		t.Fatalf("credential = %s", app.Credential())
	}
}

func TestRefusalCarriesCodeNotSecret(t *testing.T) {
	ep := newTokenEndpoint(t, func(string, url.Values) string { return "invalid_client" })
	app, _ := New(Config{ClientID: clientID, ClientSecret: "s3cret", LoginBaseURL: ep.srv.URL})
	_, err := app.Token(context.Background(), tid, GraphScope)
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_client" || oe.AADSTS != "AADSTS7000215" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatal("the error text carries the secret")
	}
	if _, err := app.Token(context.Background(), "common", GraphScope); err == nil {
		t.Fatal("a multi-tenant alias was accepted as a customer tenant")
	}
}

func TestExactlyOneCredential(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no client id: %v", err)
	}
	if _, err := New(Config{ClientID: clientID}); err == nil {
		t.Fatal("no credential was accepted")
	}
	if _, err := New(Config{ClientID: clientID, ClientSecret: "x", FIC: "managed"}); err == nil {
		t.Fatal("two credentials were accepted")
	}
	if _, err := New(Config{ClientID: clientID, FIC: "github"}); err == nil {
		t.Fatal("an unknown federated credential was accepted")
	}
}

// fakeIdentity is the managed identity's token source.
type fakeIdentity struct {
	mu        sync.Mutex
	resources []string
}

func (f *fakeIdentity) Token(_ context.Context, resource string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = append(f.resources, resource)
	return "mi-assertion-token", nil
}

// TestManagedIdentityFederatedCredential is the production path: the managed identity's token for
// api://AzureADTokenExchange becomes the client assertion.
func TestManagedIdentityFederatedCredential(t *testing.T) {
	ep := newTokenEndpoint(t, func(_ string, f url.Values) string {
		if f.Get("client_assertion_type") != AssertionType || f.Get("client_assertion") != "mi-assertion-token" || f.Get("client_secret") != "" {
			return "invalid_client"
		}
		return ""
	})
	mi := &fakeIdentity{}
	app, err := New(Config{ClientID: clientID, FIC: FICManaged, Identity: mi, LoginBaseURL: ep.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Token(context.Background(), tid, GraphScope); err != nil {
		t.Fatal(err)
	}
	form, err := app.ClientAuth(context.Background(), ep.srv.URL+"/organizations/oauth2/v2.0/token")
	if err != nil || form.Get("client_assertion") != "mi-assertion-token" {
		t.Fatalf("ClientAuth = %v, %v", form, err)
	}
	for _, r := range mi.resources {
		if r != "api://AzureADTokenExchange" {
			t.Fatalf("the managed identity was asked for %q", r)
		}
	}
	if app.Credential() != "managed-identity-fic" {
		t.Fatalf("credential = %s", app.Credential())
	}
}
