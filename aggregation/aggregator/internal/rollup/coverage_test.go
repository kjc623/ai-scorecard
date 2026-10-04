package rollup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCoverageWindowForEndsAtNextMidnight(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	w, err := CoverageWindowFor(now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.From.Format(time.RFC3339), "2026-09-28T00:00:00Z"; got != want {
		t.Errorf("From = %s, want %s", got, want)
	}
	// The window includes the day that contains now, so today's coverage is written as it happens.
	if got, want := w.To.Format(time.RFC3339), "2026-10-05T00:00:00Z"; got != want {
		t.Errorf("To = %s, want %s", got, want)
	}
}

func TestCoverageWindowForRejectsZeroLookback(t *testing.T) {
	if _, err := CoverageWindowFor(time.Now(), 0); err == nil {
		t.Fatal("a zero-day coverage window was accepted")
	}
}

// TestCoverageSnapshotAgainstPostgreSQL executes the coverage statement as text against the live
// schema, so the seam with ops.coverage_snapshot and ref.collector is verified rather than declared.
// It asserts the two properties docs/04 §11.3 depends on: every enrolled device gets a row per
// collector, and an observed collector is not later erased when its current state moves on.
func TestCoverageSnapshotAgainstPostgreSQL(t *testing.T) {
	container := psqlContainer(t)
	now := time.Now().UTC().Truncate(time.Second)
	window, err := CoverageWindowFor(now, 1)
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	b.WriteString("BEGIN;\n")
	b.WriteString("SELECT set_config('app.tenant_id', '" + itTenant + "', true);\n")
	fmt.Fprintf(&b, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
VALUES ('%s', 'coverage-integration', 'active', 'test-region', 'vendor', 'm1');`+"\n", itTenant)
	fmt.Fprintf(&b, `INSERT INTO ops.device (tenant_id, device_id, os) VALUES ('%s', '%s', 'windows');`+"\n", itTenant, itDevice)
	// One collector healthy, one tampered, one absent: three observed-state rows on the day.
	for _, row := range []struct{ collector, state string }{
		{"egress_proxy", "healthy"}, {"cli_shim", "tampered"}, {"loopback_broker", "absent"},
	} {
		fmt.Fprintf(&b, `INSERT INTO ops.collector_state (tenant_id, device_id, collector, state, last_report_at)
VALUES ('%s', '%s', '%s', '%s', '%s'::timestamptz);`+"\n", itTenant, itDevice, row.collector, row.state, now.Format(time.RFC3339))
	}

	coverage := func() string {
		stmt := CoverageSnapshotSQL
		for i, arg := range []string{itTenant, window.From.Format(time.RFC3339), window.To.Format(time.RFC3339)} {
			stmt = strings.ReplaceAll(stmt, fmt.Sprintf("$%d", i+1), q(arg))
		}
		return stmt
	}
	b.WriteString(coverage() + ";\n")

	// 6 collectors (ref.collector) × 1 device; observed counts healthy+degraded only, so 1 here.
	assertEQ(&b, "coverage_rows", fmt.Sprintf(`SELECT count(*)::text FROM ops.coverage_snapshot WHERE tenant_id='%s'`, itTenant))
	assertEQ(&b, "coverage_expected", fmt.Sprintf(`SELECT count(*) FILTER (WHERE expected)::text FROM ops.coverage_snapshot WHERE tenant_id='%s'`, itTenant))
	assertEQ(&b, "coverage_observed", fmt.Sprintf(`SELECT count(*) FILTER (WHERE observed)::text FROM ops.coverage_snapshot WHERE tenant_id='%s'`, itTenant))
	assertEQ(&b, "coverage_tampered", fmt.Sprintf(`SELECT gap_reason FROM ops.coverage_snapshot WHERE tenant_id='%s' AND collector='cli_shim'`, itTenant))
	assertEQ(&b, "coverage_absent", fmt.Sprintf(`SELECT gap_reason FROM ops.coverage_snapshot WHERE tenant_id='%s' AND collector='loopback_broker'`, itTenant))
	assertEQ(&b, "coverage_unreported", fmt.Sprintf(`SELECT gap_reason FROM ops.coverage_snapshot WHERE tenant_id='%s' AND collector='process_detector'`, itTenant))

	// The healthy collector now reports absent later the same day. Because collector_state is
	// current state, not history, the day's row must keep observed = true.
	fmt.Fprintf(&b, `UPDATE ops.collector_state SET state='absent', last_report_at='%s'::timestamptz
  WHERE tenant_id='%s' AND collector='egress_proxy';`+"\n", now.Add(time.Minute).Format(time.RFC3339), itTenant)
	b.WriteString(coverage() + ";\n")
	assertEQ(&b, "coverage_observed_monotonic", fmt.Sprintf(`SELECT count(*) FILTER (WHERE observed)::text FROM ops.coverage_snapshot WHERE tenant_id='%s'`, itTenant))
	b.WriteString("ROLLBACK;\n")

	out := runPSQL(t, container, b.String())
	got := parseKeyed(out)
	want := map[string]string{
		"coverage_rows":               "6",
		"coverage_expected":           "6",
		"coverage_observed":           "1",
		"coverage_tampered":           "tampered",
		"coverage_absent":             "unknown",
		"coverage_unreported":         "unknown",
		"coverage_observed_monotonic": "1",
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
