package store

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// This file is the test's plumbing for reaching a live PostgreSQL 17 server that another agent
// stood up (task T2: db/schema.sql applied to container `shadowpg`). It exists so the SQL statement
// text in sql.go can be executed *as text* against the real schema, which is real evidence, rather
// than only through a database/sql driver this offline repository cannot fetch.
//
// The container is discovered rather than hard-coded: T2's owner recreated it mid-session under a
// different name, and a test pinned to one name would report "no evidence" for a reason that has
// nothing to do with the code. Any running container whose image is PostgreSQL and which answers a
// probe is accepted, and SHADOWPG_CONTAINER overrides the search.

var quotedStringRE = regexp.MustCompile(`'([^']*)'`)

// psqlContainer returns a container that hosts the schema, or skips the test.
func psqlContainer(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("SHADOWPG_CONTAINER"); name != "" {
		if containerReady(t, name) {
			return name
		}
		t.Skipf("SHADOWPG_CONTAINER=%s is set but does not answer a probe with ingest.record_event()", name)
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Image}}").Output()
	if err != nil {
		t.Skipf("no PG integration evidence: docker is unavailable (%v)", err)
	}
	var candidates []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.Contains(strings.ToLower(parts[1]), "postgres") {
			candidates = append(candidates, parts[0])
		}
	}
	// The names T2 has used, preferred so a named container wins over an unrelated one.
	preferred := []string{"shadowpg", "shadowpg-invariants"}
	for _, want := range preferred {
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
	t.Skip("no PG integration evidence: no running PostgreSQL container exposes the sac schema")
	return ""
}

// liveTableHasColumn reports whether the live server has a column, so an evidence test can report
// "awaiting the schema change" rather than fail on a lab container provisioned before it.
func liveTableHasColumn(t *testing.T, container, table, column string) bool {
	t.Helper()
	out := strings.TrimSpace(runPSQL(t, container,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema = 'ops' AND table_name = "+
			q(table)+" AND column_name = "+q(column)+";"))
	return out == "1"
}

// liveTableExists reports whether an ops table is present on the live server.
func liveTableExists(t *testing.T, container, table string) bool {
	t.Helper()
	out := strings.TrimSpace(runPSQL(t, container, "SELECT to_regclass('ops."+table+"') IS NOT NULL;"))
	return out == "t"
}

// containerReady probes one container for the schema this test needs.
func containerReady(t *testing.T, container string) bool {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "psql", "-U", "postgres", "-d", "shadow",
		"-A", "-t", "-c",
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
          WHERE n.nspname = 'ingest' AND p.proname = 'record_event'`).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
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

// dockerPSQL runs one script and returns its combined output, error and all. It is separate from
// runPSQL because one test's evidence IS a failure: the M0 constraint violation.
func dockerPSQL(container, script string) (string, error) {
	cmd := exec.Command("docker", "exec", "-i", container, "psql", "-U", "postgres", "-d", "shadow",
		"-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// lines returns the non-empty output lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// q renders a string as a dollar-quoted SQL literal, which avoids every escaping question a JSON
// envelope or a certificate would otherwise raise.
func q(s string) string {
	tag := "$sac$"
	for strings.Contains(s, tag) {
		tag = "$" + tag + "$"
	}
	return tag + s + tag
}

// prepare renders a statement from sql.go into a PREPARE, so the integration test executes the
// exact text the service would send, placeholders and all, rather than a paraphrase of it.
func prepare(name, stmt string, args ...any) string {
	return "PREPARE " + name + " AS " + stmt + ";\n" + "EXECUTE " + name + "(" + joinLiterals(args) + ");\n"
}

func joinLiterals(args []any) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.(type) {
		case string:
			parts = append(parts, q(v))
		default:
			parts = append(parts, literal(a))
		}
	}
	return strings.Join(parts, ", ")
}
