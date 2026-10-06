// Command ingest-api is the device event write path. It serves POST /v1/events behind Application
// Gateway, authenticates each device by its forwarded certificate, validates every envelope
// against the contract, and records the batch in PostgreSQL.
//
// Configuration is the environment: SAC_HTTP_ADDR, SAC_REGION and SAC_CA_CERT_PEM here, and the
// SAC_PG_* connection settings read by the platform postgres package.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/platform/postgres"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/httpapi"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Environment variables read by this command.
const (
	// EnvHTTPAddr is the listen address.
	EnvHTTPAddr = "SAC_HTTP_ADDR"
	// EnvRegion is the Azure location this deployment serves; a tenant pinned elsewhere is refused.
	EnvRegion = "SAC_REGION"
	// EnvCACertPEM is the device CA certificate, PEM: the issuer every forwarded device
	// certificate must chain to.
	EnvCACertPEM = "SAC_CA_CERT_PEM"
)

const defaultAddr = "127.0.0.1:8080"

type config struct {
	addr   string
	region string
	roots  *x509.CertPool
	pg     postgres.Config
}

func loadConfig() (config, error) {
	cfg := config{addr: os.Getenv(EnvHTTPAddr), region: os.Getenv(EnvRegion)}
	if cfg.addr == "" {
		cfg.addr = defaultAddr
	}
	if cfg.region == "" {
		return config{}, fmt.Errorf("%s is not set: the region pin cannot be enforced without it", EnvRegion)
	}
	caPEM := os.Getenv(EnvCACertPEM)
	if caPEM == "" {
		return config{}, fmt.Errorf("%s is not set: forwarded device certificates cannot be verified without the device CA", EnvCACertPEM)
	}
	cfg.roots = x509.NewCertPool()
	if !cfg.roots.AppendCertsFromPEM([]byte(caPEM)) {
		return config{}, fmt.Errorf("%s holds no PEM certificate", EnvCACertPEM)
	}
	pg, err := postgres.ConfigFromEnv()
	if err != nil {
		return config{}, err
	}
	cfg.pg = pg
	return cfg, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("ingest-api stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	validator, err := contract.NewValidator()
	if err != nil {
		return err
	}
	db, err := postgres.Open(cfg.pg)
	if err != nil {
		return err
	}
	defer db.Close()
	st := store.New(db)

	srv := &httpapi.Server{
		Service: ingest.New(validator, st),
		Auth:    &auth.Certificates{Store: st, Roots: cfg.roots, Region: cfg.region},
		Ready:   st.Ping,
		Logger:  logger,
	}
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	logger.Info("ingest-api listening", "addr", ln.Addr().String(), "region", cfg.region, "database", cfg.pg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
