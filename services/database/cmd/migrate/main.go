// Command migrate brings the product database to the current schema and makes each component's
// login a member of its runtime role. The deployment runs it as a job before the services start.
//
// It connects with the SAC_PG_* variables (see services/platform/postgres); in Azure SAC_PG_USER names the
// migration identity, a Microsoft Entra administrator of the server. SAC_DB_LOGINS lists the
// component logins as a JSON array of {"login", "role", "objectId"}. It logs JSON to stdout and
// exits non-zero on failure.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/shadow-ai-capture/database"
	"github.com/shadow-ai-capture/platform/postgres"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		slog.Error("migrate failed", "err", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := postgres.ConfigFromEnv()
	if err != nil {
		return err
	}
	logins, err := database.ParseLogins(os.Getenv(database.EnvLogins))
	if err != nil {
		return err
	}
	cfg.MaxOpenConns = 1
	db, err := postgres.Open(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	slog.Info("migrating", "database", cfg.String(), "logins", len(logins))
	return database.Migrate(ctx, db, logins)
}
