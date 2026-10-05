package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/httpapi"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/testrig"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

// The vault under a token issuer (contract §2): the person on a human route is the product token
// query-api forwards, verified here; the headers beside it must agree; a service caller with no
// token keeps the service routes; and the minted retrieval URL is still redeemed with no identity
// at all (task 10).

const (
	retrievalBody = `{"event_id":"` + testrig.EventA + `"}`
	searchBody    = `{"scope":"lab","form":"terms","query":"capital"}`
)

// tokenServer is the vault as a deployment with SAC_AUTH_ISSUER runs it, over a rig whose tenant
// has a stored, readable object and full-text search in scope "lab".
func tokenServer(t *testing.T) (*httptest.Server, *testrig.Rig, *testrig.Issuer) {
	t.Helper()
	blobs := mapBlobs{}
	rig := testrig.New(t, testrig.Options{
		Tenants:     []store.Tenant{testrig.Tenant(func(tn *store.Tenant) { tn.ContentSearch = store.SearchFullText })},
		ScopeTiers:  map[string]store.SearchTier{"lab": store.SearchFullText},
		AdjustVault: func(o *vault.Options) { o.Blobs = blobs },
	})
	storeReadable(t, rig, blobs)
	iss := testrig.NewIssuer(t)
	a := &auth.TokenAuthenticator{Headers: auth.NewHeaderAuthenticator("query-api", "control-api", "ops"), Verifier: iss.Verifier()}
	ts := httptest.NewServer(httpapi.New(rig.Service, a, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, rig, iss
}

// storeReadable stores one sealed prompt for EventA whose blob the redemption can read.
func storeReadable(t *testing.T, rig *testrig.Rig, blobs mapBlobs) {
	t.Helper()
	ctx := context.Background()
	prep, err := rig.Service.PrepareObject(ctx, vault.PrepareRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
		EventID: testrig.EventA, ExpiresAt: rig.Clock.T.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("PrepareObject: %v", err)
	}
	sealed, err := protocol.SealContent(prep.DEK, testrig.EventA, []byte("What is the capital of Australia?"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	blobs["objects/one"] = sealed
	if _, err := rig.Service.FinaliseObject(ctx, vault.FinaliseRequest{
		TenantID: testrig.TenantID, ObjectID: testrig.ObjectA, SubmissionID: testrig.SubmissionA,
		EventID: testrig.EventA, BlobPath: "objects/one", CiphertextSHA256: protocol.RawDigest(sealed),
		PlaintextSizeBytes: 33, WrappedDEK: prep.WrappedDEK, KEKID: prep.KEKID, KEKVersion: prep.KEKVersion,
		ExpiresAt: rig.Clock.T.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("FinaliseObject: %v", err)
	}
}

func postWith(t *testing.T, ts *httptest.Server, path, body string, h http.Header) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("POST %s: response is not JSON: %v", path, err)
	}
	return resp.StatusCode, out
}

// asQueryAPI is what query-api sends: the person's bearer and headers that agree with it.
func asQueryAPI(token string, roles ...string) http.Header {
	h := testrig.BearerHeaders(token)
	h.Set("X-Sac-Tenant", testrig.TenantID)
	h.Set("X-Sac-Subject", "reader@lab.test")
	h.Set("X-Sac-Roles", testrig.JoinRoles(roles...))
	return h
}

func TestUnderAnIssuerTheRoleBoundaryIsTheTokens(t *testing.T) {
	ts, _, iss := tokenServer(t)
	for _, tc := range []struct {
		role              string
		search, retrieval bool
	}{
		{"viewer", false, false},
		{"admin", false, false},
		{"analyst", true, false},
		{"content_reader", true, true},
	} {
		token := iss.Mint(t, map[string]any{"roles": []string{tc.role}}, testrig.MintOptions{})
		for path, allowed := range map[string]bool{"/v1/content-search": tc.search, "/v1/content/retrieval": tc.retrieval} {
			body := searchBody
			if path == "/v1/content/retrieval" {
				body = retrievalBody
			}
			status, out := postWith(t, ts, path, body, asQueryAPI(token, tc.role))
			if allowed && status != http.StatusOK {
				t.Errorf("%s on %s: %d %v, want 200", tc.role, path, status, out)
			}
			if !allowed && (status != http.StatusForbidden || errorCode(out) != "role") {
				t.Errorf("%s on %s: %d %v, want 403 role", tc.role, path, status, out)
			}
		}
	}
}

func TestUnderAnIssuerAHumanRouteNeedsTheToken(t *testing.T) {
	ts, rig, _ := tokenServer(t)
	before := len(rig.Memory.Audit())
	// Exactly what the header-trust lab accepts: service, tenant, subject and roles as headers.
	h := http.Header{}
	h.Set("X-Sac-Service", "query-api")
	h.Set("X-Sac-Tenant", testrig.TenantID)
	h.Set("X-Sac-Subject", "reader@lab.test")
	for _, path := range []string{"/v1/content/retrieval", "/v1/content-search"} {
		h.Set("X-Sac-Roles", "content_reader")
		if status, out := postWith(t, ts, path, retrievalBody, h); status != http.StatusUnauthorized {
			t.Errorf("%s with header roles and no token: %d %v, want 401", path, status, out)
		}
		h.Del("X-Sac-Roles")
		if status, out := postWith(t, ts, path, retrievalBody, h); status != http.StatusUnauthorized {
			t.Errorf("%s with no token: %d %v, want 401", path, status, out)
		}
	}
	if after := len(rig.Memory.Audit()); after != before {
		t.Fatalf("refused requests wrote %d audit rows", after-before)
	}
}

func TestAHeaderThatDisagreesWithTheTokenIsRefused(t *testing.T) {
	ts, rig, iss := tokenServer(t)
	token := iss.Mint(t, map[string]any{"roles": []string{"content_reader"}}, testrig.MintOptions{})
	before := len(rig.Memory.Audit())
	for name, set := range map[string][2]string{
		"tenant":  {"X-Sac-Tenant", testrig.OtherTenantID},
		"subject": {"X-Sac-Subject", "someone.else@lab.test"},
		"roles":   {"X-Sac-Roles", testrig.JoinRoles("content_reader", "admin")},
	} {
		h := asQueryAPI(token, "content_reader")
		h.Set(set[0], set[1])
		status, out := postWith(t, ts, "/v1/content/retrieval", retrievalBody, h)
		if status != http.StatusForbidden || errorCode(out) != "principal_mismatch" {
			t.Errorf("a disagreeing %s header: %d %v, want 403 principal_mismatch", name, status, out)
		}
	}
	if after := len(rig.Memory.Audit()); after != before {
		t.Fatalf("a refused principal reached the vault and wrote %d audit rows", after-before)
	}
}

func TestATokenWithoutAContentRoleIsRefusedOnContentRoutes(t *testing.T) {
	ts, _, iss := tokenServer(t)
	for name, token := range map[string]string{
		"an unknown role only": iss.Mint(t, map[string]any{"roles": []string{"superuser"}}, testrig.MintOptions{}),
		"no roles claim":       iss.Mint(t, map[string]any{"roles": testrig.Absent}, testrig.MintOptions{}),
		"expired":              iss.Mint(t, map[string]any{"iat": time.Now().Unix() - 900, "exp": time.Now().Unix() - 300}, testrig.MintOptions{}),
		"for another issuer":   iss.Mint(t, map[string]any{"iss": "http://elsewhere"}, testrig.MintOptions{}),
	} {
		h := testrig.BearerHeaders(token)
		for _, path := range []string{"/v1/content/retrieval", "/v1/content-search"} {
			if status, out := postWith(t, ts, path, retrievalBody, h); status != http.StatusUnauthorized {
				t.Errorf("%s on %s: %d %v, want 401", name, path, status, out)
			}
		}
	}
}

func TestTheTokensPersonAndSessionAreWhatTheAuditRecords(t *testing.T) {
	ts, rig, iss := tokenServer(t)
	token := iss.Mint(t, map[string]any{"roles": []string{"content_reader"}, "sid": "ABCDEF0123456789"}, testrig.MintOptions{})
	if status, out := postWith(t, ts, "/v1/content/retrieval", retrievalBody, asQueryAPI(token, "content_reader")); status != http.StatusOK {
		t.Fatalf("retrieval: %d %v", status, out)
	}
	if status, out := postWith(t, ts, "/v1/content-search", searchBody, asQueryAPI(token, "content_reader")); status != http.StatusOK {
		t.Fatalf("search: %d %v", status, out)
	}
	rows := 0
	for _, e := range rig.Memory.Audit() {
		if e.ActorType != "user" {
			continue
		}
		rows++
		if e.ActorID != "reader@lab.test" {
			t.Errorf("%s audited actor %q, want the token's actor", e.Action, e.ActorID)
		}
		if e.Detail["sid"] != "abcdef0123456789" {
			t.Errorf("%s audit detail sid = %v, want the token's sid", e.Action, e.Detail["sid"])
		}
	}
	if rows < 3 {
		t.Fatalf("%d user audit rows, want the retrieval request, the grant and the search", rows)
	}
}

func TestUnderAnIssuerServiceRoutesStillAuthenticateTheService(t *testing.T) {
	ts, _, _ := tokenServer(t)
	h := http.Header{}
	h.Set("X-Sac-Service", "control-api")
	h.Set("X-Sac-Subject", "control-api")
	h.Set("X-Sac-Tenant", testrig.TenantID)
	status, out := postWith(t, ts, "/v1/content/object", `{"object_id":"`+testrig.ObjectB+`","submission_id":"`+testrig.SubmissionB+`","event_id":"`+testrig.EventB+`"}`, h)
	if status != http.StatusOK {
		t.Fatalf("control-api preparing an object under an issuer: %d %v, want 200", status, out)
	}
}

func TestUnderAnIssuerTheRetrievalURLIsStillRedeemedWithoutIdentity(t *testing.T) {
	ts, _, iss := tokenServer(t)
	token := iss.Mint(t, map[string]any{"roles": []string{"content_reader"}}, testrig.MintOptions{})
	status, out := postWith(t, ts, "/v1/content/retrieval", retrievalBody, asQueryAPI(token, "content_reader"))
	if status != http.StatusOK {
		t.Fatalf("retrieval: %d %v", status, out)
	}
	url, _ := out["retrieval_url"].(string)
	resp, err := http.Get(ts.URL + url) // no Authorization, no X-Sac-*: the grant is the credential
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "What is the capital of Australia?" {
		t.Fatalf("redeeming the URL under an issuer: %d %q", resp.StatusCode, got)
	}
}

func TestTheHeaderTrustLabIsUnchanged(t *testing.T) {
	blobs := mapBlobs{}
	rig := testrig.New(t, testrig.Options{AdjustVault: func(o *vault.Options) { o.Blobs = blobs }})
	storeReadable(t, rig, blobs)
	ts := httptest.NewServer(httpapi.New(rig.Service, auth.NewHeaderAuthenticator("query-api", "control-api", "ops"), nil).Handler())
	defer ts.Close()

	h := http.Header{}
	h.Set("X-Sac-Service", "query-api")
	h.Set("X-Sac-Tenant", testrig.TenantID)
	h.Set("X-Sac-Subject", "analyst@lab.test")
	h.Set("X-Sac-Roles", "content_reader")
	// A bearer the lab cannot verify is ignored, not refused: header mode has no issuer.
	h.Set("Authorization", "Bearer not.a.token")
	if status, out := postWith(t, ts, "/v1/content/retrieval", retrievalBody, h); status != http.StatusOK {
		t.Fatalf("header-mode retrieval with a content_reader header: %d %v, want 200", status, out)
	}
	h.Set("X-Sac-Roles", "viewer")
	if status, out := postWith(t, ts, "/v1/content/retrieval", retrievalBody, h); status != http.StatusForbidden || errorCode(out) != "role" {
		t.Fatalf("header-mode retrieval as a viewer: %d %v, want 403 role", status, out)
	}
	for _, e := range rig.Memory.Audit() {
		if _, has := e.Detail["sid"]; has {
			t.Fatalf("a header-mode audit row carries a sid: %v", e.Detail)
		}
	}
}
