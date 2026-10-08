package settings_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

type killSwitchRow struct {
	Route       string    `json:"route"`
	ReasonCode  string    `json:"reason_code"`
	EffectiveAt time.Time `json:"effective_at"`
	SetBy       string    `json:"set_by"`
}

// killSwitches is GET /admin/v1/settings' kill_switches, which must be present.
func (r *rig) killSwitches(t *testing.T) []killSwitchRow {
	t.Helper()
	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		KillSwitches *[]killSwitchRow `json:"kill_switches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.KillSwitches == nil {
		t.Fatalf("GET /admin/v1/settings has no kill_switches: %s", rec.Body)
	}
	return *got.KillSwitches
}

// An admin trips a route's kill switch with a reason code and clears it; the settings read lists
// what is tripped; each accepted write is audited with the route, the previous and the new state.
func TestKillSwitch(t *testing.T) {
	r := newRig(t)
	if got := r.killSwitches(t); len(got) != 0 {
		t.Fatalf("a new tenant has kill switches %+v", got)
	}
	put := func(route, body string) int {
		t.Helper()
		return r.do(t, "admin", "PUT", "/admin/v1/settings/kill-switch/"+route, body).Code
	}

	if code := put("proxy.tls", `{"on":true,"reason_code":"app_breakage"}`); code != http.StatusNoContent {
		t.Fatalf("trip proxy.tls: %d", code)
	}
	tripped := r.now
	r.now = r.now.Add(time.Minute)
	if code := put("proxy.loopback", `{"on":true,"reason_code":"local.model-1"}`); code != http.StatusNoContent {
		t.Fatalf("trip proxy.loopback: %d", code)
	}
	// A new reason for a tripped switch keeps the time it came into effect.
	r.now = r.now.Add(time.Minute)
	if code := put("proxy.tls", `{"on":true,"reason_code":"app_breakage_2"}`); code != http.StatusNoContent {
		t.Fatalf("re-trip proxy.tls: %d", code)
	}
	want := []killSwitchRow{
		{Route: "proxy.loopback", ReasonCode: "local.model-1", EffectiveAt: tripped.Add(time.Minute), SetBy: admin.Actor},
		{Route: "proxy.tls", ReasonCode: "app_breakage_2", EffectiveAt: tripped, SetBy: admin.Actor},
	}
	if got := r.killSwitches(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("kill switches:\n got %+v\nwant %+v", got, want)
	}

	if code := put("proxy.tls", `{"on":false}`); code != http.StatusNoContent {
		t.Fatalf("clear proxy.tls: %d", code)
	}
	if code := put("proxy.loopback", `{"on":false,"reason_code":"fixed_in_1_2"}`); code != http.StatusNoContent {
		t.Fatalf("clear proxy.loopback with a reason: %d", code)
	}
	if got := r.killSwitches(t); len(got) != 0 {
		t.Fatalf("cleared switches still listed: %+v", got)
	}

	audits := r.store.Audits()
	wantAudits := []struct {
		route          string
		previous, next map[string]any
	}{
		{"proxy.tls", map[string]any{"on": false}, map[string]any{"on": true, "reason_code": "app_breakage"}},
		{"proxy.loopback", map[string]any{"on": false}, map[string]any{"on": true, "reason_code": "local.model-1"}},
		{"proxy.tls", map[string]any{"on": true, "reason_code": "app_breakage"}, map[string]any{"on": true, "reason_code": "app_breakage_2"}},
		{"proxy.tls", map[string]any{"on": true, "reason_code": "app_breakage_2"}, map[string]any{"on": false}},
		{"proxy.loopback", map[string]any{"on": true, "reason_code": "local.model-1"}, map[string]any{"on": false}},
	}
	if len(audits) != len(wantAudits) {
		t.Fatalf("audits = %+v, want the %d accepted writes", audits, len(wantAudits))
	}
	for i, w := range wantAudits {
		a := audits[i]
		if a.Action != "tenant.kill_switch.set" || a.ActorID != admin.Actor || a.ObjectType != "kill_switch" || a.ObjectID != w.route ||
			a.Detail["route"] != w.route || a.Detail["subject"] != admin.Subject {
			t.Fatalf("audit %d = %+v", i, a)
		}
		if !reflect.DeepEqual(a.Detail["previous"], w.previous) || !reflect.DeepEqual(a.Detail["new"], w.next) {
			t.Fatalf("audit %d: previous %v, new %v; want %v, %v", i, a.Detail["previous"], a.Detail["new"], w.previous, w.next)
		}
	}
	if audits[4].Detail["reason_code"] != "fixed_in_1_2" {
		t.Fatalf("the clear's reason was not audited: %+v", audits[4].Detail)
	}
}

// A route outside the interception routes, a missing switch, a trip without a reason, a malformed
// reason and an unknown field are refused and change nothing.
func TestKillSwitchRefusals(t *testing.T) {
	r := newRig(t)
	refusals := []struct {
		route, body string
		status      int
		code        string
	}{
		{"cli.shim", `{"on":true,"reason_code":"x1"}`, http.StatusNotFound, apierr.CodeNotFound},
		{"proxy.tls", `{}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"reason_code":"x1"}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true,"reason_code":""}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true,"reason_code":"App breakage"}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true,"reason_code":"9lives"}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true,"reason_code":"` + strings.Repeat("a", 65) + `"}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":false,"reason_code":"Not A Code"}`, http.StatusBadRequest, apierr.CodeInvalidRequest},
		{"proxy.tls", `{"on":true,"reason_code":"x1","mode":"disable"}`, http.StatusBadRequest, apierr.CodeSchemaViolation},
		{"proxy.tls", `{"on":"true","reason_code":"x1"}`, http.StatusBadRequest, apierr.CodeSchemaViolation},
	}
	for _, c := range refusals {
		rec := r.do(t, "admin", "PUT", "/admin/v1/settings/kill-switch/"+c.route, c.body)
		if rec.Code != c.status || errorCode(t, rec) != c.code {
			t.Errorf("%s %s: %d %s, want %d %s", c.route, c.body, rec.Code, rec.Body, c.status, c.code)
		}
	}
	if got := r.killSwitches(t); len(got) != 0 {
		t.Fatalf("a refused write tripped %+v", got)
	}
	if n := len(r.store.Audits()); n != 0 {
		t.Fatalf("refused writes wrote %d audit rows", n)
	}
}
