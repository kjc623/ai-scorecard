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
	accounts  map[string]string
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
		},
		connected: map[string]string{}, disabled: map[string]bool{}, keys: map[string][]byte{}, accounts: map[string]string{},
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
			out[i].Connection = &ConnectionState{ConnectionID: id, Account: s.accounts[p.Provider], Status: status}
		}
	}
	return out, nil
}

func (s *adminStore) EntraConnected(context.Context, string) (bool, error) { return s.entra, nil }

func (s *adminStore) Connect(_ context.Context, _, provider, account string, sealed []byte, a Audit) (string, error) {
	if provider == Microsoft365Copilot && !s.entra {
		return "", ErrNoEntra
	}
	id := "c0000000-0000-4000-8000-0000000000" + map[string]string{ChatGPTEnterprise: "01", Microsoft365Copilot: "02", ClaudeEnterprise: "03"}[provider]
	s.connected[provider], s.keys[provider], s.accounts[provider] = id, sealed, account
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
	s.keys[provider] = nil
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

type adminRig struct {
	st   *adminStore
	pull *pullStub
	mux  *http.ServeMux
	role string
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	r := &adminRig{st: newAdminStore(), pull: &pullStub{available: map[string]bool{ChatGPTEnterprise: true, ClaudeEnterprise: true, Microsoft365Copilot: true}}, role: deploy.RoleAdmin}
	auth := func(req *http.Request) (deploy.Principal, error) {
		if req.Header.Get("Authorization") == "" {
			return deploy.Principal{}, errors.New("no bearer token")
		}
		return deploy.Principal{Tenant: adminTenant, Actor: "admin@contoso.example", Subject: "sub-1", Roles: []string{r.role}}, nil
	}
	h, err := NewHandler(r.st, auth, sealer{}, r.pull, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if len(list) != 3 || body["entra_connected"] != false {
		t.Fatalf("list %v", body)
	}
	byName := map[string]map[string]any{}
	for _, p := range list {
		m := p.(map[string]any)
		byName[m["provider"].(string)] = m
	}
	if byName[Microsoft365Copilot]["available"] != false || byName[ChatGPTEnterprise]["available"] != true {
		t.Fatalf("availability %v", list)
	}
	if byName[ChatGPTEnterprise]["connection"] != nil {
		t.Fatal("an unconnected provider shows a connection")
	}
	c := byName[ClaudeEnterprise]["connection"].(map[string]any)
	if c["account"] != "org-1" || c["status"] != "active" {
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
