package identity_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/identity/idptest"
	"github.com/shadow-ai-capture/control-api/internal/session"
	"github.com/shadow-ai-capture/control-api/internal/session/sessiontest"
)

func TestEntraHappyPath(t *testing.T) {
	r := newRig(t)
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Session == "" || res.AccessToken == "" || res.ExpiresIn <= 0 || res.ExpiresIn > 600 {
		t.Fatalf("result = %+v", res)
	}
	want := session.Principal{Tenant: tenantA, Actor: alice, Roles: []string{"analyst"}, IdP: "entra"}
	if p := res.Principal; p.Tenant != want.Tenant || p.Actor != want.Actor || p.IdP != want.IdP || strings.Join(p.Roles, ",") != "analyst" {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}
	for _, aud := range []string{session.AudienceQuery, session.AudienceVault, session.AudienceControl} {
		p, err := r.verifier.Verify(res.AccessToken, aud)
		if err != nil {
			t.Fatalf("product token for %s: %v", aud, err)
		}
		if p.Subject != r.entraConn.ID+":"+aliceOID || p.SessionID == "" {
			t.Fatalf("token sub/sid = %q/%q", p.Subject, p.SessionID)
		}
	}
	form, _ := r.entra.Form()
	if form.Get("client_secret") != entraSecret || form.Get("code_verifier") == "" {
		t.Fatalf("the code was redeemed without the app credential or the PKCE verifier")
	}
	if !has(r.auditActions(tenantA), "auth.sign_in") {
		t.Fatalf("no auth.sign_in audit row")
	}
	for _, row := range r.store.AuditRows() {
		if row.Action == "auth.sign_in" && row.ActorID != alice {
			t.Fatalf("sign-in audited as %q, want the real actor", row.ActorID)
		}
	}
}

func TestBeginBuildsPKCEStateNonce(t *testing.T) {
	r := newRig(t)
	begun, err := r.svc.Begin(ctx, identity.BeginRequest{Email: "Bob@Fabrikam.example", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(begun.AuthorizeURL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("state") == "" ||
		q.Get("nonce") == "" || q.Get("login_hint") != "bob@fabrikam.example" || q.Get("client_id") != oidcClient {
		t.Fatalf("authorize query = %v", q)
	}
	if strings.Contains(begun.AuthorizeURL, begun.Attempt) {
		t.Fatal("the attempt handle leaked into the authorize URL")
	}
}

func TestOIDCHappyPath(t *testing.T) {
	r := newRig(t)
	res, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("content_reader", "viewer"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	p := res.Principal
	if p.Tenant != tenantB || p.Actor != "bob@fabrikam.example" || p.IdP != "oidc" || strings.Join(p.Roles, ",") != "viewer,content_reader" {
		t.Fatalf("principal = %+v", p)
	}
	tok, err := r.verifier.Verify(res.AccessToken, session.AudienceQuery)
	if err != nil || tok.Subject != r.oidcConn.ID+":okta|bob" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	form, _ := r.oidc.Form()
	if form.Get("client_secret") != oidcSecret {
		t.Fatalf("the OIDC client secret was not presented (client_secret_post)")
	}
}

func TestOIDCClientSecretBasicWhenOnlyBasicIsAdvertised(t *testing.T) {
	r := newRig(t)
	r.oidc.AuthMethods = []string{"client_secret_basic"}
	if _, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer")); err != nil {
		t.Fatal(err)
	}
	form, basic := r.oidc.Form()
	if form.Get("client_secret") != "" || basic != [2]string{oidcClient, oidcSecret} {
		t.Fatalf("form secret %q, basic %v", form.Get("client_secret"), basic)
	}
}

func TestEntraIssuerMustBeTheTokensOwnTenant(t *testing.T) {
	r := newRig(t)
	r.entra.Mutate = func(c map[string]any) { c["iss"] = r.entra.Issuer(tidUnknown) }
	_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
	if ref := refusal(t, err, identity.CodeSignInFailed); !strings.Contains(ref.Reason, "issuer") {
		t.Fatalf("reason = %s", ref.Reason)
	}
}

func TestEntraTokenFromAnotherTenantsTidIsRefusedEvenWithConsistentIssuer(t *testing.T) {
	r := newRig(t)
	who := r.entraUser("admin")
	who.TenantID = tidUnknown
	_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, who)
	refusal(t, err, identity.CodeTenantNotOnboarded)
}

func TestEntraEmailFlowPinsTheConnectionsTid(t *testing.T) {
	r := newRig(t)
	who := r.entraUser("analyst")
	who.TenantID = tidUnknown // a valid token, but from a tenant other than the one the domain chose
	_, err := r.signIn(identity.BeginRequest{Email: alice}, r.entra, who)
	refusal(t, err, identity.CodeTenantNotOnboarded)
}

func TestDisabledConnection(t *testing.T) {
	t.Run("oidc disabled between begin and complete", func(t *testing.T) {
		r := newRig(t)
		begun, err := r.svc.Begin(ctx, identity.BeginRequest{Email: "bob@fabrikam.example", RedirectURI: redirect})
		if err != nil {
			t.Fatal(err)
		}
		code, state := r.oidc.Authorize(t, begun.AuthorizeURL, r.oidcUser("viewer"))
		r.store.SetConnectionStatus(r.oidcConn.ID, identity.StatusDisabled)
		_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
		refusal(t, err, identity.CodeConnectionDisabled)
		if !has(r.auditActions(tenantB), "auth.sign_in_refused") {
			t.Fatal("the refusal was not audited")
		}
	})
	t.Run("email for a tenant whose only connection is disabled", func(t *testing.T) {
		r := newRig(t)
		r.store.SetConnectionStatus(r.oidcConn.ID, identity.StatusDisabled)
		_, err := r.svc.Begin(ctx, identity.BeginRequest{Email: "bob@fabrikam.example", RedirectURI: redirect})
		refusal(t, err, identity.CodeNoSSOConnection)
	})
	t.Run("entra disabled: the definer lookup returns active connections only", func(t *testing.T) {
		r := newRig(t)
		r.store.SetConnectionStatus(r.entraConn.ID, identity.StatusDisabled)
		_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
		refusal(t, err, identity.CodeTenantNotOnboarded)
	})
}

func TestAudienceMismatch(t *testing.T) {
	r := newRig(t)
	r.oidc.Mutate = func(c map[string]any) { c["aud"] = "someone-else" }
	_, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	if ref := refusal(t, err, identity.CodeSignInFailed); !strings.Contains(ref.Reason, "audience") {
		t.Fatalf("reason = %s", ref.Reason)
	}
	r.oidc.Mutate = func(c map[string]any) { c["aud"] = []string{oidcClient, "other"} } // several, no azp
	_, err = r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	refusal(t, err, identity.CodeSignInFailed)
}

func TestOIDCIssuerMustBeExact(t *testing.T) {
	r := newRig(t)
	r.oidc.Mutate = func(c map[string]any) { c["iss"] = r.oidc.Issuer("") + "/" }
	_, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	refusal(t, err, identity.CodeSignInFailed)
}

func TestNonceReplay(t *testing.T) {
	r := newRig(t)
	// The same attempt cannot complete twice.
	begun, err := r.svc.Begin(ctx, identity.BeginRequest{Email: "bob@fabrikam.example", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	code, state := r.oidc.Authorize(t, begun.AuthorizeURL, r.oidcUser("viewer"))
	if _, err := r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state}); err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
	refusal(t, err, identity.CodeAttemptUnknown)

	// An id_token minted for the first attempt's nonce does not complete a second attempt.
	firstNonce := mustQuery(t, begun.AuthorizeURL, "nonce")
	r.oidc.Mutate = func(c map[string]any) { c["nonce"] = firstNonce }
	_, err = r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	if ref := refusal(t, err, identity.CodeSignInFailed); !strings.Contains(ref.Reason, "nonce") {
		t.Fatalf("reason = %s", ref.Reason)
	}
}

func mustQuery(t *testing.T, raw, key string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

func TestStateMismatchBurnsTheAttempt(t *testing.T) {
	r := newRig(t)
	begun, err := r.svc.Begin(ctx, identity.BeginRequest{Provider: "entra", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	code, state := r.entra.Authorize(t, begun.AuthorizeURL, r.entraUser("analyst"))
	_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state + "x"})
	refusal(t, err, identity.CodeStateMismatch)
	_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
	refusal(t, err, identity.CodeAttemptUnknown)
	if _, codes, _, _ := r.entra.Counts(); codes != 0 {
		t.Fatalf("a mismatched state still redeemed the code (%d grants)", codes)
	}
}

func TestExpiredAttempt(t *testing.T) {
	r := newRig(t)
	begun, err := r.svc.Begin(ctx, identity.BeginRequest{Provider: "entra", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	code, state := r.entra.Authorize(t, begun.AuthorizeURL, r.entraUser("analyst"))
	r.clock.Advance(identity.AttemptTTL + time.Second)
	_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
	refusal(t, err, identity.CodeAttemptExpired)
}

func TestExpiredAttemptsAreSwept(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		if _, err := r.svc.Begin(ctx, identity.BeginRequest{Provider: "entra", RedirectURI: redirect}); err != nil {
			t.Fatal(err)
		}
	}
	r.clock.Advance(identity.AttemptTTL + 2*time.Minute)
	if _, err := r.svc.Begin(ctx, identity.BeginRequest{Provider: "entra", RedirectURI: redirect}); err != nil {
		t.Fatal(err)
	}
	if n := r.store.Attempts(); n != 1 {
		t.Fatalf("attempts held = %d, want only the live one", n)
	}
}

func TestNoRoleIsRefusedNeverDefaulted(t *testing.T) {
	r := newRig(t)
	_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser())
	refusal(t, err, identity.CodeNoRole)
	_, err = r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("Global Administrator", "superuser"))
	refusal(t, err, identity.CodeNoRole)
	rows := r.store.AuditRows()
	if len(rows) == 0 || rows[len(rows)-1].Action != "auth.sign_in_refused" || rows[len(rows)-1].ActorID != alice {
		t.Fatalf("refusal audit rows = %+v", rows)
	}
}

func TestRoleMap(t *testing.T) {
	r := newRig(t)
	c, _ := r.store.Connection(r.oidcConn.ID)
	c.RoleMap = map[string]string{"SAC-Readers": "viewer", "SAC-Investigators": "content_reader", "SAC-Bogus": "root"}
	c.RolesClaim = "groups"
	r.store.AddConnection(c)
	who := r.oidcUser()
	who.Extra = map[string]any{"groups": []string{"SAC-Investigators", "analyst", "SAC-Bogus"}}
	res, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, who)
	if err != nil {
		t.Fatal(err)
	}
	// "analyst" is a product role name but the map does not name it, so it grants nothing.
	if got := strings.Join(res.Principal.Roles, ","); got != "content_reader" {
		t.Fatalf("roles = %s, want content_reader", got)
	}
	who.Extra = map[string]any{"groups": "SAC-Readers"} // a single string claim
	res, err = r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, who)
	if err != nil || strings.Join(res.Principal.Roles, ",") != "viewer" {
		t.Fatalf("roles = %v, %v", res.Principal.Roles, err)
	}
}

func TestRoleGrantUnitesWithIdPRoles(t *testing.T) {
	r := newRig(t)
	r.store.AddRoleGrant(tenantA, r.entraConn.ID, aliceOID, "admin", "test")
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Principal.Roles, ","); got != "admin" {
		t.Fatalf("roles = %s", got)
	}
	res, err = r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer"))
	if err != nil || strings.Join(res.Principal.Roles, ",") != "viewer,admin" {
		t.Fatalf("roles = %v, %v", res.Principal.Roles, err)
	}
}

// scimSetup gives tenant A a user_ref key, sealed as directory.UserRefKeys seals it, and returns
// alice's canonical ref under it.
func scimSetup(t *testing.T, r *rig) string {
	key := make([]byte, protocol.UserRefKeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	sealed, err := r.cipher.SealBytes(tenantA, key)
	if err != nil {
		t.Fatal(err)
	}
	r.store.SetUserRefKey(tenantA, sealed)
	ref, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, alice)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestSCIMDeactivationEndsTheSession(t *testing.T) {
	r := newRig(t)
	ref := scimSetup(t, r)
	r.store.SetScimUser(tenantA, ref, true)
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := r.sessions.ByHash(ctx, session.HashID(res.Session))
	if rec.UserRef != ref {
		t.Fatalf("session user_ref = %q, want %q", rec.UserRef, ref)
	}
	if _, err := r.svc.Token(ctx, res.Session); err != nil {
		t.Fatalf("Token while active: %v", err)
	}
	r.store.SetScimUser(tenantA, ref, false)
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
	r.store.SetScimUser(tenantA, ref, true) // reactivation does not resurrect an ended session
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
	if !has(r.auditActions(tenantA), "auth.session_end") {
		t.Fatal("the system-ended session was not audited")
	}
}

func TestSCIMDeletedUserCannotReMint(t *testing.T) {
	r := newRig(t)
	ref := scimSetup(t, r)
	r.store.SetScimUser(tenantA, ref, true)
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
	if err != nil {
		t.Fatal(err)
	}
	r.store.DeleteScimUser(tenantA, ref)
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
}

func TestSCIMInactiveUserCannotSignIn(t *testing.T) {
	r := newRig(t)
	scimSetup(t, r)
	// Known only by the oid alias: the derivation's second kind resolves through ops.user_ref_alias.
	key := make([]byte, protocol.UserRefKeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	oidRef, _ := protocol.DeriveUserRef(key, protocol.UserRefOID, aliceOID)
	r.store.AddAlias(tenantA, oidRef, "u_canonical")
	r.store.SetScimUser(tenantA, "u_canonical", false)
	who := r.entraUser("analyst")
	who.PreferredUsername = "renamed@contoso.example"
	_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, who)
	refusal(t, err, identity.CodeUserDeactivated)
}

func TestSessionIdleAndMaxAge(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		r := newRig(t)
		res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer"))
		if err != nil {
			t.Fatal(err)
		}
		r.clock.Advance(59 * time.Minute)
		if _, err := r.svc.Token(ctx, res.Session); err != nil {
			t.Fatalf("Token inside the idle bound: %v", err)
		}
		r.clock.Advance(session.IdleTimeout)
		_, err = r.svc.Token(ctx, res.Session)
		refusal(t, err, identity.CodeSessionEnded)
	})
	t.Run("max age", func(t *testing.T) {
		r := newRig(t)
		res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer"))
		if err != nil {
			t.Fatal(err)
		}
		for elapsed := time.Duration(0); elapsed+50*time.Minute < session.MaxAge; elapsed += 50 * time.Minute {
			r.clock.Advance(50 * time.Minute)
			if _, err := r.svc.Token(ctx, res.Session); err != nil {
				t.Fatalf("Token at %s: %v", elapsed+50*time.Minute, err)
			}
		}
		r.clock.Advance(50 * time.Minute)
		_, err = r.svc.Token(ctx, res.Session)
		refusal(t, err, identity.CodeSessionEnded)
	})
}

func TestRevoke(t *testing.T) {
	r := newRig(t)
	res, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(ctx, res.Session); err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
	if err := r.svc.Revoke(ctx, res.Session); err != nil {
		t.Fatalf("a second revoke: %v", err)
	}
	if err := r.svc.Revoke(ctx, "never-issued"); err != nil {
		t.Fatalf("revoking an unknown session: %v", err)
	}
	n := 0
	for _, row := range r.store.AuditRows() {
		if row.Action == "auth.revoke" {
			n++
			if row.ActorID != "bob@fabrikam.example" {
				t.Fatalf("revoke audited as %q", row.ActorID)
			}
		}
	}
	if n != 1 {
		t.Fatalf("auth.revoke rows = %d, want 1", n)
	}
}

func TestDisablingTheConnectionEndsLiveSessions(t *testing.T) {
	r := newRig(t)
	res, err := r.signIn(identity.BeginRequest{Email: "bob@fabrikam.example"}, r.oidc, r.oidcUser("viewer"))
	if err != nil {
		t.Fatal(err)
	}
	r.store.SetConnectionStatus(r.oidcConn.ID, identity.StatusDisabled)
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
}

func TestRefreshAtMostEveryThirtyMinutesAndFailureEndsTheSession(t *testing.T) {
	r := newRig(t)
	r.entra.RefreshTokens = true
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer"))
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(10 * time.Minute)
	if _, err := r.svc.Token(ctx, res.Session); err != nil {
		t.Fatal(err)
	}
	if _, _, refreshes, _ := r.entra.Counts(); refreshes != 0 {
		t.Fatalf("refreshed after 10 minutes (%d)", refreshes)
	}
	r.clock.Advance(25 * time.Minute)
	if _, err := r.svc.Token(ctx, res.Session); err != nil {
		t.Fatal(err)
	}
	if _, _, refreshes, _ := r.entra.Counts(); refreshes != 1 {
		t.Fatalf("refreshes = %d after 35 minutes, want 1", refreshes)
	}
	form, _ := r.entra.Form()
	if form.Get("grant_type") != "refresh_token" || form.Get("client_secret") != entraSecret {
		t.Fatalf("refresh form = %v", form)
	}
	r.entra.FailRefresh = true
	r.clock.Advance(31 * time.Minute)
	_, err = r.svc.Token(ctx, res.Session)
	refusal(t, err, identity.CodeSessionEnded)
}

func TestProviderKeyRotationIsPickedUp(t *testing.T) {
	r := newRig(t)
	if _, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer")); err != nil {
		t.Fatal(err)
	}
	// A kid the provider does not publish is refused.
	r.entra.SignKid = "invented"
	_, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer"))
	refusal(t, err, identity.CodeSignInFailed)
	// A real rotation: the new kid is fetched and verifies.
	r.entra.SignKid = ""
	r.entra.RotateKey(t, "k2")
	if _, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("viewer")); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

func TestBeginRefusals(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		name string
		req  identity.BeginRequest
		code string
	}{
		{"unknown domain", identity.BeginRequest{Email: "x@unknown.example", RedirectURI: redirect}, identity.CodeNoSSOConnection},
		{"no email", identity.BeginRequest{RedirectURI: redirect}, identity.CodeBadRequest},
		{"relative redirect", identity.BeginRequest{Provider: "entra", RedirectURI: "/callback"}, identity.CodeBadRequest},
		{"unlisted redirect", identity.BeginRequest{Provider: "entra", RedirectURI: "https://elsewhere.example/callback"}, identity.CodeBadRequest},
		{"odd provider", identity.BeginRequest{Provider: "saml", RedirectURI: redirect}, identity.CodeBadRequest},
		{"malformed invite", identity.BeginRequest{Invite: "sacinv_nope", RedirectURI: redirect}, identity.CodeInviteUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.svc.Begin(ctx, c.req)
			refusal(t, err, c.code)
		})
	}
}

func TestRedirectURIsAreRequired(t *testing.T) {
	r := newRig(t)
	mgr, _ := session.NewManager(sessiontest.NewStore(), session.ManagerConfig{})
	base := identity.Config{Store: r.store, Sessions: mgr, Issuer: r.issuer, Cipher: r.cipher}
	if _, err := identity.New(base); err == nil {
		t.Fatal("a service with no redirect URIs was built")
	}
	base.RedirectURIs = []string{"/callback"}
	if _, err := identity.New(base); err == nil {
		t.Fatal("a relative redirect URI was accepted")
	}
}

func TestInviteBeginRefusals(t *testing.T) {
	r := newRig(t)
	token, err := identity.MintInviteToken(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	now := r.clock.Now()
	used := now.Add(-time.Minute)
	inv := r.store.AddInvite(identity.Invite{TenantID: tenantA, TokenHash: identity.HashToken(token), ExpiresAt: now.Add(time.Hour), UsedAt: &used})
	_, err = r.svc.Begin(ctx, identity.BeginRequest{Invite: token, RedirectURI: redirect})
	refusal(t, err, identity.CodeInviteUsed)

	inv.UsedAt, inv.ExpiresAt = nil, now.Add(-time.Second)
	r.store.AddInvite(inv)
	_, err = r.svc.Begin(ctx, identity.BeginRequest{Invite: token, RedirectURI: redirect})
	refusal(t, err, identity.CodeInviteExpired)

	// A token naming tenant A whose hash is stored under tenant B is not found: the invite is bound
	// to the tenant in its own text.
	other, _ := identity.MintInviteToken(tenantA)
	r.store.AddInvite(identity.Invite{TenantID: tenantB, TokenHash: identity.HashToken(other), ExpiresAt: now.Add(time.Hour)})
	_, err = r.svc.Begin(ctx, identity.BeginRequest{Invite: other, RedirectURI: redirect})
	refusal(t, err, identity.CodeInviteUnknown)
}

func TestOnboardingSignInActivatesAndGrantsFirstAdmin(t *testing.T) {
	r := newRig(t)
	const tenantC = "cccccccc-0000-4000-8000-000000000003"
	const tidC = "33333333-2222-4333-8444-555555555555"
	r.store.AddTenant(tenantC, "Northwind")
	token, _ := identity.MintInviteToken(tenantC)
	inv := r.store.AddInvite(identity.Invite{TenantID: tenantC, TokenHash: identity.HashToken(token), ExpiresAt: r.clock.Now().Add(time.Hour)})
	pending := r.store.AddConnection(identity.Connection{TenantID: tenantC, Provider: identity.ProviderEntra, EntraTenantID: tidC, Status: identity.StatusPending, RolesClaim: "roles"})

	// A sign-in from another Entra tenant does not activate it.
	_, err := r.signIn(identity.BeginRequest{Invite: token, Provider: "entra"}, r.entra, r.entraUser())
	refusal(t, err, identity.CodeTenantNotOnboarded)
	if c, _ := r.store.Connection(pending.ID); c.Status != identity.StatusPending {
		t.Fatalf("connection is %s after a failed sign-in", c.Status)
	}

	carol := idptest.Identity{Subject: "0c0c0c0c-1111-4222-8333-444444444444", TenantID: tidC, PreferredUsername: "carol@northwind.example"}
	res, err := r.signIn(identity.BeginRequest{Invite: token, Provider: "entra"}, r.entra, carol)
	if err != nil {
		t.Fatalf("activating sign-in: %v", err)
	}
	if res.Principal.Tenant != tenantC || strings.Join(res.Principal.Roles, ",") != "admin" {
		t.Fatalf("principal = %+v", res.Principal)
	}
	if c, _ := r.store.Connection(pending.ID); c.Status != identity.StatusActive || c.ActivatedBy != "carol@northwind.example" {
		t.Fatalf("connection = %+v", c)
	}
	if got := r.store.InviteState(inv.ID); got.UsedAt == nil || got.UsedBy != "carol@northwind.example" {
		t.Fatalf("invite = %+v", got)
	}
	actions := r.auditActions(tenantC)
	for _, want := range []string{"onboarding_invite.use", "identity_connection.activate", "role.grant", "auth.sign_in"} {
		if !has(actions, want) {
			t.Fatalf("audit %v lacks %s", actions, want)
		}
	}
	// The invite is spent: a second sign-in with it is refused at begin.
	_, err = r.svc.Begin(ctx, identity.BeginRequest{Invite: token, Provider: "entra", RedirectURI: redirect})
	refusal(t, err, identity.CodeInviteUsed)
	// And the tenant now signs in like any other: the tid maps to the active connection.
	if _, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, carol); err != nil {
		t.Fatalf("ordinary sign-in after activation: %v", err)
	}
}

func TestInviteSpentBetweenBeginAndComplete(t *testing.T) {
	r := newRig(t)
	const tenantC = "cccccccc-0000-4000-8000-000000000003"
	r.store.AddTenant(tenantC, "Northwind")
	token, _ := identity.MintInviteToken(tenantC)
	inv := r.store.AddInvite(identity.Invite{TenantID: tenantC, TokenHash: identity.HashToken(token), ExpiresAt: r.clock.Now().Add(time.Hour)})
	// A public client (no secret) on its own provider: issuers are unique across tenants.
	northwind := idptest.NewOIDC(t, "northwind-client", "")
	northwind.Now = r.clock.Now
	r.store.AddConnection(identity.Connection{TenantID: tenantC, Provider: identity.ProviderOIDC, Issuer: northwind.Issuer(""),
		ClientID: "northwind-client", Status: identity.StatusPending})
	begun, err := r.svc.Begin(ctx, identity.BeginRequest{Invite: token, RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	code, state := northwind.Authorize(t, begun.AuthorizeURL, r.oidcUser())
	used := r.clock.Now()
	inv.UsedAt = &used
	r.store.AddInvite(inv)
	_, err = r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
	refusal(t, err, identity.CodeInviteUsed)
}

// A tenant that is not active, or whose reads are closed, gets no product token: sign-in is
// refused, a live session cannot re-mint, and reopening the tenant restores the same session.
func TestClosedTenantGetsNoToken(t *testing.T) {
	r := newRig(t)
	res, err := r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		status string
		read   bool
	}{{"active", false}, {"suspended", true}, {"offboarding", true}, {"closed", false}} {
		r.store.SetTenantAccess(tenantA, c.status, c.read)
		_, err := r.svc.Token(ctx, res.Session)
		refusal(t, err, identity.CodeTenantClosed)
		_, err = r.signIn(identity.BeginRequest{Provider: "entra"}, r.entra, r.entraUser("analyst"))
		refusal(t, err, identity.CodeTenantClosed)
	}
	r.store.SetTenantAccess(tenantA, "active", true)
	if _, err := r.svc.Token(ctx, res.Session); err != nil {
		t.Fatalf("Token after the tenant reopened: %v", err)
	}
	if !has(r.auditActions(tenantA), "auth.sign_in_refused") {
		t.Fatal("the refused sign-in was not audited")
	}
}
