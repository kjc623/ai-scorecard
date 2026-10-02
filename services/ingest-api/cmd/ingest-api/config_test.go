package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFlagWinsOverEnvironment is the precedence rule the whole vocabulary rests on. It is tested
// through the real flag package rather than a helper, because the rule is "did the operator pass
// this flag" and only the flag package knows that.
func TestFlagWinsOverEnvironment(t *testing.T) {
	t.Setenv(EnvHTTPAddr, "0.0.0.0:8080")
	t.Setenv(EnvStore, "sql")

	t.Run("environment applies when the flag is absent", func(t *testing.T) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		addr := fs.String("addr", "127.0.0.1:8443", "")
		storeKind := fs.String("store", "memory", "")
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		passed := visited(fs)
		if got := passed.str("addr", *addr, EnvHTTPAddr, "127.0.0.1:8443"); got != "0.0.0.0:8080" {
			t.Errorf("addr = %q, want the environment value", got)
		}
		if got := passed.str("store", *storeKind, EnvStore, "memory"); got != "sql" {
			t.Errorf("store = %q, want the environment value", got)
		}
	})

	t.Run("a passed flag wins", func(t *testing.T) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		addr := fs.String("addr", "127.0.0.1:8443", "")
		storeKind := fs.String("store", "memory", "")
		if err := fs.Parse([]string{"--addr", "10.0.0.1:9000", "--store", "memory"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		passed := visited(fs)
		if got := passed.str("addr", *addr, EnvHTTPAddr, "127.0.0.1:8443"); got != "10.0.0.1:9000" {
			t.Errorf("addr = %q, want the flag value", got)
		}
		if got := passed.str("store", *storeKind, EnvStore, "memory"); got != "memory" {
			t.Errorf("store = %q, want the flag value", got)
		}
	})

	t.Run("an explicitly empty flag is a request, not an absence", func(t *testing.T) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		region := fs.String("region", "eu-west", "")
		if err := fs.Parse([]string{"--region", ""}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		passed := visited(fs)
		if got := passed.str("region", *region, "SAC_REGION", "eu-west"); got != "" {
			t.Errorf("region = %q, want the empty value the operator asked for", got)
		}
	})
}

func TestBooleanResolution(t *testing.T) {
	cases := []struct {
		name     string
		envValue string
		setEnv   bool
		passFlag bool
		flagVal  bool
		def      bool
		want     bool
	}{
		{name: "no env, no flag, default true", def: true, want: true},
		{name: "env true", setEnv: true, envValue: "true", want: true},
		{name: "env TRUE", setEnv: true, envValue: "TRUE", want: true},
		{name: "env 1", setEnv: true, envValue: "1", want: true},
		{name: "env on", setEnv: true, envValue: "on", want: true},
		{name: "env false", setEnv: true, envValue: "false", want: false},
		{name: "env junk falls back to the default", setEnv: true, envValue: "banana", def: true, want: true},
		{name: "flag true beats env false", setEnv: true, envValue: "false", passFlag: true, flagVal: true, want: true},
		{name: "flag false beats env true", setEnv: true, envValue: "true", passFlag: true, flagVal: false, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const env = "SAC_TEST_BOOL"
			if c.setEnv {
				t.Setenv(env, c.envValue)
			} else {
				t.Setenv(env, "")
			}
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			b := fs.Bool("b", false, "")
			var args []string
			if c.passFlag {
				if c.flagVal {
					args = []string{"--b=true"}
				} else {
					args = []string{"--b=false"}
				}
			}
			if err := fs.Parse(args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := visited(fs).boolean("b", *b, env, c.def)
			if got != c.want {
				t.Errorf("boolean = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPostgresDSN(t *testing.T) {
	got := postgresDSN("pg.example.internal", "5432", "shadow", "ingest-api")
	want := "postgres://ingest-api@pg.example.internal:5432/shadow?sslmode=require"
	if got != want {
		t.Errorf("postgresDSN = %q, want %q", got, want)
	}
	if strings.Contains(got, ":password") || strings.Contains(got, "@:") {
		t.Errorf("the DSN must carry no password: %q", got)
	}
	if got := postgresDSN("", "", "", ""); got != "" {
		t.Errorf("postgresDSN with no host = %q, want empty", got)
	}
	if got := postgresDSN("h", "", "", ""); !strings.Contains(got, "5432") || !strings.Contains(got, "/shadow") {
		t.Errorf("defaults must fill port and database: %q", got)
	}
}

func TestValidateHTTPAddr(t *testing.T) {
	for _, ok := range []string{"0.0.0.0:8080", "127.0.0.1:8443", "[::]:8080", "localhost:1"} {
		if err := validateHTTPAddr(ok); err != nil {
			t.Errorf("validateHTTPAddr(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "8080", "0.0.0.0:", ":host"} {
		if err := validateHTTPAddr(bad); err == nil {
			t.Errorf("validateHTTPAddr(%q) accepted an address the server cannot bind", bad)
		}
	}
}

func TestCheckURL(t *testing.T) {
	if err := checkURL("X", ""); err != nil {
		t.Errorf("an unset endpoint must be allowed: %v", err)
	}
	if err := checkURL("X", "https://example.invalid/x"); err != nil {
		t.Errorf("a valid URL was rejected: %v", err)
	}
	for _, bad := range []string{"example.invalid", "://nope", "http://"} {
		if err := checkURL("X", bad); err == nil {
			t.Errorf("checkURL(%q) accepted a value that is not an absolute URL", bad)
		}
	}
}

// TestReadyzAnswersFromItsDependency covers the probe the container platform uses. It must not be a
// constant: a service whose dependency is broken has to leave the rotation.
func TestReadyzAnswersFromItsDependency(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inner := http.NewServeMux()
	inner.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	t.Run("ready", func(t *testing.T) {
		h := withProbes(inner, func(context.Context) error { return nil }, logger)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("dependency broken", func(t *testing.T) {
		h := withProbes(inner, func(context.Context) error { return errors.New("database is gone") }, logger)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"not-ready"`) {
			t.Errorf("body = %s", rec.Body.String())
		}
		// The dependency's error is logged, never returned: it can carry a hostname or a DSN.
		if strings.Contains(rec.Body.String(), "database is gone") {
			t.Error("the probe echoed the dependency error to the platform")
		}
	})

	t.Run("every other path passes through untouched", func(t *testing.T) {
		reachable := false
		inner := http.NewServeMux()
		inner.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		})
		inner.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
			reachable = true
			w.WriteHeader(http.StatusTeapot) // a status the wrapper can never produce
		})
		h := withProbes(inner, nil, logger)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
		if !reachable {
			t.Fatal("/v1/events did not reach the service handler")
		}
		if rec.Code != http.StatusTeapot {
			t.Errorf("/v1/events status = %d, want the service handler's own status", rec.Code)
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Errorf("/healthz was changed by the wrapper: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a nil probe still answers ready", func(t *testing.T) {
		h := withProbes(inner, nil, logger)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

// TestSQLRefusalIsActionable asserts the F4 decision in the form an operator reads it: the refusal
// must name the driver, the variables it read, and where the verification actually is. A refusal
// that says only "no driver" is a dead end, and a service that started in memory instead would be
// worse than either.
func TestSQLRefusalIsActionable(t *testing.T) {
	o := options{storeKind: "sql", pgHost: "pg.example.internal", pgPort: "5432", pgDatabase: "shadow", role: "ingest-api"}
	msg := sqlRefusal(o, postgresDSN(o.pgHost, o.pgPort, o.pgDatabase, o.role)).Error()
	for _, want := range []string{
		"github.com/jackc/pgx/v5/stdlib", "pgx",
		EnvPGHost, EnvPGDatabase, EnvRole,
		"pg.example.internal", "sslmode=require",
		"TestLive", "go get",
		"NOT verified",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "password=") {
		t.Errorf("the refusal prints a password: %s", msg)
	}
}
