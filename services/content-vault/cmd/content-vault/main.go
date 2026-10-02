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
	addr := fs.String("addr", "127.0.0.1:8090", "listen address")
	backend := fs.String("key-backend", "local", "local | kms")
	keyFile := fs.String("key-file", "", "local key file (development); empty means in-memory")
	allowNonLoopback := fs.Bool("allow-non-loopback", false, "acknowledge that this binds a non-loopback address")
	allowKMS := fs.Bool("allow-unimplemented-kms", false, "start with a cloud KMS backend this build has not implemented")
	storeKind := fs.String("store", "memory", "memory | sql")
	dsn := fs.String("dsn", "", "database/sql DSN (the caller must register a driver)")
	if err := fs.Parse(args); err != nil {
		return 2
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
				"Starting with it would mean a deployment reporting a KMS it does not have. Pass --allow-unimplemented-kms only to demonstrate the refusal.")
		}
		kw = keys.NewKMS(os.Getenv("CONTENT_VAULT_KMS_ENDPOINT"), os.Getenv("CONTENT_VAULT_KMS_MODE"))
	default:
		return fatalf("unknown --key-backend %q", *backend)
	}

	if *storeKind != "memory" {
		// The SQL store needs a registered database/sql driver, and no PostgreSQL driver is
		// fetchable offline (ADR 0016). The binary says so rather than half-wiring it.
		return fatalf("--store sql requires a database/sql driver registered by the embedding build; "+
			"this offline build has none (ADR 0016). The SQL path and its statements are exercised by internal/store's live-schema harness. --dsn %q", *dsn)
	}

	svc, err := vault.New(vault.Options{
		Store:      store.NewMemory(),
		Keys:       kw,
		Logger:     slog.Default(),
		ScopeTiers: scopeTiersFromEnv(),
	})
	if err != nil {
		return fatalf("%v", err)
	}
	h := httpapi.New(svc, auth.NewHeaderAuthenticator("query-api", "control-api", "ops"), slog.Default())

	if err := checkBindAddress(*addr, *allowNonLoopback); err != nil {
		return fatalf("%v", err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           h.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	slog.Info("content-vault listening",
		"addr", *addr, "key_backend", string(kw.Kind()), "ingress", "internal-only")
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
