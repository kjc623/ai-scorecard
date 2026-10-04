// Package rollup computes the usage aggregates in mart and the freshness watermarks that tell
// the read path how current they are. It is the job docs/04-dashboard-and-query.md §4 describes:
// every aggregate is written with INSERT .. ON CONFLICT DO UPDATE that REPLACES the bucket
// (brief C28), never increments it, because devices go offline and flush in bursts and because a
// subject erasure must be able to make a count fall.
//
// The statements here are frozen text. A request cannot reach them: the only variable that selects
// a statement is the bucket size, it is validated against the closed set below, and the two SQL
// fragments it selects (the date_trunc field and the series step) come from that same table rather
// than from concatenation of caller data. This is the same discipline registry.js keeps on the read
// side, and for the same reason.
package rollup

import (
	"fmt"
	"time"
)

// Bucket sizes. These are exactly the values mart.agg_*_period.bucket_size admits; hour and day
// are the only native buckets (docs/04 §2.4), so nothing else is accepted here.
const (
	BucketHour = "hour"
	BucketDay  = "day"
)

// bucketSpec is the frozen SQL for one bucket size. `trunc` is the field date_trunc takes and
// `step` the interval generate_series advances by, both as SQL literals. They live in a map so
// that the bucket size selects text rather than being interpolated into it.
type bucketSpec struct {
	trunc string
	step  string
}

var bucketSpecs = map[string]bucketSpec{
	BucketHour: {trunc: "'hour'", step: "'1 hour'"},
	BucketDay:  {trunc: "'day'", step: "'1 day'"},
}

// BucketSizes is the closed set, in the order a run should compute them (finest first).
var BucketSizes = []string{BucketHour, BucketDay}

// Aggregate is one mart table and the statement that replaces its buckets for a window.
type Aggregate struct {
	// Name is both the table name and the ops.aggregate_watermark.aggregate_name, so a source's
	// freshness is read from the row this job wrote for it.
	Name string
	// SQL takes $1 tenant uuid, $2 window lower bound (inclusive), $3 window upper bound
	// (exclusive).
	SQL string
}

// Aggregates returns the aggregate statements for one bucket size, in a deterministic order.
// An unknown bucket size is refused rather than defaulted: a typo must not silently become "day".
func Aggregates(bucketSize string) ([]Aggregate, error) {
	spec, ok := bucketSpecs[bucketSize]
	if !ok {
		return nil, fmt.Errorf("rollup: unknown bucket size %q (want %q or %q)", bucketSize, BucketHour, BucketDay)
	}
	trunc, step := spec.trunc, spec.step
	return []Aggregate{
		{Name: "mart.agg_tool_period", SQL: fmt.Sprintf(toolPeriodSQL, trunc, step, spec.trunc)},
		{Name: "mart.agg_tool_user_period", SQL: fmt.Sprintf(toolUserPeriodSQL, trunc, step, spec.trunc)},
		{Name: "mart.agg_class_period", SQL: fmt.Sprintf(classPeriodSQL, trunc, step, spec.trunc)},
		{Name: "mart.agg_org_period", SQL: fmt.Sprintf(orgPeriodSQL, trunc, step, spec.trunc)},
		{Name: "mart.agg_user_period", SQL: fmt.Sprintf(userPeriodSQL, trunc, step, spec.trunc)},
	}, nil
}

// WatermarkSQL upserts the freshness row for one (tenant, aggregate, bucket size). last_run_rows
// is how many buckets the statement inserted or replaced and is supplied by the runner from the
// statement's own row count, so it describes the run rather than being guessed.
const WatermarkSQL = `
INSERT INTO ops.aggregate_watermark
  (tenant_id, aggregate_name, bucket_size, last_complete_bucket, last_run_at, last_run_rows)
VALUES ($1, $2, $3, $4, now(), $5)
ON CONFLICT (tenant_id, aggregate_name, bucket_size) DO UPDATE
  SET last_complete_bucket = EXCLUDED.last_complete_bucket,
      last_run_at          = EXCLUDED.last_run_at,
      last_run_rows        = EXCLUDED.last_run_rows`

// SetTenantSQL puts the tenant on the session for the current transaction. Every statement the
// runner issues afterwards is scoped either by an explicit tenant_id predicate or, in production
// where the job runs as the non-superuser sac_ops, by forced row-level security. It is `local`
// (the third argument is true) so the tenant cannot leak past the transaction.
const SetTenantSQL = `SELECT set_config('app.tenant_id', $1, true)`

// Window is the half-open bucket window [From, To) a run replaces. To is aligned to the start of
// the bucket AFTER the last one being written, which is what lets the grid expression cover the
// current (open) bucket as well as the completed ones behind it.
type Window struct {
	BucketSize string
	From       time.Time
	To         time.Time
}

// WindowFor returns the trailing window for a bucket size, ending at the start of the next
// bucket after `now`. The lookback counts buckets, not wall-clock days, so a 7-bucket day window
// covers the current day and the six before it.
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

// LastCompleteBucket is the start of the newest bucket that has fully elapsed at `now`. It is
// what the freshness block shows as "complete to": the open bucket a run also writes is not
// claimed as complete.
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

// The bucket grid is built from the calendar and the known dimension members, then LEFT JOINed to
// the source, so a bucket that becomes empty after a retention expiry or an erasure is still
// written, with zeroes, rather than keeping its last value forever (docs/04 §4.4).

// toolPeriodSQL answers Q1 and part of Q2. It is the only aggregate that also carries the
// detection/rollup/degraded measures, because a tool known only from a mode-I detection has no
// submissions and would otherwise vanish from the tool inventory (docs/04 §4.6).
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

// toolUserPeriodSQL answers Q2. Subject-bearing, so its cells are suppressible below k at read
// time; the row kind is restricted to prompts because a rollup or detection names no content.
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

// classPeriodSQL answers Q4. severity is materialised from ref.rule when a label names a rule and
// from ref.data_class.default_severity otherwise: a classifier label need not name a rule, and
// dropping such a label because no ref.rule row exists would report no sensitive data where the
// classifier found some, which brief C21 forbids. classifier_version is in the key so a classifier
// change is a version change, not a mysterious shift in the numbers (docs/04 §3.4, §4.6).
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

// orgPeriodSQL answers Q3. It is empty for a tenant with no directory sync, because department
// and population live in ops.user_dim and a user with no row there cannot be attributed to a
// team. population is NULL in the dimension, and the aggregate's primary key forces the column
// NOT NULL, so a missing value is written as the empty string; the read side treats both as
// "unmapped" rather than inventing a team (docs/04 §3.3).
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

// userPeriodSQL answers Q6. It deliberately carries no score, rank or efficiency measure: brief
// §1.2 forbids productivity analytics and this is the table that could most easily become one.
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
