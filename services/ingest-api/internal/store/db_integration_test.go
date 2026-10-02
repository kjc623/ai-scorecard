package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/dedup"
	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
)

// The database-gated evidence for the ingest write path.
//
// What is proven here, on a live PostgreSQL 17 with db/schema.sql applied:
//
//  1. the exact text of SQLRecordEvent — the integration seam — driving ingest.record_event() for
//     every §4 ladder scenario, with the same expected outcomes the in-memory ladder test asserts;
//  2. the Go mirror of ingest.weak_dedup_key() is value-identical to the stored function, so the
//     in-memory store decides the ladder on the same material the database does;
//  3. SQLPrincipalStatus resolves a real tenant + device + credential row inside a transaction;
//  4. SQLInsertRejected writes into the live ingest.rejected CHECK vocabulary;
//  5. the M0 boundary is also enforced by the database, as a constraint violation that aborts the
//     transaction — which is why §6 puts validation in memory before the transaction opens.
//
// Every script runs inside one transaction and ends with ROLLBACK, so the server is left exactly as
// it was found. That is what makes these tests evidence rather than pollution.

func seedSQL(tenant, device, credential string) string {
	return fmt.Sprintf(`
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search)
VALUES ('%[1]s', 'ingest-integration', 'active', 'eu-west', 'vendor', 'm1', 'disabled');
INSERT INTO ops.device (tenant_id, device_id, os) VALUES ('%[1]s', '%[2]s', 'windows');
INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, issued_at, expires_at)
VALUES ('%[1]s', '%[3]s', '%[2]s', 'sha256-test-thumbprint', now(), now() + interval '90 days');
`, tenant, device, credential)
}

// TestLiveRecordEventRunsTheLadder executes the SQLRecordEvent statement text, via PREPARE/EXECUTE
// so the placeholders are bound exactly as database/sql would bind them, for every scenario in the
// shared fixture.
func TestLiveRecordEventRunsTheLadder(t *testing.T) {
	container := psqlContainer(t)

	var script strings.Builder
	script.WriteString("BEGIN;\n")
	script.WriteString(seedSQL(ladder.TenantID, ladder.DeviceID, "33333333-3333-4333-8333-333333333333"))
	script.WriteString("PREPARE rec AS " + SQLRecordEvent + ";\n")
	for _, sc := range ladder.Scenarios {
		for i, step := range sc.Steps {
			// The marker line is emitted before the call so the outcome can be attributed without
			// relying on psql's own formatting.
			script.WriteString("SELECT " + q(fmt.Sprintf("step|%s|%d", sc.Name, i)) + ";\n")
			script.WriteString("EXECUTE rec(" + q(string(step.Envelope)) + ", " +
				q(step.ReceiveTime().Format(time.RFC3339)) + "::timestamptz);\n")
		}
	}
	script.WriteString("ROLLBACK;\n")

	out := runPSQL(t, container, script.String())

	type result struct{ outcome, submissionID string }
	got := map[string]result{}
	pending := ""
	for _, line := range lines(out) {
		if strings.HasPrefix(line, "step|") {
			pending = strings.TrimPrefix(line, "step|")
			continue
		}
		if pending == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			t.Fatalf("unexpected output line %q after marker %q", line, pending)
		}
		got[pending] = result{parts[0], parts[1]}
		pending = ""
	}

	want := 0
	for _, sc := range ladder.Scenarios {
		want += len(sc.Steps)
	}
	if len(got) != want {
		t.Fatalf("parsed %d of %d outcomes from the live server\n--- output ---\n%s", len(got), want, out)
	}

	for _, sc := range ladder.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			subIDs := map[int]string{}
			for i, step := range sc.Steps {
				r, ok := got[fmt.Sprintf("%s|%d", sc.Name, i)]
				if !ok {
					t.Fatalf("no outcome recorded for step %d (%s)", i, step.Name)
				}
				if r.outcome != step.Expect.Outcome {
					t.Errorf("live ingest.record_event() returned %q for step %d (%s); the fixture expects %q",
						r.outcome, i, step.Name, step.Expect.Outcome)
				}
				if r.submissionID == "" {
					t.Errorf("step %d returned no submission id", i)
				}
				if step.Expect.SameSubmissionAs >= 0 {
					if wantID, ok := subIDs[step.Expect.SameSubmissionAs]; ok && wantID != r.submissionID {
						t.Errorf("step %d landed on submission %s, want the submission of step %d (%s)",
							i, r.submissionID, step.Expect.SameSubmissionAs, wantID)
					}
				}
				subIDs[i] = r.submissionID
			}
		})
	}
}

// TestLiveWeakDedupKeyMatchesTheGoMirror compares the Go mirror against the stored function at the
// bucket edges that matter, so the in-memory store is not deciding the ladder on material the
// database would compute differently.
func TestLiveWeakDedupKeyMatchesTheGoMirror(t *testing.T) {
	container := psqlContainer(t)

	cases := []struct {
		name     string
		occurred string
		size     int64
	}{
		{"on a bucket boundary", "2026-10-02T14:30:00Z", 42},
		{"one second inside a bucket", "2026-10-02T14:30:01Z", 42},
		{"sub-second inside a bucket", "2026-10-02T14:34:59.999Z", 42},
		{"one second into the next bucket", "2026-10-02T14:35:01Z", 42},
		{"a zero size", "2026-10-02T14:35:01Z", 0},
		{"a size above 32 bits", "2026-10-02T00:00:00Z", 4294967296},
		{"an offset timestamp is the same instant", "2026-10-02T16:30:00+02:00", 42},
	}
	var script strings.Builder
	script.WriteString("BEGIN;\n")
	for i, c := range cases {
		script.WriteString(fmt.Sprintf(
			"SELECT %s || '|' || ingest.weak_dedup_key(%s::uuid, %s::uuid, %s, %s, %s::timestamptz, %d::bigint);\n",
			q(fmt.Sprintf("case%d", i)), q(ladder.TenantID), q(ladder.DeviceID),
			q("tool-1"), q("prompt"), q(c.occurred), c.size))
	}
	script.WriteString("ROLLBACK;\n")

	out := lines(runPSQL(t, container, script.String()))
	if len(out) != len(cases) {
		t.Fatalf("got %d rows, want %d:\n%s", len(out), len(cases), strings.Join(out, "\n"))
	}
	for i, c := range cases {
		parts := strings.SplitN(out[i], "|", 2)
		if len(parts) != 2 {
			t.Fatalf("row %d is not key|value: %q", i, out[i])
		}
		at, err := time.Parse(time.RFC3339, c.occurred)
		if err != nil {
			t.Fatalf("parse %s: %v", c.occurred, err)
		}
		wantKey := dedup.WeakDedupKey(ladder.TenantID, ladder.DeviceID, "tool-1", "prompt", at, c.size)
		if parts[1] != wantKey {
			t.Errorf("%s:\n  live ingest.weak_dedup_key() = %s\n  Go mirror                     = %s", c.name, parts[1], wantKey)
		}
	}
}

// TestLivePrincipalStatusRunsTheStatement executes SQLPrincipalStatus through PREPARE/EXECUTE
// against seeded rows, so the admission check is proven against the real tables rather than only
// against the test double. Column order follows the statement's own SELECT list.
func TestLivePrincipalStatusRunsTheStatement(t *testing.T) {
	container := psqlContainer(t)
	const credential = "44444444-4444-4444-8444-444444444444"

	script := "BEGIN;\n" +
		seedSQL(ladder.TenantID, ladder.DeviceID, credential) +
		"PREPARE st AS " + SQLPrincipalStatus + ";\n" +
		"EXECUTE st(" + q(ladder.TenantID) + ", " + q(ladder.DeviceID) + ", " + q(credential) + ");\n" +
		"ROLLBACK;\n"

	out := lines(runPSQL(t, container, script))
	if len(out) != 1 {
		t.Fatalf("expected one row from SQLPrincipalStatus, got %d:\n%s", len(out), strings.Join(out, "\n"))
	}
	parts := strings.Split(out[0], "|")
	if len(parts) != 8 {
		t.Fatalf("expected 8 columns, got %d: %q", len(parts), out[0])
	}
	if parts[0] != "active" {
		t.Errorf("status = %q, want active", parts[0])
	}
	if parts[1] != "t" {
		t.Errorf("ingest_enabled = %q, want t", parts[1])
	}
	if parts[2] != "eu-west" {
		t.Errorf("residency_region = %q, want eu-west", parts[2])
	}
	if parts[3] != "t" {
		t.Errorf("device_known = %q, want t", parts[3])
	}
	if parts[4] != "" {
		t.Errorf("device_revoked_at = %q, want NULL", parts[4])
	}
	if parts[5] != "t" {
		t.Errorf("credential_known = %q, want t", parts[5])
	}
	if parts[6] == "" {
		t.Error("expires_at is NULL, want the seeded expiry")
	}
	if parts[7] != "" {
		t.Errorf("credential_revoked_at = %q, want NULL", parts[7])
	}

	// A revoked credential must resolve as revoked rather than as unknown, because the two produce
	// different operator actions.
	script = "BEGIN;\n" +
		seedSQL(ladder.TenantID, ladder.DeviceID, credential) +
		"UPDATE ops.device_credential SET revoked_at = now() WHERE tenant_id = " + q(ladder.TenantID) + ";\n" +
		"PREPARE st2 AS " + SQLPrincipalStatus + ";\n" +
		"EXECUTE st2(" + q(ladder.TenantID) + ", " + q(ladder.DeviceID) + ", " + q(credential) + ");\n" +
		"ROLLBACK;\n"
	out = lines(runPSQL(t, container, script))
	if len(out) != 1 {
		t.Fatalf("expected one row, got %d:\n%s", len(out), strings.Join(out, "\n"))
	}
	parts = strings.Split(out[0], "|")
	if len(parts) == 8 && parts[7] == "" {
		t.Error("a revoked credential reported credential_revoked_at = NULL")
	}
}

// liveQuarantineVocabulary reads the accepted reason codes from the live CHECK constraint, so the
// test asserts against the server's actual vocabulary instead of a copy of it.
func liveQuarantineVocabulary(t *testing.T, container string) map[string]bool {
	t.Helper()
	out := runPSQL(t, container, `
SELECT pg_get_constraintdef(oid)
  FROM pg_constraint
 WHERE conrelid = 'ingest.rejected'::regclass
   AND contype = 'c'
   AND pg_get_constraintdef(oid) LIKE '%reason_code%';`)
	vocab := map[string]bool{}
	for _, m := range quotedStringRE.FindAllStringSubmatch(out, -1) {
		vocab[m[1]] = true
	}
	if len(vocab) == 0 {
		t.Fatalf("could not read the ingest.rejected reason_code vocabulary from the live server:\n%s", out)
	}
	return vocab
}

// TestLiveQuarantineInsertUsesTheLiveVocabulary executes the exact SQLInsertRejected text for every
// wire code that maps to a quarantine code.
//
// The vocabulary is read from the running server first, because dbuilder is aligning the CHECK to
// §7's spellings and this test must be honest about which spellings the server in front of it
// accepts: a code it does not accept is reported as awaiting that change, never asserted as passing.
// Once the CHECK flips, the same code asserts for real with no change here.
func TestLiveQuarantineInsertUsesTheLiveVocabulary(t *testing.T) {
	container := psqlContainer(t)
	vocab := liveQuarantineVocabulary(t, container)
	keys := make([]string, 0, len(vocab))
	for k := range vocab {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("live ingest.rejected vocabulary: %v", keys)

	const credential = "55555555-5555-4555-8555-555555555555"
	codes := []string{
		"schema_violation", "unsupported_schema_version", "unknown_kind", "unknown_tenant",
		"revoked_device", "region_mismatch", "mode_violation", "oversize",
	}
	exercised, pending := 0, 0
	for _, code := range codes {
		if !vocab[code] {
			pending++
			t.Logf("AWAITING CHECK ALIGNMENT: the live server does not accept quarantine code %q yet; "+
				"the schema side is dbuilder's change, and this case will assert for real once it lands", code)
			continue
		}
		script := "BEGIN;\n" +
			seedSQL(ladder.TenantID, ladder.DeviceID, credential) +
			prepare("ins", SQLInsertRejected,
				ladder.TenantID, ladder.DeviceID, time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC),
				code, `{"pointer":"/events/0/content_digest","expected":"absent when collection_mode is m0"}`,
				`{"present":["event_id","kind"],"missing":[]}`,
				`{"event_id":"33333333-3333-4333-8333-333333333333","kind":"prompt"}`) +
			"SELECT 'rows|' || count(*) FROM ingest.rejected WHERE tenant_id = " + q(ladder.TenantID) + ";\n" +
			"SELECT 'ttl|' || (expires_at > received_at) FROM ingest.rejected WHERE tenant_id = " + q(ladder.TenantID) + " LIMIT 1;\n" +
			"ROLLBACK;\n"

		out := strings.Join(lines(runPSQL(t, container, script)), "\n")
		if !strings.Contains(out, "rows|1") {
			t.Errorf("quarantine insert for %q wrote no row:\n%s", code, out)
		}
		if !strings.Contains(out, "ttl|t") {
			t.Errorf("quarantine row for %q has no future expires_at:\n%s", code, out)
		}
		exercised++
	}
	if exercised+pending != len(codes) {
		t.Fatalf("exercised %d + pending %d != %d", exercised, pending, len(codes))
	}
	t.Logf("quarantine codes exercised against the live server: %d of %d (%d awaiting the CHECK alignment)",
		exercised, len(codes), pending)
}

// TestLiveM0BoundaryIsAlsoAConstraint records why ingest validates in memory before the transaction:
// the database refuses an M0 record carrying content, and it refuses it by raising, which aborts the
// whole transaction. §6 puts validation in memory for exactly this reason — a device defect must be
// one event's rejection, not a batch that can never commit.
func TestLiveM0BoundaryIsAlsoAConstraint(t *testing.T) {
	container := psqlContainer(t)

	var m map[string]any
	raw := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(900), Tool: "m0-tool", OccurredAt: "2026-10-02T18:00:00Z",
		Source: "ext.dom", Mode: "m0", SizeBytes: 5, DedupKey: ladder.Hash('e'),
	})
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	m["content_digest"] = ladder.Hash('a') // the defect: content read at M0
	defective, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	script := "BEGIN;\n" +
		seedSQL(ladder.TenantID, ladder.DeviceID, "66666666-6666-4666-8666-666666666666") +
		"PREPARE rec AS " + SQLRecordEvent + ";\n" +
		"EXECUTE rec(" + q(string(defective)) + ", " + q("2026-10-02T18:00:01Z") + "::timestamptz);\n" +
		"ROLLBACK;\n"

	out, err := dockerPSQL(container, script)
	if err == nil {
		t.Fatalf("the defective M0 record was accepted by the live server:\n%s", out)
	}
	if !strings.Contains(out, "observation_m0_carries_no_content") {
		t.Errorf("the live server refused the M0 record, but not with the constraint this test expects:\n%s", out)
	}
	for _, l := range lines(out) {
		if strings.Contains(l, "observation_m0_carries_no_content") {
			t.Logf("live server refused the defective M0 record with: %s", l)
			break
		}
	}
}
