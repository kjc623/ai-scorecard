// Package onboard is how a customer's admin links the product tenant the vendor created to their
// identity provider: the /onboard/* pages, the Entra admin-consent round trip, the OIDC provider
// form, and the vendor operator's tenant and invite commands.
//
// The invite token is the only credential before the first sign-in, so every step re-checks it, and
// none of them activates anything. A connection is created pending — Entra after Microsoft reports
// admin consent, OIDC after the provider's discovery validates — and becomes active only when a real
// sign-in through it succeeds (internal/identity's Complete), which is also what makes the person who
// signed in the tenant's first admin and spends the invite.
//
// The pages are small server-rendered HTML with no script and no external asset, served under the
// product's public origin, with no-referrer so the invite in the URL never leaks to Microsoft or the
// customer's provider in a Referer header.
package onboard

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

const (
	// PathPrefix is the onboarding pages' path on the public origin.
	PathPrefix = "/onboard/"
	// PathEntraCallback is the admin-consent redirect URI, registered on the vendor's Entra app.
	PathEntraCallback = "/onboard/entra/callback"
	// consentCookie carries the invite from the consent start to its callback, in the same browser.
	consentCookie = "sac_onboard"
	// consentMarker is the state column of a consent record in ops.auth_signin, so a record can
	// never be mistaken for a sign-in attempt (which also has a nonce and a verifier).
	consentMarker = "entra_admin_consent"
	consentTTL    = 10 * time.Minute
	maxFormBytes  = 64 << 10
)

// EntraConfig turns on the "Microsoft Entra ID" choice.
type EntraConfig struct {
	ClientID string
	// LoginBaseURL is the identity platform host; empty is https://login.microsoftonline.com. Tests
	// point it at a fake provider.
	LoginBaseURL string
	// ConsentProbe proves the consent landed by obtaining an app-only token in the consenting tenant
	// (entraapp.App.Token). The callback's `tenant` parameter is only a query string, so without the
	// probe a person holding an invite could park a pending connection on any tid; with it they can
	// park one only on a tenant that has really consented to the vendor app. Nil skips the probe.
	ConsentProbe func(ctx context.Context, entraTenantID string) error
}

// Config wires the onboarding service.
type Config struct {
	Store  identity.Store
	Cipher *directory.Cipher
	// PublicURL is the product's browser-facing origin (SAC_PUBLIC_URL), under which /onboard/*
	// reaches this service. The consent redirect URI and the sign-in hand-off are built from it.
	PublicURL string
	// SignInPath is the dashboard's sign-in route; default /login. It receives ?invite=&provider=
	// and passes both to POST /internal/v1/auth/begin.
	SignInPath string
	Entra      *EntraConfig
	HTTPClient *http.Client
	// AllowInsecureIssuers admits http issuers and private addresses. It exists for the local lab's
	// test identity provider and is never set in production.
	AllowInsecureIssuers bool
	// ProbeRetry is the first wait between consent probes; Entra replicates a new service principal
	// within seconds, so the probe tries three times. Default two seconds.
	ProbeRetry time.Duration
	Logger     *slog.Logger
	Now        func() time.Time
}

// Service serves the onboarding pages.
type Service struct {
	cfg       Config
	public    string
	secure    bool
	loginBase string
	client    *http.Client
	log       *slog.Logger
	now       func() time.Time
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Cipher == nil {
		return nil, errors.New("onboard: a store and a cipher are required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("onboard: SAC_PUBLIC_URL %q is not an absolute http(s) URL", cfg.PublicURL)
	}
	if cfg.SignInPath == "" {
		cfg.SignInPath = "/login"
	}
	if cfg.ProbeRetry <= 0 {
		cfg.ProbeRetry = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = identity.NewHTTPClient(cfg.AllowInsecureIssuers, 10*time.Second)
	}
	s := &Service{cfg: cfg, public: u.String(), secure: u.Scheme == "https", client: cfg.HTTPClient, log: cfg.Logger, now: cfg.Now}
	if cfg.Entra != nil {
		if cfg.Entra.ClientID == "" {
			return nil, errors.New("onboard: Entra onboarding needs the app's client id")
		}
		s.loginBase = strings.TrimRight(cfg.Entra.LoginBaseURL, "/")
		if s.loginBase == "" {
			s.loginBase = "https://login.microsoftonline.com"
		}
	}
	return s, nil
}

// Handler serves everything under /onboard/.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageHeaders(w)
		if r.URL.Path == PathEntraCallback {
			if r.Method != http.MethodGet {
				s.page(w, http.StatusMethodNotAllowed, errorPage("Not available", "This address only accepts the response from Microsoft."))
				return
			}
			s.entraCallback(w, r)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, PathPrefix)
		if !ok {
			s.page(w, http.StatusNotFound, errorPage("Not found", "There is no onboarding page at this address."))
			return
		}
		invite, action, _ := strings.Cut(rest, "/")
		switch {
		case action == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			s.landing(w, r, invite, "", http.StatusOK)
		case action == "entra" && r.Method == http.MethodPost && s.cfg.Entra != nil:
			s.startEntra(w, r, invite)
		case action == "oidc" && r.Method == http.MethodPost:
			s.connectOIDC(w, r, invite)
		default:
			s.page(w, http.StatusNotFound, errorPage("Not found", "There is no onboarding page at this address."))
		}
	})
}

// pageHeaders keeps the invite in the URL from leaving in a Referer, forbids framing, and permits no
// script and nothing from another host.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
}

// liveInvite resolves a token to its unused, unexpired invite, or the page that says why not.
func (s *Service) liveInvite(ctx context.Context, token string) (identity.Invite, *pageData, int) {
	tenant, err := identity.ParseInviteToken(token)
	if err != nil {
		return identity.Invite{}, errorPage("This onboarding link is not valid", "Check that the whole link was copied, or ask your vendor contact for a new one."), http.StatusNotFound
	}
	inv, err := s.cfg.Store.Invite(ctx, tenant, identity.HashToken(token))
	if errors.Is(err, identity.ErrNotFound) {
		return identity.Invite{}, errorPage("This onboarding link is not valid", "Check that the whole link was copied, or ask your vendor contact for a new one."), http.StatusNotFound
	}
	if err != nil {
		s.log.Error("onboard: invite lookup failed", "error", err)
		return identity.Invite{}, errorPage("Something went wrong", "The service could not check this link. Try again in a minute."), http.StatusServiceUnavailable
	}
	if inv.UsedAt != nil {
		return identity.Invite{}, errorPage("This onboarding link has been used", "Your organisation is already connected. Sign in from the product's sign-in page."), http.StatusGone
	}
	if !s.now().Before(inv.ExpiresAt) {
		return identity.Invite{}, errorPage("This onboarding link has expired", "Ask your vendor contact for a new one."), http.StatusGone
	}
	return inv, nil, 0
}

func (s *Service) landing(w http.ResponseWriter, r *http.Request, token, formError string, status int) {
	inv, bad, code := s.liveInvite(r.Context(), token)
	if bad != nil {
		s.page(w, code, bad)
		return
	}
	sum, err := s.cfg.Store.TenantSummary(r.Context(), inv.TenantID)
	if err != nil {
		s.log.Error("onboard: tenant summary failed", "tenant", inv.TenantID, "error", err)
		s.page(w, http.StatusServiceUnavailable, errorPage("Something went wrong", "The service could not load this onboarding. Try again in a minute."))
		return
	}
	s.page(w, status, &pageData{
		Title: "Connect " + sum.Name, Landing: true, Tenant: sum.Name, Domains: sum.Domains,
		Invite: token, Entra: s.cfg.Entra != nil, RedirectURI: s.public + "/callback",
		Expires: inv.ExpiresAt.UTC().Format("2 January 2006 15:04 UTC"), FormError: formError,
		Issuer: r.PostFormValue("issuer"), ClientID: r.PostFormValue("client_id"),
	})
}

// startEntra records a consent bound to this invite and sends the browser to Microsoft's
// admin-consent endpoint. The state is the server-side half of the binding; the cookie is the
// browser-side half, so a callback is honoured only in the browser that started it, for the invite it
// started with.
func (s *Service) startEntra(w http.ResponseWriter, r *http.Request, token string) {
	inv, bad, code := s.liveInvite(r.Context(), token)
	if bad != nil {
		s.page(w, code, bad)
		return
	}
	state, err := identity.RandomToken()
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.now().UTC()
	callback := s.public + PathEntraCallback
	if err := s.cfg.Store.PutAttempt(r.Context(), identity.Attempt{
		Hash: identity.ConsentHash(state), State: consentMarker, RedirectURI: callback, InviteID: inv.ID,
		CreatedAt: now, ExpiresAt: now.Add(consentTTL),
	}); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: consentCookie, Value: token, Path: PathEntraCallback, MaxAge: int(consentTTL.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{
		"client_id":    {s.cfg.Entra.ClientID},
		"scope":        {"https://graph.microsoft.com/.default"},
		"redirect_uri": {callback},
		"state":        {state},
	}
	http.Redirect(w, r, s.loginBase+"/organizations/v2.0/adminconsent?"+q.Encode(), http.StatusSeeOther)
}

// entraCallback receives Microsoft's admin-consent answer.
func (s *Service) entraCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	restart := errorPage("This consent response cannot be used",
		"It does not match an onboarding started in this browser in the last ten minutes. Open your onboarding link again and choose Microsoft Entra ID.")
	state := q.Get("state")
	if state == "" || len(state) > 256 {
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	a, err := s.cfg.Store.TakeAttempt(ctx, identity.ConsentHash(state))
	if errors.Is(err, identity.ErrNotFound) {
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if a.State != consentMarker || a.InviteID == "" || !s.now().Before(a.ExpiresAt) {
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	cookie, err := r.Cookie(consentCookie)
	if err != nil {
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Value: "", Path: PathEntraCallback, MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	inv, bad, code := s.liveInvite(ctx, cookie.Value)
	if bad != nil {
		s.page(w, code, bad)
		return
	}
	if inv.ID != a.InviteID {
		s.log.Warn("onboard: consent callback for one invite arrived with another's cookie", "invite", inv.ID)
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	if e := q.Get("error"); e != "" {
		s.page(w, http.StatusForbidden, errorPage("Microsoft did not grant consent",
			"Microsoft answered "+e+". An administrator who can grant tenant-wide consent (Global Administrator, Privileged Role Administrator or Cloud Application Administrator) must approve. Open your onboarding link again to retry."))
		return
	}
	tid := strings.ToLower(q.Get("tenant"))
	if !strings.EqualFold(q.Get("admin_consent"), "true") || !session.IsUUID(tid) {
		s.page(w, http.StatusBadRequest, restart)
		return
	}
	if probe := s.cfg.Entra.ConsentProbe; probe != nil {
		if err := s.probe(ctx, probe, tid); err != nil {
			s.log.Warn("onboard: consent probe failed", "invite", inv.ID, "entra_tenant", tid, "error", err)
			s.page(w, http.StatusBadGateway, errorPage("Consent could not be confirmed",
				"Microsoft reported consent, but the application cannot yet act in your tenant. Wait a minute, then open your onboarding link again."))
			return
		}
	}
	_, err = s.cfg.Store.CreatePendingConnection(ctx, identity.NewConnection{
		TenantID: inv.TenantID, Provider: identity.ProviderEntra, EntraTenantID: tid,
	}, identity.AuditEntry{
		ActorType: "system", ActorID: "onboarding-invite:" + inv.ID, Action: "identity_connection.create",
		ObjectType: "identity_connection", Detail: map[string]any{"provider": identity.ProviderEntra, "entra_tenant_id": tid},
	})
	if !s.connectionCreated(w, err, inv) {
		return
	}
	s.log.Info("onboard: Entra consent recorded; awaiting the activating sign-in", "tenant", inv.TenantID, "entra_tenant", tid)
	http.Redirect(w, r, s.signInURL(cookie.Value, identity.ProviderEntra), http.StatusSeeOther)
}

func (s *Service) probe(ctx context.Context, probe func(context.Context, string) error, tid string) error {
	wait := s.cfg.ProbeRetry
	var err error
	for try := 0; try < 3; try++ {
		if err = probe(ctx, tid); err == nil {
			return nil
		}
		if try == 2 {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait *= 2
	}
	return err
}

// connectOIDC validates a customer-entered provider and records it pending.
func (s *Service) connectOIDC(w http.ResponseWriter, r *http.Request, token string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.page(w, http.StatusBadRequest, errorPage("The form could not be read", "Go back and submit it again."))
		return
	}
	inv, bad, code := s.liveInvite(r.Context(), token)
	if bad != nil {
		s.page(w, code, bad)
		return
	}
	issuer := strings.TrimSpace(r.PostFormValue("issuer"))
	clientID := strings.TrimSpace(r.PostFormValue("client_id"))
	secret := r.PostFormValue("client_secret")
	switch {
	case issuer == "" || len(issuer) > 512:
		s.landing(w, r, token, "Enter the issuer URL your provider publishes.", http.StatusBadRequest)
		return
	case clientID == "" || len(clientID) > 256:
		s.landing(w, r, token, "Enter the client id of the application you registered.", http.StatusBadRequest)
		return
	case len(secret) > 4096:
		s.landing(w, r, token, "The client secret is longer than any provider issues.", http.StatusBadRequest)
		return
	}
	if _, err := identity.ValidateIssuer(r.Context(), s.client, issuer, s.cfg.AllowInsecureIssuers); err != nil {
		s.log.Info("onboard: OIDC provider failed validation", "invite", inv.ID, "error", err)
		s.landing(w, r, token, "The provider could not be validated: "+err.Error(), http.StatusBadRequest)
		return
	}
	sealed, err := s.cfg.Cipher.Seal(inv.TenantID, secret)
	if err != nil {
		s.fail(w, err)
		return
	}
	_, err = s.cfg.Store.CreatePendingConnection(r.Context(), identity.NewConnection{
		TenantID: inv.TenantID, Provider: identity.ProviderOIDC, Issuer: issuer, ClientID: clientID, ClientSecretEnc: sealed,
	}, identity.AuditEntry{
		ActorType: "system", ActorID: "onboarding-invite:" + inv.ID, Action: "identity_connection.create",
		ObjectType: "identity_connection", Detail: map[string]any{"provider": identity.ProviderOIDC, "issuer": issuer},
	})
	if !s.connectionCreated(w, err, inv) {
		return
	}
	s.log.Info("onboard: OIDC provider recorded; awaiting the activating sign-in", "tenant", inv.TenantID)
	http.Redirect(w, r, s.signInURL(token, identity.ProviderOIDC), http.StatusSeeOther)
}

func (s *Service) connectionCreated(w http.ResponseWriter, err error, inv identity.Invite) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, identity.ErrLinkedElsewhere):
		s.log.Warn("onboard: provider already linked to another tenant", "invite", inv.ID)
		s.page(w, http.StatusConflict, errorPage("This identity provider is already connected elsewhere",
			"It is linked to another organisation in this product, so it cannot be connected here. Contact your vendor."))
	case errors.Is(err, identity.ErrConnectionDisabled):
		s.page(w, http.StatusForbidden, errorPage("This identity provider has been disabled",
			"It was disabled for your organisation. Contact your vendor."))
	default:
		s.fail(w, err)
	}
	return false
}

// signInURL hands the browser to the dashboard's sign-in, which begins the activating sign-in with
// the invite. The invite was already in the onboarding URL, so this exposes nothing new.
func (s *Service) signInURL(token, provider string) string {
	q := url.Values{"invite": {token}, "provider": {provider}}
	return s.public + s.cfg.SignInPath + "?" + q.Encode()
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	s.log.Error("onboard: internal failure", "error", err)
	s.page(w, http.StatusServiceUnavailable, errorPage("Something went wrong", "Nothing was changed. Try again in a minute."))
}

// ---------------------------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------------------------

type pageData struct {
	Title   string
	Message string
	Landing bool

	Tenant      string
	Domains     []string
	Invite      string
	Entra       bool
	RedirectURI string
	Expires     string
	FormError   string
	Issuer      string
	ClientID    string
}

func errorPage(title, message string) *pageData { return &pageData{Title: title, Message: message} }

func (s *Service) page(w http.ResponseWriter, status int, d *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplate.Execute(w, d); err != nil {
		s.log.Error("onboard: render", "error", err)
	}
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><title>{{.Title}}</title>
<style>
body{font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:#f6f7f9;color:#1d2330}
main{max-width:44rem;margin:3rem auto;padding:0 1rem}
h1{font-size:1.5rem;margin:0 0 .5rem}h2{font-size:1.1rem;margin:0 0 .5rem}
section{background:#fff;border:1px solid #dde1e7;border-radius:6px;padding:1.25rem;margin:1rem 0}
label{display:block;font-weight:600;margin:.75rem 0 .25rem}
input{width:100%;box-sizing:border-box;padding:.5rem;border:1px solid #b9c0cb;border-radius:4px;font:inherit}
button{margin-top:1rem;padding:.55rem 1rem;border:0;border-radius:4px;background:#1f4fd1;color:#fff;font:inherit;cursor:pointer}
code{background:#eef0f4;padding:.1rem .3rem;border-radius:3px;word-break:break-all}
.muted{color:#5b6475}.error{background:#fdecec;border-color:#f1b5b5;color:#7a1414}
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Landing}}
<p>Your vendor has created this organisation for <strong>{{.Tenant}}</strong>{{if .Domains}}, for people signing in with
{{range $i, $d := .Domains}}{{if $i}}, {{end}}<code>@{{$d}}</code>{{end}}{{end}}. Connect your identity provider, then sign in
once through it: that sign-in makes you the organisation's first administrator. This link works once and expires {{.Expires}}.</p>
{{if .FormError}}<section class="error" role="alert">{{.FormError}}</section>{{end}}
{{if .Entra}}<section><h2>Microsoft Entra ID</h2>
<p>You will be asked to grant tenant-wide admin consent to the application. It asks for sign-in
(<code>openid</code>, <code>profile</code>, <code>email</code>, <code>offline_access</code>), to read Intune managed
devices (<code>DeviceManagementManagedDevices.Read.All</code>), which lets device enrolment check that a device is yours,
and to read your people and groups (<code>User.Read.All</code>, <code>GroupMember.Read.All</code>), which the product
does only once an admin turns on reading the directory; SCIM provisioning works without it.</p>
<form method="post" action="/onboard/{{.Invite}}/entra"><button type="submit">Connect Microsoft Entra ID</button></form>
</section>{{end}}
<section><h2>Another OpenID Connect provider</h2>
<p class="muted">Okta, Ping, Google, ADFS or any provider that supports OpenID Connect. Register a web application
using the authorization-code flow with this redirect URI: <code>{{.RedirectURI}}</code>, and scopes
<code>openid profile email</code>. Product roles come from a <code>roles</code> claim if your provider sends one;
otherwise roles are granted in the product after you sign in.</p>
<form method="post" action="/onboard/{{.Invite}}/oidc">
<label for="issuer">Issuer URL</label><input id="issuer" name="issuer" type="url" required value="{{.Issuer}}" placeholder="https://login.example.com">
<label for="client_id">Client id</label><input id="client_id" name="client_id" required value="{{.ClientID}}" autocomplete="off">
<label for="client_secret">Client secret</label><input id="client_secret" name="client_secret" type="password" autocomplete="off">
<button type="submit">Validate and continue</button></form>
</section>
{{else}}<p>{{.Message}}</p>{{end}}
</main></body></html>
`))
