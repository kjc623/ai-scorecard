package erase

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/shadow-ai-capture/jobs/internal/pgtest"
)

// TestEraseAgainstPostgreSQL records events, stored content, a finding and search text for two
// subjects, requests erasure of one, runs the job as sac_ops, and checks that exactly the named
// subject's data went, that one subject receipt states it, and that the request is marked complete.
func TestEraseAgainstPostgreSQL(t *testing.T) {
	ctx := context.Background()
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	f := fixture{t: t, tx: tx, tenant: tenant, device: device}

	subjectA := "u_subject_a"
	subjectB := "u_subject_b"
	received := time.Now().UTC()

	a := f.prompt(subjectA, 1, received)
	f.prompt(subjectA, 2, received)
	b := f.prompt(subjectB, 3, received)

	// A finding on subject A's first submission, and search text on both.
	rule := pgtest.Scalar(t, tx, `SELECT rule_id FROM ref.rule ORDER BY rule_id LIMIT 1`)
	pgtest.Exec(t, tx, `INSERT INTO mart.finding (tenant_id, submission_id, rule_id, detected_at, decided_locally, collection_mode)
	                    VALUES ($1, $2, $3, $4, false, 'm1')`, tenant, a, rule, received)
	f.searchText(a, received)
	f.searchText(b, received)
	// Stored content for subject A.
	f.content(a, received)

	pgtest.AsOwner(t, tx)
	pgtest.Exec(t, tx, `INSERT INTO ops.erasure_request (tenant_id, request_id, subject_ref, requested_by, requested_at)
	                    VALUES ($1, gen_random_uuid(), $2, 'admin@example.com', now())`, tenant, subjectA)
	f.expect("pending request", `SELECT count(*)::text FROM ops.erasure_request WHERE tenant_id = $1 AND completed_at IS NULL`, "1")

	pgtest.AsJobs(t, tx)
	removed, err := Tenant(ctx, tx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"ingest.search_text": 1, "ops.content": 1, "ingest.observation": 2,
		"ingest.submission": 2, "mart.finding": 1,
	}
	for name, n := range want {
		if removed[name] != n {
			t.Errorf("removed[%s] = %d, want %d", name, removed[name], n)
		}
	}

	pgtest.AsOwner(t, tx)
	// Subject A is gone; subject B is intact.
	f.expect("subject A observations", `SELECT count(*)::text FROM ingest.observation WHERE tenant_id = $1 AND user_ref = $2`, "0", subjectA)
	f.expect("subject A submissions", `SELECT count(*)::text FROM ingest.submission WHERE tenant_id = $1 AND user_ref = $2`, "0", subjectA)
	f.expect("subject B observations", `SELECT count(*)::text FROM ingest.observation WHERE tenant_id = $1 AND user_ref = $2`, "1", subjectB)
	f.expect("subject B submissions", `SELECT count(*)::text FROM ingest.submission WHERE tenant_id = $1 AND user_ref = $2`, "1", subjectB)
	f.expect("subject A findings gone", `SELECT count(*)::text FROM mart.finding WHERE tenant_id = $1 AND submission_id = $2`, "0", a)
	f.expect("subject B search text intact", `SELECT count(*)::text FROM ingest.search_text WHERE tenant_id = $1 AND submission_id = $2`, "1", b)

	f.expect("receipts", `SELECT count(*)::text FROM ops.erasure_receipt WHERE tenant_id = $1`, "1")
	f.expect("receipt scope", `SELECT scope_kind||'|'||subject_ref||'|'||requested_by||'|'||array_to_string(mechanisms, ',')
	                              FROM ops.erasure_receipt WHERE tenant_id = $1`, "subject|"+subjectA+"|admin@example.com|database")
	var counts map[string]int64
	raw := pgtest.Scalar(t, tx, `SELECT removed_counts::text FROM ops.erasure_receipt WHERE tenant_id = $1`, tenant)
	if err := json.Unmarshal([]byte(raw), &counts); err != nil {
		t.Fatalf("removed_counts %q: %v", raw, err)
	}
	for name, n := range want {
		if counts[name] != n {
			t.Errorf("receipt removed_counts[%s] = %d, want %d", name, counts[name], n)
		}
	}
	f.expect("request completed", `SELECT count(*)::text FROM ops.erasure_request WHERE tenant_id = $1 AND completed_at IS NOT NULL`, "1")

	// A second pass finds no pending request and removes nothing.
	pgtest.AsJobs(t, tx)
	again, err := Tenant(ctx, tx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range again {
		if n != 0 {
			t.Errorf("second pass removed %d from %s", n, name)
		}
	}
}

type fixture struct {
	t              *testing.T
	tx             *sql.Tx
	tenant, device string
}

func (f fixture) expect(what, query, want string, args ...any) {
	f.t.Helper()
	if got := pgtest.Scalar(f.t, f.tx, query, append([]any{f.tenant}, args...)...); got != want {
		f.t.Errorf("%s = %q, want %q", what, got, want)
	}
}

// prompt records one prompt for a subject and returns its submission id.
func (f fixture) prompt(subject string, n int, received time.Time) string {
	f.t.Helper()
	id := fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", n)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(f.tenant+id)))
	envelope, err := json.Marshal(map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": f.tenant, "device_id": f.device,
		"user_ref": subject, "tool_fingerprint": "toolA", "direction": "egress", "kind": "prompt",
		"occurred_at": received.Format(time.RFC3339), "monotonic_offset_ms": n,
		"source": "ext.page_context", "collection_mode": "m1", "size_bytes": 10,
		"content_digest": digest, "dedup_key": digest,
		"labels":             []map[string]any{{"class": "customer_pii", "score": 0.5}},
		"classifier_version": "jobs-test", "confidence": "high",
		"policy_decision": map[string]any{"rule_id": "DEFAULT_LOG", "action": "logged", "decided_locally": true},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return pgtest.Scalar(f.t, f.tx,
		`SELECT event_submission_id::text FROM ingest.record_event($1::jsonb, $2::timestamptz)`,
		string(envelope), received)
}

func (f fixture) searchText(submission string, expires time.Time) {
	f.t.Helper()
	pgtest.Exec(f.t, f.tx, `INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
	                         VALUES ($1, $2, 'prompt_body', 0, 'indexed text', $3)`, f.tenant, submission, expires.Add(30*24*time.Hour))
}

// content stores one sealed object for a submission's event, with the upload grant it needs, and
// marks the submission uploaded.
func (f fixture) content(submission string, at time.Time) {
	f.t.Helper()
	event := pgtest.Scalar(f.t, f.tx,
		`SELECT event_id::text FROM ingest.observation WHERE tenant_id = $1 AND content_digest =
		   (SELECT content_digest FROM ingest.submission WHERE tenant_id = $1 AND submission_id = $2)`,
		f.tenant, submission)
	grant := pgtest.NewUUID(f.t, f.tx)
	pgtest.Exec(f.t, f.tx, `INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id,
	                           requested_at, decided_at, decision, expires_at, used_at)
	                         VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $6::timestamptz, 'granted', $6::timestamptz + interval '1 hour', $6::timestamptz)`,
		f.tenant, grant, event, submission, f.device, at)
	pgtest.Exec(f.t, f.tx, `INSERT INTO ops.content (tenant_id, object_id, event_id, submission_id, grant_id, key_version,
	                           ciphertext, plaintext_size_bytes, raw_digest, retention_class, prompt_kind, created_at, expires_at)
	                         VALUES ($1, gen_random_uuid(), $2, $3, $4, 'v1', decode(repeat('00', 29), 'hex'), 1,
	                                 'sha256:'||repeat('0', 64), 'standard', 'user', $5::timestamptz, $5::timestamptz + interval '30 days')`,
		f.tenant, event, submission, grant, at)
	pgtest.Exec(f.t, f.tx, `UPDATE ingest.submission SET content_state = 'uploaded'
	                         WHERE tenant_id = $1 AND submission_id = $2`, f.tenant, submission)
}
