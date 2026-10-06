package expire

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

// TestExpireAgainstPostgreSQL builds a tenant with expired and live rows in every table the job
// sweeps, runs the job as sac_ops with a batch size small enough to need several batches, and
// checks that exactly the expired rows went, that one receipt records them, and that a second pass
// removes nothing and writes no receipt.
func TestExpireAgainstPostgreSQL(t *testing.T) {
	ctx := context.Background()
	tx := pgtest.Tx(t, pgtest.Open(t))
	tenant, device := pgtest.Tenant(t, tx)
	f := fixture{t: t, tx: tx, tenant: tenant, device: device}

	// Five prompts received long enough ago that their retention has passed, and two received now.
	// Retention is resolved by ingest.record_event from the receive time, as in production.
	longAgo := time.Now().UTC().AddDate(-5, 0, 0)
	var expired []string
	for n := 1; n <= 5; n++ {
		expired = append(expired, f.prompt(n, longAgo))
	}
	live := f.prompt(6, time.Now().UTC())
	shortLived := f.prompt(7, time.Now().UTC())
	f.expect("expired submissions before the pass",
		`SELECT count(*)::text FROM ingest.submission WHERE tenant_id = $1 AND expires_at < now()`, "5")

	past, future := time.Now().Add(-24*time.Hour), time.Now().Add(30*24*time.Hour)
	// Search entries: two on expired submissions, one already expired on the live submission, one live.
	f.searchText(expired[0], "prompt_body", 0, past)
	f.searchText(expired[1], "prompt_body", 0, past)
	f.searchText(live, "attachment_name", 1, past)
	f.searchText(live, "prompt_body", 0, future)
	// Stored content: an expired object on an expired submission, an expired object on a live
	// submission (content retention shorter than the event's), and one live object.
	f.content(expired[2], longAgo, past)
	f.content(shortLived, time.Now().Add(-48*time.Hour), past)
	f.content(live, time.Now(), future)
	// Quarantined envelopes: three expired, one live.
	for _, at := range []time.Time{past, past, past, future} {
		pgtest.Exec(t, tx, `INSERT INTO ingest.rejected (tenant_id, reason_code, expires_at)
		                    VALUES ($1, 'schema_violation', $2)`, tenant, at)
	}
	// A finding on an expired submission must not hold the submission back.
	rule := pgtest.Scalar(t, tx, `SELECT rule_id FROM ref.rule ORDER BY rule_id LIMIT 1`)
	pgtest.Exec(t, tx, `INSERT INTO mart.finding (tenant_id, submission_id, rule_id, detected_at, decided_locally, collection_mode)
	                    VALUES ($1, $2, $3, $4, false, 'm1')`, tenant, expired[3], rule, longAgo)

	pgtest.AsJobs(t, tx)
	removed, err := expireTenant(ctx, tx, tenant, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"ingest.search_text": 3, "ops.content": 2, "ingest.observation": 5,
		"ingest.submission": 5, "ingest.rejected": 3,
	}
	for name, n := range want {
		if removed[name] != n {
			t.Errorf("removed[%s] = %d, want %d", name, removed[name], n)
		}
	}

	pgtest.AsOwner(t, tx)
	for table, remaining := range map[string]string{
		"ingest.search_text": "1", "ops.content": "1", "ingest.observation": "2",
		"ingest.submission": "2", "ingest.rejected": "1", "mart.finding": "0",
	} {
		f.expect(table+" remaining", `SELECT count(*)::text FROM `+table+` WHERE tenant_id = $1`, remaining)
	}
	contentState := `SELECT content_state||'|'||coalesce(shredded_reason, '-') FROM ingest.submission
	                  WHERE tenant_id = $1 AND submission_id = $2`
	f.expect("submission whose content expired", contentState, "shredded|retention_expired", shortLived)
	f.expect("submission whose content is live", contentState, "uploaded|-", live)

	f.expect("receipts", `SELECT count(*)::text FROM ops.erasure_receipt WHERE tenant_id = $1`, "1")
	f.expect("receipt", `SELECT scope_kind||'|'||requested_by||'|'||array_to_string(mechanisms, ',')||'|'||(completed_at >= requested_at)::text
	                       FROM ops.erasure_receipt WHERE tenant_id = $1`, "retention|expire-job|database|true")
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

	// A second pass finds nothing and writes no second receipt.
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
	f.expect("receipts after a second pass", `SELECT count(*)::text FROM ops.erasure_receipt WHERE tenant_id = $1`, "1")
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

// prompt records one prompt received at the given time and returns its submission id.
func (f fixture) prompt(n int, received time.Time) string {
	f.t.Helper()
	id := fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", n)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(f.tenant+id)))
	envelope, err := json.Marshal(map[string]any{
		"schema_version": "1.0", "event_id": id, "tenant_id": f.tenant, "device_id": f.device,
		"user_ref": "userA", "tool_fingerprint": "toolA", "direction": "egress", "kind": "prompt",
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

func (f fixture) searchText(submission, kind string, index int, expires time.Time) {
	f.t.Helper()
	pgtest.Exec(f.t, f.tx, `INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
	                         VALUES ($1, $2, $3, $4, 'indexed text', $5)`, f.tenant, submission, kind, index, expires)
}

// content stores one sealed object for a submission's event, with the upload grant it needs, and
// marks the submission uploaded.
func (f fixture) content(submission string, created, expires time.Time) {
	f.t.Helper()
	event := pgtest.Scalar(f.t, f.tx,
		`SELECT event_id::text FROM ingest.observation WHERE tenant_id = $1 AND content_digest =
		   (SELECT content_digest FROM ingest.submission WHERE tenant_id = $1 AND submission_id = $2)`,
		f.tenant, submission)
	grant := pgtest.NewUUID(f.t, f.tx)
	pgtest.Exec(f.t, f.tx, `INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id,
	                           requested_at, decided_at, decision, expires_at, used_at)
	                         VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $6::timestamptz, 'granted', $6::timestamptz + interval '1 hour', $6::timestamptz)`,
		f.tenant, grant, event, submission, f.device, created)
	// One plaintext byte, sealed: a 12-byte nonce, the byte, and a 16-byte tag.
	pgtest.Exec(f.t, f.tx, `INSERT INTO ops.content (tenant_id, object_id, event_id, submission_id, grant_id, key_version,
	                           ciphertext, plaintext_size_bytes, raw_digest, retention_class, prompt_kind, created_at, expires_at)
	                         VALUES ($1, gen_random_uuid(), $2, $3, $4, 'v1', decode(repeat('00', 29), 'hex'), 1,
	                                 'sha256:'||repeat('0', 64), 'standard', 'user', $5, $6)`,
		f.tenant, event, submission, grant, created, expires)
	pgtest.Exec(f.t, f.tx, `UPDATE ingest.submission SET content_state = 'uploaded'
	                         WHERE tenant_id = $1 AND submission_id = $2`, f.tenant, submission)
}
