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
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

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

// defaultDriverName is the database/sql driver this binary carries, or empty.
//
// It is empty in the default build, which has no third-party dependency: `-store sql` then refuses
// with the actionable message below rather than starting and degrading. It is set to "pgx" by
// cmd/ingest-api/driver_tagged.go in the sac_sql_driver build, where the driver is linked in — so the
// tagged binary needs no -driver flag and the untagged one cannot pretend to have one.
var defaultDriverName = ""

type options struct {
	addr             string
	schemaPath       string
	region           string
	storeKind        string
	dsn              string
	driver           string
	pgHost           string
	pgPort           string
	pgDatabase       string
	role             string
	routesFile       string
	tlsCert          string
	tlsKey           string
	tlsClientCA      string
	authModes        string
	clientCertHeader string
	dpopTokenPublic  string
	dpopIssuer       string
	dpopAudience     string
	devTrust         bool
	devSeed          string
	devCredentialID  string
	verifyDedupKey   bool
	replayWindow     time.Duration
	shutdownGraceful time.Duration
}

func run() error {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:8443",
		"listen address (env "+EnvHTTPAddr+"); a container needs 0.0.0.0:8080, which the image sets")
	flag.StringVar(&o.schemaPath, "schema", "", "path to contracts/event-envelope.schema.json (env "+EnvSchema+"; default: discovered above the working directory)")
	flag.StringVar(&o.region, "region", "", "the region this deployment serves; a tenant pinned elsewhere is refused (env "+EnvRegion+"; empty disables only that check)")
	flag.StringVar(&o.storeKind, "store", "memory", "memory | sql (env "+EnvStore+")")
	flag.StringVar(&o.dsn, "dsn", "", "database DSN (with -store=sql); the caller must register a driver")
	flag.StringVar(&o.driver, "driver", "", "database/sql driver name (with -store=sql); must be registered in this binary")
	flag.StringVar(&o.pgHost, "pg-host", "", "database host (env "+EnvPGHost+"); used to build the DSN, not a password")
	flag.StringVar(&o.pgPort, "pg-port", "5432", "database port; the deployment passes no port, so it stays a flag")
	flag.StringVar(&o.pgDatabase, "pg-database", "shadow", "database name (env "+EnvPGDatabase+")")
	flag.StringVar(&o.role, "role", "ingest-api", "the identity this process runs as (env "+EnvRole+")")
	flag.StringVar(&o.routesFile, "routes-file", "testdata/route-fidelity.seed.json",
		"ref.route_fidelity rows (env "+EnvRoutesFile+"), so route ranks are never compiled in")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "server certificate (PEM)")
	flag.StringVar(&o.tlsKey, "tls-key", "", "server private key (PEM)")
	flag.StringVar(&o.tlsClientCA, "tls-client-ca", "", "CA bundle that must have issued the device certificate (PEM)")
	flag.StringVar(&o.authModes, "auth-modes", "", "comma list of production auth modes: x509, dpop (env "+EnvAuthModes+"; default: inferred from the material present)")
	flag.StringVar(&o.clientCertHeader, "tls-client-cert-header", "", "edge-forwarded leaf certificate header (env "+EnvTLSClientCertHeader+"; empty disables the forwarded x509 path)")
	flag.StringVar(&o.dpopTokenPublic, "dpop-token-public-pem", "", "deployment access-token signing public key (PEM text; env "+EnvDPoPTokenPublicPEM+")")
	flag.StringVar(&o.dpopIssuer, "dpop-issuer", "", "access-token issuer the DPoP mode requires (env "+EnvDPoPIssuer+")")
	flag.StringVar(&o.dpopAudience, "dpop-audience", "", "access-token audience the DPoP mode requires (env "+EnvDPoPAudience+")")
	flag.BoolVar(&o.devTrust, "dev-trust-principal", false, "TEST ONLY: trust X-Dev-Tenant-Id and X-Dev-Device-Id instead of a client certificate")
	flag.StringVar(&o.devCredentialID, "dev-credential-id", "dev",
		"TEST ONLY with -dev-trust-principal: the credential id the header principal presents. The in-memory store ignores it; a -store sql run needs a uuid that names an ops.device_credential row")
	flag.StringVar(&o.devSeed, "dev-seed-principal", "", "TEST ONLY with -store=memory: register tenant:device[:credential] as an active principal so a local run can accept a batch (default credential \"dev\", matching -dev-trust-principal)")
	flag.BoolVar(&o.verifyDedupKey, "verify-dedup-key", false, "diagnostic: recompute §4.5 dedup_key and reject a mismatch (off by default; §7 has no wire code for it)")
	flag.DurationVar(&o.replayWindow, "replay-window", 24*time.Hour, "duplicate_batch replay window")
	flag.DurationVar(&o.shutdownGraceful, "shutdown-grace", 10*time.Second, "graceful shutdown grace period")
	flag.Parse()

	// Everything below this line is a resolved setting: flag if it was passed, otherwise the
	// deployment's environment, otherwise the laptop default.
	passed := visited(flag.CommandLine)
	o.addr = passed.str("addr", o.addr, EnvHTTPAddr, "127.0.0.1:8443")
	o.schemaPath = passed.str("schema", o.schemaPath, EnvSchema, "")
	o.region = passed.str("region", o.region, EnvRegion, "")
	o.authModes = passed.str("auth-modes", o.authModes, EnvAuthModes, "")
	o.clientCertHeader = passed.str("tls-client-cert-header", o.clientCertHeader, EnvTLSClientCertHeader, "")
	o.dpopTokenPublic = passed.str("dpop-token-public-pem", o.dpopTokenPublic, EnvDPoPTokenPublicPEM, "")
	o.dpopIssuer = passed.str("dpop-issuer", o.dpopIssuer, EnvDPoPIssuer, "")
	o.dpopAudience = passed.str("dpop-audience", o.dpopAudience, EnvDPoPAudience, "")
	o.storeKind = passed.str("store", o.storeKind, EnvStore, "memory")
	o.pgHost = passed.str("pg-host", o.pgHost, EnvPGHost, "")
	o.pgDatabase = passed.str("pg-database", o.pgDatabase, EnvPGDatabase, "shadow")
	o.role = passed.str("role", o.role, EnvRole, "ingest-api")
	o.routesFile = passed.str("routes-file", o.routesFile, EnvRoutesFile, "testdata/route-fidelity.seed.json")
	blobEndpoint := os.Getenv(EnvBlobCiphertextEndpoint)
	appInsights := os.Getenv(EnvAppInsights)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := validateHTTPAddr(o.addr); err != nil {
		return err
	}
	if err := checkURL(EnvAppInsights, appInsights); err != nil {
		return err
	}
	if err := checkURL(EnvBlobCiphertextEndpoint, blobEndpoint); err != nil {
		return err
	}

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
	// The repository root, derived from the schema's own location, is what lets the development
	// defaults below be module-relative and still work from any working directory.
	repoRoot := filepath.Dir(filepath.Dir(schemaPath))
	logger.Info("contract loaded", "schema", schemaPath, "role", o.role, "region", o.region,
		"appinsights_configured", appInsights != "",
		"blob_ciphertext_endpoint_configured", blobEndpoint != "")
	if appInsights != "" {
		// Stated rather than implied: the deployment passes a connection string this build does not
		// export to. Adding an exporter means adding a dependency, and this build has none.
		logger.Warn("SAC_APPINSIGHTS is set but this build exports no telemetry to it; the connection string is read, validated, and never logged")
	}
	if blobEndpoint != "" {
		logger.Warn("SAC_BLOB_CIPHERTEXT_ENDPOINT is set but the ingest path performs no blob I/O; the endpoint is validated and recorded, not used")
	}

	var st store.Store
	switch o.storeKind {
	case "memory":
		routes, err := loadRoutes(o.routesFile, repoRoot)
		if err != nil {
			return err
		}
		mem := store.NewMemory(routes)
		if o.devSeed != "" {
			tenant, device, credential, err := parseSeed(o.devSeed)
			if err != nil {
				return err
			}
			mem.SetPrincipal(tenant, device, credential, store.PrincipalStatus{
				TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: o.region,
				DeviceKnown: true, CredentialKnown: true,
				CredentialExpiry: time.Now().Add(90 * 24 * time.Hour),
			})
			logger.Warn("TEST ONLY: seeded an active principal in the in-memory store",
				"tenant", tenant, "device", device, "credential", credential)
		}
		logger.Warn("running with the in-memory store: nothing is persisted and no database identity is used; this is for a local run and for tests")
		st = mem
	case "sql":
		if o.devSeed != "" {
			return errors.New("-dev-seed-principal is only for -store=memory; with a database the principal comes from ops.*")
		}
		dsn := o.dsn
		if dsn == "" {
			dsn = postgresDSN(o.pgHost, o.pgPort, o.pgDatabase, o.role)
		}
		// -driver wins; the tagged build knows its own driver and needs neither flag. The default
		// build has neither, so this is where it refuses with something actionable instead of
		// starting and quietly serving out of memory.
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

	cfg := ingest.DefaultConfig()
	cfg.VerifyDedupKey = o.verifyDedupKey
	svc, err := ingest.New(schema, st, cfg)
	if err != nil {
		return err
	}

	// The authenticator material decides how devices authenticate. The direct listener needs a server
	// key pair (files on a laptop, PEM from the environment in a container); the forwarded path needs
	// only the client CA, because the edge terminated TLS. Both are loaded before the mode set is
	// validated so a mode with no material is a startup refusal that names the setting to supply.
	material, err := loadTLSMaterial(o)
	if err != nil {
		return err
	}
	clientCA, err := loadClientCAPool(o)
	if err != nil {
		return err
	}
	tokenKey, err := loadTokenPublicKey(o.dpopTokenPublic)
	if err != nil {
		return err
	}
	am := authMaterial{
		clientCA: clientCA, directTLS: material != nil, forwardedHeader: o.clientCertHeader,
		tokenPublicKey: tokenKey, issuer: o.dpopIssuer, audience: o.dpopAudience,
	}

	var authenticator auth.Authenticator
	if o.devTrust {
		if material != nil || clientCA != nil || o.clientCertHeader != "" || tokenKey != nil || o.authModes != "" {
			return errors.New("-dev-trust-principal and production device-authentication material are mutually exclusive: one replaces the device credential, the other verifies it")
		}
		logger.Warn("TEST ONLY: trusting X-Dev-Tenant-Id / X-Dev-Device-Id; never enable this in a deployment")
		authenticator = httpapi.DevHeader{
			TenantHeader: "X-Dev-Tenant-Id", DeviceHeader: "X-Dev-Device-Id",
			CredentialID: o.devCredentialID,
		}
	} else {
		modes, err := parseAuthModes(o.authModes)
		if err != nil {
			return err
		}
		if len(modes) == 0 {
			modes = inferAuthModes(am)
		}
		if err := checkAuthMaterial(modes, am); err != nil {
			return err
		}
		var p auth.Pluggable
		for _, mode := range modes {
			switch mode {
			case protocol.AuthModeX509:
				if material != nil {
					p.Direct = &auth.MTLSAuthenticator{Store: st, Region: o.region}
				}
				if o.clientCertHeader != "" {
					p.Forwarded = &auth.ForwardedCertAuthenticator{
						Store: st, ClientCAs: clientCA, Region: o.region, Header: o.clientCertHeader,
					}
				}
			case protocol.AuthModeDPoP:
				p.DPoP = &auth.DPoPAuthenticator{
					Store: st, TokenPublicKey: tokenKey, Issuer: o.dpopIssuer, Audience: o.dpopAudience,
					Region: o.region, Now: time.Now,
				}
			}
		}
		selector, err := auth.NewPluggable(p)
		if err != nil {
			return refusalNoDeviceAuth()
		}
		logger.Info("device authentication configured",
			"modes", modes, "direct_tls", p.Direct != nil, "forwarded_header", o.clientCertHeader)
		authenticator = selector
	}

	srv := httpapi.New(svc, authenticator, batchguard.New(o.replayWindow), logger)
	// The store probe behind /readyz: reading the route table is a real query in the SQL mode and a
	// real state check in memory mode, and it is the one dependency the write path cannot run without.
	ready := func(ctx context.Context) error {
		_, err := st.RouteFidelity(ctx)
		return err
	}
	httpServer := &http.Server{
		Addr:              o.addr,
		Handler:           withProbes(srv.Handler(), ready, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	// §2.1: TLS 1.3 minimum, mutual authentication required.
	if material != nil {
		httpServer.TLSConfig = material.serverTLSConfig()
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

// refusalNoDeviceAuth is the existing "refuses to serve without device authentication" refusal. It
// is reached whenever no production mode is configured and the dev acknowledgement is absent, and
// it names every material form the deployment could supply, including the two ADR 0020 adds.
func refusalNoDeviceAuth() error {
	return fmt.Errorf(`refusing to serve without device authentication.

  supply the material as files:      --tls-cert FILE --tls-key FILE --tls-client-ca FILE
  or as PEM in the environment:      %s, %s, %s
                                     (what a deployment injects from Key Vault: the Container Apps
                                      module has no command/args and no volume mount, but it does
                                      have keyVaultEnv)
  or forward the certificate:        --tls-client-cert-header X-Client-Cert plus a client CA
  or serve DPoP:                     --auth-modes dpop --dpop-token-public-pem PEM
                                     --dpop-issuer ISS --dpop-audience AUD
  or, for a local test only:         -dev-trust-principal`,
		EnvTLSCertPEM, EnvTLSKeyPEM, EnvTLSClientCAPEM)
}

// sqlRefusal is the F4 decision (task-24), stated as a message rather than a dead end.
//
// The default build carries no PostgreSQL driver: the third-party dependency is compiled only under
// the sac_sql_driver build tag, so that `go build ./...` and `go test ./...` stay standard-library
// only and a machine with no module cache still passes. The two acceptable failures were "refuse with
// something actionable" or "hide it behind a build tag"; this does both, and neither vendors a driver
// this host cannot test nor starts in memory while a deployment believes it is persisting.
//
// The DSN is printed without a password by construction (postgresDSN builds one from the managed
// identity), and the environment values named here are never secret.
func sqlRefusal(o options, dsn string) error {
	return fmt.Errorf(`--store sql cannot start in this build: no PostgreSQL driver is compiled in.

  driver   github.com/jackc/pgx/v5/stdlib (registered as "pgx"). This build does not carry it -- the
           dependency is compiled only under the sac_sql_driver tag, which is what keeps the default
           build dependency-free (see sqlpg/doc.go).
  dsn      %s
           from %s=%s %s=%s %s=%s (read from this process; never logged with a credential in them)

  the tagged build, which uses the real driver and needs no -driver flag:
      go build -tags sac_sql_driver -o ingest-api-sql ./cmd/ingest-api
      ./ingest-api-sql -store sql -dsn "$SAC_PG_DSN"

  what IS verified:
      go test ./internal/store -run TestLive -v            # the statement text, against a live server
      go test -tags sac_sql_driver ./sqlpg/ -v             # the database/sql plumbing: pooling, the
                                                           # transaction boundary, record_event, errors

  to fetch the driver on a host with a module proxy:
      go get github.com/jackc/pgx/v5/stdlib && go mod tidy`,
		dsn, EnvPGHost, o.pgHost, EnvPGDatabase, o.pgDatabase, EnvRole, o.role)
}

// parseSeed reads tenant:device[:credential] for -dev-seed-principal.
func parseSeed(s string) (tenant, device, credential string, err error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", "", fmt.Errorf("-dev-seed-principal %q is not tenant:device[:credential]", s)
	}
	tenant, device = parts[0], parts[1]
	credential = "dev"
	if len(parts) == 3 {
		credential = parts[2]
	}
	if !contract.IsUUID(tenant) || !contract.IsUUID(device) {
		return "", "", "", fmt.Errorf("-dev-seed-principal %q: tenant and device must be uuids", s)
	}
	return tenant, device, credential, nil
}

// loadRoutes reads ref.route_fidelity rows for the in-memory store. Rank data is *data*: §4.4 is
// explicit that ranks are not compiled into services, so a local run reads the same rows the
// database would return rather than carrying its own copy.
//
// The flag's default is module-relative, and the process may be started from the repository root or
// from the module directory. Rather than fail with a path that looks plausible, the file is looked
// for in the three places it can be: as given, relative to the repository root derived from the
// contract schema, and upwards from the working directory.
func loadRoutes(path, repoRoot string) (store.RouteTable, error) {
	if table, err := store.LoadRouteTable(path); err == nil {
		return table, nil
	}
	if repoRoot != "" {
		candidate := filepath.Join(repoRoot, "ingestion", "ingest-api", filepath.FromSlash(path))
		if table, err := store.LoadRouteTable(candidate); err == nil {
			return table, nil
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if found, ferr := findUpwards(wd, filepath.FromSlash(path)); ferr == nil {
			return store.LoadRouteTable(found)
		}
	}
	return nil, fmt.Errorf("read route table %s: not found as given, at the repository root (%s), or above the working directory",
		path, repoRoot)
}

// findUpwards walks up from `from` looking for a relative path. It takes the starting directory
// rather than reading the working directory itself, so a caller -- and a test -- can name the tree
// it means instead of mutating process-wide state.
func findUpwards(from, rel string) (string, error) {
	dir, err := filepath.Abs(from)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, rel)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find %s above %s", rel, from)
		}
		dir = parent
	}
}

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
