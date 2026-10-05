// Command aggregator rolls ingest.submission and ingest.observation up into the mart aggregates
// and writes the ops.aggregate_watermark rows the read path uses to say how current each aggregate
// is. It is the job docs/03-data-platform.md §5 and docs/04-dashboard-and-query.md §4 describe.
//
// It runs as its own process on a short interval: one transaction per tenant, both the hour and the
// day bucket, each aggregate replaced rather than incremented. It carries no device credential and
// is not reachable from a browser or a device; only its health endpoints listen.
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

	"github.com/shadow-ai-capture/aggregator/internal/rollup"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aggregator:", err)
		os.Exit(1)
	}
}

// stringList collects a repeatable --tenant flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		*s = append(*s, v)
	}
	return nil
}

type options struct {
	storeKind        string
	dsn              string
	driver           string
	interval         time.Duration
	dayLookback      int
	hourLookback     int
	coverageLookback int
	tenants          stringList
	once             bool
	addr             string
}

func run() error {
	var o options
	flag.StringVar(&o.storeKind, "store", envOr("SAC_STORE", "sql"), "storage backend; only sql is served (env SAC_STORE)")
	flag.StringVar(&o.dsn, "dsn", envOr("SAC_PG_DSN", ""), "PostgreSQL DSN (env SAC_PG_DSN)")
	flag.StringVar(&o.driver, "driver", "", "database/sql driver name; the tagged build knows its own")
	flag.DurationVar(&o.interval, "interval", envDuration("SAC_INTERVAL", 5*time.Minute), "how often every tenant is recomputed (env SAC_INTERVAL)")
	flag.IntVar(&o.dayLookback, "day-lookback", envInt("SAC_DAY_LOOKBACK", 7), "trailing day buckets recomputed each run (env SAC_DAY_LOOKBACK)")
	flag.IntVar(&o.hourLookback, "hour-lookback", envInt("SAC_HOUR_LOOKBACK", 48), "trailing hour buckets recomputed each run (env SAC_HOUR_LOOKBACK)")
	flag.IntVar(&o.coverageLookback, "coverage-lookback", envInt("SAC_COVERAGE_LOOKBACK", 7), "trailing coverage days recomputed each run (env SAC_COVERAGE_LOOKBACK)")
	flag.Var(&o.tenants, "tenant", "roll up only this tenant; repeatable. Default: every tenant the session can see")
	flag.BoolVar(&o.once, "once", false, "run one pass and exit (for a lab check or a scheduled job)")
	flag.StringVar(&o.addr, "addr", envOr("SAC_HTTP_ADDR", "127.0.0.1:8080"), "health listen address (env SAC_HTTP_ADDR)")
	flag.Parse()

	if o.storeKind != "sql" {
		return fmt.Errorf("unknown -store %q: the aggregator only writes to PostgreSQL", o.storeKind)
	}
	if o.interval <= 0 {
		return fmt.Errorf("-interval must be positive, got %s", o.interval)
	}
	if o.dayLookback < 1 || o.hourLookback < 1 {
		return fmt.Errorf("lookbacks must be at least 1 bucket (day=%d hour=%d)", o.dayLookback, o.hourLookback)
	}
	if o.coverageLookback < 1 {
		return fmt.Errorf("coverage-lookback must be at least 1 day, got %d", o.coverageLookback)
	}
	if o.dsn == "" {
		return errors.New("a database DSN is required: pass -dsn or set SAC_PG_DSN")
	}

	driver := o.driver
	if driver == "" {
		driver = defaultDriverName
	}
	if driver == "" {
		return refusal(o.dsn)
	}

	db, err := sql.Open(driver, o.dsn)
	if err != nil {
		return fmt.Errorf("open database with driver %q: %w", driver, err)
	}
	defer db.Close()
	// The aggregation is set-based and short-lived; a small pool is plenty and keeps a scheduled job
	// from holding sessions a read path wants.
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	runner := &rollup.Runner{
		DB:                  db,
		DayLookback:         o.dayLookback,
		HourLookback:        o.hourLookback,
		CoverageDayLookback: o.coverageLookback,
		Log:                 logger,
	}

	if o.once {
		report, err := runPass(context.Background(), runner, o.tenants, logger)
		if err != nil {
			return err
		}
		return report.Failures()
	}

	// The loop: one pass immediately, then every interval. Each pass is independent; a failed pass
	// leaves the previous buckets and watermarks in place and is retried on the next tick.
	st := &status{}
	healthSrv := &http.Server{
		Addr:              o.addr,
		Handler:           healthHandler(st, 3*o.interval),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("aggregator health listening", "addr", o.addr, "interval", o.interval.String())
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server stopped", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pass := func() {
		started := time.Now()
		report, err := runPass(ctx, runner, o.tenants, logger)
		if err != nil {
			logger.Error("aggregate pass failed", "error", err)
			st.recordFailure(err.Error())
			return
		}
		if err := report.Failures(); err != nil {
			st.recordFailure(err.Error())
			return
		}
		var buckets int64
		for _, tr := range report.Tenants {
			for _, n := range tr.Rows {
				buckets += n
			}
		}
		st.recordSuccess(len(report.Tenants), buckets, time.Now())
		logger.Info("aggregate pass complete", "tenants", len(report.Tenants),
			"buckets", buckets, "took", time.Since(started).String())
	}

	pass()
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return healthSrv.Shutdown(shutdownCtx)
		case <-ticker.C:
			pass()
		}
	}
}

// runPass rolls up either the explicitly named tenants or every tenant the session can see.
func runPass(ctx context.Context, runner *rollup.Runner, tenants stringList, logger *slog.Logger) (rollup.RunReport, error) {
	if len(tenants) > 0 {
		logger.Info("aggregate pass starting", "tenants", len(tenants), "explicit", true)
		return runner.RunTenants(ctx, tenants), nil
	}
	report, err := runner.RunOnce(ctx)
	if err != nil {
		return report, err
	}
	return report, nil
}

// refusal is the untagged-build message. The default build carries no PostgreSQL driver because
// the dependency is compiled only under the sac_sql_driver tag; the tagged build needs no -driver.
func refusal(dsn string) error {
	return fmt.Errorf(`-store sql cannot start in this build: no PostgreSQL driver is compiled in.

  driver   github.com/jackc/pgx/v5/stdlib (registered as "pgx"), compiled only under the
           sac_sql_driver tag so the default build stays standard-library only
  dsn      %s (never logged with a credential in it)

  the tagged build, which uses the real driver and needs no -driver flag:
      go build -tags sac_sql_driver -o aggregator-sql ./cmd/aggregator
      ./aggregator-sql -dsn "$SAC_PG_DSN"`,
		redactDSN(dsn))
}

// redactDSN removes any password so a DSN can be printed in an error or a log line.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return dsn
	}
	return dsn[:scheme+3] + "***" + dsn[at:]
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
