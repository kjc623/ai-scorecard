package rollup

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The plumbing for reaching a live PostgreSQL 17 with database/schema.sql applied, so the rollup
// statements can be executed as text against the real tables. It follows ingestion/ingest-api's
// internal/store/psql_test.go: the container is discovered rather than hard-coded, and a test with
// no server skips rather than pretending to have proven anything.

func psqlContainer(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("SHADOWPG_CONTAINER"); name != "" {
		if rollupSchemaReady(t, name) {
			return name
		}
		t.Skipf("SHADOWPG_CONTAINER=%s is set but does not expose mart.agg_tool_period and ops.aggregate_watermark", name)
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Image}}").Output()
	if err != nil {
		t.Skipf("no PG integration evidence: docker is unavailable (%v)", err)
	}
	var candidates []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) == 2 && strings.Contains(strings.ToLower(parts[1]), "postgres") {
			candidates = append(candidates, parts[0])
		}
	}
	for _, want := range []string{"sac-authlab-postgres-1", "shadowpg", "shadowpg-invariants"} {
		for _, c := range candidates {
			if c == want && rollupSchemaReady(t, c) {
				return c
			}
		}
	}
	for _, c := range candidates {
		if rollupSchemaReady(t, c) {
			return c
		}
	}
	t.Skip("no PG integration evidence: no running PostgreSQL container exposes the sac schema")
	return ""
}

func rollupSchemaReady(t *testing.T, container string) bool {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "psql", "-U", "postgres", "-d", "shadow", "-A", "-t", "-c",
		`SELECT (to_regclass('mart.agg_tool_period') IS NOT NULL
		     AND to_regclass('ops.aggregate_watermark') IS NOT NULL
		     AND to_regclass('ingest.submission') IS NOT NULL)::text`).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// runPSQL executes a script in one session and returns its stdout. ON_ERROR_STOP makes any SQL
// error fail the test rather than being scrolled past.
func runPSQL(t *testing.T, container, script string) string {
	t.Helper()
	out, err := dockerPSQL(container, script)
	if err != nil {
		t.Fatalf("psql failed: %v\n--- script ---\n%s\n--- output ---\n%s", err, script, out)
	}
	return out
}

func dockerPSQL(container, script string) (string, error) {
	cmd := exec.Command("docker", "exec", "-i", container, "psql", "-U", "postgres", "-d", "shadow",
		"-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// scalar runs one query and returns its first column, trimmed.
func scalar(t *testing.T, container, query string) string {
	t.Helper()
	return strings.TrimSpace(runPSQL(t, container, query))
}

// q renders a string as a dollar-quoted SQL literal, which avoids every escaping question a JSON
// envelope would otherwise raise.
func q(s string) string {
	tag := "$sac$"
	for strings.Contains(s, tag) {
		tag = "$" + tag + "$"
	}
	return tag + s + tag
}
