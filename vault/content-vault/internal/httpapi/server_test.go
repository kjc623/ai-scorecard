package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/httpapi"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/testrig"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// server builds the HTTP surface over a rig, with a static internal principal that may read
// content: the two roles the browser-facing routes need. Role enforcement itself is tested in
// TestContentRoutesRequireAContentRole.
func server(t *testing.T, o testrig.Options) (*httptest.Server, *testrig.Rig) {
	t.Helper()
	return serverAs(t, []string{"analyst", "content_reader"}, o)
}

// serverAs is server with the caller's roles named, so a test can hold the role boundary.
func serverAs(t *testing.T, roles []string, o testrig.Options) (*httptest.Server, *testrig.Rig) {
	t.Helper()
	rig := testrig.New(t, o)
	h := httpapi.New(rig.Service, auth.StaticAuthenticator{P: auth.Principal{
		Service: "query-api", Subject: "analyst@example.com", TenantID: testrig.TenantID, Roles: roles,
	}}, nil)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return ts, rig
}

// TestContentRoutesRequireAContentRole is the vault's half of the role boundary: the component
// that returns content re-checks the session role its caller asserts, so a bug in query-api's gate
// is not the only thing between a viewer and a prompt.
func TestContentRoutesRequireAContentRole(t *testing.T) {
	retrieval := `{"event_id":"` + testrig.EventA + `"}`
	search := `{"scope":"lab","form":"terms","query":"capital"}`
	for _, tc := range []struct {
		name  string
		roles []string
	}{
		{"no roles", nil},
		{"viewer", []string{"viewer"}},
		{"admin", []string{"admin"}},
	} {
		ts, _ := serverAs(t, tc.roles, testrig.Options{})
		for _, path := range []string{"/v1/content/retrieval", "/v1/content-search"} {
			body := retrieval
			if path == "/v1/content-search" {
				body = search
			}
			status, out := post(t, ts, path, body)
			if status != http.StatusForbidden {
				t.Fatalf("%s: %s returned %d, want 403 (%v)", tc.name, path, status, out)
			}
			if code := errorCode(out); code != "role" {
				t.Fatalf("%s: %s refused with %q, want role", tc.name, path, code)
			}
		}
	}

	// An analyst may search but may not mint a retrieval URL: search is an analyst capability,
	// opening one event's stored content is the content reader's.
	ts, _ := serverAs(t, []string{"analyst"}, testrig.Options{})
	if status, out := post(t, ts, "/v1/content-search", search); status == http.StatusForbidden {
		if errorCode(out) == "role" {
			t.Fatalf("analyst was refused search by role: %v", out)
		}
	}
	if status, out := post(t, ts, "/v1/content/retrieval", retrieval); status != http.StatusForbidden {
		t.Fatalf("analyst retrieval returned %d, want 403 (%v)", status, out)
	}
}

// errorCode reads the code from the vault's error envelope, which is the only shape a refusal has.
func errorCode(out map[string]any) string {
	errObj, _ := out["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func post(t *testing.T, ts *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("POST %s: response is not JSON: %v", path, err)
	}
	return resp.StatusCode, out
}

// TestThePublicAndEdgeRoutesDoNotExist is the internal-ingress property (D6/D7) held by the router:
// the vault is reachable only from inside the network, and the device-facing and analyst-facing
// routes belong to other services. A test asserts their absence rather than a comment claiming it,
// because "nobody would add that route" is exactly the kind of assumption that ages badly.
func TestThePublicAndEdgeRoutesDoNotExist(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	edge := []struct{ method, path string }{
		{"POST", "/v1/events"},        // ingest-api, devices
		{"POST", "/v1/content/grant"}, // control-api, devices (§5.5)
		{"PUT", "/v1/content/upload"}, // blob storage, devices: content never reaches the vault
		{"GET", "/v1/policy"},         // control-api, devices
		{"POST", "/v1/enrol"},         // control-api, devices
		{"POST", "/v1/health"},        // control-api
		{"POST", "/v1/query"},         // query-api's own surface
		{"GET", "/v1/query"},
		{"GET", "/"},
		{"GET", "/admin"},
		{"GET", "/v1/content-objects"}, // no enumeration surface exists at all
	}
	for _, e := range edge {
		req, err := http.NewRequest(e.method, ts.URL+e.path, bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", e.method, e.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s returned %d; the vault must not serve this route on any surface",
				e.method, e.path, resp.StatusCode)
		}
	}
}

// TestEveryInternalRouteIsWired: the surface exists, and only for POST.
func TestEveryInternalRouteIsWired(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	internal := []string{
		"/v1/content/object",
		"/v1/content/object/finalise",
		"/v1/content/retrieval",
		"/v1/content/redeem",
		"/v1/content/shred",
		"/v1/content/rotate",
		"/v1/content-search",
	}
	for _, path := range internal {
		status, _ := post(t, ts, path, `{}`)
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			t.Errorf("POST %s is not wired (status %d)", path, status)
		}
		if status == http.StatusUnauthorized {
			t.Errorf("POST %s refused an authenticated caller as unauthenticated", path)
		}
	}
}

// TestUnauthenticatedCallersAreRefused: no principal, no service.
func TestUnauthenticatedCallersAreRefused(t *testing.T) {
	rig := testrig.New(t, testrig.Options{})
	h := httpapi.New(rig.Service, auth.NewHeaderAuthenticator("query-api", "control-api"), nil)
	ts := httptest.NewServer(h.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/content-search", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated request returned %d, want 401", resp.StatusCode)
	}

	// An authenticated service that is not in the closed caller set is refused too: the vault
	// decides who may read content, not the ingress alone.
	req, _ := http.NewRequest("POST", ts.URL+"/v1/content-search", strings.NewReader(`{}`))
	req.Header.Set("X-Sac-Service", "capture-extension")
	req.Header.Set("X-Sac-Subject", "analyst@example.com")
	req.Header.Set("X-Sac-Tenant", testrig.TenantID)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("a non-permitted service returned %d, want 401", resp2.StatusCode)
	}
}

// TestTenantComesFromThePrincipalNotTheBody is docs/02 §12: a body tenant that disagrees is
// rejected, not reconciled.
func TestTenantComesFromThePrincipalNotTheBody(t *testing.T) {
	ts, rig := server(t, testrig.Options{Tenants: []store.Tenant{testrig.Tenant(), testrig.Tenant(func(tn *store.Tenant) {
		tn.TenantID = testrig.OtherTenantID
		tn.KEKID = "kek-other"
	})}})
	rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)

	status, body := post(t, ts, "/v1/content/retrieval", `{
		"tenant_id": "`+testrig.OtherTenantID+`",
		"event_id": "`+testrig.EventA+`",
		"case_reference": "CASE-1",
		"second_approver": "approver@example.com"
	}`)
	if status != http.StatusForbidden {
		t.Fatalf("a body tenant that disagrees with the principal returned %d, want 403 (%v)", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != string(vault.DenyTenantMismatch) {
		t.Errorf("refusal reason is %v, want %s", errObj["code"], vault.DenyTenantMismatch)
	}
}

// TestFullLifecycleOverTheInternalSurface walks prepare → finalise → retrieval → redeem, which is
// the shape control-api and query-api drive.
func TestFullLifecycleOverTheInternalSurface(t *testing.T) {
	ts, _ := server(t, testrig.Options{Tenants: []store.Tenant{testrig.Tenant(func(tn *store.Tenant) {
		tn.ContentSearch = store.SearchAttachmentNames
	})}})

	status, body := post(t, ts, "/v1/content/object", `{
		"object_id": "`+testrig.ObjectA+`", "submission_id": "`+testrig.SubmissionA+`",
		"event_id": "`+testrig.EventA+`", "retention_class": "standard"}`)
	if status != http.StatusOK {
		t.Fatalf("prepare returned %d (%v)", status, body)
	}
	objectKey, _ := body["object_key_b64"].(string)
	wrapped, _ := body["wrapped_dek_b64"].(string)
	kekID, _ := body["kek_id"].(string)
	kekVersion, _ := body["kek_version"].(string)
	if objectKey == "" || wrapped == "" || kekID == "" || kekVersion == "" {
		t.Fatalf("prepare response is incomplete: %v", body)
	}
	if _, err := base64.StdEncoding.DecodeString(objectKey); err != nil {
		t.Errorf("object_key_b64 is not base64: %v", err)
	}

	digest := "sha256:" + strings.Repeat("ab", 32)
	status, body = post(t, ts, "/v1/content/object/finalise", `{
		"object_id": "`+testrig.ObjectA+`", "submission_id": "`+testrig.SubmissionA+`",
		"event_id": "`+testrig.EventA+`", "blob_path": "tenants/x/objects/a",
		"ciphertext_sha256": "`+digest+`", "plaintext_size_bytes": 4096,
		"wrapped_dek_b64": "`+wrapped+`", "kek_id": "`+kekID+`", "kek_version": "`+kekVersion+`",
		"retention_class": "standard",
		"index_units": [{"unit_kind": "attachment_name", "unit_index": 0, "body": "Q3-contract.pdf"}]}`)
	if status != http.StatusOK {
		t.Fatalf("finalise returned %d (%v)", status, body)
	}
	if body["indexed"].(float64) != 1 {
		t.Errorf("finalise indexed %v units, want 1", body["indexed"])
	}

	status, body = post(t, ts, "/v1/content/retrieval", `{
		"event_id": "`+testrig.EventA+`", "case_reference": "CASE-1",
		"second_approver": "approver@example.com", "justification": "investigation"}`)
	if status != http.StatusOK {
		t.Fatalf("retrieval returned %d (%v)", status, body)
	}
	grantID, _ := body["grant_id"].(string)
	if grantID == "" {
		t.Fatal("retrieval returned no grant id")
	}

	status, body = post(t, ts, "/v1/content/redeem", `{
		"grant_id": "`+grantID+`", "event_id": "`+testrig.EventA+`"}`)
	if status != http.StatusOK || body["state"] != "available" {
		t.Fatalf("redeem returned %d (%v)", status, body)
	}
	if body["raw_digest"] != digest {
		t.Errorf("redeem returned raw_digest %v, want %v", body["raw_digest"], digest)
	}

	// And the grant is single-use over HTTP as well.
	status, body = post(t, ts, "/v1/content/redeem", `{
		"grant_id": "`+grantID+`", "event_id": "`+testrig.EventA+`"}`)
	if status != http.StatusForbidden {
		t.Fatalf("a second redemption returned %d (%v), want 403", status, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != string(vault.DenyGrantAlreadyUsed) {
		t.Errorf("second redemption reason is %v, want %s", errObj["code"], vault.DenyGrantAlreadyUsed)
	}
}

// TestUnavailabilityIsA200WithAnExplicitResult is C17 and docs/02 §11: "an expired, erased or
// shredded record returns 200 with an explicit result, never 404 and never an empty body".
func TestUnavailabilityIsA200WithAnExplicitResult(t *testing.T) {
	ts, rig := server(t, testrig.Options{})
	obj := rig.Store(t, testrig.TenantID, testrig.ObjectA, testrig.SubmissionA, testrig.EventA)
	if _, err := rig.Service.ShredObject(t.Context(), vault.ShredRequest{
		TenantID: testrig.TenantID, ObjectID: obj.ObjectID, Reason: "erasure", RequestedBy: "dpo@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	status, body := post(t, ts, "/v1/content/retrieval", `{
		"event_id": "`+testrig.EventA+`", "case_reference": "CASE-1",
		"second_approver": "approver@example.com"}`)
	if status != http.StatusOK {
		t.Fatalf("retrieving erased content returned %d (%v), want 200 with an explicit result", status, body)
	}
	if body["state"] != "no_longer_available" {
		t.Errorf("state is %v, want no_longer_available", body["state"])
	}
	if body["reason"] != string(vault.UnavailableErasure) {
		t.Errorf("reason is %v, want erasure", body["reason"])
	}
	if ref, _ := body["receipt_ref"].(string); ref == "" {
		t.Error("the unavailability carries no receipt reference")
	}
}

// TestRefusalReasonsAreClosed: every refusal says whether its code is a member of the documented
// set, so a caller can tell a content refusal from a transport failure without a lookup table.
func TestRefusalReasonsAreClosed(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	status, body := post(t, ts, "/v1/content/redeem", `{"grant_id": "33333333-3333-4333-8333-333333333333", "event_id": "`+testrig.EventA+`"}`)
	if status != http.StatusForbidden {
		t.Fatalf("redeeming an unknown grant returned %d (%v)", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	if !vault.DenialReason(code).Valid() {
		t.Errorf("refusal code %q is outside the closed set", code)
	}
	if closed, _ := errObj["closed"].(bool); !closed {
		t.Errorf("the refusal did not report itself as a closed reason: %v", errObj)
	}
}

// TestMalformedBodiesAreTransportErrorsNotContentRefusals: the two kinds are kept apart.
func TestMalformedBodiesAreTransportErrorsNotContentRefusals(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	for _, body := range []string{`{`, `{"unknown_field": 1}`, `{"object_id": "x"}{"trailing": 1}`} {
		status, out := post(t, ts, "/v1/content/object", body)
		if status != http.StatusBadRequest {
			t.Errorf("body %q returned %d (%v), want 400", body, status, out)
		}
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != "bad_request" {
			t.Errorf("body %q produced code %v, want bad_request", body, errObj["code"])
		}
	}
}

// TestFinaliseDecodesPromptKind: the prompt_kind field crosses the internal HTTP boundary and is
// decoded, so a client_generated finalise indexes no prompt_body unit while a user one does.
func TestFinaliseDecodesPromptKind(t *testing.T) {
	tiers := map[string]store.SearchTier{"tool:chatgpt": store.SearchFullText}
	ts, _ := server(t, testrig.Options{
		Tenants:    []store.Tenant{testrig.Tenant(func(tn *store.Tenant) { tn.ContentSearch = store.SearchFullText })},
		ScopeTiers: tiers,
	})

	finalise := func(objectID, promptKind string) float64 {
		status, body := post(t, ts, "/v1/content/object", `{
			"object_id": "`+objectID+`", "submission_id": "`+testrig.SubmissionA+`",
			"event_id": "`+testrig.EventA+`", "retention_class": "standard"}`)
		if status != http.StatusOK {
			t.Fatalf("prepare returned %d (%v)", status, body)
		}
		wrapped := body["wrapped_dek_b64"].(string)
		kekID := body["kek_id"].(string)
		kekVersion := body["kek_version"].(string)

		digest := "sha256:" + strings.Repeat("ab", 32)
		status, body = post(t, ts, "/v1/content/object/finalise", `{
			"object_id": "`+objectID+`", "submission_id": "`+testrig.SubmissionA+`",
			"event_id": "`+testrig.EventA+`", "blob_path": "tenants/x/objects/`+objectID+`",
			"ciphertext_sha256": "`+digest+`", "plaintext_size_bytes": 4096,
			"wrapped_dek_b64": "`+wrapped+`", "kek_id": "`+kekID+`", "kek_version": "`+kekVersion+`",
			"retention_class": "standard", "prompt_kind": "`+promptKind+`",
			"index_units": [{"unit_kind": "prompt_body", "unit_index": 0, "body": "what is the capital of Australia"}]}`)
		if status != http.StatusOK {
			t.Fatalf("finalise returned %d (%v)", status, body)
		}
		return body["indexed"].(float64)
	}

	if got := finalise(testrig.ObjectA, "client_generated"); got != 0 {
		t.Errorf("client_generated finalise indexed %v units, want 0", got)
	}
	if got := finalise(testrig.ObjectB, "user"); got != 1 {
		t.Errorf("user finalise indexed %v units, want 1", got)
	}
}

// TestHealthReportsTheKeyBackendHonestly: an operator must be able to see that this build is not
// running a cloud KMS, and that the surface is internal.
func TestHealthReportsTheKeyBackendHonestly(t *testing.T) {
	ts, _ := server(t, testrig.Options{})
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["key_backend"] != "local-software" {
		t.Errorf("health reports key_backend %v; the test rig uses the local software wrapper", out["key_backend"])
	}
	if out["ingress"] != "internal-only" {
		t.Errorf("health reports ingress %v, want internal-only", out["ingress"])
	}
	if reasons, ok := out["denial_reasons"].([]any); !ok || len(reasons) == 0 {
		t.Error("health does not publish the closed refusal set")
	}
}
