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
		`SELECT count(*)::text FROM ops.aggregate_watermark WHERE tenant_id = $1 AND bucket_size = 'day'`, "9", tenant)

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

	// A source that goes (an erasure, a retention expiry) takes its bucket's row with it, and a
	// window with no source at all writes nothing: no zero rows stand in for data that never was.
	pgtest.AsOwner(t, tx)
	if _, err := tx.Exec(`DELETE FROM ingest.submission WHERE tenant_id = $1 AND tool_fingerprint = 'ollama_local'`, tenant); err != nil {
		t.Fatal(err)
	}
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "tool rows after the detection is erased", toolRows, "1", tenant, bucket)
	runBucket(t, tx, tenant, BucketDay, "2026-01-10T00:00:00Z", from)
	expect(t, tx, "rows for days with no data",
		`SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id = $1 AND bucket_start < $2::timestamptz`,
		"0", tenant, bucket)
}

func TestRollupTeams(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	const from, to = "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z"
	ada, grace, bob := "u_"+strings.Repeat("a", 32), "u_"+strings.Repeat("b", 32), "u_"+strings.Repeat("c", 32)

	// Ada is in a console team and the Engineering department; Grace is in the department and in
	// an OU below the team's unit; Bob is in no team.
	pgtest.Exec(t, tx, `INSERT INTO ops.user_dim (tenant_id, user_ref, department, org_unit, status) VALUES
	  ($1, $2, 'Engineering', NULL, 'active'),
	  ($1, $3, 'engineering', 'OU=Labs,OU=Research,DC=contoso,DC=com', 'active'),
	  ($1, $4, 'Sales', NULL, 'active')`, tenant, ada, grace, bob)
	newTeam := func(name string) string {
		return pgtest.Scalar(t, tx, `INSERT INTO ops.team (tenant_id, name, created_by) VALUES ($1, $2, 'test') RETURNING team_id::text`, tenant, name)
	}
	include := func(team, kind, value, other string) {
		pgtest.Exec(t, tx, `INSERT INTO ops.team_include (tenant_id, team_id, kind, match_value, included_team_id, added_by)
		  VALUES ($1, $2::uuid, $3, NULLIF($4, ''), NULLIF($5, '')::uuid, 'test')`, tenant, team, kind, value, other)
	}
	console := newTeam("Pilot")
	pgtest.Exec(t, tx, `INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by) VALUES ($1, $2, $3, 'test')`, tenant, console, ada)
	dept := newTeam("Engineering")
	include(dept, "department", "ENGINEERING", "")
	ou := newTeam("Research")
	include(ou, "org_unit", "OU=Research,DC=contoso,DC=com", "")
	// A team of a chosen person and another team: Ada, and Grace through Research.
	mixed := newTeam("Pilot and Research")
	pgtest.Exec(t, tx, `INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by) VALUES ($1, $2, $3, 'test')`, tenant, mixed, ada)
	include(mixed, "team", "", ou)

	for i, user := range []string{ada, ada, grace, bob} {
		at := fmt.Sprintf("2026-01-15T1%d:00:00Z", i)
		ev.record(t, tx, at, ev.prompt(20+i, user, "toolA", at, nil, "logged", "high", 10))
	}

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	team := `SELECT coalesce(string_agg(submissions||'|'||users, ','), '') FROM mart.agg_team_period
	          WHERE tenant_id = $1 AND team_id = $2::uuid`
	expect(t, tx, "console team", team, "2|1", tenant, console)
	expect(t, tx, "department team", team, "3|2", tenant, dept)
	expect(t, tx, "org unit team", team, "1|1", tenant, ou)
	expect(t, tx, "mixed team", team, "3|2", tenant, mixed)

	// A person leaving a team takes their usage with them on the next pass.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `DELETE FROM ops.team_member WHERE tenant_id = $1 AND team_id = $2::uuid`, tenant, console)
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "emptied console team", team, "", tenant, console)
}

// TestRollupSensitive checks that a prompt is sensitive only when its enforcement rule matched on
// a label the rule names, that each outcome is counted apart, that a class is flagged only when its
// rule names it, that attachments are summed beside the prompts able to describe one, and that
// each rule's prompts are counted by outcome while a prompt no rule matched counts under none.
func TestRollupSensitive(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	const from, to = "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z"
	const bucket = "2026-01-15T00:00:00Z"

	pgtest.Exec(t, tx, `INSERT INTO ops.enforcement_rule (tenant_id, position, rule_id, action, match, updated_by) VALUES
	  ($1, 0, 'pii.block',  'block', '{"labels":["customer_pii"]}', 'test'),
	  ($1, 1, 'pii.warn',   'warn',  '{"labels":["customer_pii"],"sanction":["sanctioned"]}', 'test'),
	  ($1, 2, 'tool.block', 'block', '{"tools":["toolA"]}', 'test'),
	  ($1, 3, 'cred.allow', 'allow', '{"labels":["credential"]}', 'test')`, tenant)

	decided := func(m map[string]any, rule, action string) map[string]any {
		m["policy_decision"] = map[string]any{"rule_id": rule, "action": action, "decided_locally": true}
		return m
	}
	attached := func(m map[string]any, names ...string) map[string]any {
		atts := []map[string]any{}
		for _, n := range names {
			atts = append(atts, map[string]any{"name": n})
		}
		m["attachments"] = atts
		return m
	}
	pii := []label{{Class: "customer_pii", Score: 0.9}}
	at := func(h int) string { return fmt.Sprintf("2026-01-15T1%d:00:00Z", h) }

	// Blocked on its label, with two attachments.
	ev.record(t, tx, at(0), attached(decided(ev.prompt(40, "userA", "toolA", at(0), pii, "blocked", "high", 10), "pii.block", "blocked"), "a.xlsx", "b.pdf"))
	// Warned on its label; only the class the rule names is flagged.
	ev.record(t, tx, at(1), attached(decided(ev.prompt(41, "userA", "toolA", at(1),
		[]label{{Class: "customer_pii", Score: 0.8}, {Class: "source_code", Score: 0.6}}, "warned", "high", 10), "pii.warn", "warned"), "c.txt"))
	// Blocked by a rule that names no label: blocked, not sensitive.
	ev.record(t, tx, at(2), decided(ev.prompt(42, "userB", "toolA", at(2),
		[]label{{Class: "source_code", Score: 0.7}}, "blocked", "high", 10), "tool.block", "blocked"))
	// A label no rule matched on.
	ev.record(t, tx, at(3), decided(ev.prompt(43, "userB", "toolA", at(3),
		[]label{{Class: "credential", Score: 0.7}}, "logged", "high", 10), "policy.default", "logged"))
	// Allowed by a rule that names its label: sensitive, and logged.
	ev.record(t, tx, at(4), decided(ev.prompt(44, "userB", "toolA", at(4),
		[]label{{Class: "credential", Score: 0.9}}, "logged", "high", 10), "cred.allow", "logged"))
	// An M0 prompt carries no content, so it cannot describe an attachment.
	m0 := decided(ev.base(45, "userC", "toolA", at(5)), "policy.default", "logged")
	for k, v := range map[string]any{"direction": "egress", "kind": "prompt", "source": "ext.page_context", "collection_mode": "m0", "size_bytes": 10} {
		m0[k] = v
	}
	ev.record(t, tx, at(5), m0)

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "tool row",
		`SELECT submissions||'|'||blocked||'|'||sensitive||'|'||sensitive_blocked||'|'||sensitive_warned||'|'||sensitive_logged||'|'||attachments||'|'||content_prompts
		   FROM mart.agg_tool_period WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
		"6|2|3|1|1|1|3|5", tenant, bucket)
	pgtest.AsOwner(t, tx)
	expect(t, tx, "the view carries the same figures",
		`SELECT sensitive||'|'||sensitive_logged||'|'||attachments||'|'||content_prompts
		   FROM mart.v_tool_usage WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
		"3|1|3|5", tenant, bucket)
	pgtest.AsJobs(t, tx)
	expect(t, tx, "class rows",
		`SELECT string_agg(class_code||'|'||submissions||'|'||flagged||'|'||b||'|'||w||'|'||l, ',' ORDER BY class_code)
		   FROM (SELECT class_code, sum(submissions) AS submissions, sum(flagged) AS flagged,
		                sum(flagged_blocked) AS b, sum(flagged_warned) AS w, sum(flagged_logged) AS l
		           FROM mart.agg_class_period
		          WHERE tenant_id = $1 AND bucket_start = $2::timestamptz GROUP BY class_code) c`,
		"credential|2|1|0|0|1,customer_pii|2|2|1|1|0,source_code|2|0|0|0|0", tenant, bucket)
	expect(t, tx, "attachment counts",
		`SELECT string_agg(coalesce(attachment_count::text, 'none'), ',' ORDER BY first_occurred_at)
		   FROM ingest.submission WHERE tenant_id = $1`,
		"2,1,none,none,none,none", tenant)
	expect(t, tx, "rule rows",
		`SELECT string_agg(rule_id||'|'||matches||'|'||blocked||'|'||warned||'|'||logged, ',' ORDER BY rule_id)
		   FROM mart.agg_rule_period WHERE tenant_id = $1 AND bucket_start = $2::timestamptz`,
		"cred.allow|1|0|0|1,pii.block|1|1|0|0,pii.warn|1|0|1|0,tool.block|1|1|0|0", tenant, bucket)

	// A rule that no longer names the label stops making its prompts sensitive on the next pass.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `UPDATE ops.enforcement_rule SET match = '{"tools":["toolA"]}' WHERE tenant_id = $1 AND rule_id = 'cred.allow'`, tenant)
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "sensitive after the rule changed",
		`SELECT sensitive||'|'||sensitive_logged FROM mart.agg_tool_period
		  WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
		"2|0", tenant, bucket)
}

func TestRollupDiscovery(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	other := pgtest.NewUUID(t, tx)
	pgtest.Exec(t, tx, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1, $2, 'windows')`, tenant, other)
	a, b := events{tenant: tenant, device: device}, events{tenant: tenant, device: other}
	const from, to = "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z"
	const bucket = "2026-01-15T00:00:00Z"
	found := func(e events, n int, tool, at, kind, basis, source string, extra map[string]any) {
		m := e.base(n, "userA", tool, at)
		for k, v := range map[string]any{"direction": "none", "kind": "discovery", "source": source,
			"collection_mode": "m0", "discovery_type": kind, "detection_basis": basis} {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		e.record(t, tx, at, m)
	}

	// One app installed on both devices and reported twice by one of them; a runtime whose models
	// differ by device; an uncatalogued process reaching one host from one device.
	found(a, 60, "app:cursor", "2026-01-15T01:00:00Z", "app_installed", "installed_scan", "inv.scan", nil)
	found(a, 61, "app:cursor", "2026-01-15T07:00:00Z", "app_installed", "installed_scan", "inv.scan", nil)
	found(b, 62, "app:cursor", "2026-01-15T02:00:00Z", "app_installed", "installed_scan", "inv.scan", nil)
	found(a, 63, "app:ollama", "2026-01-15T03:00:00Z", "local_model", "model_store", "inv.scan",
		map[string]any{"model_names": []string{"llama3", "mistral"}})
	found(b, 64, "app:ollama", "2026-01-15T04:00:00Z", "local_model", "model_store", "inv.scan",
		map[string]any{"model_names": []string{"llama3"}})
	found(b, 65, "exe:0123456789abcdef", "2026-01-15T05:00:00Z", "inference_connection", "flow_metadata", "net.flow",
		map[string]any{"destination_host": "api.example.test"})
	a.record(t, tx, "2026-01-15T06:00:00Z", a.prompt(66, "userA", "app:cursor", "2026-01-15T06:00:00Z",
		[]label{{Class: "source_code", Score: 0.5}}, "logged", "high", 10))

	rows := `SELECT string_agg(tool_fingerprint||'|'||discovery_type||'|'||item||'|'||devices||'|'||to_char(last_seen AT TIME ZONE 'UTC', 'HH24:MI'), ',' ORDER BY tool_fingerprint, discovery_type, item)
	           FROM mart.agg_discovery_period WHERE tenant_id = $1 AND bucket_size = 'day' AND bucket_start = $2::timestamptz`
	want := "app:cursor|app_installed||2|07:00,app:ollama|local_model|llama3|2|04:00,app:ollama|local_model|mistral|1|03:00," +
		"exe:0123456789abcdef|inference_connection|api.example.test|1|05:00"

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "discovery rows", rows, want, tenant, bucket)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "discovery rows after re-run", rows, want, tenant, bucket)
	runBucket(t, tx, tenant, BucketHour, "2026-01-15T03:00:00Z", "2026-01-15T05:00:00Z")
	expect(t, tx, "hour buckets",
		`SELECT string_agg(to_char(bucket_start AT TIME ZONE 'UTC', 'HH24')||'|'||item||'|'||devices, ',' ORDER BY bucket_start, item)
		   FROM mart.agg_discovery_period WHERE tenant_id = $1 AND bucket_size = 'hour'`,
		"03|llama3|1,03|mistral|1,04|llama3|1", tenant)

	// A finding that expires takes its row with it.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `SET LOCAL sac.retention_delete = on`)
	pgtest.Exec(t, tx, `DELETE FROM ingest.observation WHERE tenant_id = $1 AND tool_fingerprint LIKE 'exe:%'`, tenant)
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "rows after the connection is erased",
		`SELECT count(*)::text FROM mart.agg_discovery_period WHERE tenant_id = $1 AND bucket_size = 'day'`, "3", tenant)
}

func TestRollupAgentActivity(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	const from, to = "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z"
	const bucket = "2026-01-15T00:00:00Z"
	activity := func(n int, user, at, kind string, extra map[string]any) {
		m := ev.base(n, user, "app:claude_code", at)
		for k, v := range map[string]any{"direction": "none", "kind": "agent_activity", "source": "tool.otel",
			"collection_mode": "m0", "activity_type": kind} {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		ev.record(t, tx, at, m)
	}
	request := func(model string, in, out int) map[string]any {
		return map[string]any{"model": model, "input_tokens": in, "output_tokens": out, "duration_ms": 900}
	}

	// Two people's model requests on two models, and each one's tool call, which names no model; a
	// third person's request on one of them; a prompt to the same tool is not agent activity.
	activity(70, "userA", "2026-01-15T01:00:00Z", "model_request", request("claude-sonnet-4", 1000, 200))
	activity(71, "userA", "2026-01-15T02:00:00Z", "model_request", request("claude-sonnet-4", 500, 100))
	activity(72, "userA", "2026-01-15T02:10:00Z", "tool_call", map[string]any{"tool_name": "Bash", "outcome": "success", "duration_ms": 40})
	activity(73, "userB", "2026-01-15T03:00:00Z", "model_request", request("claude-sonnet-4", 300, 50))
	activity(74, "userB", "2026-01-15T03:05:00Z", "model_request", request("claude-haiku", 40, 10))
	activity(75, "userB", "2026-01-15T03:10:00Z", "tool_call", map[string]any{"tool_name": "Edit", "outcome": "denied", "duration_ms": 5})
	activity(77, "userC", "2026-01-15T05:00:00Z", "model_request", request("claude-haiku", 60, 20))
	ev.record(t, tx, "2026-01-15T04:00:00Z", ev.prompt(76, "userA", "app:claude_code", "2026-01-15T04:00:00Z",
		[]label{{Class: "source_code", Score: 0.5}}, "logged", "high", 10))

	rows := `SELECT string_agg(tool_fingerprint||'|'||model||'|'||requests||'|'||actions||'|'||input_tokens||'|'||output_tokens||'|'||users||'|'||tool_users, ',' ORDER BY tool_fingerprint, model)
	           FROM mart.agg_agent_period WHERE tenant_id = $1 AND bucket_size = 'day' AND bucket_start = $2::timestamptz`
	want := "app:claude_code||0|2|0|0|2|3,app:claude_code|claude-haiku|2|0|100|30|2|3,app:claude_code|claude-sonnet-4|3|0|1800|350|2|3"

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "agent rows", rows, want, tenant, bucket)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "agent rows after re-run", rows, want, tenant, bucket)
	runBucket(t, tx, tenant, BucketHour, "2026-01-15T02:00:00Z", "2026-01-15T04:00:00Z")
	expect(t, tx, "hour buckets",
		`SELECT string_agg(to_char(bucket_start AT TIME ZONE 'UTC', 'HH24')||'|'||model||'|'||requests||'|'||actions, ',' ORDER BY bucket_start, model)
		   FROM mart.agg_agent_period WHERE tenant_id = $1 AND bucket_size = 'hour'`,
		"02||0|1,02|claude-sonnet-4|1|0,03||0|1,03|claude-haiku|1|0,03|claude-sonnet-4|1|0", tenant)

	// A person's erased activity leaves the counts without them.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `SET LOCAL sac.retention_delete = on`)
	pgtest.Exec(t, tx, `DELETE FROM ingest.observation WHERE tenant_id = $1 AND user_ref = 'userB'`, tenant)
	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, from, to)
	expect(t, tx, "agent rows after one person is erased", rows,
		"app:claude_code||0|1|0|0|1|2,app:claude_code|claude-haiku|1|0|60|20|1|2,app:claude_code|claude-sonnet-4|2|0|1500|300|1|2", tenant, bucket)
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
	runBucket(t, tx, tenant, BucketHour, "2026-01-15T08:00:00Z", "2026-01-15T12:00:00Z")
	expect(t, tx, "hour buckets",
		`SELECT count(*)::text FROM mart.agg_tool_period WHERE tenant_id = $1 AND tool_fingerprint = 'toolA'`, "2", tenant)
	for _, hour := range []string{"2026-01-15T10:00:00Z", "2026-01-15T11:00:00Z"} {
		expect(t, tx, "bucket "+hour,
			`SELECT submissions::text FROM mart.agg_tool_period
			  WHERE tenant_id = $1 AND tool_fingerprint = 'toolA' AND bucket_start = $2::timestamptz`,
			"1", tenant, hour)
	}
}

// TestRollupToolsUsedByTool checks that a person's tools_used counts a tool once however many
// fingerprints it was seen under: ChatGPT (web) under an extension host hash, its own fingerprint and
// a host under its inference domain, beside a host no app claims.
func TestRollupToolsUsedByTool(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	tools := []string{
		"tf2:a64fvqypthxcqzcwgpxxeeo4kvhk5onujlq22qafww3ql422pxyq", "app:chatgpt_web",
		"host:new.chatgpt.com", "host:chat.example.org",
	}
	for i, tool := range tools {
		occurred := fmt.Sprintf("2026-01-15T1%d:00:00Z", i)
		ev.record(t, tx, occurred, ev.prompt(20+i, "userA", tool, occurred,
			[]label{{Class: "customer_pii", Score: 0.4}}, "logged", "high", 10))
	}

	pgtest.AsJobs(t, tx)
	runBucket(t, tx, tenant, BucketDay, "2026-01-15T00:00:00Z", "2026-01-16T00:00:00Z")
	expect(t, tx, "tools used",
		`SELECT submissions||'|'||tools_used FROM mart.agg_user_period
		  WHERE tenant_id = $1 AND user_ref = 'userA' AND bucket_start = $2::timestamptz`,
		"4|2", tenant, "2026-01-15T00:00:00Z")
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
// that only a healthy or degraded report counts as observed, that a collector the policy switched
// off is not expected, and that a day already observed is not erased when the collector's current
// state moves on.
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
	if len(codes) < 5 {
		t.Fatalf("ref.collector has %d rows; the test needs five", len(codes))
	}
	healthy, tampered, absent, silent, disabled := codes[0], codes[1], codes[2], codes[3], codes[4]
	for collector, state := range map[string]string{healthy: "healthy", tampered: "tampered", absent: "absent"} {
		pgtest.Exec(t, tx, `INSERT INTO ops.collector_state (tenant_id, device_id, collector, state, last_report_at)
		                    VALUES ($1, $2, $3, $4, $5)`, tenant, device, collector, state, now)
	}
	pgtest.Exec(t, tx, `INSERT INTO ops.collector_state (tenant_id, device_id, collector, state, error_code, last_report_at)
	                    VALUES ($1, $2, $3, 'absent', 'disabled_by_policy', $4)`, tenant, device, disabled, now)

	pgtest.AsJobs(t, tx)
	pgtest.Exec(t, tx, CoverageSnapshotSQL, tenant, window.From, window.To)
	count := func(filter string) string {
		return pgtest.Scalar(t, tx, `SELECT count(*) FILTER (WHERE `+filter+`)::text FROM ops.coverage_snapshot WHERE tenant_id = $1`, tenant)
	}
	gap := func(collector string) string {
		return pgtest.Scalar(t, tx, `SELECT gap_reason FROM ops.coverage_snapshot WHERE tenant_id = $1 AND collector = $2`, tenant, collector)
	}
	expected := func(collector string) string {
		return pgtest.Scalar(t, tx, `SELECT expected::text FROM ops.coverage_snapshot WHERE tenant_id = $1 AND collector = $2`, tenant, collector)
	}
	if got := count("true"); got != collectors {
		t.Errorf("coverage rows = %s, want one per collector (%s)", got, collectors)
	}
	if got, want := count("expected"), fmt.Sprint(len(codes)-1); got != want {
		t.Errorf("expected rows = %s, want %s (every collector but the one switched off)", got, want)
	}
	if got := expected(disabled); got != "false" {
		t.Errorf("expected(%s) = %s, want false for a disabled_by_policy row", disabled, got)
	}
	for _, collector := range []string{healthy, tampered, absent, silent} {
		if got := expected(collector); got != "true" {
			t.Errorf("expected(%s) = %s, want true", collector, got)
		}
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

	// The policy switches the collector back on and it reports later the same day: its latest
	// report decides, so the day is expected and observed.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `UPDATE ops.collector_state SET state = 'healthy', error_code = NULL, last_report_at = $3
	                     WHERE tenant_id = $1 AND collector = $2`, tenant, disabled, now.Add(2*time.Second))
	pgtest.AsJobs(t, tx)
	pgtest.Exec(t, tx, CoverageSnapshotSQL, tenant, window.From, window.To)
	if got := expected(disabled); got != "true" {
		t.Errorf("expected(%s) after it was switched on = %s, want true", disabled, got)
	}
	if got := count("observed"); got != "2" {
		t.Errorf("observed rows after the collector was switched on = %s, want 2", got)
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
		fmt.Sprint(len(BucketSizes)*9), tenant)
	expect(t, tx, "prompts in today's day bucket",
		`SELECT sum(submissions)::text FROM mart.agg_tool_period
		  WHERE tenant_id = $1 AND bucket_size = 'day' AND tool_fingerprint = 'toolA'`, "1", tenant)
	wantCoverage := pgtest.Scalar(t, tx, `SELECT (count(*) * $1)::text FROM ref.collector`, CoverageDayLookback)
	if got := fmt.Sprint(written[CoverageSnapshotName]); got != wantCoverage {
		t.Errorf("coverage rows written = %s, want %s (collectors x %d days)", got, wantCoverage, CoverageDayLookback)
	}
	if written["mart.agg_tool_period day"] != 1 {
		t.Errorf("day buckets written for one tool = %d, want 1: only the bucket with data", written["mart.agg_tool_period day"])
	}
}

// TestTenantPassCountsAPersonOnce: a person whose device reported under its object-id ref before
// the directory named them, and under their canonical ref since, is one person after a pass: the
// earlier submissions and the device's last user move to the canonical ref, and every bucket they
// fall in is recomputed, older than the trailing windows too.
func TestTenantPassCountsAPersonOnce(t *testing.T) {
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	ev := events{tenant: tenant, device: device}
	kyle, kyleOID := "u_"+strings.Repeat("a", 32), "u_"+strings.Repeat("b", 32)
	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	pgtest.Exec(t, tx, `INSERT INTO ops.user_dim (tenant_id, user_ref, department, display_name, status)
	  VALUES ($1, $2, 'Engineering', 'Kyle Clark', 'active')`, tenant, kyle)
	team := pgtest.Scalar(t, tx, `INSERT INTO ops.team (tenant_id, name, created_by) VALUES ($1, 'Engineering', 'test') RETURNING team_id::text`, tenant)
	pgtest.Exec(t, tx, `INSERT INTO ops.team_include (tenant_id, team_id, kind, match_value, added_by)
	  VALUES ($1, $2::uuid, 'department', 'Engineering', 'test')`, tenant, team)
	ev.record(t, tx, stamp(time.Hour), ev.prompt(61, kyle, "toolA", stamp(time.Hour), nil, "logged", "high", 10))
	ev.record(t, tx, stamp(10*24*time.Hour), ev.prompt(62, kyleOID, "toolA", stamp(10*24*time.Hour), nil, "logged", "high", 20))
	ev.record(t, tx, stamp(10*24*time.Hour-time.Minute), ev.prompt(63, kyleOID, "toolA", stamp(10*24*time.Hour-time.Minute), nil, "blocked", "high", 30))
	expect(t, tx, "the device's last user before the pass", `SELECT last_user_ref FROM ops.device WHERE tenant_id = $1`, kyleOID, tenant)

	// Passes before the directory names the object id, back when its prompts were recent, count
	// two people.
	pgtest.AsJobs(t, tx)
	since, until := now.AddDate(0, 0, -11).Truncate(24*time.Hour).Format(time.RFC3339), now.AddDate(0, 0, 1).Truncate(24*time.Hour).Format(time.RFC3339)
	for _, size := range BucketSizes {
		runBucket(t, tx, tenant, size, since, until)
	}
	expect(t, tx, "people before the alias",
		`SELECT count(DISTINCT user_ref)::text FROM mart.agg_user_period WHERE tenant_id = $1`, "2", tenant)

	// The directory writes the person's aliases, as SCIM does: their UPN-ref and their object id's.
	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `INSERT INTO ops.user_ref_alias (tenant_id, alias_ref, user_ref) VALUES ($1, $2, $2), ($1, $3, $2)`, tenant, kyle, kyleOID)
	pgtest.AsJobs(t, tx)
	if _, err := Tenant(context.Background(), tx, tenant, now); err != nil {
		t.Fatal(err)
	}

	expect(t, tx, "submissions under the object-id ref",
		`SELECT count(*)::text FROM ingest.submission WHERE tenant_id = $1 AND user_ref = $2`, "0", tenant, kyleOID)
	expect(t, tx, "the device's last user", `SELECT last_user_ref FROM ops.device WHERE tenant_id = $1`, kyle, tenant)
	for _, size := range BucketSizes {
		expect(t, tx, "people per "+size,
			`SELECT string_agg(DISTINCT user_ref, ',') FROM mart.agg_user_period WHERE tenant_id = $1 AND bucket_size = $2`,
			kyle, tenant, size)
		expect(t, tx, "tool-user rows per "+size,
			`SELECT string_agg(DISTINCT user_ref, ',') FROM mart.agg_tool_user_period WHERE tenant_id = $1 AND bucket_size = $2`,
			kyle, tenant, size)
		expect(t, tx, "prompts per "+size,
			`SELECT sum(submissions)::text||'|'||sum(block_events)::text FROM mart.agg_user_period WHERE tenant_id = $1 AND bucket_size = $2`,
			"3|1", tenant, size)
		expect(t, tx, "the team's prompts per "+size,
			`SELECT sum(submissions)::text||'|'||max(users)::text FROM mart.agg_team_period WHERE tenant_id = $1 AND bucket_size = $2`,
			"3|1", tenant, size)
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
		"direction": "none", "kind": "discovery", "source": "proc.detect",
		"collection_mode": "m0", "discovery_type": "app_running", "detection_basis": "process_event",
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
