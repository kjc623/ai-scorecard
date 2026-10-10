package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/auth/authtest"
	"github.com/shadow-ai-capture/content-vault/internal/httpapi"
	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/store/storetest"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

const (
	tenantID     = "7d3c6a52-0b8e-4f0e-9a51-2a4c1f6b9e01"
	eventID      = "0f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	grantID      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	submissionID = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	deviceID     = "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f"
)

type rig struct {
	t      *testing.T
	iss    *authtest.Issuer
	store  *storetest.Fake
	server *httptest.Server
	ready  error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, iss: authtest.New(t), store: storetest.New()}
	r.store.PutTenant(store.Tenant{TenantID: tenantID, Status: "active", ContentSearch: store.SearchFullText, IngestEnabled: true, ReadEnabled: true})
	r.store.PutSubmission(storetest.Submission{TenantID: tenantID, SubmissionID: submissionID, PromptKind: "user", ReceivedAt: time.Now().Add(-time.Minute)})
	r.store.PutGrant(tenantID, store.Grant{GrantID: grantID, EventID: eventID, DeviceID: deviceID, SubmissionID: submissionID, Decision: "granted", ExpiresAt: time.Now().Add(time.Hour)})
	keys, err := keyring.Parse("v1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := vault.New(vault.Config{Store: r.store, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	ready := func(context.Context) error { return r.ready }
	r.server = httptest.NewServer(httpapi.New(svc, r.iss.Verifier(t), ready, nil).Handler())
	t.Cleanup(r.server.Close)
	return r
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("response %d is not JSON: %q", r.status, r.body)
	}
	return out
}

func (r response) code(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (r *rig) do(method, path, token string, body []byte, headers map[string]string) response {
	r.t.Helper()
	req, err := http.NewRequest(method, r.server.URL+path, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func uploadPath() string { return "/internal/v1/tenants/" + tenantID + "/content/" + eventID }

func uploadHeaders(body []byte) map[string]string {
	return map[string]string{protocol.HeaderContentGrantID: grantID, protocol.HeaderContentRawDigest: protocol.RawDigest(body), "Content-Type": "application/octet-stream"}
}

func (r *rig) upload(token string, body []byte) response {
	r.t.Helper()
	return r.do(http.MethodPut, uploadPath(), token, body, uploadHeaders(body))
}

func TestUploadIsAuthorisedOnlyForControlAPIsServiceToken(t *testing.T) {
	r := newRig(t)
	body := []byte("prompt")
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	wrongAud := r.iss.Claims()
	wrongAud["aud"], wrongAud["svc"], wrongAud["sub"] = "sac-query", "control-api", "control-api"
	wrongIss := r.iss.Claims()
	wrongIss["iss"], wrongIss["svc"] = "https://elsewhere.example", "control-api"
	forged := r.iss.Claims()
	forged["svc"] = "control-api"

	cases := map[string]struct {
		token  string
		status int
	}{
		"no token":            {"", http.StatusUnauthorized},
		"not a JWT":           {"opaque-token", http.StatusUnauthorized},
		"wrong audience":      {r.iss.Sign(t, wrongAud), http.StatusUnauthorized},
		"wrong issuer":        {r.iss.Sign(t, wrongIss), http.StatusUnauthorized},
		"forged signature":    {authtest.SignWith(t, other, "test-key-1", forged), http.StatusUnauthorized},
		"another service":     {r.iss.Service(t, "query-api"), http.StatusForbidden},
		"a person's token":    {r.iss.Person(t, tenantID, "alice", "admin", "content_reader"), http.StatusForbidden},
		"control-api service": {r.iss.Service(t, "control-api"), http.StatusCreated},
	}
	for _, name := range []string{"no token", "not a JWT", "wrong audience", "wrong issuer", "forged signature", "another service", "a person's token", "control-api service"} {
		tc := cases[name]
		resp := r.upload(tc.token, body)
		if resp.status != tc.status {
			t.Errorf("%s: status %d (%s), want %d", name, resp.status, resp.body, tc.status)
		}
		if tc.status != http.StatusCreated && len(r.store.Content(tenantID)) != 0 {
			t.Fatalf("%s: content was stored", name)
		}
	}
	if n := len(r.store.Content(tenantID)); n != 1 {
		t.Fatalf("%d objects stored, want the one authorised upload", n)
	}
}

func TestUploadAnswers(t *testing.T) {
	r := newRig(t)
	token := r.iss.Service(t, "control-api")
	body := []byte(`{"prompt":"merger","attachments":[{"name":"plan.pdf"}]}`)

	first := r.upload(token, body)
	if first.status != http.StatusCreated {
		t.Fatalf("first upload: %d %s", first.status, first.body)
	}
	got := first.json(t)
	indexed, _ := got["indexed"].(map[string]any)
	if got["state"] != "stored" || got["event_id"] != eventID || got["grant_id"] != grantID || got["raw_digest"] != protocol.RawDigest(body) ||
		got["size_bytes"] != float64(len(body)) || got["replayed"] != false || indexed["prompt_body"] != float64(1) || indexed["attachment_name"] != float64(1) {
		t.Fatalf("first upload answer = %v", got)
	}

	retry := r.upload(token, body)
	if retry.status != http.StatusOK || retry.json(t)["replayed"] != true || retry.json(t)["object_id"] != got["object_id"] {
		t.Fatalf("retry: %d %s, want 200 replayed with the same object", retry.status, retry.body)
	}

	different := []byte("something else")
	if resp := r.upload(token, different); resp.status != http.StatusConflict || resp.code(t) != "already_stored" {
		t.Fatalf("a different body under the used grant: %d %s", resp.status, resp.body)
	}
}

func TestUploadRequestValidation(t *testing.T) {
	r := newRig(t)
	token := r.iss.Service(t, "control-api")
	body := []byte("prompt")
	cases := map[string]struct {
		path    string
		headers map[string]string
		status  int
	}{
		"tenant not a uuid":      {"/internal/v1/tenants/acme/content/" + eventID, uploadHeaders(body), http.StatusBadRequest},
		"event not a uuid":       {"/internal/v1/tenants/" + tenantID + "/content/e1", uploadHeaders(body), http.StatusBadRequest},
		"no grant header":        {uploadPath(), map[string]string{protocol.HeaderContentRawDigest: protocol.RawDigest(body)}, http.StatusBadRequest},
		"no digest header":       {uploadPath(), map[string]string{protocol.HeaderContentGrantID: grantID}, http.StatusBadRequest},
		"malformed digest":       {uploadPath(), map[string]string{protocol.HeaderContentGrantID: grantID, protocol.HeaderContentRawDigest: "md5:abc"}, http.StatusBadRequest},
		"digest of another body": {uploadPath(), map[string]string{protocol.HeaderContentGrantID: grantID, protocol.HeaderContentRawDigest: protocol.RawDigest([]byte("x"))}, http.StatusBadRequest},
	}
	for name, tc := range cases {
		if resp := r.do(http.MethodPut, tc.path, token, body, tc.headers); resp.status != tc.status {
			t.Errorf("%s: %d %s, want %d", name, resp.status, resp.body, tc.status)
		}
	}
	if len(r.store.Content(tenantID)) != 0 || !r.store.Grant(tenantID, grantID).UsedAt.IsZero() {
		t.Fatal("a malformed upload stored content or used the grant")
	}
}

func TestAnOversizedUploadIsRefusedBeforeItIsRead(t *testing.T) {
	r := newRig(t)
	big := make([]byte, protocol.MaxContentObjectBytes+1)
	resp := r.upload(r.iss.Service(t, "control-api"), big)
	if resp.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.status)
	}

	// Without a Content-Length the body is cut off at the limit as it is read.
	req, _ := http.NewRequest(http.MethodPut, r.server.URL+uploadPath(), io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+r.iss.Service(t, "control-api"))
	for k, v := range uploadHeaders(big) {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize: status %d, want 413", res.StatusCode)
	}
}

func TestUploadGrantRefusalsCarryTheirCode(t *testing.T) {
	r := newRig(t)
	g := r.store.Grant(tenantID, grantID)
	g.Decision = "denied"
	r.store.PutGrant(tenantID, g)
	resp := r.upload(r.iss.Service(t, "control-api"), []byte("x"))
	if resp.status != http.StatusForbidden || resp.code(t) != "grant_not_granted" {
		t.Fatalf("denied grant: %d %s", resp.status, resp.body)
	}
}

func retrieval(r *rig, token string, body string) response {
	r.t.Helper()
	return r.do(http.MethodPost, "/v1/content/retrieval", token, []byte(body), map[string]string{"Content-Type": "application/json"})
}

func TestRetrievalOverHTTP(t *testing.T) {
	r := newRig(t)
	content := []byte("the stored prompt")
	if resp := r.upload(r.iss.Service(t, "control-api"), content); resp.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.status, resp.body)
	}
	reader := r.iss.Person(t, tenantID, "alice@example.com", "content_reader")
	body := `{"event_id":"` + eventID + `","case_reference":"CASE-9","second_approver":"bob@example.com","justification":""}`

	for name, tc := range map[string]struct {
		token  string
		status int
	}{
		"no token":           {"", http.StatusUnauthorized},
		"analyst only":       {r.iss.Person(t, tenantID, "carol", "analyst"), http.StatusForbidden},
		"an admin":           {r.iss.Person(t, tenantID, "dave", "admin"), http.StatusOK},
		"a service token":    {r.iss.Service(t, "control-api"), http.StatusForbidden},
		"another tenant's":   {r.iss.Person(t, "c2b1f0e4-5d6a-4b7c-8e9f-0a1b2c3d4e5f", "alice", "content_reader"), http.StatusForbidden},
		"unknown body field": {reader, http.StatusBadRequest},
	} {
		b := body
		if name == "unknown body field" {
			b = `{"event_id":"` + eventID + `","tenant_id":"` + tenantID + `"}`
		}
		if resp := retrieval(r, tc.token, b); resp.status != tc.status {
			t.Errorf("%s: %d %s, want %d", name, resp.status, resp.body, tc.status)
		}
	}

	resp := retrieval(r, reader, body)
	if resp.status != http.StatusOK {
		t.Fatalf("retrieval: %d %s", resp.status, resp.body)
	}
	got := resp.json(t)
	url, _ := got["retrieval_url"].(string)
	if got["state"] != "available" || got["raw_digest"] != protocol.RawDigest(content) || !strings.HasPrefix(url, vault.RetrievalPath+tenantID+"/") {
		t.Fatalf("retrieval answer = %v", got)
	}

	// The browser redeems the URL through the dashboard server, which adds no identity.
	redeemed := r.do(http.MethodGet, url, "", nil, nil)
	if redeemed.status != http.StatusOK || !bytes.Equal(redeemed.body, content) {
		t.Fatalf("redeem: %d %q", redeemed.status, redeemed.body)
	}
	if redeemed.header.Get(protocol.HeaderContentRawDigest) != protocol.RawDigest(content) ||
		redeemed.header.Get("Cache-Control") != "no-store" || redeemed.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("redeem headers = %v", redeemed.header)
	}
	if again := r.do(http.MethodGet, url, "", nil, nil); again.status != http.StatusForbidden || again.code(t) != "grant_already_used" {
		t.Fatalf("second redemption: %d %s", again.status, again.body)
	}
	if bad := r.do(http.MethodGet, vault.RetrievalPath+"acme/1", "", nil, nil); bad.status != http.StatusBadRequest {
		t.Fatalf("malformed retrieval URL: %d", bad.status)
	}
}

func TestGoneContentIsAnExplicitAnswer(t *testing.T) {
	r := newRig(t)
	if resp := r.upload(r.iss.Service(t, "control-api"), []byte("x")); resp.status != http.StatusCreated {
		t.Fatal(resp.status)
	}
	reader := r.iss.Person(t, tenantID, "alice", "content_reader")
	minted := retrieval(r, reader, `{"event_id":"`+eventID+`"}`).json(t)
	object := r.store.Content(tenantID)[0].ObjectID
	r.store.EditContent(tenantID, object, func(c *store.Content) bool { c.ExpiresAt = time.Now().Add(-time.Second); return true })

	resp := retrieval(r, reader, `{"event_id":"`+eventID+`"}`)
	if got := resp.json(t); resp.status != http.StatusOK || got["state"] != "no_longer_available" || got["reason"] != "retention_expired" {
		t.Fatalf("retrieval of expired content: %d %s", resp.status, resp.body)
	}
	redeem := r.do(http.MethodGet, minted["retrieval_url"].(string), "", nil, nil)
	if got := redeem.json(t); redeem.status != http.StatusGone || got["reason"] != "retention_expired" {
		t.Fatalf("redemption of expired content: %d %s", redeem.status, redeem.body)
	}
	missing := retrieval(r, reader, `{"event_id":"6f7a8b9c-0d1e-4f2a-9b3c-4d5e6f7a8b9c"}`)
	if missing.status != http.StatusForbidden || missing.code(t) != "no_content_object" {
		t.Fatalf("retrieval with no content: %d %s", missing.status, missing.body)
	}
}

func TestSearchOverHTTP(t *testing.T) {
	r := newRig(t)
	if resp := r.upload(r.iss.Service(t, "control-api"), []byte(`{"prompt":"renewal terms for contoso"}`)); resp.status != http.StatusCreated {
		t.Fatal(resp.status)
	}
	analyst := r.iss.Person(t, tenantID, "alice", "analyst")
	search := func(token, body string) response {
		return r.do(http.MethodPost, "/v1/content-search", token, []byte(body), map[string]string{"Content-Type": "application/json"})
	}
	resp := search(analyst, `{"form":"terms","query":"contoso","limit":20,"subject":"","received_from":"2026-01-01T00:00:00Z"}`)
	if resp.status != http.StatusOK {
		t.Fatalf("search: %d %s", resp.status, resp.body)
	}
	got := resp.json(t)
	hits, _ := got["hits"].([]any)
	if got["state"] != "available" || got["effective_tier"] != "full_text" || len(hits) != 1 || got["truncated"] != false {
		t.Fatalf("search answer = %v", got)
	}
	if hit := hits[0].(map[string]any); hit["submission_id"] != submissionID || hit["unit_kind"] != "prompt_body" {
		t.Fatalf("hit = %v", hit)
	}
	if resp := search(r.iss.Person(t, tenantID, "v", "viewer"), `{"query":"contoso"}`); resp.status != http.StatusForbidden {
		t.Fatalf("viewer: %d", resp.status)
	}
	if resp := search(analyst, `{"query":"contoso","scope":"default"}`); resp.status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", resp.status)
	}
	if resp := search(analyst, `{"query":""}`); resp.status != http.StatusBadRequest || resp.code(t) != "invalid_request" {
		t.Fatalf("empty query: %d %s", resp.status, resp.body)
	}
}

func TestProbes(t *testing.T) {
	r := newRig(t)
	if resp := r.do(http.MethodGet, "/healthz", "", nil, nil); resp.status != http.StatusOK {
		t.Fatalf("healthz: %d", resp.status)
	}
	if resp := r.do(http.MethodGet, "/readyz", "", nil, nil); resp.status != http.StatusOK {
		t.Fatalf("readyz: %d", resp.status)
	}
	r.ready = errors.New("connection refused")
	if resp := r.do(http.MethodGet, "/readyz", "", nil, nil); resp.status != http.StatusServiceUnavailable {
		t.Fatalf("readyz with the database down: %d", resp.status)
	}
	if resp := r.do(http.MethodGet, "/healthz", "", nil, nil); resp.status != http.StatusOK {
		t.Fatal("liveness depends on the database")
	}
}

func TestOnlyTheDocumentedRoutesExist(t *testing.T) {
	r := newRig(t)
	token := r.iss.Service(t, "control-api")
	for _, route := range []string{
		"POST /v1/content/object", "POST /v1/content/object/finalise", "POST /v1/content/redeem",
		"POST /v1/content/shred", "POST /v1/content/rotate", "POST /v1/content/grant", "POST /v1/content",
		"GET /internal/v1/tenants/" + tenantID + "/content/" + eventID,
	} {
		method, path, _ := strings.Cut(route, " ")
		if resp := r.do(method, path, token, []byte("{}"), nil); resp.status != http.StatusNotFound && resp.status != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d", route, resp.status)
		}
	}
}
