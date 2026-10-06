// Command content-vault stores prompt content encrypted in PostgreSQL and is the only component that
// can decrypt it. It has internal ingress only: control-api uploads content under per-event grants,
// query-api forwards analysts' searches and retrieval requests, and the dashboard server forwards
// the single-use retrieval URLs it mints.
//
// Configuration is the environment; see README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/platform/postgres"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/httpapi"
	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// Environment variables. The database is configured by platform/postgres's SAC_PG_* variables.
const (
	EnvHTTPAddr         = "SAC_HTTP_ADDR"
	EnvContentKeys      = "SAC_CONTENT_KEYS"
	EnvAuthIssuer       = "SAC_AUTH_ISSUER"
	EnvAuthAudience     = "SAC_AUTH_AUDIENCE"
	EnvAuthJWKSURL      = "SAC_AUTH_JWKS_URL"
	EnvRetrievalURLBase = "SAC_RETRIEVAL_URL_BASE"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("content-vault: stopped", "error", err)
		os.Exit(1)
	}
}

// config is the validated environment.
type config struct {
	addr             string
	keys             *keyring.Keyring
	auth             auth.Config
	retrievalURLBase string
	db               postgres.Config
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		addr:             getenv(EnvHTTPAddr),
		retrievalURLBase: getenv(EnvRetrievalURLBase),
		auth: auth.Config{
			Issuer:   getenv(EnvAuthIssuer),
			Audience: getenv(EnvAuthAudience),
			JWKSURL:  getenv(EnvAuthJWKSURL),
		},
	}
	if c.addr == "" {
		c.addr = "127.0.0.1:8080"
	}
	if err := checkAddr(c.addr); err != nil {
		return config{}, err
	}
	spec := getenv(EnvContentKeys)
	if spec == "" {
		return config{}, fmt.Errorf("%s is required: content cannot be stored or read without the keyring", EnvContentKeys)
	}
	keys, err := keyring.Parse(spec)
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", EnvContentKeys, err)
	}
	c.keys = keys
	if c.auth.Issuer == "" {
		return config{}, fmt.Errorf("%s is required: every route but a retrieval URL is authorised by the issuer's tokens", EnvAuthIssuer)
	}
	for name, value := range map[string]string{EnvAuthIssuer: c.auth.Issuer, EnvAuthJWKSURL: c.auth.JWKSURL, EnvRetrievalURLBase: c.retrievalURLBase} {
		if err := checkURL(name, value); err != nil {
			return config{}, err
		}
	}
	return c, nil
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.db, err = postgres.ConfigFromEnv(); err != nil {
		return err
	}
	db, err := postgres.Open(cfg.db)
	if err != nil {
		return err
	}
	defer db.Close()
	st := store.New(db)

	verifier, err := auth.NewVerifier(cfg.auth)
	if err != nil {
		return err
	}
	svc, err := vault.New(vault.Config{Store: st, Keys: cfg.keys, RetrievalURLBase: cfg.retrievalURLBase, Logger: log})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           httpapi.New(svc, verifier, st.Ping, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	log.Info("content-vault: listening", "addr", cfg.addr, "database", cfg.db.String(),
		"issuer", cfg.auth.Issuer, "audience", verifier.Audience, "jwks", verifier.JWKSURL,
		"key_version", cfg.keys.Current(), "key_versions", cfg.keys.Versions(),
		"retrieval_url_base", cfg.retrievalURLBase)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Info("content-vault: shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func checkAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q is not host:port", EnvHTTPAddr, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s %q has no valid port", EnvHTTPAddr, addr)
	}
	return nil
}

func checkURL(name, value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s %q is not an absolute http(s) URL", name, value)
	}
	return nil
}
