package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/identity/idptest"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

const (
	tenantA     = "aaaaaaaa-0000-4000-8000-000000000001" // on Entra
	tenantB     = "bbbbbbbb-0000-4000-8000-000000000002" // on a generic OIDC provider
	tidA        = "11111111-2222-4333-8444-555555555555"
	tidUnknown  = "99999999-2222-4333-8444-555555555555"
	entraApp    = "app-client-id"
	entraSecret = "entra-lab-secret"
	oidcClient  = "sac-oidc-client"
	oidcSecret  = "oidc-client-secret"
	redirect    = "https://app.example.test/callback"
	alice       = "alice@contoso.example"
	aliceOID    = "0a0a0a0a-1111-4222-8333-444444444444"
)

// clock is the one time source shared by the service, the session manager, the token issuer and the
// fake providers, so a test can age a session without the provider's tokens disagreeing.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type secretAuth struct{ secret string }

func (s secretAuth) ClientAuth(context.Context, string) (url.Values, error) {
	return url.Values{"client_secret": {s.secret}}, nil
}

type rig struct {
	t         *testing.T
	clock     *clock
	store     *identity.MemoryStore
	sessions  *session.MemoryStore
	cipher    *directory.Cipher
	issuer    *session.Issuer
	verifier  *session.Verifier
	svc       *identity.Service
	entra     *idptest.IdP
	oidc      *idptest.IdP
	entraConn identity.Connection
	oidcConn  identity.Connection
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := directory.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ks, err := session.NewKeySet(sk)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := session.NewIssuer(ks, session.IssuerConfig{Issuer: "http://control-api:8080", Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	sessStore := session.NewMemoryStore()
	mgr, err := session.NewManager(sessStore, session.ManagerConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	entra := idptest.NewEntra(t, entraApp, entraSecret)
	entra.Now = clk.Now
	oidc := idptest.NewOIDC(t, oidcClient, oidcSecret)
	oidc.Now = clk.Now

	st := identity.NewMemoryStore()
	st.AddTenant(tenantA, "Contoso")
	st.AddTenant(tenantB, "Fabrikam")
	st.AddEmailDomain("contoso.example", tenantA)
	st.AddEmailDomain("fabrikam.example", tenantB)
	activated := clk.Now().Add(-time.Hour)
	entraConn := st.AddConnection(identity.Connection{
		TenantID: tenantA, Provider: identity.ProviderEntra, EntraTenantID: tidA, Scopes: "openid profile email",
		RolesClaim: "roles", Status: identity.StatusActive, ActivatedAt: &activated,
	})
	sealed, err := cipher.Seal(tenantB, oidcSecret)
	if err != nil {
		t.Fatal(err)
	}
	oidcConn := st.AddConnection(identity.Connection{
		TenantID: tenantB, Provider: identity.ProviderOIDC, Issuer: oidc.Issuer(""), ClientID: oidcClient,
		ClientSecretEnc: sealed, Scopes: "openid profile email", RolesClaim: "roles", Status: identity.StatusActive,
		ActivatedAt: &activated,
	})

	svc, err := identity.New(identity.Config{
		Store: st, Sessions: mgr, Issuer: issuer, Cipher: cipher,
		Entra:            &identity.EntraConfig{ClientID: entraApp, Auth: secretAuth{entraSecret}, LoginBaseURL: entra.Base()},
		AllowInsecureIdP: true,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{
		t: t, clock: clk, store: st, sessions: sessStore, cipher: cipher, issuer: issuer,
		verifier: session.NewVerifier(issuer), svc: svc, entra: entra, oidc: oidc,
		entraConn: entraConn, oidcConn: oidcConn,
	}
}

var ctx = context.Background()

// signIn runs begin → provider → complete.
func (r *rig) signIn(req identity.BeginRequest, idp *idptest.IdP, who idptest.Identity) (identity.CompleteResult, error) {
	r.t.Helper()
	if req.RedirectURI == "" {
		req.RedirectURI = redirect
	}
	begun, err := r.svc.Begin(ctx, req)
	if err != nil {
		r.t.Fatalf("Begin: %v", err)
	}
	code, state := idp.Authorize(r.t, begun.AuthorizeURL, who)
	return r.svc.Complete(ctx, identity.CompleteRequest{Attempt: begun.Attempt, Code: code, State: state})
}

func (r *rig) entraUser(roles ...string) idptest.Identity {
	return idptest.Identity{Subject: aliceOID, TenantID: tidA, PreferredUsername: alice, Roles: roles}
}

func (r *rig) oidcUser(roles ...string) idptest.Identity {
	return idptest.Identity{Subject: "okta|bob", Email: "bob@fabrikam.example", Roles: roles}
}

// refusal asserts err is a Refusal with code.
func refusal(t *testing.T, err error, code string) *identity.Refusal {
	t.Helper()
	var r *identity.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want refusal %s", err, code)
	}
	if r.Code != code {
		t.Fatalf("refusal = %s (%s), want %s", r.Code, r.Reason, code)
	}
	return r
}

func (r *rig) auditActions(tenant string) []string {
	var out []string
	for _, row := range r.store.AuditRows() {
		if row.TenantID == tenant {
			out = append(out, row.Action)
		}
	}
	return out
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
