package main

import (
	"context"
	"database/sql"
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

// sync-directory loads a directory export into ops.user_dim (docs/03 §3.3, docs/04 §3.3; the Q2
// decision record). It is the lab's path: it gives the sample tenant's simulated people a department
// without an identity provider. A customer's people arrive through SCIM, pushed by their identity
// provider to /scim/v2 (internal/scim); there is no pull from Microsoft Graph, because the product's
// Entra application asks for no directory-read permission.
//
// The command is an operator entry point, not a device route: the HTTP service knows nothing about
// it. It runs once and exits, or on --interval until stopped.
//
//	control-api sync-directory --store sql --dsn "$SAC_PG_DSN" \
//	  --file backlog/06-directory-sync/sample-directory.json --tenant "$SAMPLE"
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
	fs.StringVar(&provider, "provider", "file", "directory provider: file (the only one; identity providers push through SCIM)")
	fs.StringVar(&filePath, "file", "", "JSON directory export")
	fs.Var(&tenants, "tenant", "sync only this tenant; repeatable. Default: every tenant the session can see")
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

	source, err := buildDirectorySource(provider, filePath)
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
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("no directory key: set SAC_DIRECTORY_KEY or --directory-key to a base64 32-byte value (it seals ops.user_dim.directory_object_id_enc)")
	}
	return directory.DecodeKey(encoded)
}

// buildDirectorySource selects the provider. The file is the only one left: --provider stays so the
// lab's existing command lines keep working, and the old entra value is refused with the reason
// rather than ignored.
func buildDirectorySource(provider, filePath string) (directory.Source, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "file", "":
		if filePath == "" {
			return nil, fmt.Errorf("sync-directory needs --file")
		}
		return directory.NewFileSource(filePath), nil
	case "entra":
		return nil, fmt.Errorf("--provider entra was removed: an Entra tenant provisions people through SCIM (/scim/v2), and the product's Entra application holds no directory-read permission")
	default:
		return nil, fmt.Errorf("unknown --provider %q (want file)", provider)
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
