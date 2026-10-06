package postgres

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingTokens struct{ calls atomic.Int32 }

func (c *countingTokens) Token(context.Context, string) (string, error) {
	c.calls.Add(1)
	return "entra-token", nil
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvHost, "db.internal")
	t.Setenv(EnvPort, "6432")
	t.Setenv(EnvDatabase, "sac")
	t.Setenv(EnvUser, "ingest-api")
	t.Setenv(EnvPassword, "")
	t.Setenv(EnvSSLMode, "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.internal" || cfg.Port != 6432 || cfg.Database != "sac" || cfg.User != "ingest-api" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if got := cfg.String(); got != "postgres://ingest-api@db.internal:6432/sac (auth=entra, sslmode=require)" {
		t.Fatalf("String() = %q", got)
	}
}

func TestConfigFromEnvRejectsMissingSettings(t *testing.T) {
	for _, missing := range []string{EnvHost, EnvDatabase, EnvUser} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv(EnvHost, "h")
			t.Setenv(EnvDatabase, "d")
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPort, "")
			t.Setenv(missing, "")
			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("err = %v, want one naming %s", err, missing)
			}
		})
	}
}

func TestStringNeverShowsThePassword(t *testing.T) {
	cfg := Config{Host: "h", Database: "d", User: "u", Password: "hunter2"}
	if s := cfg.String(); strings.Contains(s, "hunter2") || !strings.Contains(s, "auth=password") {
		t.Fatalf("String() = %q", s)
	}
}

// Every new connection asks for a token before it dials, so a pool pointed at a closed port still
// shows the token being requested.
func TestEntraTokenIsRequestedPerConnection(t *testing.T) {
	tokens := &countingTokens{}
	db, err := Open(Config{Host: "127.0.0.1", Port: 1, Database: "d", User: "u", SSLMode: "disable", Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("ping to a closed port succeeded")
	}
	if tokens.calls.Load() == 0 {
		t.Fatal("no Entra token was requested before connecting")
	}
}

func TestOpenWithoutPasswordOrIdentityFails(t *testing.T) {
	t.Setenv("IDENTITY_ENDPOINT", "")
	t.Setenv("IDENTITY_HEADER", "")
	if _, err := Open(Config{Host: "h", Database: "d", User: "u"}); err == nil {
		t.Fatal("Open succeeded with neither a password nor a managed identity")
	}
}
