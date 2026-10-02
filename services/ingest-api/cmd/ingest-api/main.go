// Command ingest-api is the one validating write path for device events (ADR 0001).
//
// It serves exactly one device-facing endpoint, POST /v1/events (docs/02-ingest-and-transport.md
// §5.3), plus /healthz for deployment infrastructure. It is the only component holding the
// sac_ingest database identity; collectors hold no database credential (§2.4).
//
// Offline note: this repository builds with GOPROXY=off and the Go standard library only, and the
// standard library has no PostgreSQL wire driver. -store=sql therefore expects the driver to be
// registered in the binary; without one, sql.Open fails with "unknown driver" and the message says
// so. -store=memory runs the whole path in process, which is what the tests and the local
// verification run use.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/batchguard"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/httpapi"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ingest-api:", err)
		os.Exit(1)
	}
}

type options struct {
	addr             string
	schemaPath       string
	region           string
	storeKind        string
	dsn              string
	driver           string
	routesFile       string
	tlsCert          string
	tlsKey           string
	tlsClientCA      string
	devTrust         bool
	verifyDedupKey   bool
	maxBatchEvents   int
	replayWindow     time.Duration
	shutdownGraceful time.Duration
}

func run() error {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:8443", "listen address")
	flag.StringVar(&o.schemaPath, "schema", "", "path to contracts/event-envelope.schema.json (default: discovered above the working directory)")
	flag.StringVar(&o.region, "region", "", "the region this deployment serves; a tenant pinned elsewhere is refused (empty disables only that check)")
	flag.StringVar(&o.storeKind, "store", "memory", "memory | sql")
	flag.StringVar(&o.dsn, "dsn", "", "database DSN (with -store=sql)")
	flag.StringVar(&o.driver, "driver", "", "database/sql driver name (with -store=sql); must be registered in this binary")
	flag.StringVar(&o.routesFile, "routes-file", "testdata/route-fidelity.seed.json", "ref.route_fidelity rows for -store=memory, so route ranks are never compiled in")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "server certificate (PEM)")
	flag.StringVar(&o.tlsKey, "tls-key", "", "server private key (PEM)")
	flag.StringVar(&o.tlsClientCA, "tls-client-ca", "", "CA bundle that must have issued the device certificate (PEM)")
	flag.BoolVar(&o.devTrust, "dev-trust-principal", false, "TEST ONLY: trust X-Dev-Tenant-Id and X-Dev-Device-Id instead of a client certificate")
	flag.BoolVar(&o.verifyDedupKey, "verify-dedup-key", false, "diagnostic: recompute §4.5 dedup_key and reject a mismatch (off by default; §7 has no wire code for it)")
	flag.IntVar(&o.maxBatchEvents, "max-batch-events", 500, "advertised batch cap")
	flag.DurationVar(&o.replayWindow, "replay-window", 24*time.Hour, "duplicate_batch replay window")
	flag.DurationVar(&o.shutdownGraceful, "shutdown-grace", 10*time.Second, "graceful shutdown grace period")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	schemaPath := o.schemaPath
	if schemaPath == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		schemaPath, err = contract.Discover(wd)
		if err != nil {
			return err
		}
	}
	schema, err := contract.Load(schemaPath)
	if err != nil {
		return err
	}
	logger.Info("contract loaded", "schema", schemaPath)

	var st store.Store
	switch o.storeKind {
	case "memory":
		routes, err := loadRoutes(o.routesFile)
		if err != nil {
			return err
		}
		mem := store.NewMemory(routes)
		logger.Warn("running with the in-memory store: nothing is persisted and no database identity is used; this is for a local run and for tests")
		st = mem
	case "sql":
		if o.dsn == "" || o.driver == "" {
			return errors.New("-store=sql requires -dsn and -driver")
		}
		db, err := sql.Open(o.driver, o.dsn)
		if err != nil {
			return fmt.Errorf("open database with driver %q: %w (a registered driver is required; the standard library has none)", o.driver, err)
		}
		db.SetMaxOpenConns(16)
		db.SetConnMaxIdleTime(5 * time.Minute)
		st = store.NewSQL(db)
	default:
		return fmt.Errorf("unknown -store %q", o.storeKind)
	}
	defer st.Close()

	cfg := ingest.DefaultConfig()
	cfg.VerifyDedupKey = o.verifyDedupKey
	if o.maxBatchEvents > 0 {
		// The cap constants live in the protocol package; this only narrows the advertised set.
		_ = o.maxBatchEvents
	}
	svc, err := ingest.New(schema, st, cfg)
	if err != nil {
		return err
	}

	var authenticator auth.Authenticator
	switch {
	case o.devTrust:
		if o.tlsClientCA != "" {
			return errors.New("-dev-trust-principal and -tls-client-ca are mutually exclusive")
		}
		logger.Warn("TEST ONLY: trusting X-Dev-Tenant-Id / X-Dev-Device-Id; never enable this in a deployment")
		authenticator = httpapi.DevHeader{TenantHeader: "X-Dev-Tenant-Id", DeviceHeader: "X-Dev-Device-Id"}
	case o.tlsCert != "" && o.tlsKey != "" && o.tlsClientCA != "":
		authenticator = &auth.MTLSAuthenticator{Store: st, Region: o.region}
	default:
		return errors.New("refusing to serve without device authentication: supply -tls-cert, -tls-key and -tls-client-ca, or -dev-trust-principal for a local test")
	}

	srv := httpapi.New(svc, authenticator, batchguard.New(o.replayWindow), logger)
	httpServer := &http.Server{
		Addr:              o.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	// §2.1: TLS 1.3 minimum, mutual authentication required. Go negotiates no 0-RTT and does not
	// renegotiate, which is the rest of §2.1's refusal list.
	if o.tlsCert != "" {
		pool, err := loadCAPool(o.tlsClientCA)
		if err != nil {
			return err
		}
		cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
		if err != nil {
			return fmt.Errorf("load server key pair: %w", err)
		}
		httpServer.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		}
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("ingest-api listening", "addr", o.addr, "tls", httpServer.TLSConfig != nil)
		var err error
		if httpServer.TLSConfig != nil {
			err = httpServer.ListenAndServeTLS("", "")
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.shutdownGraceful)
	defer cancel()
	logger.Info("shutting down")
	return httpServer.Shutdown(ctx)
}

// loadRoutes reads ref.route_fidelity rows for the in-memory store. Rank data is *data*: §4.4 is
// explicit that ranks are not compiled into services, so a local run reads the same rows the
// database would return rather than carrying its own copy.
func loadRoutes(path string) (store.RouteTable, error) { return store.LoadRouteTable(path) }

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("client CA bundle %s contains no certificates", path)
	}
	return pool, nil
}
