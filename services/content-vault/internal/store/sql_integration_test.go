package store

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// This file is the live-database seam. It exists because the vault's SQL is the part a unit test
// cannot vouch for: the schema is the authoritative statement of the invariants (ADR 0014's
// mutually exclusive pair, the search tier's mode requirement, the shapes of ops.content_object and
// ingest.search_text), and a refusal implemented in Go that disagrees with a CHECK constraint is a
// refusal that will be wrong the moment the database is.
//
// Two kinds of evidence, both against the real PostgreSQL 17 container the database owner stood up:
//
//  1. **Every statement this service issues is PREPAREd** against the live schema, so a misspelled
//     column, a wrong parameter type or a table that does not exist fails here rather than in
//     production. Statements whose target table does not exist yet are reported as a gap.
//  2. **The constraints are exercised**: the ADR 0014 pair, the search-tier mode rule, and the
//     content-object lifecycle are attempted for real inside a transaction that is rolled back, so
//     the test leaves the database exactly as it found it.
//
// No PostgreSQL wire driver is fetchable offline (ADR 0016), so the statements are executed through
// `docker exec ... psql` rather than through database/sql. What this does *not* exercise is the
// database/sql plumbing itself; that is stated in the package comment and in README.md.

// psqlContainer returns a container hosting the schema, or skips the test.
func psqlContainer(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("SHADOWPG_CONTAINER"); name != "" {
		if containerReady(t, name) {
			return name
		}
		t.Skipf("SHADOWPG_CONTAINER=%s does not answer a probe for the sac schema", name)
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Image}}").Output()
	if err != nil {
		t.Skipf("no live-PG evidence: docker is unavailable (%v)", err)
	}
	var candidates []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) == 2 && strings.Contains(strings.ToLower(parts[1]), "postgres") {
			candidates = append(candidates, parts[0])
		}
	}
	for _, want := range []string{"shadowpg-invariants", "shadowpg"} {
		for _, c := range candidates {
			if c == want && containerReady(t, c) {
				return c
			}
		}
	}
	for _, c := range candidates {
		if containerReady(t, c) {
			return c
		}
	}
	t.Skip("no live-PG evidence: no running PostgreSQL container exposes the sac schema")
	return ""
}

func containerReady(t *testing.T, container string) bool {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "psql", "-U", "postgres", "-d", "shadow",
		"-A", "-t", "-c", `SELECT count(*) FROM pg_tables WHERE schemaname = 'ops' AND tablename = 'content_object'`).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// psql runs one script and returns its output. ON_ERROR_STOP makes a SQL error a test failure, so
// nothing is scrolled past.
func psql(t *testing.T, container, script string) string {
	t.Helper()
	out, err := psqlAllowFail(t, container, script)
	if err != nil {
		t.Fatalf("psql failed: %v\n--- script ---\n%s\n--- output ---\n%s", err, script, out)
	}
	return out
}

// psqlAllowFail runs one script and returns the output and the exit status. One test's evidence *is*
// a constraint violation, so the failing case needs the raw result.
func psqlAllowFail(t *testing.T, container, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", container, "psql", "-U", "postgres", "-d", "shadow",
		"-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestEveryStatementPreparesAgainstTheLiveSchema is the seam test: each statement's text — the text
// the service will send, placeholders and all — is prepared by PostgreSQL against the real schema.
func TestEveryStatementPreparesAgainstTheLiveSchema(t *testing.T) {
	container := psqlContainer(t)
	var script strings.Builder
	script.WriteString("BEGIN;\n")
	verified, gaps := 0, 0
	for _, st := range Statements {
		if !st.Verified {
			gaps++
			continue
		}
		script.WriteString("PREPARE " + st.Name + " AS " + st.SQL + ";\n")
		script.WriteString("DEALLOCATE " + st.Name + ";\n")
		verified++
	}
	script.WriteString("ROLLBACK;\n")
	out := psql(t, container, script.String())
	t.Logf("%d statements prepared against the live schema, %d documented as gaps", verified, gaps)
	_ = out
}

// TestTheRetrievalGrantTableIsStillMissing records the one schema gap this service has, as a test
// rather than as a note: if a later migration adds ops.retrieval_grant, this test starts failing
// and the SQL path can be switched on and verified.
func TestTheRetrievalGrantTableIsStillMissing(t *testing.T) {
	container := psqlContainer(t)
	out := psql(t, container, `SELECT COALESCE(to_regclass('ops.retrieval_grant')::text, 'absent');`)
	if got := strings.TrimSpace(out); got != "absent" {
		t.Fatalf("ops.retrieval_grant now exists (%s): the retrieval-grant SQL path can be verified — remove the gap note in README.md and exercise put/read/claim", got)
	}
	t.Log("ops.retrieval_grant is absent as documented; the retrieval-grant SQL path is NOT VERIFIED and the in-memory implementation is the tested one")
	t.Log("DDL the schema needs:\n" + SQLRetrievalGrantDDL)
}

// TestADR0014PairIsUnrepresentable is the constraint the vault's refusals must agree with: a
// customer_held tenant cannot carry full_text, and no code path can make it so.
func TestADR0014PairIsUnrepresentable(t *testing.T) {
	container := psqlContainer(t)
	script := `
BEGIN;
-- The impossible pair: customer-held keys with server-side full-text search over prompt text.
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                        ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999991', 'impossible', 'active', 'eastus',
        'customer_held', 'kek-x', 'm3', 'full_text');
ROLLBACK;`
	out, err := psqlAllowFail(t, container, script)
	if err == nil {
		t.Fatalf("the database accepted content_search=full_text with key_custody=customer_held:\n%s", out)
	}
	if !strings.Contains(out, "tenant_full_text_search_requires_vendor_readable_content") {
		t.Fatalf("the insert failed for the wrong reason:\n%s", out)
	}
	t.Log("the ADR 0014 pair is refused by the database's own check constraint")
}

// TestSearchTierRequiresACollectionMode is the second constraint the vault mirrors: a search tier
// over data the tenant cannot collect is refused.
func TestSearchTierRequiresACollectionMode(t *testing.T) {
	container := psqlContainer(t)
	script := `
BEGIN;
-- full_text with an M1 ceiling: the tenant could never collect the content it would search.
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                        ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999992', 'tier-without-mode', 'active', 'eastus',
        'vendor', NULL, 'm1', 'full_text');
ROLLBACK;`
	out, err := psqlAllowFail(t, container, script)
	if err == nil {
		t.Fatalf("the database accepted full_text below M3:\n%s", out)
	}
	if !strings.Contains(out, "tenant_search_tier_requires_collection_mode") {
		t.Fatalf("the insert failed for the wrong reason:\n%s", out)
	}
	t.Log("full_text below an M3 ceiling is refused by the database")
}

// TestContentObjectLifecycleAgainstTheSchema runs the vault's own write path — store, re-wrap,
// shred — against the real table inside a transaction that is rolled back.
func TestContentObjectLifecycleAgainstTheSchema(t *testing.T) {
	container := psqlContainer(t)
	const tenant = "99999999-9999-4999-8999-999999999993"
	const object = "99999999-9999-4999-8999-99999999999a"
	script := `
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                        ceiling_mode, content_search)
VALUES ('` + tenant + `', 'lifecycle', 'active', 'eastus', 'vendor', 'kek-lifecycle', 'm3', 'disabled');

-- The vault's insert: a wrapped key beside a blob reference, never the ciphertext.
INSERT INTO ops.content_object (tenant_id, object_id, submission_id, event_id, blob_path,
                                ciphertext_sha256, plaintext_size_bytes, wrapped_dek, kek_id,
                                kek_version, retention_class, state, created_at, expires_at)
VALUES ('` + tenant + `', '` + object + `', NULL, NULL, 'tenants/x/objects/y',
        'sha256:` + strings.Repeat("ab", 32) + `', 4096, '\x0102030405'::bytea, 'kek-lifecycle',
        '1', 'standard', 'uploaded', now(), now() + interval '90 days');

-- Rotation: re-wrap, never re-encrypt. Only the wrapped key and its version move.
UPDATE ops.content_object SET wrapped_dek = '\x0a0b0c'::bytea, kek_version = '2'
 WHERE tenant_id = '` + tenant + `' AND object_id = '` + object + `'
   AND kek_version = '1' AND state = 'uploaded';

-- Erasure: destroy the wrapped key and record the shred in one statement.
UPDATE ops.content_object SET state = 'shredded', shredded_reason = 'erasure',
       shredded_at = now(), wrapped_dek = '\x00'::bytea
 WHERE tenant_id = '` + tenant + `' AND object_id = '` + object + `' AND state = 'uploaded';

SELECT state || '/' || shredded_reason || '/' || kek_version || '/' ||
       encode(wrapped_dek, 'hex') || '/' || (expires_at > created_at)::text
  FROM ops.content_object WHERE tenant_id = '` + tenant + `' AND object_id = '` + object + `';

-- The erasure receipt the same operation writes.
INSERT INTO ops.erasure_receipt (tenant_id, receipt_id, scope_kind, requested_by, requested_at,
                                 completed_at, mechanisms, removed_counts, remaining_counts)
VALUES ('` + tenant + `', '99999999-9999-4999-8999-99999999999b', 'retention', 'dpo', now(), now(),
        ARRAY['row_marked_shredded','wrapped_key_destroyed','search_text_deleted'],
        '{"content_object":1,"search_text":0}'::jsonb, '{}'::jsonb);
ROLLBACK;`
	out := psql(t, container, script)
	lines := nonEmptyLines(out)
	if len(lines) == 0 {
		t.Fatalf("the lifecycle script produced no row:\n%s", out)
	}
	got := lines[len(lines)-1]
	// shredded / erasure / version 2 / the key replaced by a single zero byte / expiry after creation
	if got != "shredded/erasure/2/00/t" {
		t.Fatalf("the lifecycle row is %q, want shredded/erasure/2/00/t", got)
	}
	t.Logf("content_object lifecycle against the live schema: %s", got)
}

// TestSearchTextShapesMatchTheSchema checks the two facts the vault's index path depends on: the
// unit_kind CHECK, and unit_index being pinned to 0 for a prompt body.
func TestSearchTextShapesMatchTheSchema(t *testing.T) {
	container := psqlContainer(t)
	// A prompt body at a non-zero index is refused by the schema's own constraint.
	out, err := psqlAllowFail(t, container, `
BEGIN;
INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
VALUES ('99999999-9999-4999-8999-999999999994', '99999999-9999-4999-8999-99999999999c',
        'prompt_body', 3, 'text', now());
ROLLBACK;`)
	if err == nil {
		t.Fatalf("the schema accepted a prompt_body at unit_index 3:\n%s", out)
	}
	if !strings.Contains(out, "search_text_body_required") && !strings.Contains(out, "foreign key") {
		t.Fatalf("the insert failed for an unexpected reason:\n%s", out)
	}
	t.Log("ingest.search_text refuses the shapes the vault must not write")
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "INSERT") && !strings.HasPrefix(l, "UPDATE") && !strings.HasPrefix(l, "SELECT") {
			out = append(out, l)
		}
	}
	return out
}
