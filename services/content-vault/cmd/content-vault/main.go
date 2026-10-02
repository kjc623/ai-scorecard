// Command content-vault is the content vault: the only component that can unwrap a content key
// (docs/06 §5.3, D7).
//
// **Internal ingress only.** It has no device-facing endpoint and no user-facing endpoint: devices
// reach content through control-api's grant decision and then write ciphertext straight to blob
// storage, and browsers reach content through query-api, which calls this service over the internal
// network under its own service identity (docs/02 §11). The binary refuses to bind a non-loopback
// address unless the operator says so explicitly, which is the most a process can check about its
// own ingress — and the README states that the acknowledgement is not a substitute for the network
// control.
//
// Subcommands:
//
//	serve       run the internal HTTP surface
//	schema-sql  print the SQL this service issues, as a psql script, for the live-schema harness
//	version     print the build's identity and the key backend it would use
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/httpapi"
	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// Version is the service's version.
const Version = "0.1.0-dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var code int
	switch args[0] {
	case "serve":
		code = runServe(args[1:])
	case "schema-sql":
		code = runSchemaSQL(args[1:])
	case "version":
		fmt.Printf("content-vault %s\n", Version)
	default:
		fmt.Fprintf(os.Stderr, "content-vault: unknown subcommand %q\n\n", args[0])
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `content-vault — grant-bound content retrieval (docs/06 §5.3, docs/02 §10-11)

  serve       --addr HOST:PORT [--key-backend local|kms] [--key-file FILE]
              [--allow-non-loopback] [--allow-unimplemented-kms]
  schema-sql  [--out FILE]
  version

The service has internal ingress only. Binding a non-loopback address requires
--allow-non-loopback, and the deployment must additionally lock its origin to query-api and
control-api (docs/02 §12); the flag is an acknowledgement, not a control.
`)
}

func fatalf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "content-vault: "+format+"\n", args...)
	return 1
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "listen address (env "+EnvHTTPAddr+"); a container needs 0.0.0.0:8080, which the image sets")
	backend := fs.String("key-backend", "local", "local | kms (env "+EnvKeyBackend+")")
	keyFile := fs.String("key-file", "", "local key file (development); empty means in-memory")
	keyVaultURI := fs.String("keyvault-uri", "", "Key Vault URI the kms backend would call (env "+EnvKeyVaultURI+")")
	blobEndpoint := fs.String("blob-ciphertext-endpoint", "", "private blob endpoint for ciphertext (env "+EnvBlobCiphertextEndpoint+")")
	role := fs.String("role", "content-vault", "the identity this process runs as (env "+EnvRole+")")
	allowNonLoopback := fs.Bool("allow-non-loopback", false, "acknowledge that this binds a non-loopback address (env "+EnvAllowNonLoopback+", or "+EnvInternalOnly+"=true from a deployment that has internal ingress)")
	allowKMS := fs.Bool("allow-unimplemented-kms", false, "start with a cloud KMS backend this build has not implemented")
	storeKind := fs.String("store", "memory", "memory | sql (env "+EnvStore+")")
	dsn := fs.String("dsn", "", "database/sql DSN (the caller must register a driver)")
	pgHost := fs.String("pg-host", "", "database host (env "+EnvPGHost+"); used to build the DSN, not a password")
	pgPort := fs.String("pg-port", "5432", "database port; the deployment passes no port, so it stays a flag")
	pgDatabase := fs.String("pg-database", "shadow", "database name (env "+EnvPGDatabase+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The deployment's names, with a flag winning. Everything below is a resolved setting.
	passed := visited(fs)
	*addr = passed.str("addr", *addr, EnvHTTPAddr, "127.0.0.1:8090")
	*backend = passed.str("key-backend", *backend, EnvKeyBackend, "local")
	*keyVaultURI = passed.str("keyvault-uri", *keyVaultURI, EnvKeyVaultURI, "")
	*blobEndpoint = passed.str("blob-ciphertext-endpoint", *blobEndpoint, EnvBlobCiphertextEndpoint, "")
	*role = passed.str("role", *role, EnvRole, "content-vault")
	*storeKind = passed.str("store", *storeKind, EnvStore, "memory")
	*pgHost = passed.str("pg-host", *pgHost, EnvPGHost, "")
	*pgDatabase = passed.str("pg-database", *pgDatabase, EnvPGDatabase, "shadow")
	*allowNonLoopback = passed.boolean("allow-non-loopback", *allowNonLoopback, EnvAllowNonLoopback, false)
	appInsights := os.Getenv(EnvAppInsights)

	// A non-loopback bind needs an acknowledgement from the operator or from the deployment. The
	// deployment's form is SAC_INTERNAL_ONLY=true, which says the fact the flag asks a person to
	// assert; it is still an acknowledgement and not a control (docs/02 §12, and the README).
	internalOnly := envTrue(EnvInternalOnly)
	acknowledged := *allowNonLoopback || internalOnly

	logger := slog.Default()
	if err := validateHTTPAddr(*addr); err != nil {
		return fatalf("%v", err)
	}
	if err := checkURL(EnvKeyVaultURI, *keyVaultURI); err != nil {
		return fatalf("%v", err)
	}
	if err := checkURL(EnvBlobCiphertextEndpoint, *blobEndpoint); err != nil {
		return fatalf("%v", err)
	}
	if appInsights != "" {
		// The deployment passes a connection string this build does not export to. Read, reported,
		// and never logged: a connection string carries a key.
		slog.Warn("SAC_APPINSIGHTS is set but this build exports no telemetry to it; the connection string is read, validated, and never logged")
	}

	// The key backend is the component's whole reason to exist, so the one thing this binary must
	// never do is claim a backend it does not have.
	var kw keys.KeyWrapper
	switch *backend {
	case "local":
		if *keyFile != "" {
			w, err := keys.OpenLocal(*keyFile)
			if err != nil {
				return fatalf("opening the local key file: %v", err)
			}
			kw = w
		} else {
			kw = keys.NewLocal()
		}
	case "kms":
		if !*allowKMS {
			return fatalf("the azure-key-vault backend is NOT IMPLEMENTED in this build (no network, no cloud SDK: TOOLCHAIN-DECISION.md §3). " +
				"Starting with it would mean a deployment reporting a KMS it does not have. Pass --allow-unimplemented-kms only to demonstrate the refusal. " +
				"(" + EnvKeyBackend + "=" + *backend + ", " + EnvKeyVaultURI + "=" + *keyVaultURI + ")")
		}
		endpoint := *keyVaultURI
		if endpoint == "" {
			endpoint = os.Getenv("CONTENT_VAULT_KMS_ENDPOINT")
		}
		kw = keys.NewKMS(endpoint, os.Getenv("CONTENT_VAULT_KMS_MODE"))
	default:
		return fatalf("unknown --key-backend %q (want local or kms)", *backend)
	}

	if *storeKind != "memory" {
		if *storeKind != "sql" {
			return fatalf("unknown --store %q (want memory or sql)", *storeKind)
		}
		// The SQL store needs a registered database/sql driver, and no PostgreSQL driver is
		// fetchable offline (ADR 0016). Saying which driver, which variables and which evidence
		// exists is the difference between a refusal and a dead end; the one thing this must never
		// do is start in memory and let a deployment believe it is persisting.
		databaseDSN := postgresDSN(*pgHost, *pgPort, *pgDatabase, *role)
		return fatalf(`--store sql cannot start in this build: no PostgreSQL driver is compiled in.

  driver       github.com/jackc/pgx/v5/stdlib (registered as "pgx"); a Go module, deliberately not vendored by an offline build
  dsn          %s   (from %s=%s %s=%s %s=%s)
  --dsn        %q
  what IS verified: every statement this service issues is rendered by `+"`schema-sql`"+` and executed against a
  live PostgreSQL by internal/store's live-schema harness -- the SQL text, not the database/sql plumbing.
  to close it on a host with a module proxy:
      go get github.com/jackc/pgx/v5/stdlib
      go build ./...
      content-vault serve -store sql -dsn "$SAC_PG_DSN"`,
			databaseDSN, EnvPGHost, *pgHost, EnvPGDatabase, *pgDatabase, EnvRole, *role, *dsn)
	}

	svc, err := vault.New(vault.Options{
		Store:      store.NewMemory(),
		Keys:       kw,
		Logger:     logger,
		ScopeTiers: scopeTiersFromEnv(),
	})
	if err != nil {
		return fatalf("%v", err)
	}
	h := httpapi.New(svc, auth.NewHeaderAuthenticator("query-api", "control-api", "ops"), logger)

	if err := checkBindAddress(*addr, acknowledged); err != nil {
		return fatalf("%v", err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           withProbes(h.Handler(), kw, *storeKind, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	ingress := "internal-only"
	if internalOnly {
		ingress = "internal-only (declared by " + EnvInternalOnly + ")"
	}
	slog.Info("content-vault listening",
		"addr", *addr, "key_backend", string(kw.Kind()), "ingress", ingress, "role", *role,
		"store", *storeKind, "non_loopback_acknowledged", acknowledged,
		"blob_ciphertext_endpoint_configured", *blobEndpoint != "",
		"appinsights_configured", appInsights != "")
	if *blobEndpoint != "" {
		// Stated rather than implied: the deployment passes this endpoint and this build performs no
		// blob I/O. It is validated at boot so a typo fails now, and reported so nobody assumes it is
		// in use.
		slog.Warn("SAC_BLOB_CIPHERTEXT_ENDPOINT is set but this build performs no blob I/O; the endpoint is validated and recorded, not used")
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fatalf("serving: %v", err)
	}
	return 0
}

// checkBindAddress refuses a non-loopback bind without an explicit acknowledgement.
func checkBindAddress(addr string, acknowledged bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q is not host:port: %w", addr, err)
	}
	if acknowledged {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--addr %q is not a loopback address and content-vault has internal ingress only (D7): "+
		"binding it would expose the only component that can unwrap content keys. Pass --allow-non-loopback to acknowledge", addr)
}

// scopeTiersFromEnv reads the signed bundle's per-scope search tiers. The bundle itself is
// control-api's artefact; this is the shape this service is handed, as `scope=tier` pairs.
func scopeTiersFromEnv() map[string]store.SearchTier {
	out := map[string]store.SearchTier{}
	for _, pair := range strings.Split(os.Getenv("CONTENT_VAULT_SCOPE_TIERS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		tier := store.SearchTier(parts[1])
		if tier.Valid() {
			out[parts[0]] = tier
		}
	}
	return out
}

// runSchemaSQL prints the SQL this service issues as a psql script, so the live-schema harness
// executes the *same text* the service would send rather than a copy of it. That is the same
// discipline ingest-api uses, and it is what makes the harness evidence rather than documentation.
func runSchemaSQL(args []string) int {
	fs := flag.NewFlagSet("schema-sql", flag.ContinueOnError)
	out := fs.String("out", "", "write to a file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var b strings.Builder
	b.WriteString("-- generated by content-vault schema-sql; every statement the service issues.\n")
	b.WriteString("\\set ON_ERROR_STOP on\nBEGIN;\n")
	for _, st := range store.Statements {
		fmt.Fprintf(&b, "\n-- %s: %s\n", st.Name, st.Purpose)
		if !st.Verified {
			fmt.Fprintf(&b, "-- NOT EXECUTED: %s does not exist in db/schema.sql; see SQLRetrievalGrantDDL\n", st.Name)
			continue
		}
		fmt.Fprintf(&b, "PREPARE %s AS %s;\nDEALLOCATE %s;\n", st.Name, st.SQL, st.Name)
	}
	b.WriteString("\nROLLBACK;\n")
	b.WriteString("\n-- The retrieval-grant table this service needs and the schema does not yet have:\n")
	for _, line := range strings.Split(store.SQLRetrievalGrantDDL, "\n") {
		b.WriteString("-- " + line + "\n")
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
			return fatalf("writing %s: %v", *out, err)
		}
		return 0
	}
	fmt.Print(b.String())
	return 0
}

var _ = context.Background
