package rollup

import (
	"fmt"
	"time"
)

// CoverageSnapshotName is the table the coverage statement writes, and the name its row count is
// reported under.
const CoverageSnapshotName = "ops.coverage_snapshot"

// CoverageSnapshotSQL records fleet coverage: for each enrolled device, each collector and each
// day, whether the collector was expected to report and whether it did. It reads
// ops.collector_state rather than the event tables.
//
// Three rules the statement holds:
//
//   - The expected set comes from the calendar, the enrolled devices and ref.collector, not from
//     the reports that arrived. A device that reported nothing still produces rows, so its silence
//     is visible as a gap.
//   - A collector whose latest report in the day says the signed policy switched it off (detail
//     disabled_by_policy, stored as error_code) is not expected that day, so it is not a gap. Its
//     row still names a gap reason when it was not observed, because the table requires one.
//   - observed is monotonic within a day. ops.collector_state is current state, not history, so a
//     later pass that reads a newer last_report_at must not turn a day that was observed back into
//     "not observed".
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
         bool_or(cs.state IN ('healthy','degraded'))   AS reported,
         bool_or(cs.state = 'tampered')               AS tampered,
         bool_or(cs.error_code = 'disabled_by_policy') AS disabled
    FROM ops.collector_state cs
   WHERE cs.tenant_id = $1::uuid
     AND cs.last_report_at >= $2::timestamptz AND cs.last_report_at < $3::timestamptz
   GROUP BY 1, 2, 3, 4
)
INSERT INTO ops.coverage_snapshot
  (tenant_id, snapshot_day, device_id, collector, expected, observed, gap_reason)
SELECT g.tenant_id, g.snapshot_day, g.device_id, g.collector_code,
       NOT coalesce(o.disabled, false),
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

// CoverageWindow is the trailing run of whole UTC days a pass recomputes, as the half-open range
// [From, To).
type CoverageWindow struct {
	From time.Time
	To   time.Time
}

// CoverageWindowFor returns the lookback days ending with the day that contains now, so today's
// coverage is written as it happens. lookback must be at least 1.
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
