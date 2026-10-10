package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/shadow-ai-capture/control-api/internal/deviceca/devicecatest"
)

func testEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(ecKey)
	sessionKey := filepath.Join(dir, "session.pem")
	if err := os.WriteFile(sessionKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	pk8, _ := x509.MarshalPKCS8PrivateKey(edKey)
	policyKey := filepath.Join(dir, "policy.pem")
	if err := os.WriteFile(policyKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8}), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := devicecatest.New(t)
	dirKey := make([]byte, 32)
	_, _ = rand.Read(dirKey)
	return map[string]string{
		EnvRegion:               "eastus",
		EnvCACertPEM:            string(ca.CertPEM),
		EnvCAKeyPEM:             string(ca.KeyPEM),
		EnvAuthIssuer:           "https://control-api.internal.example/",
		EnvSessionSigningKey:    sessionKey,
		EnvInternalToken:        strings.Repeat("t", 40),
		EnvPublicURL:            "https://app.example.com/",
		EnvDirectoryKey:         base64.StdEncoding.EncodeToString(dirKey),
		EnvPublicDeviceEndpoint: "https://devices.example.com",
		EnvPolicySigningKey:     policyKey,
		EnvAgentReleaseDir:      filepath.Join(dir, "release"),
		EnvContentVaultURL:      "https://content-vault.internal.example",
	}
}

func lookup(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestConfigDerivesTheRedirectURIsFromThePublicURL(t *testing.T) {
	env := testEnv(t)
	cfg, err := loadConfig(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RedirectURIs, []string{"https://app.example.com/callback"}) {
		t.Fatalf("redirect URIs = %v", cfg.RedirectURIs)
	}
	if cfg.HTTPAddr != defaultHTTPAddr || cfg.AllowInsecureIdP || cfg.AuthIssuer != "https://control-api.internal.example" {
		t.Fatalf("config = %+v", cfg)
	}
	env[EnvAuthRedirectURIs] = "http://dashboard:8787/callback, http://dashboard-sample:8787/callback"
	env[EnvAuthAllowInsecureIdP] = "true"
	cfg, err = loadConfig(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://app.example.com/callback", "http://dashboard:8787/callback", "http://dashboard-sample:8787/callback"}
	if !slices.Equal(cfg.RedirectURIs, want) || !cfg.AllowInsecureIdP {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestConfigReportsEveryMissingSetting(t *testing.T) {
	_, err := loadConfig(lookup(map[string]string{EnvHTTPAddr: "nope", EnvAuthRedirectURIs: "not a url"}))
	if err == nil {
		t.Fatal("an empty environment was accepted")
	}
	for _, name := range []string{
		EnvCACertPEM, EnvCAKeyPEM, EnvAuthIssuer, EnvSessionSigningKey, EnvInternalToken, EnvPublicURL,
		EnvDirectoryKey, EnvPublicDeviceEndpoint, EnvPolicySigningKey, EnvContentVaultURL, EnvHTTPAddr, EnvAuthRedirectURIs,
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s:\n%v", name, err)
		}
	}
}

// The service wires from a complete environment without touching the database, and mounts every
// surface on the one listener.
func TestWireMountsEverySurface(t *testing.T) {
	cfg, err := loadConfig(lookup(testEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	pg, err := pgx.ParseConfig("postgres://control-api@127.0.0.1:1/shadow?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*pg)
	defer db.Close()
	srv, _, _, err := wire(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/readyz", http.StatusServiceUnavailable},
		{http.MethodGet, "/.well-known/jwks.json", http.StatusOK},
		{http.MethodPost, "/v1/enrol", http.StatusBadRequest},
		{http.MethodPost, "/v1/health", http.StatusBadRequest},
		{http.MethodGet, "/v1/policy", http.StatusUnauthorized},
		{http.MethodPost, "/v1/content/grant", http.StatusBadRequest},
		{http.MethodPost, "/v1/content", http.StatusUnauthorized},
		{http.MethodGet, "/v1/extension/updates.xml", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/auth/begin", http.StatusUnauthorized},
		{http.MethodGet, "/admin/v1/deployment", http.StatusUnauthorized},
		{http.MethodGet, "/scim/v2/Users", http.StatusUnauthorized},
		{http.MethodGet, "/onboard/sacinv_not-a-token", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader("")))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, rec.Code, c.want, rec.Body.String())
		}
	}
}
