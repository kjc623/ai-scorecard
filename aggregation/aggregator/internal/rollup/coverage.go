package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Coverage is the fleet coverage fact of brief R11 and docs/04 §11.3: what each device's collection
// path was expected to report, and what it actually reported, one row per device per collector per
// day. It is a different kind of read from the usage aggregates — it rolls up ops.collector_state,
// not the event tables (docs/03 §?; master doc D5) — and it is written by the same scheduled job
// because a coverage fact is as stale as any other if nothing recomputes it.
//
// Two rules this statement holds:
//
//   - The expected set comes from the calendar and ref.collector, not from surviving reports. A
//     device that reported nothing must still produce rows, or its silence is invisible — which is
//     exactly the failure C24 exists to prevent.
//   - `observed` is monotonic within a day. Once a collector is seen reporting on a day, a later run
//     that happens to read a `last_report_at` from a newer day must not overwrite the day's answer
//     back to "not observed". `ops.collector_state` is current state, not history, so without this a
//     past day's coverage would be erased the next time the device reports.
const CoverageSnapshotSQL = `
WITH days AS (
  SELECT generate_series($2::date, $3::date - 1, interval '1 day')::date AS snapshot_day
),
collectors AS (
  SELECT collector_code FROM ref.collector
),
grid AS (
  SELECT d.tenant_id, days.snapshot_day, d.device_id, c.collector_code
    FROM ops.device d
    CROSS JOIN days
    CROSS JOIN collectors c
   WHERE d.tenant_id = $1::uuid AND d.revoked_at IS NULL
),
observed AS (
  SELECT cs.tenant_id, cs.device_id, cs.collector,
         (cs.last_report_at AT TIME ZONE 'UTC')::date AS snapshot_day,
         bool_or(cs.state IN ('healthy','degraded')) AS reported,
         bool_or(cs.state = 'tampered')             AS tampered
    FROM ops.collector_state cs
   WHERE cs.tenant_id = $1::uuid
     AND cs.last_report_at >= $2::timestamptz AND cs.last_report_at < $3::timestamptz
   GROUP BY 1, 2, 3, 4
)
INSERT INTO ops.coverage_snapshot
  (tenant_id, snapshot_day, device_id, collector, expected, observed, gap_reason)
SELECT g.tenant_id, g.snapshot_day, g.device_id, g.collector_code,
       true,
       coalesce(o.reported, false),
       CASE
         WHEN coalesce(o.reported, false) THEN NULL
         WHEN coalesce(o.tampered, false) THEN 'tampered'
         ELSE 'unknown'
       END
  FROM grid g
  LEFT JOIN observed o
    ON o.tenant_id = g.tenant_id
   AND o.device_id = g.device_id
   AND o.collector = g.collector_code
   AND o.snapshot_day = g.snapshot_day
ON CONFLICT (tenant_id, snapshot_day, device_id, collector) DO UPDATE
  SET expected   = EXCLUDED.expected,
      observed   = ops.coverage_snapshot.observed OR EXCLUDED.observed,
      gap_reason = CASE
        WHEN ops.coverage_snapshot.observed OR EXCLUDED.observed THEN NULL
        ELSE EXCLUDED.gap_reason
      END`

// CoverageWindow is the trailing day window a run recomputes, [From, To) as day bounds. The end is
// exclusive and is the start of the day after the last one written, so days includes today.
type CoverageWindow struct {
	From time.Time
	To   time.Time
}

// CoverageWindowFor returns the trailing window of whole UTC days ending with the day that contains
// `now`. lookback is a number of days and must be at least 1.
func CoverageWindowFor(now time.Time, lookback int) (CoverageWindow, error) {
	if lookback < 1 {
		return CoverageWindow{}, fmt.Errorf("rollup: coverage lookback must be at least 1 day, got %d", lookback)
	}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return CoverageWindow{
		From: today.AddDate(0, 0, -(lookback - 1)),
		To:   today.AddDate(0, 0, 1),
	}, nil
}

// runCoverage writes the coverage rows for one tenant inside the caller's transaction and returns
// how many rows the statement touched.
func (r *Runner) runCoverage(ctx context.Context, tx *sql.Tx, tenant string, now time.Time) (int64, error) {
	window, err := CoverageWindowFor(now, r.coverageLookback())
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, CoverageSnapshotSQL, tenant, window.From, window.To)
	if err != nil {
		return 0, fmt.Errorf("rollup: %s: %w", CoverageSnapshotName, err)
	}
	written, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rollup: %s: rows affected: %w", CoverageSnapshotName, err)
	}
	return written, nil
}

// CoverageSnapshotName is the name the coverage statement is reported under, so a run's log and its
// tenant report name the same thing.
const CoverageSnapshotName = "ops.coverage_snapshot"

func (r *Runner) coverageLookback() int {
	if r.CoverageDayLookback > 0 {
		return r.CoverageDayLookback
	}
	return 7
}
