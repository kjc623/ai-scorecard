// Package rollup recomputes the dashboard's derived data for one tenant: the mart usage
// aggregates and their freshness watermarks, the findings, and the fleet coverage snapshot.
//
// Every aggregate is written with INSERT .. ON CONFLICT DO UPDATE that replaces the bucket rather
// than incrementing it. Devices go offline and flush in bursts, and an erasure or a retention
// expiry must be able to make a count fall; recomputing a trailing window and replacing each
// bucket in it handles both, and makes a re-run idempotent.
//
// The statements are fixed text. The only input that selects SQL is the bucket size, which is
// checked against a closed set; the date_trunc field and the series step it selects come from a
// table in this package, never from caller data.
package rollup

import (
	"fmt"
	"time"
)

// Bucket sizes: exactly the values mart.agg_*_period.bucket_size admits.
const (
	BucketHour = "hour"
	BucketDay  = "day"
)

// bucketSpec is the fixed SQL for one bucket size: the field date_trunc takes and the interval
// generate_series advances by, both as SQL literals.
type bucketSpec struct {
	trunc string
	step  string
}

var bucketSpecs = map[string]bucketSpec{
	BucketHour: {trunc: "'hour'", step: "'1 hour'"},
	BucketDay:  {trunc: "'day'", step: "'1 day'"},
}

// BucketSizes is the closed set, in the order a pass computes them (finest first).
var BucketSizes = []string{BucketHour, BucketDay}

// Aggregate is one mart table and the statement that replaces its buckets for a window.
type Aggregate struct {
	// Name is both the table name and the ops.aggregate_watermark.aggregate_name, so a table's
	// freshness is read from the row this job wrote for it.
	Name string
	// SQL takes $1 tenant uuid, $2 window lower bound (inclusive), $3 window upper bound
	// (exclusive).
	SQL string
}

// Aggregates returns the aggregate statements for one bucket size, in a fixed order. An unknown
// bucket size is refused rather than defaulted, so a typo cannot silently become "day".
func Aggregates(bucketSize string) ([]Aggregate, error) {
	spec, ok := bucketSpecs[bucketSize]
	if !ok {
		return nil, fmt.Errorf("rollup: unknown bucket size %q (want %q or %q)", bucketSize, BucketHour, BucketDay)
	}
	trunc, step := spec.trunc, spec.step
	return []Aggregate{
		{Name: "mart.agg_tool_period", SQL: fmt.Sprintf(toolPeriodSQL, trunc, step, trunc)},
		{Name: "mart.agg_tool_user_period", SQL: fmt.Sprintf(toolUserPeriodSQL, trunc, step, trunc)},
		{Name: "mart.agg_class_period", SQL: fmt.Sprintf(classPeriodSQL, trunc, step, trunc)},
		{Name: "mart.agg_org_period", SQL: fmt.Sprintf(orgPeriodSQL, trunc, step, trunc)},
		{Name: "mart.agg_user_period", SQL: fmt.Sprintf(userPeriodSQL, trunc, step, trunc)},
	}, nil
}

// WatermarkSQL upserts the freshness row for one (tenant, aggregate, bucket size). last_run_rows
// is the number of buckets the aggregate statement inserted or replaced, taken from that
// statement's own row count.
const WatermarkSQL = `
INSERT INTO ops.aggregate_watermark
  (tenant_id, aggregate_name, bucket_size, last_complete_bucket, last_run_at, last_run_rows)
VALUES ($1, $2, $3, $4, now(), $5)
ON CONFLICT (tenant_id, aggregate_name, bucket_size) DO UPDATE
  SET last_complete_bucket = EXCLUDED.last_complete_bucket,
      last_run_at          = EXCLUDED.last_run_at,
      last_run_rows        = EXCLUDED.last_run_rows`

// Window is the half-open bucket window [From, To) a pass replaces. To is the start of the bucket
// after the newest one written, so the window covers the current, still-open bucket as well as the
// completed ones behind it.
type Window struct {
	BucketSize string
	From       time.Time
	To         time.Time
}

// WindowFor returns the trailing window for a bucket size, ending at the start of the next bucket
// after now. The lookback counts buckets, so a 7-bucket day window covers today and the six days
// before it.
func WindowFor(bucketSize string, now time.Time, lookback int) (Window, error) {
	if _, ok := bucketSpecs[bucketSize]; !ok {
		return Window{}, fmt.Errorf("rollup: unknown bucket size %q", bucketSize)
	}
	if lookback < 1 {
		return Window{}, fmt.Errorf("rollup: lookback must be at least 1 bucket, got %d", lookback)
	}
	now = now.UTC()
	var from, to time.Time
	switch bucketSize {
	case BucketHour:
		to = now.Truncate(time.Hour).Add(time.Hour)
		from = to.Add(-time.Duration(lookback) * time.Hour)
	case BucketDay:
		to = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		from = to.AddDate(0, 0, -lookback)
	}
	return Window{BucketSize: bucketSize, From: from, To: to}, nil
}

// LastCompleteBucket is the start of the newest bucket that has fully elapsed at now. It is what
// the dashboard shows as "complete to": the open bucket a pass also writes is not claimed as
// complete.
func LastCompleteBucket(bucketSize string, now time.Time) (time.Time, error) {
	if _, ok := bucketSpecs[bucketSize]; !ok {
		return time.Time{}, fmt.Errorf("rollup: unknown bucket size %q", bucketSize)
	}
	now = now.UTC()
	switch bucketSize {
	case BucketHour:
		return now.Truncate(time.Hour).Add(-time.Hour), nil
	default:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1), nil
	}
}

// Each aggregate builds its bucket grid from the calendar and the known dimension members, then
// LEFT JOINs the source, so a bucket that becomes empty after a retention expiry or an erasure is
// written with zeroes rather than keeping its last value.

// toolPeriodSQL is the per-tool usage behind the tool inventory. It is the only aggregate that
// also carries the detection, rollup and degraded measures, because a tool known only from a
// process detection has no submissions and would otherwise vanish from the inventory.
var toolPeriodSQL = `
WITH buckets AS (
  SELECT generate_series(
           date_trunc(%[1]s, $2::timestamptz),
           date_trunc(%[1]s, $3::timestamptz) - interval %[2]s,
           interval %[2]s) AS bucket_start
),
tools AS (
  SELECT tool_fingerprint FROM ops.tool WHERE tenant_id = $1
  UNION
  SELECT DISTINCT tool_fingerprint FROM ingest.submission
   WHERE tenant_id = $1 AND received_at >= $2 AND received_at < $3
),
grid AS (SELECT b.bucket_start, t.tool_fingerprint FROM buckets b CROSS JOIN tools t),
src AS (
  SELECT date_trunc(%[1]s, s.received_at) AS bucket_start,
         s.tool_fingerprint,
         count(*) FILTER (WHERE s.kind = 'prompt')                                  AS submissions,
         count(DISTINCT s.user_ref) FILTER (WHERE s.kind = 'prompt')                AS users,
         coalesce(sum(s.size_bytes) FILTER (WHERE s.kind = 'prompt'), 0)::bigint   AS bytes_total,
         count(*) FILTER (WHERE s.kind = 'prompt' AND s.policy_action = 'blocked')  AS blocked,
         count(*) FILTER (WHERE s.kind = 'prompt' AND s.policy_action = 'warned')   AS warned,
         count(*) FILTER (WHERE s.kind = 'prompt' AND s.policy_action = 'logged')   AS logged,
         count(*) FILTER (WHERE s.kind = 'model_detection')                         AS detections,
         count(*) FILTER (WHERE s.kind = 'usage_rollup')                            AS rollup_events,
         count(*) FILTER (WHERE s.confidence = 'degraded')                          AS degraded_events
    FROM ingest.submission s
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3
   GROUP BY 1, 2
)
INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint,
  submissions, users, bytes_total, blocked, warned, logged, detections, rollup_events, degraded_events)
SELECT $1, g.bucket_start, %[3]s, g.tool_fingerprint,
       coalesce(src.submissions, 0), coalesce(src.users, 0), coalesce(src.bytes_total, 0),
       coalesce(src.blocked, 0), coalesce(src.warned, 0), coalesce(src.logged, 0),
       coalesce(src.detections, 0), coalesce(src.rollup_events, 0), coalesce(src.degraded_events, 0)
  FROM grid g
  LEFT JOIN src ON src.bucket_start = g.bucket_start
               AND src.tool_fingerprint = g.tool_fingerprint
ON CONFLICT (tenant_id, bucket_start, bucket_size, tool_fingerprint) DO UPDATE
  SET submissions = EXCLUDED.submissions, users = EXCLUDED.users,
      bytes_total = EXCLUDED.bytes_total, blocked = EXCLUDED.blocked,
      warned = EXCLUDED.warned, logged = EXCLUDED.logged,
      detections = EXCLUDED.detections, rollup_events = EXCLUDED.rollup_events,
      degraded_events = EXCLUDED.degraded_events`

// toolUserPeriodSQL is usage per tool per person. Its cells name a person, so the read side
// suppresses cells smaller than k; it counts prompts only, because a rollup or a detection is not
// a submission by a person.
var toolUserPeriodSQL = `
WITH buckets AS (
  SELECT generate_series(
           date_trunc(%[1]s, $2::timestamptz),
           date_trunc(%[1]s, $3::timestamptz) - interval %[2]s,
           interval %[2]s) AS bucket_start
),
members AS (
  SELECT DISTINCT tool_fingerprint, user_ref
    FROM ingest.submission
   WHERE tenant_id = $1 AND received_at >= $2 AND received_at < $3 AND kind = 'prompt'
),
grid AS (SELECT b.bucket_start, m.tool_fingerprint, m.user_ref FROM buckets b CROSS JOIN members m),
src AS (
  SELECT date_trunc(%[1]s, s.received_at) AS bucket_start, s.tool_fingerprint, s.user_ref,
         count(*) AS submissions,
         coalesce(sum(s.size_bytes), 0)::bigint AS bytes_total
    FROM ingest.submission s
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3 AND s.kind = 'prompt'
   GROUP BY 1, 2, 3
)
INSERT INTO mart.agg_tool_user_period (tenant_id, bucket_start, bucket_size, tool_fingerprint,
  user_ref, submissions, bytes_total)
SELECT $1, g.bucket_start, %[3]s, g.tool_fingerprint, g.user_ref,
       coalesce(src.submissions, 0), coalesce(src.bytes_total, 0)
  FROM grid g
  LEFT JOIN src ON src.bucket_start = g.bucket_start
               AND src.tool_fingerprint = g.tool_fingerprint
               AND src.user_ref = g.user_ref
ON CONFLICT (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref) DO UPDATE
  SET submissions = EXCLUDED.submissions, bytes_total = EXCLUDED.bytes_total`

// classPeriodSQL is sensitive data by class. Severity comes from ref.rule when a label names a
// rule and from ref.data_class.default_severity otherwise: a classifier label need not name a
// rule, and dropping such a label would report no sensitive data where the classifier found some.
// classifier_version is in the key, so a classifier change shows as a version change rather than
// an unexplained shift in the numbers.
var classPeriodSQL = `
WITH buckets AS (
  SELECT generate_series(
           date_trunc(%[1]s, $2::timestamptz),
           date_trunc(%[1]s, $3::timestamptz) - interval %[2]s,
           interval %[2]s) AS bucket_start
),
labelled AS (
  SELECT date_trunc(%[1]s, s.received_at) AS bucket_start,
         l.value->>'class' AS class_code,
         s.tool_fingerprint,
         coalesce(r.severity, dc.default_severity) AS severity,
         coalesce(s.classifier_version, '') AS classifier_version,
         s.submission_id, s.user_ref, s.confidence,
         coalesce((l.value->>'score')::numeric, 0) AS score
    FROM ingest.submission s
    CROSS JOIN LATERAL jsonb_array_elements(s.labels) AS l(value)
    JOIN ref.data_class dc ON dc.class_code = l.value->>'class'
    LEFT JOIN ref.rule r ON r.rule_id = l.value->>'rule_id'
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3
     AND s.kind = 'prompt'
     AND s.labels IS NOT NULL AND jsonb_typeof(s.labels) = 'array'
),
members AS (
  SELECT DISTINCT class_code, tool_fingerprint, severity, classifier_version FROM labelled
),
src AS (
  SELECT bucket_start, class_code, tool_fingerprint, severity, classifier_version,
         count(DISTINCT submission_id) AS submissions,
         count(DISTINCT user_ref)      AS users,
         max(score)                    AS max_score,
         count(DISTINCT submission_id) FILTER (WHERE confidence = 'degraded') AS degraded_events
    FROM labelled
   GROUP BY 1, 2, 3, 4, 5
)
INSERT INTO mart.agg_class_period (tenant_id, bucket_start, bucket_size, class_code,
  tool_fingerprint, severity, classifier_version, submissions, users, max_score, degraded_events)
SELECT $1, g.bucket_start, %[3]s, g.class_code, g.tool_fingerprint, g.severity, g.classifier_version,
       coalesce(src.submissions, 0), coalesce(src.users, 0),
       src.max_score, coalesce(src.degraded_events, 0)
  FROM (SELECT b.bucket_start, m.class_code, m.tool_fingerprint, m.severity, m.classifier_version
          FROM buckets b CROSS JOIN members m) g
  LEFT JOIN src ON src.bucket_start = g.bucket_start
               AND src.class_code = g.class_code
               AND src.tool_fingerprint = g.tool_fingerprint
               AND src.severity = g.severity
               AND src.classifier_version = g.classifier_version
ON CONFLICT (tenant_id, bucket_start, bucket_size, class_code, tool_fingerprint, severity,
             classifier_version) DO UPDATE
  SET submissions = EXCLUDED.submissions, users = EXCLUDED.users,
      max_score = EXCLUDED.max_score, degraded_events = EXCLUDED.degraded_events`

// orgPeriodSQL is usage by department. It is empty for a tenant with no directory sync, because
// department and population live in ops.user_dim and a person with no row there cannot be
// attributed to a team. The aggregate's primary key makes population NOT NULL, so a missing
// population is written as the empty string, which the read side shows as unmapped.
var orgPeriodSQL = `
WITH buckets AS (
  SELECT generate_series(
           date_trunc(%[1]s, $2::timestamptz),
           date_trunc(%[1]s, $3::timestamptz) - interval %[2]s,
           interval %[2]s) AS bucket_start
),
members AS (
  SELECT DISTINCT ud.department, s.tool_fingerprint, coalesce(ud.population, '') AS population
    FROM ingest.submission s
    JOIN ops.user_dim ud ON ud.tenant_id = s.tenant_id AND ud.user_ref = s.user_ref
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3
     AND s.kind = 'prompt' AND ud.department IS NOT NULL
),
src AS (
  SELECT date_trunc(%[1]s, s.received_at) AS bucket_start, ud.department, s.tool_fingerprint,
         coalesce(ud.population, '') AS population,
         count(*) AS submissions,
         count(DISTINCT s.user_ref) AS users
    FROM ingest.submission s
    JOIN ops.user_dim ud ON ud.tenant_id = s.tenant_id AND ud.user_ref = s.user_ref
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3
     AND s.kind = 'prompt' AND ud.department IS NOT NULL
   GROUP BY 1, 2, 3, 4
)
INSERT INTO mart.agg_org_period (tenant_id, bucket_start, bucket_size, department, population,
  tool_fingerprint, submissions, users)
SELECT $1, g.bucket_start, %[3]s, g.department, g.population, g.tool_fingerprint,
       coalesce(src.submissions, 0), coalesce(src.users, 0)
  FROM (SELECT b.bucket_start, m.department, m.population, m.tool_fingerprint
          FROM buckets b CROSS JOIN members m) g
  LEFT JOIN src ON src.bucket_start = g.bucket_start
               AND src.department = g.department
               AND src.population = g.population
               AND src.tool_fingerprint = g.tool_fingerprint
ON CONFLICT (tenant_id, bucket_start, bucket_size, department, tool_fingerprint, population)
  DO UPDATE SET submissions = EXCLUDED.submissions, users = EXCLUDED.users`

// userPeriodSQL is usage per person. It deliberately carries no score, rank or efficiency
// measure: the product reports AI usage, not productivity, and this is the table that could most
// easily be turned into a productivity ranking.
var userPeriodSQL = `
WITH buckets AS (
  SELECT generate_series(
           date_trunc(%[1]s, $2::timestamptz),
           date_trunc(%[1]s, $3::timestamptz) - interval %[2]s,
           interval %[2]s) AS bucket_start
),
members AS (
  SELECT DISTINCT user_ref FROM ingest.submission
   WHERE tenant_id = $1 AND received_at >= $2 AND received_at < $3 AND kind = 'prompt'
),
src AS (
  SELECT date_trunc(%[1]s, s.received_at) AS bucket_start, s.user_ref,
         count(*) AS submissions,
         coalesce(sum(s.size_bytes), 0)::bigint AS bytes_total,
         count(DISTINCT s.tool_fingerprint) AS tools_used,
         count(*) FILTER (WHERE s.policy_action = 'blocked') AS block_events
    FROM ingest.submission s
   WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3 AND s.kind = 'prompt'
   GROUP BY 1, 2
)
INSERT INTO mart.agg_user_period (tenant_id, bucket_start, bucket_size, user_ref,
  submissions, bytes_total, tools_used, block_events)
SELECT $1, g.bucket_start, %[3]s, g.user_ref,
       coalesce(src.submissions, 0), coalesce(src.bytes_total, 0),
       coalesce(src.tools_used, 0), coalesce(src.block_events, 0)
  FROM (SELECT b.bucket_start, m.user_ref FROM buckets b CROSS JOIN members m) g
  LEFT JOIN src ON src.bucket_start = g.bucket_start AND src.user_ref = g.user_ref
ON CONFLICT (tenant_id, bucket_start, bucket_size, user_ref) DO UPDATE
  SET submissions = EXCLUDED.submissions, bytes_total = EXCLUDED.bytes_total,
      tools_used = EXCLUDED.tools_used, block_events = EXCLUDED.block_events`

// FindingName is the table the findings statement writes. A finding is not a bucket aggregate: it
// has no bucket and no watermark, so it is kept out of Aggregates and written once per pass.
const FindingName = "mart.finding"

// FindingsSQL derives findings from classified prompt submissions in the half-open window
// [$2, $3). A finding is raised when a prompt carries a label whose rule_id names a rule in
// ref.rule, the server-side copy of the published ruleset and mart.finding.rule_id's foreign key,
// so a rule that was never published raises nothing.
//
// Severity and class are not stored: mart.v_finding reads them from the current ref.rule row, so
// a customer who edits a rule sees the change on every finding that names it.
//
// ON CONFLICT DO NOTHING makes re-evaluation idempotent: running the same window again inserts
// nothing, and an existing finding is never rewritten.
//
// The prompts CTE keeps only array labels before jsonb_array_elements runs, because the lateral
// function is evaluated per row and must never be handed a non-array.
const FindingsSQL = `
WITH prompts AS (
  SELECT s.tenant_id, s.submission_id, s.received_at, s.decided_locally, s.collection_mode, s.labels
    FROM ingest.submission s
   WHERE s.tenant_id = $1
     AND s.received_at >= $2 AND s.received_at < $3
     AND s.kind = 'prompt'
     AND s.labels IS NOT NULL AND jsonb_typeof(s.labels) = 'array'
)
INSERT INTO mart.finding (tenant_id, submission_id, rule_id, detected_at, decided_locally, collection_mode)
SELECT DISTINCT p.tenant_id, p.submission_id, l.value->>'rule_id',
       p.received_at, coalesce(p.decided_locally, false), p.collection_mode
  FROM prompts p
  CROSS JOIN LATERAL jsonb_array_elements(p.labels) AS l(value)
  JOIN ref.rule r ON r.rule_id = l.value->>'rule_id'
ON CONFLICT (tenant_id, submission_id, rule_id) DO NOTHING`
