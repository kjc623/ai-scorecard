package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Runner replaces the mart buckets for each tenant and records the freshness watermark for every
// aggregate it touched. One tenant's work is one transaction, so a failure leaves that tenant's
// previous bucket contents in place and advances no watermark (docs/03 §5.4).
type Runner struct {
	DB           *sql.DB
	DayLookback  int
	HourLookback int
	// CoverageDayLookback is the trailing window of coverage days recomputed each run. Zero uses 7.
	CoverageDayLookback int
	// Now is injectable so a test can pin the window. It defaults to time.Now.
	Now func() time.Time
	Log *slog.Logger
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Tenants lists the tenants to roll up. It runs before any tenant is set on the session, so under
// forced row-level security a non-superuser role sees no rows here; a deployment therefore runs
// this job with a role allowed to enumerate tenants, or supplies the list explicitly. The lab runs
// it as the same role every other lab service uses, which can see every tenant.
func (r *Runner) Tenants(ctx context.Context) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT tenant_id::text FROM ops.tenant WHERE status <> 'closed' ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("rollup: list tenants: %w", err)
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("rollup: scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

// TenantReport is what one tenant's run produced, for logging and for a test to assert on.
type TenantReport struct {
	Tenant string
	// Rows maps "aggregate bucket_size" to the number of buckets the statement wrote.
	Rows map[string]int64
}

// RunTenant rolls one tenant up in a single transaction and records its watermarks. All
// aggregates for both bucket sizes are recomputed on every run: the trailing window is what makes
// a late-arriving spool flush correct, and replacing the bucket rather than incrementing it is
// what makes a re-run idempotent.
func (r *Runner) RunTenant(ctx context.Context, tenant string) (TenantReport, error) {
	report := TenantReport{Tenant: tenant, Rows: map[string]int64{}}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("rollup: begin tenant %s: %w", tenant, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, SetTenantSQL, tenant); err != nil {
		return report, fmt.Errorf("rollup: set tenant %s: %w", tenant, err)
	}

	now := r.now()
	for _, bucketSize := range BucketSizes {
		lookback := r.HourLookback
		if bucketSize == BucketDay {
			lookback = r.DayLookback
		}
		window, err := WindowFor(bucketSize, now, lookback)
		if err != nil {
			return report, err
		}
		lastComplete, err := LastCompleteBucket(bucketSize, now)
		if err != nil {
			return report, err
		}
		aggregates, err := Aggregates(bucketSize)
		if err != nil {
			return report, err
		}
		for _, aggregate := range aggregates {
			res, err := tx.ExecContext(ctx, aggregate.SQL, tenant, window.From, window.To)
			if err != nil {
				return report, fmt.Errorf("rollup: %s (%s): %w", aggregate.Name, bucketSize, err)
			}
			written, err := res.RowsAffected()
			if err != nil {
				return report, fmt.Errorf("rollup: %s (%s): rows affected: %w", aggregate.Name, bucketSize, err)
			}
			if _, err := tx.ExecContext(ctx, WatermarkSQL,
				tenant, aggregate.Name, bucketSize, lastComplete, written); err != nil {
				return report, fmt.Errorf("rollup: watermark %s (%s): %w", aggregate.Name, bucketSize, err)
			}
			report.Rows[aggregate.Name+" "+bucketSize] = written
		}
	}

	// Findings are derived from the same ingest rows, in the same transaction, but they are not
	// bucket aggregates: one pass over the trailing day window, insert-only. A rule the classifier
	// did not publish raises nothing, and re-running the window inserts nothing new.
	dayWindow, err := WindowFor(BucketDay, now, r.DayLookback)
	if err != nil {
		return report, err
	}
	findingsRes, err := tx.ExecContext(ctx, FindingsSQL, tenant, dayWindow.From, dayWindow.To)
	if err != nil {
		return report, fmt.Errorf("rollup: %s: %w", FindingName, err)
	}
	findingsWritten, err := findingsRes.RowsAffected()
	if err != nil {
		return report, fmt.Errorf("rollup: %s: rows affected: %w", FindingName, err)
	}
	report.Rows[FindingName] = findingsWritten

	// Coverage is a daily fact rather than a bucket aggregate, but it is written by the same run for
	// the same reason: a fact nothing recomputes is a fact that quietly stops being true (R11).
	coverageRows, err := r.runCoverage(ctx, tx, tenant, now)
	if err != nil {
		return report, err
	}
	report.Rows[CoverageSnapshotName] = coverageRows

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("rollup: commit tenant %s: %w", tenant, err)
	}
	return report, nil
}

// RunReport is the outcome of one pass over every tenant.
type RunReport struct {
	Tenants []TenantReport
	// Failed names the tenants whose transaction rolled back, with the reason.
	Failed map[string]error
}

// Failures returns one error naming every tenant that failed, or nil when none did.
func (r RunReport) Failures() error {
	if len(r.Failed) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.Failed))
	for name := range r.Failed {
		names = append(names, name)
	}
	sort.Strings(names)
	msgs := make([]string, 0, len(names))
	for _, name := range names {
		msgs = append(msgs, fmt.Sprintf("%s: %v", name, r.Failed[name]))
	}
	return fmt.Errorf("rollup: %d tenant(s) failed: %s", len(names), joinErrors(msgs))
}

// RunOnce rolls up every tenant the session can enumerate.
func (r *Runner) RunOnce(ctx context.Context) (RunReport, error) {
	tenants, err := r.Tenants(ctx)
	if err != nil {
		return RunReport{}, err
	}
	return r.RunTenants(ctx, tenants), nil
}

// RunTenants rolls up an explicit list. A tenant that fails does not stop the others: each commits
// separately, and the failure is reported rather than hidden. Passing the list explicitly is also
// how a deployment whose job role cannot enumerate tenants under row-level security supplies them.
func (r *Runner) RunTenants(ctx context.Context, tenants []string) RunReport {
	report := RunReport{Failed: map[string]error{}}
	for _, tenant := range tenants {
		tr, err := r.RunTenant(ctx, tenant)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				report.Failed[tenant] = err
				return report
			}
			r.log().Error("aggregate run failed for tenant; previous buckets left in place",
				"tenant", tenant, "error", err)
			report.Failed[tenant] = err
			continue
		}
		report.Tenants = append(report.Tenants, tr)
		r.log().Info("aggregate run complete", "tenant", tenant, "buckets_written", totalRows(tr))
	}
	return report
}

func totalRows(tr TenantReport) int64 {
	var total int64
	for _, n := range tr.Rows {
		total += n
	}
	return total
}

func joinErrors(msgs []string) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "; "
		}
		out += m
	}
	return out
}
