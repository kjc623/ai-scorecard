package rollup

import (
	"fmt"
	"strings"
	"testing"
)

// TestDeviceLivenessTransitionsAgainstPostgreSQL pins the four states docs/04 §3.7 requires to stay
// distinct, by constructing one device of each kind against the live view. Reporting, stale,
// never_reported and revoked are four facts; a revoked device is not a quiet one and silence is
// never reported as health.
func TestDeviceLivenessTransitionsAgainstPostgreSQL(t *testing.T) {
	container := psqlContainer(t)

	dev := func(n int) string { return fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", n) }
	reporting, stale, never, revoked := dev(11), dev(12), dev(13), dev(14)

	var b strings.Builder
	b.WriteString("BEGIN;\n")
	b.WriteString("SELECT set_config('app.tenant_id', '" + itTenant + "', true);\n")
	fmt.Fprintf(&b, `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
VALUES ('%s', 'liveness-integration', 'active', 'test-region', 'vendor', 'm1');`+"\n", itTenant)
	for _, d := range []struct {
		id       string
		lastSeen string
		revoked  bool
	}{
		{reporting, "now()", false},
		{stale, "now() - interval '25 hours'", false},
		{never, "NULL", false},
		{revoked, "now()", true},
	} {
		last := "NULL"
		if d.lastSeen != "NULL" {
			last = d.lastSeen
		}
		rev := "NULL"
		if d.revoked {
			rev = "now()"
		}
		fmt.Fprintf(&b, `INSERT INTO ops.device (tenant_id, device_id, os, last_seen_at, revoked_at)
VALUES ('%s', '%s', 'windows', %s, %s);`+"\n", itTenant, d.id, last, rev)
	}

	for key, id := range map[string]string{
		"liveness_reporting":      reporting,
		"liveness_stale":          stale,
		"liveness_never_reported": never,
		"liveness_revoked":        revoked,
	} {
		assertEQ(&b, key, fmt.Sprintf(`SELECT liveness FROM mart.v_device_liveness WHERE tenant_id='%s' AND device_id='%s'`, itTenant, id))
	}
	b.WriteString("ROLLBACK;\n")

	out := runPSQL(t, container, b.String())
	got := parseKeyed(out)
	want := map[string]string{
		"liveness_reporting":      "reporting",
		"liveness_stale":          "stale",
		"liveness_never_reported": "never_reported",
		"liveness_revoked":        "revoked",
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
