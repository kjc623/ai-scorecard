package identity_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/identity"
)

const internalToken = "lab-internal-token-0123456789abcdef"

func (r *rig) handler(t *testing.T) http.Handler {
	h, err := r.svc.InternalHandler(internalToken)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func call(t *testing.T, h http.Handler, path, bearer string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestInternalTokenIsRequired(t *testing.T) {
	r := newRig(t)
	h := r.handler(t)
	body := identity.BeginRequest{Provider: "entra", RedirectURI: redirect}
	for _, bearer := range []string{"", "wrong", internalToken + "x", strings.ToUpper(internalToken)} {
		rec, out := call(t, h, identity.PathBegin, bearer, body)
		if rec.Code != http.StatusUnauthorized || out["error"] != "unauthorized" {
			t.Fatalf("bearer %q: %d %v", bearer, rec.Code, out)
		}
	}
	rec, out := call(t, h, identity.PathBegin, internalToken, body)
	if rec.Code != http.StatusOK || out["attempt"] == "" || out["authorize_url"] == "" {
		t.Fatalf("with the token: %d %v", rec.Code, out)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("an internal answer may carry an attempt or a token; it must not be cached")
	}
	if _, err := r.svc.InternalHandler("short"); err == nil {
		t.Fatal("a guessable internal token was accepted")
	}
}

func TestInternalAPIShapes(t *testing.T) {
	r := newRig(t)
	h := r.handler(t)

	rec, out := call(t, h, identity.PathBegin, internalToken, identity.BeginRequest{Email: "x@nowhere.example", RedirectURI: redirect})
	if rec.Code != http.StatusNotFound || out["error"] != "no_sso_connection" {
		t.Fatalf("unknown domain: %d %v", rec.Code, out)
	}

	rec, out = call(t, h, identity.PathBegin, internalToken, identity.BeginRequest{Email: "bob@fabrikam.example", RedirectURI: redirect})
	if rec.Code != http.StatusOK {
		t.Fatalf("begin: %d %v", rec.Code, out)
	}
	code, state := r.oidc.Authorize(t, out["authorize_url"].(string), r.oidcUser("analyst"))
	rec, out = call(t, h, identity.PathComplete, internalToken, identity.CompleteRequest{Attempt: out["attempt"].(string), Code: code, State: state})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %v", rec.Code, out)
	}
	sess := out["session"].(string)
	p := out["principal"].(map[string]any)
	if p["tenant"] != tenantB || p["actor"] != "bob@fabrikam.example" || p["idp"] != "oidc" || out["access_token"] == "" || out["expires_in"].(float64) <= 0 {
		t.Fatalf("complete body = %v", out)
	}
	if len(p) != 4 {
		t.Fatalf("principal carries more than its four members: %v", p)
	}

	rec, out = call(t, h, identity.PathToken, internalToken, map[string]string{"session": sess})
	if rec.Code != http.StatusOK || out["access_token"] == "" || out["principal"] == nil {
		t.Fatalf("token: %d %v", rec.Code, out)
	}
	rec, _ = call(t, h, identity.PathRevoke, internalToken, map[string]string{"session": sess})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec, out = call(t, h, identity.PathToken, internalToken, map[string]string{"session": sess})
	if rec.Code != http.StatusUnauthorized || out["error"] != "session_ended" {
		t.Fatalf("token after revoke: %d %v", rec.Code, out)
	}

	// A decided refusal is a 403 with its code and nothing else.
	rec, out = call(t, h, identity.PathBegin, internalToken, identity.BeginRequest{Provider: "entra", RedirectURI: redirect})
	if rec.Code != http.StatusOK {
		t.Fatalf("begin: %d %v", rec.Code, out)
	}
	code, state = r.entra.Authorize(t, out["authorize_url"].(string), r.entraUser())
	rec, out = call(t, h, identity.PathComplete, internalToken, identity.CompleteRequest{Attempt: out["attempt"].(string), Code: code, State: state})
	if rec.Code != http.StatusForbidden || out["error"] != "no_role" || len(out) != 1 {
		t.Fatalf("no role: %d %v", rec.Code, out)
	}

	req := httptest.NewRequest(http.MethodPost, identity.PathBegin, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", w.Code)
	}
}
