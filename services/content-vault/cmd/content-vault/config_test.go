package main

import (
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
)

func TestFlagWinsOverEnvironment(t *testing.T) {
	t.Setenv(EnvHTTPAddr, "0.0.0.0:8080")
	t.Setenv(EnvKeyBackend, "kms")
	t.Setenv(EnvKeyVaultURI, "https://kv.example.invalid/")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "")
	backend := fs.String("key-backend", "local", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	passed := visited(fs)
	if got := passed.str("addr", *addr, EnvHTTPAddr, "127.0.0.1:8090"); got != "0.0.0.0:8080" {
		t.Errorf("addr = %q, want the environment value", got)
	}
	if got := passed.str("key-backend", *backend, EnvKeyBackend, "local"); got != "kms" {
		t.Errorf("key-backend = %q, want the environment value", got)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	addr = fs.String("addr", "127.0.0.1:8090", "")
	if err := fs.Parse([]string{"--addr", "10.1.2.3:9000"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := visited(fs).str("addr", *addr, EnvHTTPAddr, "127.0.0.1:8090"); got != "10.1.2.3:9000" {
		t.Errorf("addr = %q, want the flag value", got)
	}
}

// TestBindAcknowledgement covers the vault's one deliberate refusal. The deployment's
// SAC_INTERNAL_ONLY=true is accepted as the acknowledgement, because it states the fact the flag
// asks a person to assert — and it must be exactly "true", not merely present.
func TestBindAcknowledgement(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		flagAck bool
		env     string
		setEnv  bool
		wantErr bool
	}{
		{name: "loopback needs no acknowledgement", addr: "127.0.0.1:8090"},
		{name: "localhost needs no acknowledgement", addr: "localhost:8090"},
		{name: "ipv6 loopback needs no acknowledgement", addr: "[::1]:8090"},
		{name: "non-loopback refused by default", addr: "0.0.0.0:8080", wantErr: true},
		{name: "flag acknowledges", addr: "0.0.0.0:8080", flagAck: true},
		{name: "SAC_INTERNAL_ONLY=true acknowledges", addr: "0.0.0.0:8080", setEnv: true, env: "true"},
		{name: "SAC_INTERNAL_ONLY is case-insensitive", addr: "0.0.0.0:8080", setEnv: true, env: "TRUE"},
		{name: "a non-affirmative value does not acknowledge", addr: "0.0.0.0:8080", setEnv: true, env: "yes-please", wantErr: true},
		{name: "an address without a port is refused", addr: "0.0.0.0", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setEnv {
				t.Setenv(EnvInternalOnly, c.env)
			} else {
				t.Setenv(EnvInternalOnly, "")
			}
			acknowledged := c.flagAck || envTrue(EnvInternalOnly)
			err := checkBindAddress(c.addr, acknowledged)
			if c.wantErr && err == nil {
				t.Fatalf("checkBindAddress(%q, %v) accepted a bind it should refuse", c.addr, acknowledged)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("checkBindAddress(%q, %v) = %v", c.addr, acknowledged, err)
			}
		})
	}
}

// TestReadinessRefusesAnUnimplementedBackend is the probe's whole point: a process that started with
// an acknowledged-but-absent KMS is alive and must not be sent traffic.
func TestReadinessRefusesAnUnimplementedBackend(t *testing.T) {
	local := keys.NewLocal()
	if reason, ready := readiness(local, "memory"); !ready {
		t.Errorf("the local backend must be ready: %s", reason)
	}
	if _, ready := readiness(nil, "memory"); ready {
		t.Error("no key backend must not report ready")
	}
	kms := keys.NewKMS("https://kv.example.invalid/", "software")
	if reason, ready := readiness(kms, "memory"); ready {
		t.Error("the unimplemented kms backend must not report ready")
	} else if !strings.Contains(reason, "not implemented") {
		t.Errorf("reason = %q, want it to name the missing backend", reason)
	}
}

func TestReadyzShape(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inner := http.NewServeMux()
	inner.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	inner.HandleFunc("/v1/content/retrieval", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	// Ready: the local backend, and the body names what is in use.
	h := withProbes(inner, keys.NewLocal(), "memory", logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{`"status":"ready"`, "local-software", `"store":"memory"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s is missing %s", rec.Body.String(), want)
		}
	}

	// Not ready: the unimplemented KMS.
	h = withProbes(inner, keys.NewKMS("https://kv.example.invalid/", "software"), "memory", logger)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an unimplemented backend", rec.Code)
	}

	// The service's own paths are untouched.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/content/retrieval", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("the wrapper changed a service path: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz was changed by the wrapper: %d", rec.Code)
	}
}

func TestValidateHTTPAddrAndCheckURL(t *testing.T) {
	for _, ok := range []string{"0.0.0.0:8080", "127.0.0.1:8090", "[::]:8080"} {
		if err := validateHTTPAddr(ok); err != nil {
			t.Errorf("validateHTTPAddr(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "8080", "0.0.0.0:", ":host", "0.0.0.0:0"} {
		if err := validateHTTPAddr(bad); err == nil {
			t.Errorf("validateHTTPAddr(%q) accepted an address the server cannot bind", bad)
		}
	}
	if err := checkURL("X", ""); err != nil {
		t.Errorf("an unset endpoint must be allowed: %v", err)
	}
	if err := checkURL("X", "https://kv.example.invalid/"); err != nil {
		t.Errorf("a valid URL was rejected: %v", err)
	}
	if err := checkURL("X", "kv.example.invalid"); err == nil {
		t.Error("a bare host is not an absolute URL")
	}
}

// TestPostgresDSNCarriesNoPassword: infra/main.bicep passes no password, §5.4 forbids one in a
// plaintext variable, and the deployed database authenticates the managed identity.
func TestPostgresDSNCarriesNoPassword(t *testing.T) {
	got := postgresDSN("pg.example.internal", "5432", "shadow", "content-vault")
	want := "postgres://content-vault@pg.example.internal:5432/shadow?sslmode=require"
	if got != want {
		t.Errorf("postgresDSN = %q, want %q", got, want)
	}
}
