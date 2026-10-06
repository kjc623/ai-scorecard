// Command jobs runs the scheduled database jobs. Each subcommand makes one pass over every tenant
// and exits; the scheduler decides when the next pass starts.
//
//	jobs aggregate   recompute the dashboard aggregates, findings and coverage (every 5 minutes)
//	jobs expire      delete rows past their retention and write a receipt (daily)
//
// Each tenant's work is one transaction scoped to that tenant. A tenant that fails is rolled back,
// logged and counted, the remaining tenants still run, and the process exits non-zero.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/platform/postgres"

	"github.com/shadow-ai-capture/jobs/internal/erase"
	"github.com/shadow-ai-capture/jobs/internal/expire"
	"github.com/shadow-ai-capture/jobs/internal/rollup"
	"github.com/shadow-ai-capture/jobs/internal/tenant"
)

// maxConns bounds the pool. Tenants are processed one at a time, so the job holds a single
// session at once and never competes with the services for connections.
const maxConns = 4

const usage = "usage: jobs aggregate|expire|erase"

// job is one subcommand: the work done for one tenant inside its transaction, and the log key its
// per-table row counts are reported under.
type job struct {
	name      string
	countsKey string
	run       func(ctx context.Context, tx *sql.Tx, tenant string) (map[string]int64, error)
}

var jobs = map[string]job{
	"aggregate": {
		name:      "aggregate",
		countsKey: "written",
		run: func(ctx context.Context, tx *sql.Tx, id string) (map[string]int64, error) {
			return rollup.Tenant(ctx, tx, id, time.Now())
		},
	},
	"expire": {
		name:      "expire",
		countsKey: "removed",
		run: func(ctx context.Context, tx *sql.Tx, id string) (map[string]int64, error) {
			return expire.Tenant(ctx, tx, id)
		},
	},
	"erase": {
		name:      "erase",
		countsKey: "removed",
		run: func(ctx context.Context, tx *sql.Tx, id string) (map[string]int64, error) {
			return erase.Tenant(ctx, tx, id)
		},
	},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	j, ok := jobs[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("job", j.name)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, j, logger); err != nil {
		logger.Error("pass failed", "error", err.Error())
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, j job, logger *slog.Logger) error {
	cfg, err := postgres.ConfigFromEnv()
	if err != nil {
		return err
	}
	cfg.MaxOpenConns = maxConns
	db, err := postgres.Open(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	started := time.Now()
	ids, err := tenant.IDs(ctx, db)
	if err != nil {
		return err
	}
	logger.Info("pass started", "database", cfg.String(), "tenants", len(ids))

	succeeded, failed := 0, 0
	for _, id := range ids {
		tenantStarted := time.Now()
		var counts map[string]int64
		err := tenant.Run(ctx, db, id, func(tx *sql.Tx) error {
			var err error
			counts, err = j.run(ctx, tx, id)
			return err
		})
		if err != nil {
			failed++
			logger.Error("tenant failed", "tenant", id, "error", err.Error())
			if ctx.Err() != nil {
				break
			}
			continue
		}
		succeeded++
		logger.Info("tenant complete", "tenant", id, j.countsKey, counts,
			"duration_ms", time.Since(tenantStarted).Milliseconds())
	}

	logger.Info("pass complete", "tenants", len(ids), "succeeded", succeeded, "failed", failed,
		"duration_ms", time.Since(started).Milliseconds())
	if ctx.Err() != nil {
		return fmt.Errorf("pass interrupted: %w", context.Cause(ctx))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d tenants failed", failed, len(ids))
	}
	return nil
}
