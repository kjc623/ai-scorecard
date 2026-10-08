package policyserve_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/policyserve"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

const (
	tenantA = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	deviceA = "22222222-2222-4222-8222-222222222222"
)

var (
	signingKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	quiet      = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type rig struct {
	store *storetest.Memory
	svc   *policyserve.Service
	now   time.Time
}

func newRig(t *testing.T, mutate func(*policyserve.Config)) *rig {
	t.Helper()
	r := &rig{store: storetest.New(), now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	r.store.AddTenant(store.Tenant{TenantID: tenantA, Status: "active", IngestEnabled: true})
	r.store.SetCeiling(tenantA, "m3")
	r.store.SetCatalogueHosts("api.openai.com", "API.Anthropic.com", "chatgpt.com", "api.openai.com")
	cfg := policyserve.Config{Now: func() time.Time { return r.now }, RecheckInterval: -1, Logger: quiet}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := policyserve.New(r.store, signingKey, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

func deviceAuth(r *http.Request) (string, string, error) { return tenantA, deviceA, nil }

func (r *rig) get(t *testing.T, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://edge.example/v1/policy", nil)
	if ifNoneMatch != "" {
		req.Header.Set(protocol.HeaderIfNoneMatch, ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	r.svc.Handler(deviceAuth).ServeHTTP(rec, req)
	return rec
}

// payloadOf verifies the envelope's signature with the public key and returns the decoded payload.
func payloadOf(t *testing.T, envelope []byte) (map[string]any, []byte) {
	t.Helper()
	var sb struct {
		KeyID     string          `json:"key_id"`
		Algorithm string          `json:"algorithm"`
		Payload   json.RawMessage `json:"payload"`
		Signature string          `json:"signature"`
	}
	if err := json.Unmarshal(envelope, &sb); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if sb.KeyID != policyserve.DefaultKeyID || sb.Algorithm != "ed25519" {
		t.Fatalf("envelope names key %q alg %q", sb.KeyID, sb.Algorithm)
	}
	sig, err := base64.StdEncoding.DecodeString(sb.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(signingKey.Public().(ed25519.PublicKey), sb.Payload, sig) {
		t.Fatal("the signature does not verify under the policy public key")
	}
	var p map[string]any
	if err := json.Unmarshal(sb.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p, sb.Payload
}

func TestServesASignedBundleComposedFromTheTenant(t *testing.T) {
	r := newRig(t, nil)
	rec := r.get(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp protocol.PolicyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("ETag") != protocol.PolicyETag(resp.BundleVersion) || resp.SchemaVersion != protocol.PolicySchemaVersion {
		t.Fatalf("ETag %q, response %+v", rec.Header().Get("ETag"), resp)
	}
	p, _ := payloadOf(t, resp.SignedBundle)
	if p["version"] != resp.BundleVersion || p["tenant_default_mode"] != "m3" {
		t.Fatalf("payload version/mode = %v/%v, response version %s", p["version"], p["tenant_default_mode"], resp.BundleVersion)
	}
	if v, _ := strconv.ParseInt(resp.BundleVersion, 10, 64); v < r.now.Unix() {
		t.Fatalf("first version %d is below the minting time %d", v, r.now.Unix())
	}
	ic := p["interception"].(map[string]any)
	hosts, _ := json.Marshal(ic["seed_hosts"])
	if string(hosts) != `["api.anthropic.com","api.openai.com","chatgpt.com"]` || ic["proxy_canary"] != "api.anthropic.com:443" {
		t.Fatalf("interception = %v", ic)
	}
	if _, ok := ic["root_ca_pem"]; ok {
		t.Fatal("the server put a root CA in the bundle; the device generates its own")
	}
	shim := p["cli_shim"].(map[string]any)
	if _, ok := shim["managed_dir"]; ok || shim["proxy_addr"] != policyserve.DefaultProxyListen {
		t.Fatalf("cli_shim = %v", shim)
	}
	for _, field := range []string{"classifier", "proc_detect", "shape_predicate", "loopback", "spool"} {
		if _, ok := p[field]; ok {
			t.Errorf("the payload carries %q, which this service does not compose", field)
		}
	}

	rows := r.store.PolicyBundles(tenantA)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	sum := sha256.Sum256(resp.SignedBundle)
	if !bytes.Equal(row.SignedEnvelope, resp.SignedBundle) || row.SignedDigest != "sha256:"+hex.EncodeToString(sum[:]) ||
		row.SignatureKID != policyserve.DefaultKeyID || row.RetentionClass != "standard" {
		t.Fatalf("stored row = %+v", row)
	}
	if string(row.ScopeMatrix) != `{"tenant_default":"m3"}` {
		t.Fatalf("scope matrix %s would not satisfy the ceiling trigger's shape", row.ScopeMatrix)
	}
	audits := r.store.Audits()
	if len(audits) != 1 || audits[0].Action != "policy_bundle.publish" || audits[0].ObjectID != resp.BundleVersion {
		t.Fatalf("audits = %+v", audits)
	}
}

// TestComposeCarriesRequestedModeAndOverrides: the requested collection mode (which may be below the
// ceiling) is the bundle's tenant_default_mode, and each narrower per-tool override rides in
// tool_modes and the stored scope matrix, so the ceiling trigger sees it.
func TestComposeCarriesRequestedModeAndOverrides(t *testing.T) {
	r := newRig(t, nil)
	r.store.SeedCollectionMode(tenantA, "m2")
	r.store.SeedScopeOverride(tenantA, "tls_b6681b043244c43f", "m0")

	rec := r.get(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp protocol.PolicyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	p, _ := payloadOf(t, resp.SignedBundle)
	if p["tenant_default_mode"] != "m2" {
		t.Fatalf("tenant_default_mode = %v, want m2", p["tenant_default_mode"])
	}
	toolModes := p["tool_modes"].(map[string]any)
	if toolModes["tls_b6681b043244c43f"] != "m0" {
		t.Fatalf("tool_modes = %v", toolModes)
	}
	row := r.store.PolicyBundles(tenantA)[0]
	if !strings.Contains(string(row.ScopeMatrix), `"tool_modes"`) || !strings.Contains(string(row.ScopeMatrix), `"m0"`) {
		t.Fatalf("scope matrix %s does not carry the override", row.ScopeMatrix)
	}
}

// endpointOf is the served bundle's endpoint section, re-encoded with sorted keys.
func endpointOf(t *testing.T, envelope []byte) string {
	t.Helper()
	p, _ := payloadOf(t, envelope)
	e, ok := p["endpoint"]
	if !ok {
		t.Fatal("the payload has no endpoint section")
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// canonical re-encodes a JSON document with sorted keys and no spacing.
func canonical(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestComposeEndpointSection: a tenant without rows is served the defaults; a tenant row sets the
// collector switches and a tool row one tool's, and each change mints a new version.
func TestComposeEndpointSection(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	section := func(inventory, flows, managedOnly, cursorHooks, codexOTel bool) string {
		return canonical(t, fmt.Sprintf(`{
		  "inventory": {"enabled": %t, "interval_minutes": 360},
		  "processes": {"enabled": true},
		  "flows":     {"enabled": %t},
		  "otel":      {"enabled": true, "http_listen": "127.0.0.1:47318", "grpc_listen": "127.0.0.1:47317"},
		  "hooks":     {"enabled": true, "managed_only": %t},
		  "tools": {
		    "claude_code": {"otel": true,  "hooks": true},
		    "codex":       {"otel": %t,    "hooks": false},
		    "copilot":     {"otel": true,  "hooks": false},
		    "cursor":      {"otel": false, "hooks": %t}
		  },
		  "discovery_daily_budget": 200
		}`, inventory, flows, managedOnly, codexOTel, cursorHooks))
	}

	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := endpointOf(t, v1.Envelope), section(true, true, false, true, true); got != want {
		t.Fatalf("defaults:\n got %s\nwant %s", got, want)
	}

	audit := store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: "admin@contoso.example", Action: "test", ObjectType: "tenant", ObjectID: tenantA}
	if err := r.store.SetEndpointCollectors(ctx, tenantA, store.EndpointCollectors{
		Inventory: false, Processes: true, Flows: false, OTel: true, Hooks: true, HooksManagedOnly: true,
	}, audit); err != nil {
		t.Fatal(err)
	}
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("changed collectors: version %s, want newer than %s", v2.Version, v1.Version)
	}
	if got, want := endpointOf(t, v2.Envelope), section(false, false, true, true, true); got != want {
		t.Fatalf("tenant row:\n got %s\nwant %s", got, want)
	}

	if err := r.store.SetEndpointTool(ctx, tenantA, "cursor", store.EndpointTool{OTel: false, Hooks: false}, audit); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetEndpointTool(ctx, tenantA, "codex", store.EndpointTool{OTel: false, Hooks: false}, audit); err != nil {
		t.Fatal(err)
	}
	v3, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v3.Version, v2.Version) {
		t.Fatalf("changed tools: version %s, want newer than %s", v3.Version, v2.Version)
	}
	if got, want := endpointOf(t, v3.Envelope), section(false, false, true, false, false); got != want {
		t.Fatalf("tool rows:\n got %s\nwant %s", got, want)
	}
}

// TestComposeInterceptionEnabled: a tenant that never turned TLS inspection on is served
// interception.enabled false, named in the payload; turning it on mints a new version that carries
// true, and the stored row's feature state follows it.
func TestComposeInterceptionEnabled(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	enabledOf := func(envelope []byte) any {
		t.Helper()
		p, _ := payloadOf(t, envelope)
		ic := p["interception"].(map[string]any)
		v, ok := ic["enabled"]
		if !ok {
			t.Fatalf("interception does not name enabled: %v", ic)
		}
		return v
	}
	pacOf := func(envelope []byte) (any, bool) {
		t.Helper()
		p, _ := payloadOf(t, envelope)
		v, ok := p["interception"].(map[string]any)["pac_listen"]
		return v, ok
	}
	featureOf := func() string {
		t.Helper()
		rows := r.store.PolicyBundles(tenantA)
		return string(rows[len(rows)-1].FeatureState)
	}

	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if got := enabledOf(v1.Envelope); got != false {
		t.Fatalf("default interception.enabled = %v, want false", got)
	}
	if got, ok := pacOf(v1.Envelope); ok {
		t.Fatalf("default bundle names pac_listen %v, want it absent", got)
	}
	if got := featureOf(); got != `{"cli_shim":false,"proxy_tls":false}` {
		t.Fatalf("feature state with inspection off = %s", got)
	}

	audit := store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: "admin@contoso.example", Action: "test", ObjectType: "tenant", ObjectID: tenantA}
	if err := r.store.SetTLSInspection(ctx, tenantA, true, audit); err != nil {
		t.Fatal(err)
	}
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("turning inspection on: version %s, want newer than %s", v2.Version, v1.Version)
	}
	if got := enabledOf(v2.Envelope); got != true {
		t.Fatalf("interception.enabled after turning it on = %v, want true", got)
	}
	if got, ok := pacOf(v2.Envelope); !ok || got != policyserve.PACListen {
		t.Fatalf("inspection on: pac_listen = %v (present %v), want %s", got, ok, policyserve.PACListen)
	}
	if got := featureOf(); got != `{"cli_shim":true,"proxy_tls":true}` {
		t.Fatalf("feature state with inspection on = %s", got)
	}

	if err := r.store.SetTLSInspection(ctx, tenantA, false, audit); err != nil {
		t.Fatal(err)
	}
	v3, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v3.Version, v2.Version) || enabledOf(v3.Envelope) != false {
		t.Fatalf("turning inspection off: version %s after %s, enabled %v", v3.Version, v2.Version, enabledOf(v3.Envelope))
	}
	if got, ok := pacOf(v3.Envelope); ok {
		t.Fatalf("inspection off again: pac_listen %v is still named", got)
	}
}

// sectionOf is one top-level member of the served bundle, re-encoded with sorted keys.
func sectionOf(t *testing.T, envelope []byte, name string) string {
	t.Helper()
	p, _ := payloadOf(t, envelope)
	v, ok := p[name]
	if !ok {
		t.Fatalf("the payload has no %s", name)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestComposeRulesAndSanctionedTools: a tenant with neither is served two empty lists, named in the
// payload; its rules are served in its order with every match list, and its sanctioned tools sorted.
// A rule change and a sanction change each mint a new version; un-sanctioning a tool takes it out.
func TestComposeRulesAndSanctionedTools(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	audit := store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: "admin@contoso.example", Action: "test", ObjectType: "tenant", ObjectID: tenantA}

	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if got := sectionOf(t, v1.Envelope, "rules"); got != `[]` {
		t.Fatalf("rules without any = %s, want []", got)
	}
	if got := sectionOf(t, v1.Envelope, "sanctioned_tools"); got != `[]` {
		t.Fatalf("sanctioned_tools without any = %s, want []", got)
	}

	rules := []store.EnforcementRule{
		{RuleID: "block_credentials", Action: "block", Match: store.RuleMatch{Labels: []string{"credential"}},
			Message: "Remove the credential and try again.", Link: "https://intranet.example/ai"},
		{RuleID: "warn_unsanctioned", Action: "warn", Match: store.RuleMatch{Categories: []string{"coding_agent"}, Sanction: []string{"unsanctioned"}},
			Message: "Use the approved coding agent."},
		{RuleID: "allow_rest", Action: "allow", Match: store.RuleMatch{Routes: []string{"tool.hook"}}},
	}
	if err := r.store.ReplaceEnforcementRules(ctx, tenantA, rules, audit); err != nil {
		t.Fatal(err)
	}
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("changed rules: version %s, want newer than %s", v2.Version, v1.Version)
	}
	wantRules := canonical(t, `[
	  {"rule_id":"block_credentials","action":"block","match":{"labels":["credential"],"tools":[],"categories":[],"sanction":[],"routes":[]},
	   "message":"Remove the credential and try again.","link":"https://intranet.example/ai"},
	  {"rule_id":"warn_unsanctioned","action":"warn","match":{"labels":[],"tools":[],"categories":["coding_agent"],"sanction":["unsanctioned"],"routes":[]},
	   "message":"Use the approved coding agent."},
	  {"rule_id":"allow_rest","action":"allow","match":{"labels":[],"tools":[],"categories":[],"sanction":[],"routes":["tool.hook"]},"message":""}]`)
	if got := sectionOf(t, v2.Envelope, "rules"); got != wantRules {
		t.Fatalf("rules:\n got %s\nwant %s", got, wantRules)
	}

	sanction := func(fp, state string) {
		t.Helper()
		if err := r.store.SetToolSanction(ctx, tenantA, fp, state, audit); err != nil {
			t.Fatal(err)
		}
	}
	sanction("app:cursor", "sanctioned")
	sanction("app:claude_code", "sanctioned")
	sanction("app:windsurf", "unsanctioned")
	v3, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v3.Version, v2.Version) {
		t.Fatalf("changed sanctions: version %s, want newer than %s", v3.Version, v2.Version)
	}
	if got := sectionOf(t, v3.Envelope, "sanctioned_tools"); got != `["app:claude_code","app:cursor"]` {
		t.Fatalf("sanctioned_tools = %s", got)
	}
	if again, _ := r.svc.Current(ctx, tenantA); again.Version != v3.Version {
		t.Fatalf("version moved to %s with no change", again.Version)
	}

	sanction("app:cursor", "unknown")
	v4, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v4.Version, v3.Version) || sectionOf(t, v4.Envelope, "sanctioned_tools") != `["app:claude_code"]` {
		t.Fatalf("un-sanctioning: version %s after %s, sanctioned_tools %s", v4.Version, v3.Version, sectionOf(t, v4.Envelope, "sanctioned_tools"))
	}

	// Reordering the same rules is a change too: the first match wins on the device.
	if err := r.store.ReplaceEnforcementRules(ctx, tenantA, []store.EnforcementRule{rules[2], rules[0], rules[1]}, audit); err != nil {
		t.Fatal(err)
	}
	v5, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := payloadOf(t, v5.Envelope)
	first := p["rules"].([]any)[0].(map[string]any)["rule_id"]
	if !newer(v5.Version, v4.Version) || first != "allow_rest" {
		t.Fatalf("reordered rules: version %s after %s, first rule %v", v5.Version, v4.Version, first)
	}
}

// TestComposeKillSwitches: a tenant with no switch tripped is served no kill_switches; a tripped
// switch is served with mode disable, its route, reason and the time it was tripped, in route
// order, and mints a new version; clearing it takes it out again.
func TestComposeKillSwitches(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	at := func(d time.Duration) store.AuditEntry {
		return store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: "admin@contoso.example",
			Action: "tenant.kill_switch.set", ObjectType: "kill_switch", OccurredAt: r.now.Add(d)}
	}

	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := payloadOf(t, v1.Envelope); p["kill_switches"] != nil {
		t.Fatalf("kill_switches without any tripped = %v, want none", p["kill_switches"])
	}

	if err := r.store.SetKillSwitch(ctx, tenantA, "proxy.tls", true, "app_breakage", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetKillSwitch(ctx, tenantA, "proxy.loopback", true, "local_model_breakage", at(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(3 * time.Minute)
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("tripped switches: version %s, want newer than %s", v2.Version, v1.Version)
	}
	want := canonical(t, `[
	  {"provider":"proxy.loopback","mode":"disable","effective_at":"2026-10-05T12:02:00Z","reason_code":"local_model_breakage"},
	  {"provider":"proxy.tls","mode":"disable","effective_at":"2026-10-05T12:01:00Z","reason_code":"app_breakage"}]`)
	if got := sectionOf(t, v2.Envelope, "kill_switches"); got != want {
		t.Fatalf("kill_switches:\n got %s\nwant %s", got, want)
	}

	if err := r.store.SetKillSwitch(ctx, tenantA, "proxy.loopback", false, "", at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetKillSwitch(ctx, tenantA, "proxy.tls", false, "", at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	v3, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := payloadOf(t, v3.Envelope); !newer(v3.Version, v2.Version) || p["kill_switches"] != nil {
		t.Fatalf("cleared switches: version %s after %s, kill_switches %v", v3.Version, v2.Version, p["kill_switches"])
	}
}

// TestComposeCatalog: a deployment without a catalog serves an empty list, named in the payload;
// the catalog is served sorted by app key and each app's signals by platform, kind and value,
// whatever order the store read it in, so re-reading it in another order mints nothing, and a
// changed signal mints a new version.
func TestComposeCatalog(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if got := sectionOf(t, v1.Envelope, "catalog"); got != `[]` {
		t.Fatalf("catalog without any = %s, want []", got)
	}

	cursor := store.CatalogApp{AppKey: "cursor", Category: "ide", Signals: []store.CatalogSignal{
		{Platform: "windows", Kind: "publisher", Value: "Anysphere"},
		{Platform: "macos", Kind: "macos_bundle_id", Value: "com.todesktop.230313mzl4w4u92"},
	}}
	ollama := store.CatalogApp{AppKey: "ollama", Category: "local_runtime", Signals: []store.CatalogSignal{
		{Platform: "windows", Kind: "windows_exe", Value: "ollama.exe"},
		{Platform: "any", Kind: "listen_port", Value: "11434"},
		{Platform: "windows", Kind: "windows_exe", Value: "ollama app.exe"},
	}}
	bare := store.CatalogApp{AppKey: "claude_code", Category: "coding_agent"}
	r.store.SetCatalog(ollama, cursor, bare)
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("a new catalog: version %s, want newer than %s", v2.Version, v1.Version)
	}
	want := canonical(t, `[
	  {"app_key":"claude_code","category":"coding_agent","signals":[]},
	  {"app_key":"cursor","category":"ide","signals":[
	    {"platform":"macos","kind":"macos_bundle_id","value":"com.todesktop.230313mzl4w4u92"},
	    {"platform":"windows","kind":"publisher","value":"Anysphere"}]},
	  {"app_key":"ollama","category":"local_runtime","signals":[
	    {"platform":"any","kind":"listen_port","value":"11434"},
	    {"platform":"windows","kind":"windows_exe","value":"ollama app.exe"},
	    {"platform":"windows","kind":"windows_exe","value":"ollama.exe"}]}]`)
	if got := sectionOf(t, v2.Envelope, "catalog"); got != want {
		t.Fatalf("catalog:\n got %s\nwant %s", got, want)
	}
	// The payload's own bytes carry the order, not only the decoded document.
	_, payload := payloadOf(t, v2.Envelope)
	if i, j := bytes.Index(payload, []byte(`"app_key":"cursor"`)), bytes.Index(payload, []byte(`"app_key":"ollama"`)); i < 0 || j < i {
		t.Fatalf("the served catalog is not in app key order: %s", payload)
	}

	reordered := ollama
	reordered.Signals = []store.CatalogSignal{ollama.Signals[2], ollama.Signals[0], ollama.Signals[1]}
	r.store.SetCatalog(cursor, bare, reordered)
	if again, _ := r.svc.Current(ctx, tenantA); again.Version != v2.Version {
		t.Fatalf("the same catalog in another order moved the version to %s", again.Version)
	}

	changed := cursor
	changed.Signals = []store.CatalogSignal{{Platform: "windows", Kind: "windows_exe", Value: "Cursor.exe"}}
	r.store.SetCatalog(changed, bare, ollama)
	v3, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v3.Version, v2.Version) || !strings.Contains(sectionOf(t, v3.Envelope, "catalog"), `"value":"Cursor.exe"`) {
		t.Fatalf("a changed signal: version %s after %s, catalog %s", v3.Version, v2.Version, sectionOf(t, v3.Envelope, "catalog"))
	}
}

func TestIfNoneMatchAnswers304(t *testing.T) {
	r := newRig(t, nil)
	first := r.get(t, "")
	etag := first.Header().Get("ETag")
	rec := r.get(t, etag)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Fatalf("status %d body %q etag %q", rec.Code, rec.Body, rec.Header().Get("ETag"))
	}
	if rec := r.get(t, `"1"`); rec.Code != http.StatusOK {
		t.Fatalf("a stale ETag got %d, want 200", rec.Code)
	}
}

// TestVersionIsStableUntilAnInputChanges: polling mints nothing; a changed ceiling, collection mode
// or override mints exactly one new, higher version.
func TestVersionIsStableUntilAnInputChanges(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	v1, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		v, err := r.svc.Current(ctx, tenantA)
		if err != nil || v.Version != v1.Version || !bytes.Equal(v.Envelope, v1.Envelope) {
			t.Fatalf("unchanged inputs: version %s (%v), want %s with the same bytes", v.Version, err, v1.Version)
		}
	}
	if n := len(r.store.PolicyBundles(tenantA)); n != 1 {
		t.Fatalf("rows = %d after polling, want 1", n)
	}

	r.store.SetCeiling(tenantA, "m1")
	v2, err := r.svc.Current(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if !newer(v2.Version, v1.Version) {
		t.Fatalf("changed ceiling: version %s, want newer than %s", v2.Version, v1.Version)
	}
	if p, _ := payloadOf(t, v2.Envelope); p["tenant_default_mode"] != "m1" {
		t.Fatalf("mode = %v", p["tenant_default_mode"])
	}
	if again, _ := r.svc.Current(ctx, tenantA); again.Version != v2.Version {
		t.Fatalf("version moved again to %s with no change", again.Version)
	}

	r.store.SetCatalogueHosts("api.anthropic.com")
	v3, _ := r.svc.Current(ctx, tenantA)
	if !newer(v3.Version, v2.Version) {
		t.Fatalf("changed catalogue: version %s, want newer than %s", v3.Version, v2.Version)
	}

	// A rotated key id re-signs the same content as a new version, so a device pinned to the new
	// key is never served a bundle it cannot verify.
	rotated, err := policyserve.New(r.store, signingKey, policyserve.Config{KeyID: "policy-key-2", Now: func() time.Time { return r.now }, RecheckInterval: -1, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	v4, _ := rotated.Current(ctx, tenantA)
	if !newer(v4.Version, v3.Version) {
		t.Fatalf("rotated key: version %s, want newer than %s", v4.Version, v3.Version)
	}
	if n := len(r.store.PolicyBundles(tenantA)); n != 4 {
		t.Fatalf("rows = %d, want 4", n)
	}
}

func newer(a, b string) bool {
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt(b, 10, 64)
	return x > y
}

// TestVersionOrdersAfterAnyEarlierBundle: a previous version above the clock (a bundle minted
// later, or a skewed clock) is still exceeded.
func TestVersionOrdersAfterAnyEarlierBundle(t *testing.T) {
	r := newRig(t, nil)
	r.store.AddPolicyBundle(store.PolicyBundle{TenantID: tenantA, Version: r.now.Unix() + 1000})
	v, err := r.svc.Current(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != strconv.FormatInt(r.now.Unix()+1001, 10) {
		t.Fatalf("version = %s, want one above the legacy row", v.Version)
	}
}

// A stored bundle whose payload carries a field this service no longer composes is replaced by a new
// version rather than served again: the device's strict decoder would refuse it.
func TestAStoredBundleWithRetiredFieldsIsReplaced(t *testing.T) {
	r := newRig(t, nil)
	first, err := r.svc.Current(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	_, payload := payloadOf(t, first.Envelope)
	stale := append([]byte(`{"classifier":{"release_id":"r1","state":"shadow"},`), payload[1:]...)
	envelope, _ := json.Marshal(policyserve.SignedBundle{KeyID: policyserve.DefaultKeyID, Algorithm: "ed25519",
		Payload: stale, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signingKey, stale))})
	v, _ := strconv.ParseInt(first.Version, 10, 64)
	r.store.AddPolicyBundle(store.PolicyBundle{TenantID: tenantA, Version: v + 1, SignatureKID: policyserve.DefaultKeyID, SignedEnvelope: envelope})
	again, err := r.svc.Current(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if again.Version == first.Version || again.Version == strconv.FormatInt(v+1, 10) || bytes.Contains(again.Envelope, []byte("classifier")) {
		t.Fatalf("the stale bundle was served: version %s", again.Version)
	}
}

func TestInactiveOrUnknownTenantIs403(t *testing.T) {
	r := newRig(t, nil)
	r.store.AddTenant(store.Tenant{TenantID: tenantA, Status: "suspended", IngestEnabled: false})
	if rec := r.get(t, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("suspended tenant: status %d", rec.Code)
	}
	if _, err := r.svc.Current(context.Background(), "99999999-9999-4999-8999-999999999999"); !isStatus(err, 403) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestHandlerRefusesUnauthenticatedAndOtherMethods(t *testing.T) {
	r := newRig(t, nil)
	h := r.svc.Handler(func(*http.Request) (string, string, error) {
		return "", "", apierr.New(401, apierr.CodeRevokedDevice, "no credential")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/policy", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.svc.Handler(deviceAuth).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policy", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
	if len(r.store.PolicyBundles(tenantA)) != 0 {
		t.Fatal("a refused request minted a bundle")
	}
}

func TestRecheckIntervalCachesTheServedBundle(t *testing.T) {
	r := newRig(t, func(c *policyserve.Config) { c.RecheckInterval = time.Minute })
	ctx := context.Background()
	v1, _ := r.svc.Current(ctx, tenantA)
	r.store.SetCeiling(tenantA, "m0")
	if v, _ := r.svc.Current(ctx, tenantA); v.Version != v1.Version {
		t.Fatal("the cache was not used inside the interval")
	}
}

func TestSigningKeyForms(t *testing.T) {
	der, err := x509.MarshalPKCS8PrivateKey(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, raw := range map[string][]byte{
		"pkcs8 pem": pemKey,
		"hex seed":  []byte(hex.EncodeToString(signingKey.Seed()) + "\n"),
		"hex 64":    []byte(hex.EncodeToString(signingKey)),
	} {
		k, err := policyserve.ParseSigningKey(raw)
		if err != nil || !bytes.Equal(k, signingKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := append([]byte(nil), signingKey...)
	bad[63] ^= 1
	for name, raw := range map[string][]byte{
		"mismatched 64": []byte(hex.EncodeToString(bad)),
		"short":         []byte("abcd"),
		"rsa pem":       []byte("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"),
	} {
		if _, err := policyserve.ParseSigningKey(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	path := filepath.Join(t.TempDir(), "policy.key")
	if err := os.WriteFile(path, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := policyserve.LoadSigningKey(path); err != nil || policyserve.PublicKeyHex(k) != hex.EncodeToString(signingKey.Public().(ed25519.PublicKey)) {
		t.Fatalf("LoadSigningKey: %v", err)
	}
}

func isStatus(err error, status int) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Status == status
}

// TestServedBundleVerifiesWithTheDevicesVerifier runs device/capture-core/policy's own Verifier
// over a bundle exactly as GET /v1/policy serves it. capture-core is a separate module and
// control-api must not require it, so the test writes a throwaway module that replaces the device
// modules with their paths in this repository and runs it with the local toolchain. It
// catches the one drift a mirror can have: a field the device does not know, which its strict
// decoder would refuse.
func TestServedBundleVerifiesWithTheDevicesVerifier(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a capture-core program; skipped in -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	device := filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "device")
	if _, err := os.Stat(filepath.Join(device, "capture-core", "policy", "verify.go")); err != nil {
		t.Fatalf("capture-core is missing from this repository: %v", err)
	}

	r := newRig(t, nil)
	// Settings off the defaults, so a switch the device decoded as its zero value would show.
	audit := store.AuditEntry{TenantID: tenantA, ActorType: store.ActorUser, ActorID: "admin@contoso.example", Action: "test", ObjectType: "tenant", ObjectID: tenantA}
	if err := r.store.SetEndpointCollectors(context.Background(), tenantA, store.EndpointCollectors{
		Inventory: true, Processes: true, Flows: false, OTel: true, Hooks: true, HooksManagedOnly: true,
	}, audit); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetTLSInspection(context.Background(), tenantA, true, audit); err != nil {
		t.Fatal(err)
	}
	// Rules using every field and match list, and two sanctioned tools.
	if err := r.store.ReplaceEnforcementRules(context.Background(), tenantA, []store.EnforcementRule{
		{RuleID: "block_credentials", Action: "block", Match: store.RuleMatch{Labels: []string{"credential"}, Tools: []string{"app:cursor"},
			Categories: []string{"ide"}, Sanction: []string{"unsanctioned"}, Routes: []string{"proxy.tls", "tool.hook"}},
			Message: "Remove the credential and try again.", Link: "https://intranet.example/ai"},
		{RuleID: "allow.rest", Action: "allow"},
	}, audit); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"app:claude_code", "app:cursor"} {
		if err := r.store.SetToolSanction(context.Background(), tenantA, fp, "sanctioned", audit); err != nil {
			t.Fatal(err)
		}
	}
	// A catalog with an app without signals and one with signals of several platforms and kinds.
	r.store.SetCatalog(
		store.CatalogApp{AppKey: "ollama", Category: "local_runtime", Signals: []store.CatalogSignal{
			{Platform: "windows", Kind: "windows_exe", Value: "ollama app.exe"},
			{Platform: "any", Kind: "listen_port", Value: "11434"},
			{Platform: "windows", Kind: "model_store", Value: `%USERPROFILE%\.ollama\models`},
		}},
		store.CatalogApp{AppKey: "cursor", Category: "ide", Signals: []store.CatalogSignal{
			{Platform: "macos", Kind: "macos_bundle_id", Value: "com.todesktop.230313mzl4w4u92"},
		}},
		store.CatalogApp{AppKey: "continue", Category: "ide_assistant"},
	)
	// Both interception routes' kill switches.
	for _, route := range store.KillSwitchRoutes {
		trip := audit
		trip.OccurredAt = r.now.Add(-time.Minute)
		if err := r.store.SetKillSwitch(context.Background(), tenantA, route, true, "app_breakage", trip); err != nil {
			t.Fatal(err)
		}
	}
	rec := r.get(t, "")
	var resp protocol.PolicyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	abs := func(p string) string { a, _ := filepath.Abs(p); return filepath.ToSlash(a) }
	gomod := "module sacpolicycheck\n\ngo 1.27\n\nrequire github.com/shadow-ai-capture/device/capture-core v0.0.0\n\n" +
		"replace github.com/shadow-ai-capture/device/capture-core => " + abs(filepath.Join(device, "capture-core")) + "\n" +
		"replace github.com/shadow-ai-capture/device/protocol => " + abs(filepath.Join(device, "protocol")) + "\n" +
		"replace github.com/shadow-ai-capture/device/capture-spool => " + abs(filepath.Join(device, "capture-spool")) + "\n"
	program := `package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/shadow-ai-capture/device/capture-core/policy"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	pub, _ := hex.DecodeString(os.Args[2])
	v, err := policy.NewVerifier(os.Args[3], ed25519.PublicKey(pub))
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	store, err := policy.NewStore(v)
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	res := store.Apply(raw)
	b := store.InForce()
	if b == nil {
		fmt.Println("ERR", res.Outcome, res.Cause, res.Err)
		os.Exit(1)
	}
	// The endpoint section, the rules, the sanctioned tools and the catalog as the device decoded
	// them, in the device's JSON names, keys sorted, and two catalog lookups.
	sorted := func(v any) string {
		var doc any
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &doc)
		raw, _ = json.Marshal(doc)
		return string(raw)
	}
	_, killed := b.KillSwitchFor("proxy.loopback")
	fmt.Println("OK", res.Outcome, b.Version, b.TenantDefault, b.Interception.Enabled, len(b.Interception.SeedHosts), b.CLIShim.ProxyAddr, b.Interception.PacListen, b.Intercepts("api.openai.com", 443), sorted(b.Endpoint), sorted(b.Rules), sorted(b.SanctionedTools), sorted(b.Catalog), b.AppsByPort(11434), b.Category("cursor"), sorted(b.KillSwitches), killed)
}
`
	for name, body := range map[string]string{"go.mod": gomod, "main.go": program} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundlePath := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(bundlePath, resp.SignedBundle, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "run", ".", bundlePath, r.svc.PublicKeyHex(), r.svc.KeyID())
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capture-core's verifier refused the served bundle: %v\n%s", err, out)
	}
	// The device's re-encoding of the endpoint section, the rules, the sanctioned tools and the
	// catalog equals the served one: every name matched, and no value was dropped on the way.
	want := "OK accepted " + resp.BundleVersion + " m3 true 3 " + policyserve.DefaultProxyListen + " " + policyserve.PACListen + " true " + endpointOf(t, resp.SignedBundle) +
		" " + sectionOf(t, resp.SignedBundle, "rules") + " " + sectionOf(t, resp.SignedBundle, "sanctioned_tools") +
		" " + sectionOf(t, resp.SignedBundle, "catalog") + " [ollama] ide " + sectionOf(t, resp.SignedBundle, "kill_switches") + " true"
	if !strings.Contains(want, `"link":"https://intranet.example/ai"`) || !strings.Contains(want, `"routes":["proxy.tls","tool.hook"]`) || !strings.Contains(want, `["app:claude_code","app:cursor"]`) ||
		!strings.Contains(want, `{"app_key":"continue","category":"ide_assistant","signals":[]}`) || !strings.Contains(want, `"value":"%USERPROFILE%\\.ollama\\models"`) {
		t.Fatalf("the served bundle does not carry the rules, sanctioned tools and catalog under test: %s", want)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasSuffix(got, want) {
		t.Fatalf("verifier output %q, want %q", got, want)
	}

	// The control: a correctly signed payload with one field the device does not know is refused,
	// so the check above would catch a mirror that drifted.
	_, payload := payloadOf(t, resp.SignedBundle)
	drifted := append([]byte(`{"unknown_policy_field":1,`), payload[1:]...)
	tampered, _ := json.Marshal(map[string]any{"key_id": r.svc.KeyID(), "algorithm": "ed25519",
		"payload": json.RawMessage(drifted), "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(signingKey, drifted))})
	if err := os.WriteFile(bundlePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(goBin, "run", ".", bundlePath, r.svc.PublicKeyHex(), r.svc.KeyID())
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "bundle_schema_invalid") {
		t.Fatalf("a bundle with an unknown field was not refused as schema-invalid: %v\n%s", err, out)
	}
}
