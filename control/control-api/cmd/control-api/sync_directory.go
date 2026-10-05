package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
)

// sync-directory is the scheduled directory synchronisation (docs/03 §3.3, docs/04 §3.3; the Q2
// decision record). It lives in the control-api binary and image because the architecture assigns
// directory work to the control plane, and it shares the role's existing grant on ops.user_dim.
//
// The command is an operator or scheduler entry point, not a device route: the HTTP service knows
// nothing about it. It runs once and exits, or on --interval until stopped. Entra ID is the first
// provider; the file provider is the lab's offline source and the path for a customer whose
// directory is not Entra.
//
//	control-api sync-directory --store sql --dsn "$SAC_PG_DSN" \
//	  --provider entra --entra-tenant "$TENANT" --entra-client-id "$ID" --entra-client-secret "$SECRET"
//
//	control-api sync-directory --store sql --dsn "$SAC_PG_DSN" \
//	  --provider file --file backlog/06-directory-sync/sample-directory.json --tenant "$SAMPLE"
//
// The directory key (SAC_DIRECTORY_KEY, base64, 32 bytes) seals directory_object_id_enc. It is a
// deployment secret with no default: without it the command refuses, because writing an unsealed
// directory identifier would be worse than not writing one.
func runSyncDirectory(args []string) error {
	fs := flag.NewFlagSet("sync-directory", flag.ContinueOnError)
	var (
		storeKind, dsn, driver, pgHost, pgPort, pgDatabase, role string
		provider, filePath                                       string
		tenants                                                  stringList
		entraTenant, entraClientID, entraClientSecret            string
		userRefAttribute, populationAttribute                    string
		loginBase, graphBase                                     string
		keyBase64, keyFile                                       string
		interval                                                 time.Duration
	)
	fs.StringVar(&storeKind, "store", "sql", "persistence mode; only sql persists a directory")
	fs.StringVar(&dsn, "dsn", "", "database DSN; the caller must register a driver")
	fs.StringVar(&driver, "driver", "", "database/sql driver name; must be registered in this binary")
	fs.StringVar(&pgHost, "pg-host", "", "database host (used to build the DSN, not a password)")
	fs.StringVar(&pgPort, "pg-port", "5432", "database port")
	fs.StringVar(&pgDatabase, "pg-database", "shadow", "database name")
	fs.StringVar(&role, "role", "control-api", "the identity this process runs as")
	fs.StringVar(&provider, "provider", "file", "directory provider: file | entra")
	fs.StringVar(&filePath, "file", "", "JSON directory export (with --provider file)")
	fs.Var(&tenants, "tenant", "sync only this tenant; repeatable. Default: every tenant the session can see")
	fs.StringVar(&entraTenant, "entra-tenant", "", "Entra ID tenant id or domain (with --provider entra)")
	fs.StringVar(&entraClientID, "entra-client-id", "", "registered application (client) id")
	fs.StringVar(&entraClientSecret, "entra-client-secret", os.Getenv("SAC_ENTRA_CLIENT_SECRET"), "registered application client secret (env SAC_ENTRA_CLIENT_SECRET)")
	fs.StringVar(&userRefAttribute, "entra-user-ref-attribute", "onPremisesSamAccountName", "directory attribute whose value is the device's --user-ref")
	fs.StringVar(&populationAttribute, "entra-population-attribute", "", "directory attribute copied to ops.user_dim.population")
	fs.StringVar(&loginBase, "entra-login-base", "", "override the Microsoft login base URL (tests only)")
	fs.StringVar(&graphBase, "entra-graph-base", "", "override the Graph base URL (tests only)")
	fs.StringVar(&keyBase64, "directory-key", os.Getenv("SAC_DIRECTORY_KEY"), "base64 32-byte key sealing directory_object_id_enc (env SAC_DIRECTORY_KEY)")
	fs.StringVar(&keyFile, "directory-key-file", "", "file holding the base64 directory key")
	fs.DurationVar(&interval, "interval", 0, "run every interval instead of once (0 = run once and exit)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if storeKind != "sql" {
		return fmt.Errorf("sync-directory needs -store sql: an in-memory directory writes nothing, and a directory sync that silently persists nothing is worse than one that refuses")
	}
	resolvedDSN := dsn
	if resolvedDSN == "" {
		resolvedDSN = postgresDSN(pgHost, pgPort, pgDatabase, role)
	}
	if resolvedDSN == "" {
		return fmt.Errorf("no database: pass -dsn, or -pg-host (with the other -pg-* flags)")
	}
	resolvedDriver := driver
	if resolvedDriver == "" {
		resolvedDriver = defaultDriverName
	}
	if resolvedDriver == "" {
		return fmt.Errorf("no PostgreSQL driver is registered in this binary; build with -tags sac_sql_driver")
	}

	key, err := resolveDirectoryKey(keyBase64, keyFile)
	if err != nil {
		return err
	}
	cipher, err := directory.NewCipher(key)
	if err != nil {
		return err
	}

	source, err := buildDirectorySource(provider, filePath, graphSourceConfig{
		TenantID:            entraTenant,
		ClientID:            entraClientID,
		ClientSecret:        entraClientSecret,
		UserRefAttribute:    userRefAttribute,
		PopulationAttribute: populationAttribute,
		LoginBaseURL:        loginBase,
		GraphBaseURL:        graphBase,
	})
	if err != nil {
		return err
	}

	db, err := sql.Open(resolvedDriver, resolvedDSN)
	if err != nil {
		return fmt.Errorf("open database with driver %q: %w", resolvedDriver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		return fmt.Errorf("database is not reachable: %w", err)
	}

	store := directory.NewSQL(db)
	syncer := &directory.Syncer{Source: source, Store: store, Cipher: cipher}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pass := func(ctx context.Context) error {
		ids := tenants
		if len(ids) == 0 {
			ids, err = listTenants(ctx, db)
			if err != nil {
				return err
			}
		}
		for _, tenantID := range ids {
			res, err := syncer.Sync(ctx, tenantID)
			if errors.Is(err, directory.ErrUnknownTenant) {
				logger.Warn("directory sync skipped an unknown tenant", "tenant", tenantID)
				continue
			}
			if err != nil {
				return fmt.Errorf("tenant %s: %w", tenantID, err)
			}
			logger.Info("directory sync complete", "tenant", res.TenantID, "provider", source.Name(),
				"read", res.Read, "synced", res.Synced, "skipped", res.Skipped,
				"unmapped", res.Unmapped, "retired", res.Retired)
		}
		return nil
	}

	logger.Info("directory sync starting", "provider", source.Name(), "interval", interval.String())
	if err := pass(ctx); err != nil {
		return err
	}
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := pass(ctx); err != nil {
				logger.Error("directory sync failed", "error", err.Error())
			}
		}
	}
}

// resolveDirectoryKey accepts the base64 key directly or from a file. Both forms exist because a
// deployment injects a secret as an environment variable and a person on a laptop keeps a file.
func resolveDirectoryKey(encoded, file string) ([]byte, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("directory key file: %w", err)
		}
		encoded = strings.TrimSpace(string(raw))
	}
	if encoded == "" {
		return nil, fmt.Errorf("no directory key: set SAC_DIRECTORY_KEY or --directory-key to a base64 32-byte value (it seals ops.user_dim.directory_object_id_enc)")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("directory key is not valid base64: %w", err)
	}
	return key, nil
}

// graphSourceConfig is the CLI's slice of the Entra provider's settings.
type graphSourceConfig struct {
	TenantID, ClientID, ClientSecret      string
	UserRefAttribute, PopulationAttribute string
	LoginBaseURL, GraphBaseURL            string
}

// buildDirectorySource selects the provider. The two providers are chosen at the command line rather
// than guessed, so a missing secret is a clear error and never a silent fallback to a file.
func buildDirectorySource(provider, filePath string, g graphSourceConfig) (directory.Source, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "file":
		if filePath == "" {
			return nil, fmt.Errorf("--provider file needs --file")
		}
		return directory.NewFileSource(filePath), nil
	case "entra":
		return &directory.GraphSource{
			TenantID:            g.TenantID,
			ClientID:            g.ClientID,
			ClientSecret:        g.ClientSecret,
			UserRefAttribute:    g.UserRefAttribute,
			PopulationAttribute: g.PopulationAttribute,
			LoginBaseURL:        g.LoginBaseURL,
			GraphBaseURL:        g.GraphBaseURL,
		}, nil
	default:
		return nil, fmt.Errorf("unknown --provider %q (want file or entra)", provider)
	}
}

// listTenants enumerates the tenants to sync when none is named. It runs before any tenant is set
// on the session, so a role constrained by forced row-level security sees nothing here and must be
// given --tenant explicitly; the lab's superuser does see the rows. The same caveat is recorded for
// the aggregator (aggregation/aggregator/README.md).
func listTenants(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id::text FROM ops.tenant WHERE status <> 'closed' ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no tenant to sync: none is named and the session sees no tenants (pass --tenant)")
	}
	return ids, nil
}

// stringList collects a repeatable flag. It is a copy of the aggregator's helper rather than a
// shared package: the two binaries are separate modules and a flag helper is not worth a dependency.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("empty value")
	}
	*s = append(*s, v)
	return nil
}
