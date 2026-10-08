package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// TestKillSwitchesAgainstPostgres drives the kill switches as sac_control: a new tenant has none; a
// trip is read back by the Settings read and the policy read, in route order; a re-trip keeps the
// time the switch came into effect; a clear removes it; a route outside the interception routes and
// an unknown tenant are refused; each write is audited with the route, the previous and the new
// state.
func TestKillSwitchesAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	audit := func(at time.Time) store.AuditEntry {
		return store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
			Action: "tenant.kill_switch.set", ObjectType: "kill_switch", OccurredAt: at}
	}
	read := func() []store.KillSwitch {
		t.Helper()
		s, err := st.Settings(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		in, err := st.PolicyInputs(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.KillSwitches, in.KillSwitches) {
			t.Fatalf("the Settings read %+v and the policy read %+v differ", s.KillSwitches, in.KillSwitches)
		}
		return s.KillSwitches
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("a new tenant has kill switches %+v", got)
	}

	if err := st.SetKillSwitch(ctx, tenant, "proxy.tls", true, "app_breakage", audit(t0)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKillSwitch(ctx, tenant, "proxy.loopback", true, "local_model_breakage", audit(t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKillSwitch(ctx, tenant, "proxy.tls", true, "app_breakage_2", audit(t0.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	want := []store.KillSwitch{
		{Route: "proxy.loopback", ReasonCode: "local_model_breakage", EffectiveAt: t0.Add(time.Minute), SetBy: "admin@example.com"},
		{Route: "proxy.tls", ReasonCode: "app_breakage_2", EffectiveAt: t0, SetBy: "admin@example.com"},
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kill switches:\n got %+v\nwant %+v", got, want)
	}

	if err := st.SetKillSwitch(ctx, tenant, "cli.shim", true, "x1", audit(t0)); !errors.Is(err, store.ErrUnknownKillSwitchRoute) {
		t.Fatalf("route outside the interception routes: %v", err)
	}
	if err := st.SetKillSwitch(ctx, pgtest.UUID(t), "proxy.tls", true, "x1", audit(t0)); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}

	if err := st.SetKillSwitch(ctx, tenant, "proxy.tls", false, "", audit(t0.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("after clearing proxy.tls: %+v", got)
	}

	rows, err := owner.QueryContext(ctx, `SELECT coalesce(object_id, ''), detail->>'route', detail->'previous', detail->'new' FROM ops.audit
		WHERE tenant_id = $1::uuid AND action = 'tenant.kill_switch.set' ORDER BY audit_seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][4]string
	for rows.Next() {
		var objectID, route string
		var previous, next []byte
		if err := rows.Scan(&objectID, &route, &previous, &next); err != nil {
			t.Fatal(err)
		}
		got = append(got, [4]string{objectID, route, canonicalJSON(t, previous), canonicalJSON(t, next)})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantAudits := [][4]string{
		{"", "proxy.tls", `{"on":false}`, `{"on":true,"reason_code":"app_breakage"}`},
		{"", "proxy.loopback", `{"on":false}`, `{"on":true,"reason_code":"local_model_breakage"}`},
		{"", "proxy.tls", `{"on":true,"reason_code":"app_breakage"}`, `{"on":true,"reason_code":"app_breakage_2"}`},
		{"", "proxy.tls", `{"on":true,"reason_code":"app_breakage_2"}`, `{"on":false}`},
	}
	if !reflect.DeepEqual(got, wantAudits) {
		t.Fatalf("audits =\n%v\nwant\n%v", got, wantAudits)
	}
}
