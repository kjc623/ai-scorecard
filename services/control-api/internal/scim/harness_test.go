package scim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/directory"
)

// The tenants every test uses. tenantA is 'clear' and carries the protocol test-vector key, so the
// vectors in device/protocol/userref_test.go come out of the SCIM mapping unchanged; tenantB is
// 'hashed' and mints its own key on first need.
const (
	tenantA = "aaaaaaaa-0000-4000-8000-00000000000a"
	tenantB = "bbbbbbbb-0000-4000-8000-00000000000b"

	vectorKey   = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	vectorUPN   = "u_ed0bf663359a09dab5105ed60a56e835" // upn "Ada.Lovelace@Contoso.com "
	vectorOID   = "u_a504db1d5d49e461d150819d7d9286e8" // oid "6F9619FF-8B86-D011-B42D-00C04FC964FF"
	vectorAcct  = "u_07eff34d88f27c2bd74a0274623831b7" // acct `DESKTOP-01\Kyle`
	vectorOIDID = "6F9619FF-8B86-D011-B42D-00C04FC964FF"
)

type harness struct {
	t       *testing.T
	mem     *Memory
	keys    *directory.UserRefKeys
	cipher  *directory.Cipher
	svc     *Service
	tokens  *Tokens
	srv     *httptest.Server
	clockMu sync.Mutex
	clock   time.Time
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	cipher, err := directory.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemory()
	mem.AddTenant(tenantA, "clear")
	mem.AddTenant(tenantB, "hashed")
	keyStore := &memKeyStore{sealed: map[string][]byte{}}
	key, err := protocol.DecodeUserRefKey(vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.SealBytes(tenantA, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyStore.InitUserRefKey(context.Background(), tenantA, sealed); err != nil {
		t.Fatal(err)
	}
	keys, err := directory.NewUserRefKeys(keyStore, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://app.example.test/scim/v2"
	}
	svc, err := NewService(mem, keys, cipher, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, mem: mem, keys: keys, cipher: cipher, svc: svc, tokens: NewTokens(mem),
		clock: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}
	// A clock that moves, so created/lastModified and paging order are distinct and stable.
	tick := func() time.Time {
		h.clockMu.Lock()
		defer h.clockMu.Unlock()
		h.clock = h.clock.Add(time.Second)
		return h.clock
	}
	svc.Now = tick
	h.tokens.Now = tick
	svc.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	handler := NewHandler(svc, "/scim/v2", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux.Handle("/scim/v2", handler)
	mux.Handle("/scim/v2/", handler)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// token mints a SCIM token for tenant through the admin path, as the deployment page would.
func (h *harness) token(tenant string) (string, string) {
	h.t.Helper()
	id, token, err := h.tokens.Create(context.Background(), tenant, "test", "admin@example.test")
	if err != nil {
		h.t.Fatalf("create token: %v", err)
	}
	return token, id
}

// do sends one request. body may be a string (sent verbatim, for the providers' exact payloads) or
// a value (encoded). The decoded response is nil for an empty body.
func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/scim+json")
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if len(bytes.TrimSpace(raw)) == 0 {
		return res.StatusCode, nil
	}
	out, err := decodeObject(raw)
	if err != nil {
		h.t.Fatalf("%s %s: response is not a JSON object: %s", method, path, raw)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/scim+json") {
		h.t.Fatalf("%s %s: content type %q", method, path, ct)
	}
	return res.StatusCode, out
}

func (h *harness) mustDo(want int, method, path, token string, body any) map[string]any {
	h.t.Helper()
	got, out := h.do(method, path, token, body)
	if got != want {
		h.t.Fatalf("%s %s = %d, want %d: %v", method, path, got, want, out)
	}
	return out
}

func filterPath(collection, filter string, extra ...string) string {
	q := url.Values{"filter": {filter}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return "/scim/v2/" + collection + "?" + q.Encode()
}

func (h *harness) storedUser(tenant, id string) UserRow {
	h.t.Helper()
	for _, u := range h.mem.StoredUsers(tenant) {
		if u.ID == id {
			return u
		}
	}
	h.t.Fatalf("no stored user %s", id)
	return UserRow{}
}

func (h *harness) dim(tenant, ref string) UserDimRow {
	h.t.Helper()
	row, ok := h.mem.UserDim(tenant, ref)
	if !ok {
		h.t.Fatalf("no ops.user_dim row for %s", ref)
	}
	return row
}

func (h *harness) ref(tenant string, kind protocol.UserRefKind, value string) string {
	h.t.Helper()
	key, err := h.keys.Key(context.Background(), tenant)
	if err != nil {
		h.t.Fatal(err)
	}
	ref, err := protocol.DeriveUserRef(key, kind, value)
	if err != nil {
		h.t.Fatal(err)
	}
	return ref
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func resources(t *testing.T, list map[string]any) []map[string]any {
	t.Helper()
	raw, ok := list["Resources"].([]any)
	if !ok {
		t.Fatalf("list response has no Resources array: %v", list)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func total(list map[string]any) int {
	n, _ := list["totalResults"].(json.Number).Int64()
	return int(n)
}

// doHeader sends a bodiless request with a raw Authorization header (or none).
func (h *harness) doHeader(method, path, authorization string) (int, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out, _ := decodeObject(raw)
	return res.StatusCode, out
}

// memKeyStore is a map-backed directory.UserRefKeyStore that accepts any tenant.
type memKeyStore struct {
	mu     sync.Mutex
	sealed map[string][]byte
}

func (m *memKeyStore) SealedUserRefKey(_ context.Context, tenantID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.sealed[tenantID]...), nil
}

func (m *memKeyStore) InitUserRefKey(_ context.Context, tenantID string, sealed []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sealed[tenantID]) == 0 {
		m.sealed[tenantID] = append([]byte(nil), sealed...)
	}
	return append([]byte(nil), m.sealed[tenantID]...), nil
}
