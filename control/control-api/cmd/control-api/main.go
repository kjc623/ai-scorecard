// Command control-api is the device-facing control plane (docs/02-ingest-and-transport.md §5.1,
// §5.2; ADR 0020 decisions 3 and 4).
//
// It serves POST /v1/enrol and POST /v1/token, plus /healthz and /readyz for the platform. Enrolment
// issues a per-device credential -- an X.509 leaf from a CertificateSigner, or a registered DPoP
// public key -- and the token endpoint issues a short-lived, DPoP-bound access token. The device's
// private key never leaves the device: enrolment carries a CSR or a public JWK.
//
// Offline note: this repository builds with GOPROXY=off and the Go standard library only, and the
// standard library has no PostgreSQL wire driver. -store=sql therefore expects the driver to be
// registered in the binary (see cmd/control-api/driver_tagged.go); without one, sql.Open fails with
// "unknown driver" and the message says so.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/dpop"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/httpapi"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/signer"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/token"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "control-api:", err)
		os.Exit(1)
	}
}

// defaultDriverName is the database/sql driver this binary carries, or empty. It is set to "pgx" by
// driver_tagged.go in the sac_sql_driver build; the untagged binary refuses -store=sql rather than
// starting in memory while a deployment believes it is persisting.
var defaultDriverName = ""

type options struct {
	addr              string
	storeKind         string
	dsn               string
	driver            string
	pgHost            string
	pgPort            string
	pgDatabase        string
	role              string
	region            string
	caCertFile        string
	caKeyFile         string
	tokenKeyPEM       string
	tokenIssuer       string
	tokenAudience     string
	sans              string
	enrolmentTokenTTL time.Duration
	credentialTTL     time.Duration
	shutdownGraceful  time.Duration
}

func run() error {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:8443",
		"listen address (env "+EnvHTTPAddr+"); a container needs 0.0.0.0:8080, which the image sets")
	flag.StringVar(&o.storeKind, "store", "memory", "memory | sql (env "+EnvStore+")")
	flag.StringVar(&o.dsn, "dsn", "", "database DSN (with -store=sql); the caller must register a driver")
	flag.StringVar(&o.driver, "driver", "", "database/sql driver name (with -store=sql); must be registered in this binary")
	flag.StringVar(&o.pgHost, "pg-host", "", "database host (env "+EnvPGHost+"); used to build the DSN, not a password")
	flag.StringVar(&o.pgPort, "pg-port", "5432", "database port; the deployment passes no port, so it stays a flag")
	flag.StringVar(&o.pgDatabase, "pg-database", "shadow", "database name (env "+EnvPGDatabase+")")
	flag.StringVar(&o.role, "role", "control-api", "the identity this process runs as (env "+EnvRole+")")
	flag.StringVar(&o.region, "region", "", "the region this deployment serves; a tenant pinned elsewhere is refused (env "+EnvRegion+"; empty disables only that check)")
	flag.StringVar(&o.caCertFile, "ca-cert", "", "LocalCA certificate (PEM file); omitted generates a fresh development CA")
	flag.StringVar(&o.caKeyFile, "ca-key", "", "LocalCA private key (PEM file); required with -ca-cert")
	flag.StringVar(&o.tokenKeyPEM, "dpop-token-key-pem", "", "ES256 access-token signing key: PEM text or a PEM file path (env "+EnvDPoPTokenKeyPEM+"); required")
	flag.StringVar(&o.tokenIssuer, "token-issuer", "", "iss claim of issued access tokens (env "+EnvTokenIssuer+")")
	flag.StringVar(&o.tokenAudience, "token-audience", "", "aud claim of issued access tokens (env "+EnvTokenAudience+")")
	flag.StringVar(&o.sans, "sans", "", "comma-separated DNS names / IPs the issued leaf SAN carries; empty means none")
	flag.DurationVar(&o.enrolmentTokenTTL, "enrolment-token-ttl", 24*time.Hour,
		"life an operator-minted enrolment token would carry (env "+EnvEnrolmentTokenTTL+")")
	flag.DurationVar(&o.credentialTTL, "credential-ttl", 90*24*time.Hour,
		"life of an issued device credential (env "+EnvCredentialTTL+")")
	flag.DurationVar(&o.shutdownGraceful, "shutdown-grace", 10*time.Second, "graceful shutdown grace period")
	flag.Parse()

	passed := visited(flag.CommandLine)
	o.addr = passed.str("addr", o.addr, EnvHTTPAddr, "127.0.0.1:8443")
	o.storeKind = passed.str("store", o.storeKind, EnvStore, "memory")
	o.pgHost = passed.str("pg-host", o.pgHost, EnvPGHost, "")
	o.pgDatabase = passed.str("pg-database", o.pgDatabase, EnvPGDatabase, "shadow")
	o.role = passed.str("role", o.role, EnvRole, "control-api")
	o.region = passed.str("region", o.region, EnvRegion, "")
	o.tokenIssuer = passed.str("token-issuer", o.tokenIssuer, EnvTokenIssuer, "")
	o.tokenAudience = passed.str("token-audience", o.tokenAudience, EnvTokenAudience, "")
	o.credentialTTL = passed.duration("credential-ttl", o.credentialTTL, EnvCredentialTTL, 90*24*time.Hour)
	o.enrolmentTokenTTL = passed.duration("enrolment-token-ttl", o.enrolmentTokenTTL, EnvEnrolmentTokenTTL, 24*time.Hour)
	keyVaultURI := os.Getenv(EnvKeyVaultURI)
	appInsights := os.Getenv(EnvAppInsights)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := validateHTTPAddr(o.addr); err != nil {
		return err
	}
	if err := checkURL(EnvKeyVaultURI, keyVaultURI); err != nil {
		return err
	}
	if err := checkURL(EnvAppInsights, appInsights); err != nil {
		return err
	}
	if appInsights != "" {
		// Stated rather than implied: the deployment passes a connection string this build does not
		// export to. Adding an exporter means adding a dependency, and this build has none.
		logger.Warn("SAC_APPINSIGHTS is set but this build exports no telemetry to it; the connection string is read, validated, and never logged")
	}

	// §5.1: the access token's signing key is required. Refusing to start without it is the point.
	tokenKeyPEM := o.tokenKeyPEM
	if tokenKeyPEM == "" {
		tokenKeyPEM = os.Getenv(EnvDPoPTokenKeyPEM)
	}
	if tokenKeyPEM == "" {
		return fmt.Errorf("refusing to start without the access-token signing key: set %s or --dpop-token-key-pem "+
			"(an endpoint that issued tokens under an unstated key would be a different authority from the one configured)", EnvDPoPTokenKeyPEM)
	}
	tokenKeyBytes, err := pemValue(tokenKeyPEM, EnvDPoPTokenKeyPEM)
	if err != nil {
		return err
	}
	tokenKey, err := jose.ParseECPrivateKeyPEM(tokenKeyBytes)
	if err != nil {
		return err
	}

	sg, err := loadSigner(o, keyVaultURI, logger)
	if err != nil {
		return err
	}

	var st store.Store
	switch o.storeKind {
	case "memory":
		st = store.NewMemory()
		logger.Warn("running with the in-memory store: nothing is persisted and no database identity is used; this is for a local run and for tests")
	case "sql":
		dsn := o.dsn
		if dsn == "" {
			dsn = postgresDSN(o.pgHost, o.pgPort, o.pgDatabase, o.role)
		}
		driver := o.driver
		if driver == "" {
			driver = defaultDriverName
		}
		if driver == "" {
			return sqlRefusal(o, dsn)
		}
		db, err := sql.Open(driver, dsn)
		if err != nil {
			return fmt.Errorf("open database with driver %q: %w (a registered driver is required; the standard library has none)", driver, err)
		}
		db.SetMaxOpenConns(16)
		db.SetConnMaxIdleTime(5 * time.Minute)
		st = store.NewSQL(db)
		logger.Info("serving from PostgreSQL", "driver", driver, "dsn", redactDSN(dsn))
	default:
		return fmt.Errorf("unknown -store %q (want memory or sql)", o.storeKind)
	}
	defer st.Close()

	enrolSvc, err := enrol.New(st, sg, enrol.Config{
		Region:        o.region,
		CredentialTTL: o.credentialTTL,
		ProofSkew:     dpop.DefaultSkew,
	})
	if err != nil {
		return err
	}
	tokenSvc, err := token.New(st, tokenKey, token.Config{
		Issuer:    o.tokenIssuer,
		Audience:  o.tokenAudience,
		TokenTTL:  15 * time.Minute,
		ProofSkew: dpop.DefaultSkew,
		Region:    o.region,
	})
	if err != nil {
		return err
	}
	verifier, err := token.NewVerifier(&tokenKey.PublicKey, o.tokenIssuer, o.tokenAudience, nil)
	if err != nil {
		return err
	}

	srv := httpapi.New(enrolSvc, tokenSvc, verifier, st, logger)
	// The store probe behind /readyz: reading ops.tenant is a real query in SQL mode and a real state
	// check in memory mode. An unknown tenant is a healthy database, so only a transport error fails.
	ready := func(ctx context.Context) error {
		_, err := st.Tenant(ctx, "00000000-0000-0000-0000-000000000000")
		if err == nil || errors.Is(err, store.ErrUnknownTenant) {
			return nil
		}
		return err
	}
	httpServer := &http.Server{
		Addr:              o.addr,
		Handler:           withProbes(srv.Handler(), ready, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("control-api listening", "addr", o.addr, "store", o.storeKind,
			"signer", sg.Name(), "region", o.region,
			"keyvault_configured", keyVaultURI != "", "token_kid", tokenSvc.KeyID(),
			"credential_ttl", o.credentialTTL.String())
		errCh <- httpServer.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-stop:
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.shutdownGraceful)
	defer cancel()
	logger.Info("shutting down")
	return httpServer.Shutdown(ctx)
}

// loadSigner selects the certificate authority. A configured Key Vault URI selects the KeyVaultSigner,
// which refuses clearly until this build carries a vault client; otherwise a LocalCA loads the
// configured key pair or generates a fresh development one. The choice is stated at startup, never
// made silently per request.
func loadSigner(o options, keyVaultURI string, logger *slog.Logger) (signer.CertificateSigner, error) {
	if keyVaultURI != "" {
		logger.Warn("SAC_KEYVAULT_URI is set: certificate signing selects the Key Vault signer, which this build does not implement; a certificate enrolment will be refused until the vault client lands")
		return &signer.KeyVaultSigner{VaultURI: keyVaultURI}, nil
	}
	var caCert, caKey []byte
	if (o.caCertFile == "") != (o.caKeyFile == "") {
		return nil, fmt.Errorf("LocalCA needs --ca-cert and --ca-key together, or neither to generate a development CA")
	}
	if o.caCertFile != "" {
		var err error
		if caCert, err = os.ReadFile(o.caCertFile); err != nil {
			return nil, fmt.Errorf("read CA certificate: %w", err)
		}
		if caKey, err = os.ReadFile(o.caKeyFile); err != nil {
			return nil, fmt.Errorf("read CA key: %w", err)
		}
	} else {
		caCert = []byte(os.Getenv(EnvCACertPEM))
		caKey = []byte(os.Getenv(EnvCAKeyPEM))
	}
	sans := splitList(o.sans)
	ca, err := signer.NewLocalCA(caCert, caKey, o.credentialTTL, sans)
	if err != nil {
		return nil, err
	}
	if len(caCert) == 0 {
		logger.Warn("no CA material was configured: a fresh development CA was generated in process; every credential is invalid after a restart")
	}
	return ca, nil
}

// pemValue accepts PEM text or a path to a PEM file, so the same flag works with an environment
// secret (text) and with a file on a laptop.
func pemValue(value, name string) ([]byte, error) {
	if strings.Contains(value, "-----BEGIN") {
		return []byte(value), nil
	}
	b, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("%s: value is neither PEM text nor a readable file: %w", name, err)
	}
	return b, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sqlRefusal is stated as a message rather than a dead end: the default build carries no PostgreSQL
// driver, and the driver is compiled only under the sac_sql_driver tag so `go build ./...` stays
// standard-library only. The message names the tagged build rather than starting in memory while a
// deployment believes it is persisting.
func sqlRefusal(o options, dsn string) error {
	return fmt.Errorf(`--store sql cannot start in this build: no PostgreSQL driver is compiled in.

  driver   github.com/jackc/pgx/v5/stdlib (registered as "pgx"). This build does not carry it -- the
           dependency is compiled only under the sac_sql_driver tag, which is what keeps the default
           build dependency-free (see sqlpg/doc.go).
  dsn      %s
           from %s=%s %s=%s %s=%s (read from this process; never logged with a credential in them)

  the tagged build, which uses the real driver and needs no -driver flag:
      go build -tags sac_sql_driver -o control-api-sql ./cmd/control-api
      ./control-api-sql -store sql -dsn "$SAC_PG_DSN"

  what IS verified:
      go test -tags sac_sql_driver ./sqlpg/ -v

  to fetch the driver on a host with a module proxy:
      go get github.com/jackc/pgx/v5/stdlib && go mod tidy`,
		dsn, EnvPGHost, o.pgHost, EnvPGDatabase, o.pgDatabase, EnvRole, o.role)
}
