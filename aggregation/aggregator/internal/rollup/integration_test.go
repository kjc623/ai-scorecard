package rollup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestRollupAgainstPostgreSQL is the database evidence for the aggregation job. It runs against a
// live server with database/schema.sql applied, drives events through the real write path
// (ingest.record_event), runs the frozen rollup statements as text, and checks three properties
// the task names:
//
//  1. the aggregates are the right shape — prompt submissions counted once, distinct subjects,
//     class fan-out, detection-only tools present, rollup volumes kept out of submission counts;
//  2. a bucket recomputed twice is identical (idempotent, re-runnable);
//  3. an event that arrives late for an already-computed bucket is absorbed by recomputation,
//     and no duplicate bucket row appears.
//
// Everything runs inside one transaction and ends with ROLLBACK, so the server is left as found.

const (
	itTenant = "aaaaaaaa-0000-4000-8000-000000000001"
	itDevice = "aaaaaaaa-0000-4000-8000-000000000002"
)

func TestRollupAgainstPostgreSQL(t *testing.T) {
	container := psqlContainer(t)

	from := "2026-01-15T00:00:00Z"
	to := "2026-01-16T00:00:00Z"

	var b strings.Builder
	b.WriteString("BEGIN;\n")
	b.WriteString("SELECT set_config('app.tenant_id', '" + itTenant + "', true);\n")
	fmt.Fprintf(&b, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
VALUES ('%s', 'rollup-integration', 'active', 'test-region', 'vendor', 'm1');`+"\n", itTenant)
	fmt.Fprintf(&b, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ('%s', '%s', 'windows');`+"\n", itTenant, itDevice)

	// Two people, one tool, two prompts from the same person, plus a detection-only tool and a
	// rollout. One prompt is degraded so degraded_events is exercised.
	recordEvent(&b, "2026-01-15T10:00:00Z", promptEnvelope(eventUUID(1), "userA", "toolA", "2026-01-15T10:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.9, Rule: "NO_SUCH_RULE"}}, "logged", "high", 100))
	recordEvent(&b, "2026-01-15T11:00:00Z", promptEnvelope(eventUUID(2), "userA", "toolA", "2026-01-15T11:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.5}}, "blocked", "high", 200))
	recordEvent(&b, "2026-01-15T12:00:00Z", promptEnvelope(eventUUID(3), "userB", "toolA", "2026-01-15T12:00:00Z",
		[]label{{Class: "source_code", Score: 0.8}}, "warned", "degraded", 50))
	recordEvent(&b, "2026-01-15T13:00:00Z", detectionEnvelope(eventUUID(4), "userB", "ollama_local", "2026-01-15T13:00:00Z"))
	recordEvent(&b, "2026-01-15T14:00:00Z", rollupEnvelope(eventUUID(5), "userB", "toolA", "2026-01-15T14:00:00Z"))

	// Phase 1: the first run.
	runAggregates(&b, BucketDay, from, to, true)
	assertEQ(&b, "baseline_toolA", `SELECT submissions||'|'||users||'|'||bytes_total||'|'||blocked||'|'||warned||'|'||logged||'|'||detections||'|'||rollup_events||'|'||degraded_events
  FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_detect", `SELECT submissions||'|'||users||'|'||detections
  FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='ollama_local' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_toolA_rows", `SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_userA", `SELECT submissions||'|'||bytes_total||'|'||tools_used||'|'||block_events
  FROM mart.agg_user_period WHERE tenant_id='`+itTenant+`' AND user_ref='userA' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_userB", `SELECT submissions||'|'||bytes_total||'|'||tools_used||'|'||block_events
  FROM mart.agg_user_period WHERE tenant_id='`+itTenant+`' AND user_ref='userB' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_tooluser", `SELECT string_agg(tool_fingerprint||'|'||user_ref||'|'||submissions||'|'||bytes_total, E'\n' ORDER BY user_ref)
  FROM mart.agg_tool_user_period WHERE tenant_id='`+itTenant+`' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_classes", `SELECT string_agg(class_code||'|'||severity||'|'||submissions||'|'||users||'|'||max_score||'|'||degraded_events, E'\n' ORDER BY class_code)
  FROM mart.agg_class_period WHERE tenant_id='`+itTenant+`' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "baseline_org_rows", `SELECT count(*)::text FROM mart.agg_org_period WHERE tenant_id='`+itTenant+`'`)
	assertEQ(&b, "baseline_watermarks", `SELECT count(*)::text FROM ops.aggregate_watermark WHERE tenant_id='`+itTenant+`' AND bucket_size='day'`)

	// Phase 2: a second run with the same inputs must produce identical rows. This is C28's
	// idempotency: a retried job cannot double a bucket.
	runAggregates(&b, BucketDay, from, to, false)
	assertEQ(&b, "idempotent_toolA", `SELECT submissions||'|'||users||'|'||bytes_total||'|'||blocked||'|'||warned||'|'||logged||'|'||detections||'|'||rollup_events||'|'||degraded_events
  FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "idempotent_toolA_rows", `SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)

	// Phase 3: a device that was offline flushes into the same bucket. Recomputing absorbs it and
	// still leaves exactly one row for the bucket.
	recordEvent(&b, "2026-01-15T23:00:00Z", promptEnvelope(eventUUID(6), "userC", "toolA", "2026-01-15T23:00:00Z",
		[]label{{Class: "customer_pii", Score: 0.4}}, "logged", "high", 10))
	runAggregates(&b, BucketDay, from, to, false)
	assertEQ(&b, "late_toolA", `SELECT submissions||'|'||users||'|'||bytes_total
  FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)
	assertEQ(&b, "late_toolA_rows", `SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND bucket_start='2026-01-15T00:00:00Z'::timestamptz`)

	b.WriteString("ROLLBACK;\n")

	out := runPSQL(t, container, b.String())
	got := parseKeyed(out)

	want := map[string]string{
		// prompt submissions only; bytes 100+200+50; rollup volume excluded from submissions.
		"baseline_toolA":      "3|2|350|1|1|1|0|1|1",
		"baseline_detect":     "0|0|1",
		"baseline_toolA_rows": "2",
		"baseline_userA":      "2|300|1|1",
		"baseline_userB":      "1|50|1|0",
		"baseline_tooluser":   "toolA|userA|2|300\ntoolA|userB|1|50",
		"baseline_classes":    "customer_pii|high|2|1|0.900|0\nsource_code|high|1|1|0.800|1",
		"baseline_org_rows":   "0",
		"baseline_watermarks": "5",
		// A second identical run leaves the same values and the same number of rows.
		"idempotent_toolA":      "3|2|350|1|1|1|0|1|1",
		"idempotent_toolA_rows": "2",
		// The late prompt is absorbed by recomputation; one row, not two.
		"late_toolA":      "4|3|360",
		"late_toolA_rows": "2",
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("%s = %q, want %q", key, got[key], expected)
		}
	}
	if t.Failed() {
		t.Logf("--- psql output ---\n%s", out)
	}
}

// TestRollupHourBucket proves the hour statements run and bucket on the hour, not the day.
func TestRollupHourBucket(t *testing.T) {
	container := psqlContainer(t)

	var b strings.Builder
	b.WriteString("BEGIN;\n")
	b.WriteString("SELECT set_config('app.tenant_id', '" + itTenant + "', true);\n")
	fmt.Fprintf(&b, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
VALUES ('%s', 'rollup-hour', 'active', 'test-region', 'vendor', 'm1');`+"\n", itTenant)
	fmt.Fprintf(&b, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ('%s', '%s', 'windows');`+"\n", itTenant, itDevice)
	recordEvent(&b, "2026-01-15T10:20:00Z", promptEnvelope(eventUUID(7), "userA", "toolA", "2026-01-15T10:20:00Z",
		[]label{{Class: "customer_pii", Score: 0.7}}, "logged", "high", 10))
	recordEvent(&b, "2026-01-15T11:20:00Z", promptEnvelope(eventUUID(8), "userA", "toolA", "2026-01-15T11:20:00Z",
		[]label{{Class: "customer_pii", Score: 0.7}}, "logged", "high", 10))

	runAggregates(&b, BucketHour, "2026-01-15T10:00:00Z", "2026-01-15T12:00:00Z", false)
	assertEQ(&b, "hour_buckets", `SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA'`)
	assertEQ(&b, "hour_ten", `SELECT submissions::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA' AND bucket_start='2026-01-15T10:00:00Z'::timestamptz`)
	assertEQ(&b, "hour_eleven", `SELECT submissions::text FROM mart.agg_tool_period WHERE tenant_id='`+itTenant+`' AND tool_fingerprint='toolA' AND bucket_start='2026-01-15T11:00:00Z'::timestamptz`)
	b.WriteString("ROLLBACK;\n")

	out := runPSQL(t, container, b.String())
	got := parseKeyed(out)
	want := map[string]string{"hour_buckets": "2", "hour_ten": "1", "hour_eleven": "1"}
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("%s = %q, want %q", key, got[key], expected)
		}
	}
}

// --- helpers -----------------------------------------------------------------------------------

type label struct {
	Class string  `json:"class"`
	Score float64 `json:"score"`
	Rule  string  `json:"rule_id,omitempty"`
}

func promptEnvelope(id, user, tool, occurred string, labels []label, action, confidence string, size int) string {
	body := map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": itTenant, "device_id": itDevice,
		"user_ref": user, "tool_fingerprint": tool, "direction": "egress", "kind": "prompt",
		"occurred_at": occurred, "monotonic_offset_ms": 1000, "source": "ext.page_context",
		"collection_mode": "m1", "size_bytes": size,
		"content_digest": digestOf(id), "dedup_key": digestOf(id), "labels": labels,
		"classifier_version": "lab-test", "confidence": confidence,
		"policy_decision": map[string]any{"rule_id": "DEFAULT_LOG", "action": action, "decided_locally": true},
	}
	return mustJSON(body)
}

func detectionEnvelope(id, user, tool, occurred string) string {
	return mustJSON(map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": itTenant, "device_id": itDevice,
		"user_ref": user, "tool_fingerprint": tool, "direction": "none", "kind": "model_detection",
		"occurred_at": occurred, "monotonic_offset_ms": 2000, "source": "proc.detect",
		"collection_mode": "m0", "detection_basis": "process_scan", "dedup_key": digestOf(id),
	})
}

func rollupEnvelope(id, user, tool, occurred string) string {
	return mustJSON(map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": itTenant, "device_id": itDevice,
		"user_ref": user, "tool_fingerprint": tool, "direction": "none", "kind": "usage_rollup",
		"occurred_at": occurred, "monotonic_offset_ms": 3000, "source": "cli.shim",
		"collection_mode": "m0", "window_start": "2026-01-15T13:00:00Z", "window_end": "2026-01-15T14:00:00Z",
		"submission_count": 7, "bytes_total": 700, "dedup_key": digestOf(id),
	})
}

func digestOf(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("sha256:%x", sum)
}

// eventUUID renders a deterministic UUID-shaped event id; ingest.record_event casts it to uuid.
func eventUUID(n int) string {
	return fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", n)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// recordEvent drives one observation through the real write path. PERFORM discards the function's
// result row so it does not pollute the parsed output.
func recordEvent(b *strings.Builder, received string, envelope string) {
	fmt.Fprintf(b, "DO $do$ BEGIN PERFORM ingest.record_event(%s::jsonb, '%s'::timestamptz); END $do$;\n", q(envelope), received)
}

// runAggregates renders every frozen statement as a PREPARE/EXECUTE pair against the exact text
// the runner would send, so the test exercises the SQL rather than a paraphrase of it.
func runAggregates(b *strings.Builder, bucketSize, from, to string, withWatermark bool) {
	aggs, err := Aggregates(bucketSize)
	if err != nil {
		panic(err)
	}
	lastComplete := "2026-01-14T00:00:00Z"
	if bucketSize == BucketHour {
		lastComplete = "2026-01-15T09:00:00Z"
	}
	for i, a := range aggs {
		name := fmt.Sprintf("agg_%s_%d", bucketSize, i)
		fmt.Fprintf(b, "PREPARE %s AS %s;\n", name, a.SQL)
		fmt.Fprintf(b, "EXECUTE %s('%s'::uuid, '%s'::timestamptz, '%s'::timestamptz);\nDEALLOCATE %s;\n", name, itTenant, from, to, name)
		if withWatermark {
			wm := name + "_wm"
			fmt.Fprintf(b, "PREPARE %s AS %s;\n", wm, WatermarkSQL)
			fmt.Fprintf(b, "EXECUTE %s('%s'::uuid, '%s', '%s', '%s'::timestamptz, 1);\nDEALLOCATE %s;\n",
				wm, itTenant, a.Name, bucketSize, lastComplete, wm)
		}
	}
}

// assertEQ emits one labelled value so the Go side can compare without a psql failure aborting the
// transaction before ROLLBACK.
func assertEQ(b *strings.Builder, key, query string) {
	fmt.Fprintf(b, "SELECT '%s|' || COALESCE((%s)::text, '<null>');\n", key, query)
}

// parseKeyed turns the labelled lines into a map. Blank lines and anything without a known key
// prefix are ignored; a value containing newlines is joined back together.
func parseKeyed(out string) map[string]string {
	got := map[string]string{}
	key := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		idx := strings.Index(line, "|")
		if idx < 0 {
			if key != "" {
				got[key] += "\n" + line
			}
			continue
		}
		candidate := line[:idx]
		if isKnownKey(candidate) {
			key = candidate
			got[key] = line[idx+1:]
			continue
		}
		if key != "" {
			got[key] += "\n" + line
		}
	}
	return got
}

func isKnownKey(k string) bool {
	for _, prefix := range []string{"baseline_", "idempotent_", "late_", "hour_"} {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
