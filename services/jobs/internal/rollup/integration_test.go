package rollup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/jobs/internal/pgtest"
)

// The tests in this file run the statements against PostgreSQL with services/database/schema.sql applied
// (see package pgtest). Events go in through the real write path, ingest.record_event, as the
// connecting role; the rollup statements then run as sac_ops, the role the jobs' login holds, so a
// missing grant or a row-level security policy that hides rows fails here.

func TestRollupAgainstPostgreSQL(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	const from, to = "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z"
	const bucket = "2026-01-15T00:00:00Z"

	// Two people and one tool, two prompts from the same person, one degraded; a detection-only
	// tool; a usage rollup, which must not count as a submission.
	ev.record(t, tx, "2026-01-15T10:00:00Z", ev.prompt(1, "userA", "toolA", "2026-01-15T10:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.9, Rule: "NO_SUCH_RULE"}}, "logged", "high", 100))
	ev.record(t, tx, "2026-01-15T11:00:00Z", ev.prompt(2, "userA", "toolA", "2026-01-15T11:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.5}}, "blocked", "high", 200))
	ev.record(t, tx, "2026-01-15T12:00:00Z", ev.prompt(3, "userB", "toolA", "2026-01-15T12:00:00Z",
		[]label{{Class: "source_code", Score: 0.8}}, "warned", "degraded", 50))
	ev.record(t, tx, "2026-01-15T13:00:00Z", ev.detection(4, "userB", "ollama_local", "2026-01-15T13:00:00Z"))
	ev.record(t, tx, "2026-01-15T14:00:00Z", ev.rollup(5, "userB", "toolA", "2026-01-15T14:00:00Z"))

	toolA := `SELECT submissions||'|'||users||'|'||bytes_total||'|'||blocked||'|'||warned||'|'||logged||'|'||detections||'|'||rollup_events||'|'||degraded_events
	            FROM mart.agg_tool_period WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`
	toolRows := `SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id = $1 AND bucket_start = $2::timestamptz`

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "tool row", toolA, "3|2|350|1|1|1|0|1|1", tenant, bucket)
	expect(t, tx, "detection-only tool",
		`SELECT submissions||'|'||users||'|'||detections FROM mart.agg_tool_period
		  WHERE tenant_id = $1 AND tool_fingerprint = 'ollama_local' AND bucket_start = $2::timestamptz`,
		"0|0|1", tenant, bucket)
	expect(t, tx, "tool rows", toolRows, "2", tenant, bucket)
	for user, want := range map[string]string{"userA": "2|300|1|1", "userB": "1|50|1|0"} {
		expect(t, tx, "user "+user,
			`SELECT submissions||'|'||bytes_total||'|'||tools_used||'|'||block_events FROM mart.agg_user_period
			  WHERE tenant_id = $1 AND user_ref = $3 AND bucket_start = $2::timestamptz`,
			want, tenant, bucket, user)
	}
	expect(t, tx, "tool-user rows",
		`SELECT string_agg(tool_fingerprint||'|'||user_ref||'|'||submissions||'|'||bytes_total, ',' ORDER BY user_ref)
		   FROM mart.agg_tool_user_period WHERE tenant_id = $1 AND bucket_start = $2::timestamptz`,
		"toolA|userA|2|300,toolA|userB|1|50", tenant, bucket)
	expect(t, tx, "class rows",
		`SELECT string_agg(class_code||'|'||severity||'|'||submissions||'|'||users||'|'||max_score||'|'||degraded_events, ',' ORDER BY class_code)
		   FROM mart.agg_class_period WHERE tenant_id = $1 AND bucket_start = $2::timestamptz`,
		"customer_pii|high|2|1|0.900|0,source_code|high|1|1|0.800|1", tenant, bucket)
	expect(t, tx, "org rows without a directory",
		`SELECT count(*)::text FROM mart.agg_org_period WHERE tenant_id = $1`, "0", tenant)
	expect(t, tx, "day watermarks",
		`SELECT count(*)::text FROM ops.aggregate_watermark WHERE tenant_id = $1 AND bucket_size = 'day'`, "5", tenant)

	// A second run over the same inputs leaves the same values and the same number of rows.
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "tool row after re-run", toolA, "3|2|350|1|1|1|0|1|1", tenant, bucket)
	expect(t, tx, "tool rows after re-run", toolRows, "2", tenant, bucket)

	// A device that was offline flushes into the same bucket; recomputing absorbs it into the
	// existing row.
	pgtest.AsOwner(t, tx)
	ev.record(t, tx, "2026-01-15T23:00:00Z", ev.prompt(6, "userC", "toolA", "2026-01-15T23:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.4}}, "logged", "high", 10))
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "tool row after late arrival",
		`SELECT submissions||'|'||users||'|'||bytes_total FROM mart.agg_tool_period
		  WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
		"4|3|360", tenant, bucket)
	expect(t, tx, "tool rows after late arrival", toolRows, "2", tenant, bucket)
}

func TestRollupHourBuckets(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	ev.record(t, tx, "2026-01-15T10:20:00Z", ev.prompt(7, "userA", "toolA", "2026-01-15T10:20:00Z",
		[]label{{Class: "customer_pii", Score: 0.7}}, "logged", "high", 10))
	ev.record(t, tx, "2026-01-15T11:20:00Z", ev.prompt(8, "userA", "toolA", "2026-01-15T11:20:00Z",
		[]label{{Class: "customer_pii", Score: 0.7}}, "logged", "high", 10))

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketHour, "2026-01-15T10:00:00Z", "2026-01-15T12:00:00Z")
	expect(t, tx, "hour buckets",
		`SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id = $1 AND tool_fingerprint = 'toolA'`, "2", tenant)
	for _, hour := range []string{"2026-01-15T10:00:00Z", "2026-01-15T11:00:00Z"} {
		expect(t, tx, "bucket "+hour,
			`SELECT submissions::text FROM mart.agg_tool_period
			  WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
			"1", tenant, hour)
	}
}

// TestFindingsAgainstPostgreSQL checks that a label naming a published rule raises exactly one
// finding, a label naming an unpublished rule raises none, and re-evaluating the window inserts
// nothing new.
func TestFindingsAgainstPostgreSQL(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	rule := pgtest.Scalar(t, tx, `SELECT rule_id FROM ref.rule ORDER BY rule_id LIMIT 1`)
	class := pgtest.Scalar(t, tx, `SELECT class_code FROM ref.rule WHERE rule_id = $1`, rule)

	ev.record(t, tx, "2026-02-10T10:00:00Z", ev.prompt(11, "userA", "toolA", "2026-02-10T10:00:00Z",
		[]label{{Class: class, Score: 0.9, Rule: rule}}, "logged", "high", 10))
	ev.record(t, tx, "2026-02-10T11:00:00Z", ev.prompt(12, "userA", "toolA", "2026-02-10T11:00:00Z",
		[]label{{Class: class, Score: 0.9, Rule: "NO_SUCH_RULE"}}, "logged", "high", 10))

	pgtest.AsJobs(t, tx)
	from, to := mustTime(t, "2026-02-10T00:00:00Z"), mustTime(t, "2026-02-11T00:00:00Z")
	pgtest.Exec(t, tx, FindingsSQL, tenant, from, to)
	expect(t, tx, "findings", `SELECT count(*)::text FROM mart.finding WHERE tenant_id = $1`, "1", tenant)
	expect(t, tx, "finding rule", `SELECT rule_id FROM mart.finding WHERE tenant_id = $1`, rule, tenant)
	pgtest.Exec(t, tx, FindingsSQL, tenant, from, to)
	expect(t, tx, "findings after re-run", `SELECT count(*)::text FROM mart.finding WHERE tenant_id = $1`, "1", tenant)
}

// TestCoverageSnapshotAgainstPostgreSQL checks that every enrolled device gets a row per collector,
// that only a healthy or degraded report counts as observed, and that a day already observed is
// not erased when the collector's current state moves on.
func TestCoverageSnapshotAgainstPostgreSQL(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	now := time.Now().UTC().Truncate(time.Second)
	window, err := CoverageWindowFor(now, 1)
	if err != nil {
		t.Fatal(err)
	}
	collectors := pgtest.Scalar(t, tx, `SELECT count(*)::text FROM ref.collector`)
	codes := strings.Split(pgtest.Scalar(t, tx,
		`SELECT string_agg(collector_code, ',' ORDER BY collector_code) FROM ref.collector`), ",")
	if len(codes) < 4 {
		t.Fatalf("ref.collector has %d rows; the test needs four", len(codes))
	}
	healthy, tampered, absent, silent := codes[0], codes[1], codes[2], codes[3]
	for collector, state := range map[string]string{healthy: "healthy", tampered: "tampered", absent: "absent"} {
		pgtest.Exec(t, tx, `INSERT INTO ops.collector_state (tenant_id, device_id, collector, state, last_report_at)
		                    VALUES ($1, $2, $3, $4, $5)`, tenant, device, collector, state, now)
	}

	pgtest.AsJobs(t, tx)
	pgtest.Exec(t, tx, CoverageSnapshotSQL, tenant, window.From, window.To)
	count := func(filter string) string {
		return pgtest.Scalar(t, tx, `SELECT count(*) FILTER (WHERE `+filter+`)::text FROM ops.coverage_snapshot WHERE tenant_id = $1`, tenant)
	}
	gap := func(collector string) string {
		return pgtest.Scalar(t, tx, `SELECT gap_reason FROM ops.coverage_snapshot WHERE tenant_id = $1 AND collector = $2`, tenant, collector)
	}
	if got := count("true"); got != collectors {
		t.Errorf("coverage rows = %s, want one per collector (%s)", got, collectors)
	}
	if got := count("expected"); got != collectors {
		t.Errorf("expected rows = %s, want %s", got, collectors)
	}
	if got := count("observed"); got != "1" {
		t.Errorf("observed rows = %s, want 1", got)
	}
	for collector, want := range map[string]string{tampered: "tampered", absent: "unknown", silent: "unknown", healthy: "<null>"} {
		if got := gap(collector); got != want {
			t.Errorf("gap_reason(%s) = %s, want %s", collector, got, want)
		}
	}

	// The healthy collector reports absent later the same day; the day stays observed.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `UPDATE ops.collector_state SET state = 'absent', last_report_at = $3
	                     WHERE tenant_id = $1 AND collector = $2`, tenant, healthy, now.Add(time.Minute))
	pgtest.AsJobs(t, tx)
	pgtest.Exec(t, tx, CoverageSnapshotSQL, tenant, window.From, window.To)
	if got := count("observed"); got != "1" {
		t.Errorf("observed rows after the state moved on = %s, want 1", got)
	}
}

// TestTenantPassAsJobsRole runs one tenant's complete pass as sac_ops, which is what proves the
// role holds every grant the pass needs.
func TestTenantPassAsJobsRole(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	now := time.Now().UTC()
	received := now.Add(-time.Hour).Format(time.RFC3339)
	ev.record(t, tx, received, ev.prompt(21, "userA", "toolA", received,
		[]label{{Class: "customer_pii", Score: 0.9}}, "logged", "high", 42))

	pgtest.AsJobs(t, tx)
	written, err := Tenant(context.Background(), tx, tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, tx, "watermarks", `SELECT count(*)::text FROM ops.aggregate_watermark WHERE tenant_id = $1`,
		fmt.Sprint(len(BucketSizes)*5), tenant)
	expect(t, tx, "prompts in today's day bucket",
		`SELECT sum(submissions)::text FROM mart.agg_tool_period
		  WHERE tenant_id = $1 AND bucket_size = 'day' AND tool_fingerprint = 'toolA'`, "1", tenant)
	wantCoverage := pgtest.Scalar(t, tx, `SELECT (count(*) * $1)::text FROM ref.collector`, CoverageDayLookback)
	if got := fmt.Sprint(written[CoverageSnapshotName]); got != wantCoverage {
		t.Errorf("coverage rows written = %s, want %s (collectors x %d days)", got, wantCoverage, CoverageDayLookback)
	}
	if written["mart.agg_tool_period day"] != DayLookback {
		t.Errorf("day buckets written for one tool = %d, want %d", written["mart.agg_tool_period day"], DayLookback)
	}
}

// --- helpers -------------------------------------------------------------------------------------

// runBucket runs every aggregate for one bucket size over [from, to), each followed by its
// watermark, exactly as Tenant does.
func runBucket(t *testing.T, tx *sql.Tx, tenant, bucketSize, from, to string) {
	t.Helper()
	aggs, err := Aggregates(bucketSize)
	if err != nil {
		t.Fatal(err)
	}
	f, u := mustTime(t, from), mustTime(t, to)
	lastComplete := u.Add(-time.Hour)
	for _, a := range aggs {
		res, err := tx.ExecContext(context.Background(), a.SQL, tenant, f, u)
		if err != nil {
			t.Fatalf("%s (%s): %v", a.Name, bucketSize, err)
		}
		n, _ := res.RowsAffected()
		pgtest.Exec(t, tx, WatermarkSQL, tenant, a.Name, bucketSize, lastComplete, n)
	}
}

func expect(t *testing.T, tx *sql.Tx, what, query, want string, args ...any) {
	t.Helper()
	if got := pgtest.Scalar(t, tx, query, args...); got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

type label struct {
	Class string  `json:"class"`
	Score float64 `json:"score"`
	Rule  string  `json:"rule_id,omitempty"`
}

// events builds envelopes for one tenant and device and records them through ingest.record_event.
type events struct {
	tenant, device string
}

func (e events) record(t *testing.T, tx *sql.Tx, received string, envelope map[string]any) {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, tx, `SELECT ingest.record_event($1::jsonb, $2::timestamptz)`, string(body), received)
}

func (e events) base(n int, user, tool, occurred string) map[string]any {
	id := fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", n)
	return map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": e.tenant, "device_id": e.device,
		"user_ref": user, "tool_fingerprint": tool, "occurred_at": occurred,
		"monotonic_offset_ms": 1000 * n, "dedup_key": digest(e.tenant + id),
	}
}

func (e events) prompt(n int, user, tool, occurred string, labels []label, action, confidence string, size int) map[string]any {
	m := e.base(n, user, tool, occurred)
	for k, v := range map[string]any{
		"direction": "egress", "kind": "prompt", "source": "ext.page_context", "collection_mode": "m1",
		"size_bytes": size, "content_digest": m["dedup_key"], "labels": labels,
		"classifier_version": "jobs-test", "confidence": confidence,
		"policy_decision": map[string]any{"rule_id": "DEFAULT_LOG", "action": action, "decided_locally": true},
	} {
		m[k] = v
	}
	return m
}

func (e events) detection(n int, user, tool, occurred string) map[string]any {
	m := e.base(n, user, tool, occurred)
	for k, v := range map[string]any{
		"direction": "none", "kind": "model_detection", "source": "proc.detect",
		"collection_mode": "m0", "detection_basis": "process_scan",
	} {
		m[k] = v
	}
	return m
}

func (e events) rollup(n int, user, tool, occurred string) map[string]any {
	m := e.base(n, user, tool, occurred)
	for k, v := range map[string]any{
		"direction": "none", "kind": "usage_rollup", "source": "cli.shim", "collection_mode": "m0",
		"window_start": "2026-01-15T13:00:00Z", "window_end": "2026-01-15T14:00:00Z",
		"submission_count": 7, "bytes_total": 700,
	} {
		m[k] = v
	}
	return m
}

func digest(seed string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(seed)))
}
