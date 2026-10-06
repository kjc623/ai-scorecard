package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Trailing windows recomputed on every pass. They are counted from received_at, the server's
// clock, so a device that was offline flushes into current buckets and the windows never need to
// grow with device downtime. What they bound is how long a missed pass, an erasure or an expiry
// takes to be reflected: up to 48 hours for hour buckets and 7 days for day buckets and coverage.
const (
	HourLookback        = 48
	DayLookback         = 7
	CoverageDayLookback = 7
)

// Execer is satisfied by *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Tenant recomputes one tenant's aggregates, watermarks, findings and coverage. tx must already
// be scoped to the tenant; the caller commits, so a failure leaves the previous buckets and
// watermarks in place. It returns the rows each statement wrote, keyed "<table> <bucket size>"
// for the bucket aggregates and by table name otherwise.
func Tenant(ctx context.Context, tx Execer, tenant string, now time.Time) (map[string]int64, error) {
	written := map[string]int64{}
	for _, bucketSize := range BucketSizes {
		lookback := HourLookback
		if bucketSize == BucketDay {
			lookback = DayLookback
		}
		window, err := WindowFor(bucketSize, now, lookback)
		if err != nil {
			return nil, err
		}
		lastComplete, err := LastCompleteBucket(bucketSize, now)
		if err != nil {
			return nil, err
		}
		aggregates, err := Aggregates(bucketSize)
		if err != nil {
			return nil, err
		}
		for _, aggregate := range aggregates {
			n, err := exec(ctx, tx, aggregate.SQL, tenant, window.From, window.To)
			if err != nil {
				return nil, fmt.Errorf("rollup: %s (%s): %w", aggregate.Name, bucketSize, err)
			}
			if _, err := tx.ExecContext(ctx, WatermarkSQL, tenant, aggregate.Name, bucketSize, lastComplete, n); err != nil {
				return nil, fmt.Errorf("rollup: watermark %s (%s): %w", aggregate.Name, bucketSize, err)
			}
			written[aggregate.Name+" "+bucketSize] = n
		}
	}

	// Findings come from the same submissions over the trailing day window, insert-only.
	dayWindow, err := WindowFor(BucketDay, now, DayLookback)
	if err != nil {
		return nil, err
	}
	n, err := exec(ctx, tx, FindingsSQL, tenant, dayWindow.From, dayWindow.To)
	if err != nil {
		return nil, fmt.Errorf("rollup: %s: %w", FindingName, err)
	}
	written[FindingName] = n

	coverage, err := CoverageWindowFor(now, CoverageDayLookback)
	if err != nil {
		return nil, err
	}
	n, err = exec(ctx, tx, CoverageSnapshotSQL, tenant, coverage.From, coverage.To)
	if err != nil {
		return nil, fmt.Errorf("rollup: %s: %w", CoverageSnapshotName, err)
	}
	written[CoverageSnapshotName] = n
	return written, nil
}

// exec runs one statement and returns the number of rows it affected.
func exec(ctx context.Context, tx Execer, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
