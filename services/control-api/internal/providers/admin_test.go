package providers

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

	"github.com/shadow-ai-capture/control-api/internal/deploy"
)

const adminTenant = "6f1c2a52-58f4-4a51-9b1e-1f6d2f0a0001"

// adminStore holds the providers and the tenant's connections in memory.
type adminStore struct {
	providers []Provider
	entra     bool
	connected map[string]string // provider -> connection id
	disabled  map[string]bool
	keys      map[string][]byte
	hashes    map[string]string
	accounts  map[string]string
	every     map[string]int
	started   map[string]time.Time
	audits    []Audit
}

func newAdminStore() *adminStore {
	return &adminStore{
		providers: []Provider{
			{Provider: ChatGPTEnterprise, DisplayName: "ChatGPT Enterprise", Vendor: "openai", Credential: "api_key",
				CredentialLabel: "Compliance API key", AccountLabel: "Workspace ID", Tools: []Tool{{App: "chatgpt_web", Name: "ChatGPT (web)"}}},
			{Provider: Microsoft365Copilot, DisplayName: "Microsoft 365 Copilot", Vendor: "microsoft", Credential: "entra_app",
				Tools: []Tool{{App: "m365_copilot", Name: "Microsoft 365 Copilot"}}},
			{Provider: ClaudeEnterprise, DisplayName: "Claude Enterprise", Vendor: "anthropic", Credential: "api_key",
				CredentialLabel: "Compliance API key", AccountLabel: "Organization ID"},
			{Provider: GeminiCodeAssist, DisplayName: "Gemini Code Assist", Vendor: "google", Credential: "intake",
				Tools: []Tool{{App: "gemini_code_assist", Name: "Gemini Code Assist"}}},
		},
		connected: map[string]string{}, disabled: map[string]bool{}, keys: map[string][]byte{}, hashes: map[string]string{},
		accounts: map[string]string{}, every: map[string]int{}, started: map[string]time.Time{},
	}
}

func (s *adminStore) Providers(context.Context, string) ([]Provider, error) {
	out := make([]Provider, len(s.providers))
	copy(out, s.providers)
	for i, p := range out {
		if id, ok := s.connected[p.Provider]; ok {
			status := "active"
			if s.disabled[p.Provider] {
				status = "disabled"
			}
			every := s.every[p.Provider]
			if every == 0 {
				every = DefaultPullEvery
			}
			c := &ConnectionState{ConnectionID: id, Account: s.accounts[p.Provider], Status: status, PullEveryMinutes: every}
			if at, ok := s.started[p.Provider]; ok {
				c.LastStartedAt = &at
			}
			out[i].Connection = c
		}
	}
	return out, nil
}

func (s *adminStore) EntraConnected(context.Context, string) (bool, error) { return s.entra, nil }

func (s *adminStore) Connect(_ context.Context, _, provider, account string, sealed []byte, tokenHash string, a Audit) (string, error) {
	if provider == Microsoft365Copilot && !s.entra {
		return "", ErrNoEntra
	}
	id := "c0000000-0000-4000-8000-0000000000" + map[string]string{ChatGPTEnterprise: "01", Microsoft365Copilot: "02",
		ClaudeEnterprise: "03", GeminiCodeAssist: "04"}[provider]
	s.connected[provider], s.keys[provider], s.hashes[provider], s.accounts[provider] = id, sealed, tokenHash, account
	delete(s.disabled, provider)
	a.ObjectID = id
	s.audits = append(s.audits, a)
	return id, nil
}

func (s *adminStore) Disconnect(_ context.Context, _, provider string, a Audit) error {
	if _, ok := s.connected[provider]; !ok || s.disabled[provider] {
		return ErrNotFound
	}
	s.disabled[provider] = true
	s.keys[provider], s.hashes[provider] = nil, ""
	s.audits = append(s.audits, a)
	return nil
}

func (s *adminStore) Connection(_ context.Context, _, provider string) (string, bool, error) {
	id, ok := s.connected[provider]
	if !ok {
		return "", false, ErrNotFound
	}
	return id, !s.disabled[provider], nil
}

func (s *adminStore) Schedule(_ context.Context, _, provider string, minutes int, a Audit) error {
	id, ok := s.connected[provider]
	if !ok || s.disabled[provider] {
		return ErrNotFound
	}
	s.every[provider] = minutes
	a.ObjectID = id
	s.audits = append(s.audits, a)
	return nil
}

func (s *adminStore) Audit(_ context.Context, _ string, a Audit) error {
	s.audits = append(s.audits, a)
	return nil
}

type sealer struct{}

func (sealer) Seal(tenant, plaintext string) ([]byte, error) {
	return []byte("sealed(" + tenant + "):" + plaintext), nil
}

type pullStub struct {
	available map[string]bool
	triggered []string
}

func (p *pullStub) Available(provider string) bool { return p.available[provider] }

func (p *pullStub) Trigger(_, connectionID string) bool {
	p.triggered = append(p.triggered, connectionID)
	return true
}

// intakeStub is an intake receiving Gemini Code Assist's log by Pub/Sub.
type intakeStub struct{ available map[string]bool }

func (i *intakeStub) Available(provider string) bool { return i.available[provider] }

func (i *intakeStub) Kind(provider string) string {
	if i.available[provider] {
		return KindPubSub
	}
	return ""
}

func (*intakeStub) URL(provider, connectionID string) string {
	return "https://app.contoso.example/intake/v1/" + provider + "/" + connectionID
}

type adminRig struct {
	st     *adminStore
	pull   *pullStub
	intake *intakeStub
	mux    *http.ServeMux
	role   string
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	r := &adminRig{st: newAdminStore(), pull: &pullStub{available: map[string]bool{ChatGPTEnterprise: true, ClaudeEnterprise: true, Microsoft365Copilot: true}},
		intake: &intakeStub{available: map[string]bool{GeminiCodeAssist: true}}, role: deploy.RoleAdmin}
	auth := func(req *http.Request) (deploy.Principal, error) {
		if req.Header.Get("Authorization") == "" {
			return deploy.Principal{}, errors.New("no bearer token")
		}
		return deploy.Principal{Tenant: adminTenant, Actor: "admin@contoso.example", Subject: "sub-1", Roles: []string{r.role}}, nil
	}
	h, err := NewHandler(r.st, auth, sealer{}, r.pull, r.intake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.mux = http.NewServeMux()
	h.Register(r.mux)
	return r
}

func (r *adminRig) do(method, path, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTheListIsEveryProviderWithWhatThisDeploymentReads(t *testing.T) {
	r := newAdminRig(t)
	r.pull.available[Microsoft365Copilot] = false
	r.st.connected[ClaudeEnterprise], r.st.accounts[ClaudeEnterprise] = "c0000000-0000-4000-8000-000000000003", "org-1"
	code, body := r.do(http.MethodGet, "/admin/v1/providers", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	list, _ := body["providers"].([]any)
	if len(list) != 4 || body["entra_connected"] != false {
		t.Fatalf("list %v", body)
	}
	byName := map[string]map[string]any{}
	for _, p := range list {
		m := p.(map[string]any)
		byName[m["provider"].(string)] = m
	}
	if byName[Microsoft365Copilot]["available"] != false || byName[ChatGPTEnterprise]["available"] != true ||
		byName[GeminiCodeAssist]["available"] != true {
		t.Fatalf("availability %v", list)
	}
	if byName[GeminiCodeAssist]["intake"] != KindPubSub || byName[ChatGPTEnterprise]["intake"] != "" {
		t.Fatalf("intake kinds %v", list)
	}
	if byName[ChatGPTEnterprise]["connection"] != nil {
		t.Fatal("an unconnected provider shows a connection")
	}
	c := byName[ClaudeEnterprise]["connection"].(map[string]any)
	if c["account"] != "org-1" || c["status"] != "active" || c["intake_url"] != "" {
		t.Fatalf("connection %v", c)
	}
	if strings.Contains(strings.ToLower(jsonOf(body)), "key\":\"") {
		t.Fatal("the list carries a key")
	}
}

func TestConnectingSealsTheKeyAuditsWithoutItAndPullsAtOnce(t *testing.T) {
	r := newAdminRig(t)
	code, body := r.do(http.MethodPut, "/admin/v1/providers/chatgpt_enterprise", `{"account":" ws-1 ","key":" sk-admin-123 "}`)
	if code != http.StatusOK || body["connection_id"] != "c0000000-0000-4000-8000-000000000001" || body["queued"] != true {
		t.Fatalf("connect = %d %v", code, body)
	}
	if string(r.st.keys[ChatGPTEnterprise]) != "sealed("+adminTenant+"):sk-admin-123" || r.st.accounts[ChatGPTEnterprise] != "ws-1" {
		t.Fatalf("stored %q for account %q", r.st.keys[ChatGPTEnterprise], r.st.accounts[ChatGPTEnterprise])
	}
	if len(r.pull.triggered) != 1 || r.pull.triggered[0] != "c0000000-0000-4000-8000-000000000001" {
		t.Fatalf("triggered %v", r.pull.triggered)
	}
	a := r.st.audits[0]
	if a.Action != "provider.connect" || a.Actor != "admin@contoso.example" || a.ObjectType != "provider_connection" ||
		a.Detail["provider"] != ChatGPTEnterprise || a.Detail["account"] != "ws-1" || strings.Contains(jsonOf(a.Detail), "sk-admin") {
		t.Fatalf("audit %+v", a)
	}
}

func TestAConnectionNeedsWhatItsProviderAsksFor(t *testing.T) {
	r := newAdminRig(t)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/admin/v1/providers/chatgpt_enterprise", `{"account":"ws-1"}`, http.StatusBadRequest},
		{"/admin/v1/providers/chatgpt_enterprise", `{"key":"k"}`, http.StatusBadRequest},
		{"/admin/v1/providers/chatgpt_enterprise", `{"account":"ws/../1","key":"k"}`, http.StatusBadRequest},
		{"/admin/v1/providers/chatgpt_enterprise", `{"account":"ws-1","key":"two words"}`, http.StatusBadRequest},
		{"/admin/v1/providers/chatgpt_enterprise", `{"account":"ws-1","key":"k","extra":1}`, http.StatusBadRequest},
		{"/admin/v1/providers/microsoft_365_copilot", `{"key":"k"}`, http.StatusBadRequest},
		{"/admin/v1/providers/microsoft_365_copilot", `{}`, http.StatusConflict},
		{"/admin/v1/providers/gemini_enterprise", `{}`, http.StatusNotFound},
	} {
		if code, body := r.do(http.MethodPut, c.path, c.body); code != c.want {
			t.Errorf("PUT %s %s = %d %v, want %d", c.path, c.body, code, body, c.want)
		}
	}
	r.st.entra = true
	if code, _ := r.do(http.MethodPut, "/admin/v1/providers/microsoft_365_copilot", ``); code != http.StatusOK {
		t.Fatalf("Copilot with Entra connected = %d", code)
	}
	if r.st.keys[Microsoft365Copilot] != nil {
		t.Fatal("Copilot stored a key")
	}
	r.pull.available[ClaudeEnterprise] = false
	if code, _ := r.do(http.MethodPut, "/admin/v1/providers/claude_enterprise", `{"account":"org-1","key":"k"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("an unavailable provider = %d", code)
	}
}

func TestDisconnectingAndPullingNow(t *testing.T) {
	r := newAdminRig(t)
	if code, _ := r.do(http.MethodPost, "/admin/v1/providers/claude_enterprise/run", ""); code != http.StatusConflict {
		t.Fatalf("run before connecting = %d", code)
	}
	if code, _ := r.do(http.MethodDelete, "/admin/v1/providers/claude_enterprise", ""); code != http.StatusNotFound {
		t.Fatalf("disconnect before connecting = %d", code)
	}
	r.do(http.MethodPut, "/admin/v1/providers/claude_enterprise", `{"account":"org-1","key":"sk-ant-api01-x"}`)
	if code, body := r.do(http.MethodPost, "/admin/v1/providers/claude_enterprise/run", ""); code != http.StatusAccepted || body["queued"] != true {
		t.Fatalf("run = %d %v", code, body)
	}
	// A provider this deployment no longer reads can still be disconnected.
	r.pull.available[ClaudeEnterprise] = false
	if code, _ := r.do(http.MethodDelete, "/admin/v1/providers/claude_enterprise", ""); code != http.StatusNoContent {
		t.Fatalf("disconnect = %d", code)
	}
	if r.st.keys[ClaudeEnterprise] != nil || !r.st.disabled[ClaudeEnterprise] {
		t.Fatal("the key outlived the connection")
	}
	r.pull.available[ClaudeEnterprise] = true
	if code, _ := r.do(http.MethodPost, "/admin/v1/providers/claude_enterprise/run", ""); code != http.StatusConflict {
		t.Fatalf("run while disconnected = %d", code)
	}
	var actions []string
	for _, a := range r.st.audits {
		actions = append(actions, a.Action+":"+a.ObjectID)
	}
	if want := "provider.connect:c0000000-0000-4000-8000-000000000003 provider.run:c0000000-0000-4000-8000-000000000003 provider.disconnect:"; strings.Join(actions, " ") != want {
		t.Fatalf("audits %v", actions)
	}
}

func TestAnAdminChoosesHowOftenAConnectionIsPulled(t *testing.T) {
	r := newAdminRig(t)
	put := func(provider, body string) (int, map[string]any) {
		return r.do(http.MethodPut, "/admin/v1/providers/"+provider+"/schedule", body)
	}
	if code, _ := put(ClaudeEnterprise, `{"pull_every_minutes":360}`); code != http.StatusConflict {
		t.Fatalf("scheduling before connecting = %d", code)
	}
	r.do(http.MethodPut, "/admin/v1/providers/claude_enterprise", `{"account":"org-1","key":"sk-ant-api01-x"}`)
	_, list := r.do(http.MethodGet, "/admin/v1/providers", "")
	if jsonOf(list["pull_every_choices"]) != "[15,30,60,360,1440]" {
		t.Fatalf("choices %v", list["pull_every_choices"])
	}
	connection := func(provider string) map[string]any {
		t.Helper()
		_, list := r.do(http.MethodGet, "/admin/v1/providers", "")
		for _, p := range list["providers"].([]any) {
			if m := p.(map[string]any); m["provider"] == provider {
				c, _ := m["connection"].(map[string]any)
				return c
			}
		}
		return nil
	}
	if c := connection(ClaudeEnterprise); c["pull_every_minutes"] != float64(60) || c["next_pull_at"] != nil {
		t.Fatalf("a connection never pulled: %v", c)
	}

	for _, body := range []string{`{"pull_every_minutes":45}`, `{"pull_every_minutes":0}`, `{}`, `{"pull_every_minutes":"60"}`, `{"pull_every_minutes":60,"x":1}`} {
		if code, _ := put(ClaudeEnterprise, body); code != http.StatusBadRequest {
			t.Errorf("schedule %s = %d, want 400", body, code)
		}
	}
	if code, body := put(ClaudeEnterprise, `{"pull_every_minutes":360}`); code != http.StatusNoContent {
		t.Fatalf("schedule = %d %v", code, body)
	}
	r.st.started[ClaudeEnterprise] = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	if c := connection(ClaudeEnterprise); c["pull_every_minutes"] != float64(360) || c["next_pull_at"] != "2026-10-17T15:00:00Z" {
		t.Fatalf("after scheduling: %v", c)
	}
	a := r.st.audits[len(r.st.audits)-1]
	if a.Action != "provider.schedule" || a.ObjectID != "c0000000-0000-4000-8000-000000000003" ||
		a.Detail["provider"] != ClaudeEnterprise || a.Detail["pull_every_minutes"] != 360 || a.Actor != "admin@contoso.example" {
		t.Fatalf("audit %+v", a)
	}

	// An intake is sent as it is written, so it has no schedule.
	r.do(http.MethodPut, "/admin/v1/providers/gemini_code_assist", `{}`)
	if code, _ := put(GeminiCodeAssist, `{"pull_every_minutes":60}`); code != http.StatusConflict {
		t.Fatalf("scheduling an intake = %d", code)
	}
	if c := connection(GeminiCodeAssist); c["next_pull_at"] != nil {
		t.Fatalf("an intake shows a next pull: %v", c)
	}
	r.do(http.MethodDelete, "/admin/v1/providers/claude_enterprise", "")
	if code, _ := put(ClaudeEnterprise, `{"pull_every_minutes":60}`); code != http.StatusConflict {
		t.Fatalf("scheduling a disconnected provider = %d", code)
	}
	if c := connection(ClaudeEnterprise); c["next_pull_at"] != nil {
		t.Fatalf("a disconnected provider shows a next pull: %v", c)
	}
}

func TestConnectingAnIntakeIssuesItsTokenOnce(t *testing.T) {
	r := newAdminRig(t)
	for _, body := range []string{`{"key":"k"}`, `{"account":"proj-1"}`} {
		if code, _ := r.do(http.MethodPut, "/admin/v1/providers/gemini_code_assist", body); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, code)
		}
	}
	code, body := r.do(http.MethodPut, "/admin/v1/providers/gemini_code_assist", `{}`)
	const id = "c0000000-0000-4000-8000-000000000004"
	token, _ := body["intake_token"].(string)
	if code != http.StatusOK || body["connection_id"] != id || body["queued"] != false ||
		body["intake_url"] != "https://app.contoso.example/intake/v1/gemini_code_assist/"+id || !strings.HasPrefix(token, IntakeTokenPrefix) {
		t.Fatalf("connect = %d %v", code, body)
	}
	if r.st.hashes[GeminiCodeAssist] != HashIntakeToken(token) || r.st.keys[GeminiCodeAssist] != nil {
		t.Fatalf("stored hash %q, key %q", r.st.hashes[GeminiCodeAssist], r.st.keys[GeminiCodeAssist])
	}
	if len(r.pull.triggered) != 0 {
		t.Fatalf("an intake was pulled: %v", r.pull.triggered)
	}
	if a := r.st.audits[0]; a.Detail["intake_token"] != "issued" || strings.Contains(jsonOf(a), token) {
		t.Fatalf("audit %+v", a)
	}

	_, list := r.do(http.MethodGet, "/admin/v1/providers", "")
	if strings.Contains(jsonOf(list), token) || strings.Contains(jsonOf(list), "sha256:") {
		t.Fatal("the list carries the token")
	}
	for _, p := range list["providers"].([]any) {
		if m := p.(map[string]any); m["provider"] == GeminiCodeAssist {
			if c := m["connection"].(map[string]any); c["intake_url"] != body["intake_url"] {
				t.Fatalf("connection %v", c)
			}
		}
	}
	if code, _ := r.do(http.MethodPost, "/admin/v1/providers/gemini_code_assist/run", ""); code != http.StatusConflict {
		t.Fatalf("pulling an intake = %d", code)
	}

	_, again := r.do(http.MethodPut, "/admin/v1/providers/gemini_code_assist", ``)
	if next, _ := again["intake_token"].(string); next == token || r.st.hashes[GeminiCodeAssist] != HashIntakeToken(next) {
		t.Fatal("connecting again did not replace the token")
	}
	if code, _ := r.do(http.MethodDelete, "/admin/v1/providers/gemini_code_assist", ""); code != http.StatusNoContent || r.st.hashes[GeminiCodeAssist] != "" {
		t.Fatalf("disconnect = %d, hash %q", code, r.st.hashes[GeminiCodeAssist])
	}
	_, list = r.do(http.MethodGet, "/admin/v1/providers", "")
	for _, p := range list["providers"].([]any) {
		if m := p.(map[string]any); m["provider"] == GeminiCodeAssist {
			if c := m["connection"].(map[string]any); c["intake_url"] != "" {
				t.Fatalf("a disabled intake shows its URL: %v", c)
			}
		}
	}
	r.intake.available[GeminiCodeAssist] = false
	if code, _ := r.do(http.MethodPut, "/admin/v1/providers/gemini_code_assist", `{}`); code != http.StatusServiceUnavailable {
		t.Fatalf("an intake this deployment does not receive = %d", code)
	}
}

func TestEveryRouteNeedsAnAdmin(t *testing.T) {
	r := newAdminRig(t)
	r.role = "analyst"
	if code, _ := r.do(http.MethodGet, "/admin/v1/providers", ""); code != http.StatusForbidden {
		t.Fatalf("analyst = %d", code)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/providers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
