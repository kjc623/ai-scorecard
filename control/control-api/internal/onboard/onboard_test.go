package onboard_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/identity/idptest"
	"github.com/shadow-ai-capture/control-api/internal/onboard"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

const (
	public   = "https://app.example.test"
	tenantA  = "aaaaaaaa-0000-4000-8000-000000000001" // already onboarded on Entra
	tenantC  = "cccccccc-0000-4000-8000-000000000003" // being onboarded
	tidA     = "11111111-2222-4333-8444-555555555555"
	tidC     = "33333333-2222-4333-8444-555555555555"
	appID    = "app-client-id"
	appKey   = "app-secret"
	carolOID = "0c0c0c0c-1111-4222-8333-444444444444"
)

type secretAuth struct{}

func (secretAuth) ClientAuth(context.Context, string) (url.Values, error) {
	return url.Values{"client_secret": {appKey}}, nil
}

type rig struct {
	t       *testing.T
	store   *identity.MemoryStore
	cipher  *directory.Cipher
	ident   *identity.Service
	svc     *onboard.Service
	h       http.Handler
	entra   *idptest.IdP
	oidc    *idptest.IdP
	now     time.Time
	probeMu sync.Mutex
	probes  []string
	probeOK bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, now: time.Now().UTC().Truncate(time.Second), probeOK: true}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	r.cipher, _ = directory.NewCipher(key)
	r.entra = idptest.NewEntra(t, appID, appKey)
	r.oidc = idptest.NewOIDC(t, "northwind-oidc", "northwind-secret")
	r.store = identity.NewMemoryStore()
	r.store.AddTenant(tenantA, "Contoso")
	r.store.AddTenant(tenantC, "Northwind")
	r.store.AddEmailDomain("northwind.example", tenantC)
	r.store.AddConnection(identity.Connection{TenantID: tenantA, Provider: identity.ProviderEntra, EntraTenantID: tidA,
		Status: identity.StatusActive, RolesClaim: "roles"})

	sk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ks, _ := session.NewKeySet(sk)
	issuer, _ := session.NewIssuer(ks, session.IssuerConfig{Issuer: "http://control-api:8080"})
	mgr, _ := session.NewManager(session.NewMemoryStore(), session.ManagerConfig{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	r.ident, err = identity.New(identity.Config{
		Store: r.store, Sessions: mgr, Issuer: issuer, Cipher: r.cipher, AllowInsecureIdP: true, Logger: logger,
		Entra: &identity.EntraConfig{ClientID: appID, Auth: secretAuth{}, LoginBaseURL: r.entra.Base()},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.svc, err = onboard.New(onboard.Config{
		Store: r.store, Cipher: r.cipher, PublicURL: public + "/", AllowInsecureIssuers: true, Logger: logger,
		ProbeRetry: time.Millisecond,
		Entra: &onboard.EntraConfig{ClientID: appID, LoginBaseURL: r.entra.Base(), ConsentProbe: func(_ context.Context, tid string) error {
			r.probeMu.Lock()
			defer r.probeMu.Unlock()
			r.probes = append(r.probes, tid)
			if !r.probeOK {
				return errors.New("AADSTS700016: application not found in the directory")
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.h = r.svc.Handler()
	return r
}

// invite issues an invite for tenant C directly into the store, as `tenant invite` would.
func (r *rig) invite(tenant string, expires time.Duration) (string, identity.Invite) {
	tok, err := identity.MintInviteToken(tenant)
	if err != nil {
		r.t.Fatal(err)
	}
	inv := r.store.AddInvite(identity.Invite{TenantID: tenant, TokenHash: identity.HashToken(tok), CreatedBy: "vendor:test",
		CreatedAt: r.now, ExpiresAt: time.Now().Add(expires)})
	return tok, inv
}

func (r *rig) do(method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func cookieOf(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie set", name)
	return nil
}

// startConsent posts the Entra choice and returns the state Microsoft would echo and the cookie.
func (r *rig) startConsent(token string) (string, *http.Cookie) {
	r.t.Helper()
	rec := r.do(http.MethodPost, "/onboard/"+token+"/entra", url.Values{})
	if rec.Code != http.StatusSeeOther {
		r.t.Fatalf("start consent: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != r.entra.Base()+"/organizations/v2.0/adminconsent" {
		r.t.Fatalf("consent redirect = %s", loc)
	}
	q := loc.Query()
	if q.Get("client_id") != appID || q.Get("scope") != "https://graph.microsoft.com/.default" ||
		q.Get("redirect_uri") != public+"/onboard/entra/callback" || q.Get("state") == "" {
		r.t.Fatalf("consent query = %v", q)
	}
	if strings.Contains(loc.String(), token) {
		r.t.Fatal("the invite leaked into the Microsoft URL")
	}
	c := cookieOf(r.t, rec, "sac_onboard")
	if !c.HttpOnly || c.Path != "/onboard/entra/callback" || c.SameSite != http.SameSiteLaxMode || !c.Secure {
		r.t.Fatalf("cookie = %+v", c)
	}
	return q.Get("state"), c
}

func callback(state, tid string) string {
	q := url.Values{"admin_consent": {"True"}, "tenant": {tid}, "state": {state}, "scope": {"https://graph.microsoft.com/.default"}}
	return "/onboard/entra/callback?" + q.Encode()
}

func (r *rig) connectionsOf(tenant string) []identity.Connection {
	conns, _ := r.store.TenantConnections(context.Background(), tenant)
	return conns
}

func TestLandingPage(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	rec := r.do(http.MethodGet, "/onboard/"+tok, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Northwind") || !strings.Contains(body, "@northwind.example") ||
		!strings.Contains(body, "Connect Microsoft Entra ID") || !strings.Contains(body, public+"/callback") {
		t.Fatalf("landing: %d\n%s", rec.Code, body)
	}
	for _, external := range []string{"<script", "<link", "<img", "src=", "@import", "url("} {
		if strings.Contains(body, external) {
			t.Fatalf("the page carries %q: no script and no asset from anywhere", external)
		}
	}
	h := rec.Header()
	if h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		h.Get("Cache-Control") != "no-store" || h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers = %v", h)
	}
}

func TestInviteReuseExpiryAndWrongTenant(t *testing.T) {
	r := newRig(t)
	expired, _ := r.invite(tenantC, -time.Minute)
	if rec := r.do(http.MethodGet, "/onboard/"+expired, nil); rec.Code != http.StatusGone {
		t.Fatalf("expired: %d", rec.Code)
	}
	// A token naming tenant A whose hash is stored under C is not found anywhere.
	forged, _ := identity.MintInviteToken(tenantA)
	r.store.AddInvite(identity.Invite{TenantID: tenantC, TokenHash: identity.HashToken(forged), ExpiresAt: time.Now().Add(time.Hour)})
	if rec := r.do(http.MethodGet, "/onboard/"+forged, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong tenant: %d", rec.Code)
	}
	if rec := r.do(http.MethodGet, "/onboard/sacinv_garbage", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed: %d", rec.Code)
	}
	// Reuse: after the activating sign-in the link is spent.
	tok, _ := r.invite(tenantC, time.Hour)
	r.onboardEntra(t, tok)
	if rec := r.do(http.MethodGet, "/onboard/"+tok, nil); rec.Code != http.StatusGone {
		t.Fatalf("reused invite page: %d", rec.Code)
	}
	if rec := r.do(http.MethodPost, "/onboard/"+tok+"/entra", url.Values{}); rec.Code != http.StatusGone {
		t.Fatalf("reused invite consent: %d", rec.Code)
	}
}

// onboardEntra runs the whole Entra onboarding: consent, callback, and the activating sign-in.
func (r *rig) onboardEntra(t *testing.T, tok string) identity.CompleteResult {
	t.Helper()
	state, cookie := r.startConsent(tok)
	rec := r.do(http.MethodGet, callback(state, tidC), nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	next, _ := url.Parse(rec.Header().Get("Location"))
	if next.Scheme+"://"+next.Host+next.Path != public+"/login" || next.Query().Get("invite") != tok || next.Query().Get("provider") != "entra" {
		t.Fatalf("hand-off = %s", next)
	}
	if c := cookieOf(t, rec, "sac_onboard"); c.MaxAge >= 0 {
		t.Fatal("the consent cookie was not cleared")
	}
	conns := r.connectionsOf(tenantC)
	if len(conns) != 1 || conns[0].Status != identity.StatusPending || conns[0].EntraTenantID != tidC {
		t.Fatalf("connections after consent = %+v", conns)
	}
	// What the dashboard's /login does with ?invite=&provider=.
	begun, err := r.ident.Begin(context.Background(), identity.BeginRequest{
		Invite: next.Query().Get("invite"), Provider: next.Query().Get("provider"), RedirectURI: public + "/callback"})
	if err != nil {
		t.Fatal(err)
	}
	code, st := r.entra.Authorize(t, begun.AuthorizeURL, idptest.Identity{Subject: carolOID, TenantID: tidC, PreferredUsername: "carol@northwind.example"})
	res, err := r.ident.Complete(context.Background(), identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: st})
	if err != nil {
		t.Fatalf("activating sign-in: %v", err)
	}
	return res
}

func TestEntraOnboardingActivatesOnlyAfterSignIn(t *testing.T) {
	r := newRig(t)
	tok, inv := r.invite(tenantC, time.Hour)
	res := r.onboardEntra(t, tok)
	if res.Principal.Tenant != tenantC || strings.Join(res.Principal.Roles, ",") != "admin" || res.Principal.Actor != "carol@northwind.example" {
		t.Fatalf("principal = %+v", res.Principal)
	}
	c := r.connectionsOf(tenantC)[0]
	if c.Status != identity.StatusActive || c.ActivatedBy != "carol@northwind.example" {
		t.Fatalf("connection = %+v", c)
	}
	if got := r.store.InviteState(inv.ID); got.UsedAt == nil {
		t.Fatal("the invite was not spent")
	}
	if len(r.probes) != 1 || r.probes[0] != tidC {
		t.Fatalf("consent probes = %v", r.probes)
	}
	var actions []string
	for _, row := range r.store.AuditRows() {
		if row.TenantID == tenantC {
			actions = append(actions, row.Action+"/"+row.ActorID)
		}
	}
	want := []string{"identity_connection.create/onboarding-invite:" + inv.ID, "onboarding_invite.use/carol@northwind.example",
		"identity_connection.activate/carol@northwind.example", "role.grant/carol@northwind.example", "auth.sign_in/carol@northwind.example"}
	if strings.Join(actions, " ") != strings.Join(want, " ") {
		t.Fatalf("audit = %v\nwant    %v", actions, want)
	}
}

func TestConsentCallbackMustMatchState(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	otherTok, _ := r.invite(tenantC, time.Hour)
	state, cookie := r.startConsent(tok)
	_, otherCookie := r.startConsent(otherTok)

	cases := []struct {
		name   string
		target string
		cookie *http.Cookie
	}{
		{"no state", "/onboard/entra/callback?admin_consent=True&tenant=" + tidC, cookie},
		{"forged state", callback("forged-state", tidC), cookie},
		{"no cookie", callback(state, tidC), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cookies []*http.Cookie
			if c.cookie != nil {
				cookies = append(cookies, c.cookie)
			}
			if rec := r.do(http.MethodGet, c.target, nil, cookies...); rec.Code != http.StatusBadRequest {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	// The "no cookie" case consumed the state: a state is single-use even when the call fails.
	if rec := r.do(http.MethodGet, callback(state, tidC), nil, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed state: %d", rec.Code)
	}
	// A state bound to one invite with the cookie of another (a CSRF or mix-up) is refused.
	state2, _ := r.startConsent(tok)
	if rec := r.do(http.MethodGet, callback(state2, tidC), nil, otherCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("crossed invite: %d", rec.Code)
	}
	if n := len(r.connectionsOf(tenantC)); n != 0 {
		t.Fatalf("%d connections were created by refused callbacks", n)
	}
}

func TestConsentRefusedOrUnprovable(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	state, cookie := r.startConsent(tok)
	q := url.Values{"error": {"access_denied"}, "error_description": {"AADSTS65004: User declined"}, "state": {state}}
	if rec := r.do(http.MethodGet, "/onboard/entra/callback?"+q.Encode(), nil, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("declined: %d", rec.Code)
	}
	r.probeOK = false
	state, cookie = r.startConsent(tok)
	if rec := r.do(http.MethodGet, callback(state, tidC), nil, cookie); rec.Code != http.StatusBadGateway {
		t.Fatalf("unprovable consent: %d", rec.Code)
	}
	if len(r.probes) != 3 {
		t.Fatalf("probe tried %d times, want 3", len(r.probes))
	}
	if n := len(r.connectionsOf(tenantC)); n != 0 {
		t.Fatalf("%d connections created without proven consent", n)
	}
}

func TestTidAlreadyLinkedElsewhere(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	state, cookie := r.startConsent(tok)
	rec := r.do(http.MethodGet, callback(state, tidA), nil, cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already connected elsewhere") {
		t.Fatalf("linked elsewhere: %d %s", rec.Code, rec.Body)
	}
	if n := len(r.connectionsOf(tenantC)); n != 0 {
		t.Fatalf("%d connections created for a tid another tenant holds", n)
	}
}

func TestOIDCOnboarding(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	form := url.Values{"issuer": {r.oidc.Issuer("")}, "client_id": {"northwind-oidc"}, "client_secret": {"northwind-secret"}}
	rec := r.do(http.MethodPost, "/onboard/"+tok+"/oidc", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("oidc form: %d %s", rec.Code, rec.Body)
	}
	next, _ := url.Parse(rec.Header().Get("Location"))
	if next.Query().Get("provider") != "oidc" || next.Query().Get("invite") != tok {
		t.Fatalf("hand-off = %s", next)
	}
	conns := r.connectionsOf(tenantC)
	if len(conns) != 1 || conns[0].Status != identity.StatusPending || conns[0].Issuer != r.oidc.Issuer("") {
		t.Fatalf("connections = %+v", conns)
	}
	if secret, err := r.cipher.Open(tenantC, conns[0].ClientSecretEnc); err != nil || secret != "northwind-secret" {
		t.Fatalf("stored secret opens to %q, %v", secret, err)
	}
	begun, err := r.ident.Begin(context.Background(), identity.BeginRequest{Invite: tok, Provider: "oidc", RedirectURI: public + "/callback"})
	if err != nil {
		t.Fatal(err)
	}
	code, st := r.oidc.Authorize(t, begun.AuthorizeURL, idptest.Identity{Subject: "00u-dave", Email: "dave@northwind.example"})
	res, err := r.ident.Complete(context.Background(), identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: st})
	if err != nil {
		t.Fatalf("activating sign-in: %v", err)
	}
	if strings.Join(res.Principal.Roles, ",") != "admin" || res.Principal.IdP != "oidc" {
		t.Fatalf("principal = %+v", res.Principal)
	}
	// From now on the domain routes work emails to the connection.
	begun, err = r.ident.Begin(context.Background(), identity.BeginRequest{Email: "erin@northwind.example", RedirectURI: public + "/callback"})
	if err != nil || !strings.HasPrefix(begun.AuthorizeURL, r.oidc.Issuer("")) {
		t.Fatalf("email sign-in after onboarding: %v %s", err, begun.AuthorizeURL)
	}
}

func TestOIDCFormValidation(t *testing.T) {
	r := newRig(t)
	tok, _ := r.invite(tenantC, time.Hour)
	cases := map[string]url.Values{
		"issuer mismatch": {"issuer": {r.oidc.Issuer("") + "/"}, "client_id": {"c"}},
		"no client id":    {"issuer": {r.oidc.Issuer("")}},
		"unreachable":     {"issuer": {"http://127.0.0.1:1"}, "client_id": {"c"}},
		"not a URL":       {"issuer": {"login.example.com"}, "client_id": {"c"}},
		"query on issuer": {"issuer": {r.oidc.Issuer("") + "?x=1"}, "client_id": {"c"}},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			rec := r.do(http.MethodPost, "/onboard/"+tok+"/oidc", form)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "role=\"alert\"") {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if n := len(r.connectionsOf(tenantC)); n != 0 {
		t.Fatalf("%d connections created from invalid forms", n)
	}
}

func TestOIDCIssuerLinkedElsewhere(t *testing.T) {
	r := newRig(t)
	r.store.AddConnection(identity.Connection{TenantID: tenantA, Provider: identity.ProviderOIDC, Issuer: r.oidc.Issuer(""),
		ClientID: "x", Status: identity.StatusActive})
	tok, _ := r.invite(tenantC, time.Hour)
	rec := r.do(http.MethodPost, "/onboard/"+tok+"/oidc", url.Values{"issuer": {r.oidc.Issuer("")}, "client_id": {"northwind-oidc"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPrivateAddressesRefusedOutsideTheLab(t *testing.T) {
	client := identity.NewHTTPClient(false, 2*time.Second)
	_, err := identity.ValidateIssuer(context.Background(), client, "https://127.0.0.1:9/realm", false)
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("err = %v", err)
	}
	if _, err := identity.ValidateIssuer(context.Background(), client, "http://idp.example.com", false); err == nil {
		t.Fatal("an http issuer was accepted outside the lab")
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{"Contoso.COM": "contoso.com", "@lab.test": "lab.test", "sub.example.co.uk.": "sub.example.co.uk"} {
		if got, err := onboard.NormalizeDomain(in); err != nil || got != want {
			t.Fatalf("%q -> %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "a@b.com", "-x.com", "x..com", "exa mple.com"} {
		if _, err := onboard.NormalizeDomain(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
