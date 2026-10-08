package settings_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/settings"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const tenantA = "5a3c0de0-7e57-4a11-9000-0000000d3a01"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var admin = deploy.Principal{Tenant: tenantA, Actor: "admin@contoso.example", Subject: "conn:oid-1", Roles: []string{"viewer", "admin"}}

type rig struct {
	store *storetest.Memory
	mux   *http.ServeMux
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{store: storetest.New(), mux: http.NewServeMux(),
		now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	r.store.AddTenant(store.Tenant{TenantID: tenantA, Status: "active", IngestEnabled: true})
	r.store.SetCeiling(tenantA, "m3")
	r.store.SetCatalogueTools(
		store.ToolDecision{ToolFingerprint: "tls_b6681b043244c43f", DisplayName: "Claude Code"},
		store.ToolDecision{ToolFingerprint: "tls_f32477ff734d70d1", DisplayName: "OpenAI API"},
	)
	auth := func(req *http.Request) (deploy.Principal, error) {
		switch req.Header.Get("X-Test-Principal") {
		case "admin":
			return admin, nil
		case "analyst":
			return deploy.Principal{Tenant: tenantA, Actor: "analyst@contoso.example", Roles: []string{"analyst"}}, nil
		}
		return deploy.Principal{}, errors.New("no token")
	}
	h, err := settings.NewHandler(r.store, auth, settings.Config{Now: func() time.Time { return r.now }, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h.Register(r.mux)
	return r
}

func (r *rig) do(t *testing.T, principal, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

var routes = []struct{ method, path, body string }{
	{"GET", "/admin/v1/settings", ""},
	{"PUT", "/admin/v1/settings/collection-mode", `{"collection_mode":"m1"}`},
	{"PUT", "/admin/v1/settings/scope-override", `{"tool_fingerprint":"tls_b6681b043244c43f","collection_mode":"m0"}`},
	{"PUT", "/admin/v1/settings/retention", `{"applies_to":"event","ttl_days":90}`},
	{"PUT", "/admin/v1/settings/content-search", `{"content_search":"attachment_names"}`},
	{"PUT", "/admin/v1/settings/tools/tls_b6681b043244c43f/sanction", `{"sanctioned_state":"unsanctioned"}`},
	{"PUT", "/admin/v1/settings/endpoint", endpointBody},
	{"PUT", "/admin/v1/settings/endpoint/tools/cursor", `{"otel":false,"hooks":false}`},
	{"PUT", "/admin/v1/settings/tls-inspection", `{"enabled":true}`},
}

const endpointBody = `{"inventory":false,"processes":true,"flows":false,"otel":true,"hooks":true,"hooks_managed_only":true}`

func TestEveryRouteRefusesANonAdmin(t *testing.T) {
	r := newRig(t)
	for _, rt := range routes {
		if rec := r.do(t, "", rt.method, rt.path, rt.body); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != apierr.CodeUnauthenticated {
			t.Errorf("%s %s without a token: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
		if rec := r.do(t, "analyst", rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden || errorCode(t, rec) != apierr.CodeForbidden {
			t.Errorf("%s %s as analyst: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	if len(r.store.Audits()) != 0 {
		t.Fatal("a refused request wrote an audit row")
	}
}

func TestGet(t *testing.T) {
	r := newRig(t)
	r.store.AddDevice(store.Device{TenantID: tenantA, DeviceID: "22222222-2222-4222-8222-222222222222", OS: "windows", Hostname: "LAPTOP-1", EnrolledAt: r.now.Add(-2 * time.Hour)})
	r.store.SetDeviceMode(tenantA, "22222222-2222-4222-8222-222222222222", "m2")
	r.store.SeedCollectionMode(tenantA, "m2")
	r.store.SeedScopeOverride(tenantA, "tls_b6681b043244c43f", "m1")
	r.store.SeedRetention(tenantA, "event", 120)
	r.store.SeedContentSearch(tenantA, "attachment_names")
	r.store.SetToolSanction(context.Background(), tenantA, "tls_f32477ff734d70d1", "unsanctioned",
		store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: admin.Actor, Action: "tool.sanction", ObjectType: "tool", ObjectID: "tls_f32477ff734d70d1", OccurredAt: r.now})

	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		CeilingMode          string            `json:"ceiling_mode"`
		CollectionMode       string            `json:"collection_mode"`
		ScopeOverrides       map[string]string `json:"scope_overrides"`
		EventRetentionDays   *int              `json:"event_retention_days"`
		ContentRetentionDays *int              `json:"content_retention_days"`
		RetentionDefaults    struct {
			EventDays   int `json:"event_days"`
			ContentDays int `json:"content_days"`
		} `json:"retention_defaults"`
		ContentSearch string `json:"content_search"`
		Tools         []struct {
			ToolFingerprint string `json:"tool_fingerprint"`
			SanctionedState string `json:"sanctioned_state"`
		} `json:"tools"`
		Devices []struct {
			DeviceID       string `json:"device_id"`
			CollectionMode string `json:"collection_mode"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CeilingMode != "m3" || got.CollectionMode != "m2" || got.ScopeOverrides["tls_b6681b043244c43f"] != "m1" {
		t.Fatalf("modes = %+v", got)
	}
	if got.EventRetentionDays == nil || *got.EventRetentionDays != 120 {
		t.Fatalf("event retention = %v", got.EventRetentionDays)
	}
	if got.ContentRetentionDays != nil {
		t.Fatalf("content retention = %v, want nil (unset)", got.ContentRetentionDays)
	}
	if got.RetentionDefaults.EventDays != 90 || got.RetentionDefaults.ContentDays != 30 {
		t.Fatalf("defaults = %+v", got.RetentionDefaults)
	}
	if got.ContentSearch != "attachment_names" {
		t.Fatalf("content search = %s", got.ContentSearch)
	}
	if len(got.Tools) != 2 || got.Tools[1].SanctionedState != "unsanctioned" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if len(got.Devices) != 1 || got.Devices[0].CollectionMode != "m2" {
		t.Fatalf("devices = %+v", got.Devices)
	}
}

func TestCollectionMode(t *testing.T) {
	r := newRig(t)
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/collection-mode", body)
	}
	if rec := put(`{"collection_mode":"m2"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set m2: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"collection_mode":"m3"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set m3 (the ceiling): %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"collection_mode":"m4"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", rec.Code)
	}
	// Above the ceiling is refused with a named 409.
	r.store.SetCeiling(tenantA, "m1")
	if rec := put(`{"collection_mode":"m2"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeCollectionExceedsCeiling {
		t.Fatalf("above ceiling: %d %s", rec.Code, rec.Body)
	}
	// NULL follows the ceiling again.
	if rec := put(`{"collection_mode":null}`); rec.Code != http.StatusNoContent {
		t.Fatalf("follow ceiling: %d %s", rec.Code, rec.Body)
	}
	s, _ := r.store.Settings(context.Background(), tenantA)
	if s.CollectionMode != "" {
		t.Fatalf("collection mode = %q, want empty (follow ceiling)", s.CollectionMode)
	}
	// Audits carry the actor, old value and new value.
	var found bool
	for _, a := range r.store.Audits() {
		if a.Action != "tenant.collection_mode.set" {
			continue
		}
		found = true
		if a.ActorID != admin.Actor || a.Detail["previous"] != "" || a.Detail["new"] != "m2" {
			t.Fatalf("first audit = %+v", a)
		}
		break
	}
	if !found {
		t.Fatal("no collection_mode audit was written")
	}
}

func TestScopeOverride(t *testing.T) {
	r := newRig(t)
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/scope-override", body)
	}
	// An override wider than the ceiling is refused even while following the ceiling.
	r.store.SetCeiling(tenantA, "m1")
	if rec := put(`{"tool_fingerprint":"tls_b6681b043244c43f","collection_mode":"m2"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeScopeOverrideTooWide {
		t.Fatalf("override above the ceiling: %d %s", rec.Code, rec.Body)
	}
	r.store.SetCeiling(tenantA, "m3")
	// Request a mode below the ceiling so a wider override is possible to refuse.
	r.store.SeedCollectionMode(tenantA, "m2")
	// A narrower override is accepted.
	if rec := put(`{"tool_fingerprint":"tls_b6681b043244c43f","collection_mode":"m1"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("override: %d %s", rec.Code, rec.Body)
	}
	// A wider override is refused.
	if rec := put(`{"tool_fingerprint":"tls_b6681b043244c43f","collection_mode":"m3"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeScopeOverrideTooWide {
		t.Fatalf("wider override: %d %s", rec.Code, rec.Body)
	}
	// Clearing is accepted.
	if rec := put(`{"tool_fingerprint":"tls_b6681b043244c43f","collection_mode":null}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"collection_mode":"m1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fingerprint: %d", rec.Code)
	}
	s, _ := r.store.Settings(context.Background(), tenantA)
	if _, ok := s.ScopeOverrides["tls_b6681b043244c43f"]; ok {
		t.Fatalf("override not cleared: %+v", s.ScopeOverrides)
	}
}

func TestRetention(t *testing.T) {
	r := newRig(t)
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/retention", body)
	}
	if rec := put(`{"applies_to":"event","ttl_days":120}`); rec.Code != http.StatusNoContent {
		t.Fatalf("event: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"applies_to":"content","ttl_days":45}`); rec.Code != http.StatusNoContent {
		t.Fatalf("content: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"applies_to":"content","ttl_days":0}`); rec.Code != http.StatusBadRequest || errorCode(t, rec) != apierr.CodeRetentionOutOfRange {
		t.Fatalf("zero ttl: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"applies_to":"other","ttl_days":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad applies_to: %d", rec.Code)
	}
	s, _ := r.store.Settings(context.Background(), tenantA)
	if s.EventRetentionDays == nil || *s.EventRetentionDays != 120 || s.ContentRetentionDays == nil || *s.ContentRetentionDays != 45 {
		t.Fatalf("retention = %v / %v", s.EventRetentionDays, s.ContentRetentionDays)
	}
}

func TestContentSearch(t *testing.T) {
	r := newRig(t)
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/content-search", body)
	}
	if rec := put(`{"content_search":"attachment_names"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("attachment_names: %d %s", rec.Code, rec.Body)
	}
	// full_text needs an M3 ceiling.
	r.store.SetCeiling(tenantA, "m1")
	if rec := put(`{"content_search":"full_text"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeSearchTierRequiresMode {
		t.Fatalf("full_text on m1: %d %s", rec.Code, rec.Body)
	}
	// attachment_names needs a ceiling above M0.
	r.store.SetCeiling(tenantA, "m0")
	if rec := put(`{"content_search":"attachment_names"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeSearchTierRequiresMode {
		t.Fatalf("attachment_names on m0: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"content_search":"everything"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad tier: %d", rec.Code)
	}
}

func TestToolSanction(t *testing.T) {
	r := newRig(t)
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/tools/tls_b6681b043244c43f/sanction", body)
	}
	if rec := put(`{"sanctioned_state":"unsanctioned"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("unsanctioned: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"sanctioned_state":"maybe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state: %d", rec.Code)
	}
	s, _ := r.store.Settings(context.Background(), tenantA)
	if s.Tools[0].SanctionedState != "unsanctioned" {
		t.Fatalf("state = %s", s.Tools[0].SanctionedState)
	}
	var found bool
	for _, a := range r.store.Audits() {
		if a.Action == "tool.sanction" && a.ActorID == admin.Actor && a.Detail["previous"] == "unknown" && a.Detail["new"] == "unsanctioned" {
			found = true
		}
	}
	if !found {
		t.Fatal("no tool.sanction audit with actor, old and new values")
	}
}

type endpointGot struct {
	Inventory        bool `json:"inventory"`
	Processes        bool `json:"processes"`
	Flows            bool `json:"flows"`
	OTel             bool `json:"otel"`
	Hooks            bool `json:"hooks"`
	HooksManagedOnly bool `json:"hooks_managed_only"`
	Tools            map[string]struct {
		OTel  bool `json:"otel"`
		Hooks bool `json:"hooks"`
	} `json:"tools"`
}

func (r *rig) endpoint(t *testing.T) endpointGot {
	t.Helper()
	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		Endpoint *endpointGot `json:"endpoint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Endpoint == nil {
		t.Fatalf("GET /admin/v1/settings has no endpoint: %s", rec.Body)
	}
	return *got.Endpoint
}

func TestEndpointCollectors(t *testing.T) {
	r := newRig(t)
	// A tenant that never set them reads the defaults.
	e := r.endpoint(t)
	if !e.Inventory || !e.Processes || !e.Flows || !e.OTel || !e.Hooks || e.HooksManagedOnly {
		t.Fatalf("default collectors = %+v", e)
	}
	if len(e.Tools) != 4 || !e.Tools["claude_code"].OTel || !e.Tools["claude_code"].Hooks || e.Tools["cursor"].OTel ||
		!e.Tools["cursor"].Hooks || !e.Tools["codex"].OTel || e.Tools["codex"].Hooks || e.Tools["copilot"].Hooks {
		t.Fatalf("default tools = %+v", e.Tools)
	}

	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/endpoint", body)
	}
	if rec := put(endpointBody); rec.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	e = r.endpoint(t)
	if e.Inventory || !e.Processes || e.Flows || !e.OTel || !e.Hooks || !e.HooksManagedOnly {
		t.Fatalf("collectors after the write = %+v", e)
	}
	// A body without every switch, or with an unknown one, changes nothing.
	if rec := put(`{"inventory":true}`); rec.Code != http.StatusBadRequest || errorCode(t, rec) != apierr.CodeInvalidRequest {
		t.Fatalf("partial body: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"inventory":true,"processes":true,"flows":true,"otel":true,"hooks":true,"hooks_managed_only":false,"tls":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body)
	}
	if e := r.endpoint(t); e.Inventory {
		t.Fatal("a refused write changed the settings")
	}

	audits := r.store.Audits()
	if len(audits) != 1 {
		t.Fatalf("audits = %+v, want the one accepted write", audits)
	}
	a := audits[0]
	if a.Action != "tenant.endpoint_collectors.set" || a.ActorID != admin.Actor || a.ObjectType != "tenant" || a.ObjectID != tenantA || a.Detail["subject"] != admin.Subject {
		t.Fatalf("audit = %+v", a)
	}
	prev, _ := a.Detail["previous"].(map[string]any)
	next, _ := a.Detail["new"].(map[string]any)
	if prev["inventory"] != true || prev["hooks_managed_only"] != false || next["inventory"] != false || next["flows"] != false || next["hooks_managed_only"] != true {
		t.Fatalf("audit values: previous %v, new %v", prev, next)
	}
}

func TestEndpointTool(t *testing.T) {
	r := newRig(t)
	put := func(tool, body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/endpoint/tools/"+tool, body)
	}
	if rec := put("cursor", `{"otel":false,"hooks":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set cursor: %d %s", rec.Code, rec.Body)
	}
	e := r.endpoint(t)
	if e.Tools["cursor"].Hooks || e.Tools["cursor"].OTel || !e.Tools["claude_code"].Hooks {
		t.Fatalf("tools after the write = %+v", e.Tools)
	}
	if rec := put("ollama", `{"otel":true,"hooks":true}`); rec.Code != http.StatusNotFound || errorCode(t, rec) != apierr.CodeNotFound {
		t.Fatalf("unknown tool: %d %s", rec.Code, rec.Body)
	}
	if rec := put("codex", `{"otel":false}`); rec.Code != http.StatusBadRequest || errorCode(t, rec) != apierr.CodeInvalidRequest {
		t.Fatalf("missing hooks: %d %s", rec.Code, rec.Body)
	}

	audits := r.store.Audits()
	if len(audits) != 1 {
		t.Fatalf("audits = %+v, want the one accepted write", audits)
	}
	a := audits[0]
	if a.Action != "tenant.endpoint_tool.set" || a.ActorID != admin.Actor || a.ObjectType != "endpoint_tool" || a.ObjectID != "cursor" || a.Detail["tool_key"] != "cursor" {
		t.Fatalf("audit = %+v", a)
	}
	prev, _ := a.Detail["previous"].(map[string]any)
	next, _ := a.Detail["new"].(map[string]any)
	if prev["otel"] != false || prev["hooks"] != true || next["otel"] != false || next["hooks"] != false {
		t.Fatalf("audit values: previous %v, new %v", prev, next)
	}
}

func (r *rig) tlsInspection(t *testing.T) bool {
	t.Helper()
	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		TLSInspection *bool `json:"tls_inspection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TLSInspection == nil {
		t.Fatalf("GET /admin/v1/settings has no tls_inspection: %s", rec.Body)
	}
	return *got.TLSInspection
}

func TestTLSInspection(t *testing.T) {
	r := newRig(t)
	// A tenant that never set it reads off.
	if r.tlsInspection(t) {
		t.Fatal("TLS inspection is on for a tenant that never turned it on")
	}
	put := func(body string) *httptest.ResponseRecorder {
		return r.do(t, "admin", "PUT", "/admin/v1/settings/tls-inspection", body)
	}
	if rec := put(`{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("turn on: %d %s", rec.Code, rec.Body)
	}
	if !r.tlsInspection(t) {
		t.Fatal("TLS inspection is off after it was turned on")
	}
	// A body without the switch, with another type or with an unknown field changes nothing.
	for _, body := range []string{`{}`, `{"enabled":null}`} {
		if rec := put(body); rec.Code != http.StatusBadRequest || errorCode(t, rec) != apierr.CodeInvalidRequest {
			t.Fatalf("body %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"enabled":"false"}`, `{"enabled":false,"hosts":[]}`} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if !r.tlsInspection(t) {
		t.Fatal("a refused write changed the setting")
	}
	if rec := put(`{"enabled":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body)
	}
	if r.tlsInspection(t) {
		t.Fatal("TLS inspection is on after it was turned off")
	}

	audits := r.store.Audits()
	if len(audits) != 2 {
		t.Fatalf("audits = %+v, want the two accepted writes", audits)
	}
	for i, want := range []struct{ previous, next bool }{{false, true}, {true, false}} {
		a := audits[i]
		if a.Action != "tenant.tls_inspection.set" || a.ActorID != admin.Actor || a.ObjectType != "tenant" || a.ObjectID != tenantA || a.Detail["subject"] != admin.Subject {
			t.Fatalf("audit %d = %+v", i, a)
		}
		if a.Detail["previous"] != want.previous || a.Detail["new"] != want.next {
			t.Fatalf("audit %d values: previous %v, new %v; want %v, %v", i, a.Detail["previous"], a.Detail["new"], want.previous, want.next)
		}
	}
}
