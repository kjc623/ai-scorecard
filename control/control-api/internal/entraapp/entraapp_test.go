package entraapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	cfg := ConfigFromEnv(func(k string) string {
		return map[string]string{EnvClientID: clientID, EnvFIC: "managed", "AZURE_CLIENT_ID": "mi-id", "IDENTITY_ENDPOINT": "http://x"}[k]
	})
	if cfg.ClientID != clientID || cfg.FIC != "managed" || cfg.ManagedIdentityClientID != "mi-id" || cfg.IdentityEndpoint != "http://x" {
		t.Fatalf("ConfigFromEnv = %+v", cfg)
	}
}

// writeCert makes a self-signed RSA certificate and key, as the owner would upload to the app.
func writeCert(t *testing.T) (string, *x509.Certificate) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "sac-entra-app"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	body := append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	path := filepath.Join(t.TempDir(), "entra-app.pem")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cert
}

// TestCertificateClientAssertionShape checks the assertion exactly as Entra does: RS256, x5t = the
// base64url SHA-1 thumbprint of the uploaded certificate, aud = the token endpoint called, iss = sub
// = the client id, a jti, and a life of at most ten minutes — and the signature verifies under the
// certificate's public key.
func TestCertificateClientAssertionShape(t *testing.T) {
	path, cert := writeCert(t)
	var assertion string
	ep := newTokenEndpoint(t, func(_ string, f url.Values) string {
		if f.Get("client_assertion_type") != AssertionType || f.Get("client_secret") != "" {
			return "invalid_client"
		}
		assertion = f.Get("client_assertion")
		return ""
	})
	app, err := New(Config{ClientID: clientID, CertFile: path, LoginBaseURL: ep.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Token(context.Background(), tid, GraphScope); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion %q is not a compact JWS", assertion)
	}
	var header map[string]string
	var claims map[string]any
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(hb, &header)
	_ = json.Unmarshal(cb, &claims)
	thumb := sha1.Sum(cert.Raw)
	if header["alg"] != "RS256" || header["typ"] != "JWT" || header["x5t"] != base64.RawURLEncoding.EncodeToString(thumb[:]) {
		t.Fatalf("header = %v", header)
	}
	wantAud := ep.srv.URL + "/" + tid + "/oauth2/v2.0/token"
	if claims["aud"] != wantAud || claims["iss"] != clientID || claims["sub"] != clientID || claims["jti"] == "" {
		t.Fatalf("claims = %v", claims)
	}
	if life := claims["exp"].(float64) - claims["nbf"].(float64); life <= 0 || life > 600 {
		t.Fatalf("assertion life = %vs", life)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("assertion signature: %v", err)
	}
	// The same credential authenticates a sign-in's code redemption, with that endpoint as aud.
	params, err := app.ClientAuth(context.Background(), "https://login.example/organizations/oauth2/v2.0/token")
	if err != nil || params.Get("client_assertion") == "" || params.Get("client_assertion_type") != AssertionType {
		t.Fatalf("ClientAuth = %v, %v", params, err)
	}
}

func TestCertificateFileMustHoldAMatchingRSAKey(t *testing.T) {
	path, _ := writeCert(t)
	other, _ := writeCert(t)
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(other)
	// a's key with b's certificate
	keyBlock, _ := pem.Decode(a)
	var certBlock *pem.Block
	for rest := b; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			certBlock = blk
		}
	}
	mixed := append(pem.EncodeToMemory(keyBlock), pem.EncodeToMemory(certBlock)...)
	if _, err := parseCertCredential(mixed, clientID, time.Now); err == nil {
		t.Fatal("a key that is not the certificate's was accepted")
	}
}

// TestManagedIdentityFederatedCredential is the production path: the container's managed identity
// token for api://AzureADTokenExchange becomes the client assertion. Both platform endpoints are
// exercised: Container Apps (IDENTITY_ENDPOINT + X-IDENTITY-HEADER) and IMDS (Metadata: true).
func TestManagedIdentityFederatedCredential(t *testing.T) {
	for _, mode := range []string{"container-apps", "imds"} {
		t.Run(mode, func(t *testing.T) {
			var miCalls int
			mi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				miCalls++
				q := r.URL.Query()
				ok := q.Get("resource") == FICAudience && q.Get("client_id") == "user-assigned-mi"
				if mode == "container-apps" {
					ok = ok && r.Header.Get("X-IDENTITY-HEADER") == "platform-secret" && q.Get("api-version") == "2019-08-01"
				} else {
					ok = ok && r.Header.Get("Metadata") == "true" && q.Get("api-version") == "2018-02-01"
				}
				if !ok {
					w.WriteHeader(400)
					return
				}
				// expires_on is unix seconds as a string, as both platform endpoints send it.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "mi-assertion-token", "expires_on": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
				})
			}))
			defer mi.Close()
			ep := newTokenEndpoint(t, func(_ string, f url.Values) string {
				if f.Get("client_assertion_type") != AssertionType || f.Get("client_assertion") != "mi-assertion-token" {
					return "invalid_client"
				}
				return ""
			})
			cfg := Config{ClientID: clientID, FIC: "managed", ManagedIdentityClientID: "user-assigned-mi", LoginBaseURL: ep.srv.URL}
			if mode == "container-apps" {
				cfg.IdentityEndpoint, cfg.IdentityHeader = mi.URL, "platform-secret"
			} else {
				cfg.IMDSURL = mi.URL
			}
			app, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.Token(context.Background(), tid, GraphScope); err != nil {
				t.Fatal(err)
			}
			if _, err := app.Token(context.Background(), "22222222-2222-4333-8444-555555555555", GraphScope); err != nil {
				t.Fatal(err)
			}
			if miCalls != 1 {
				t.Fatalf("managed identity endpoint called %d times; its token must be cached", miCalls)
			}
			if app.Credential() != "managed-identity-fic" {
				t.Fatalf("credential = %s", app.Credential())
			}
		})
	}
}
