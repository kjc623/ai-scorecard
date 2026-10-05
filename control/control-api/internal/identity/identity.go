// Package identity is control-api as the OpenID Connect relying party for every customer identity
// provider (contract §0.2, §0.3, §3): Microsoft Entra ID through the vendor's multi-tenant app, and
// any other OIDC provider a customer connects at onboarding.
//
// The dashboard server is a thin client of this package's internal API. It asks Begin for an
// authorize URL, sends the browser there, and hands the code back to Complete; everything that makes
// the sign-in trustworthy — PKCE, state and nonce, the token exchange, the id_token check, which
// product tenant the person belongs to, their roles — happens here, on the server, against state the
// browser never sees. The result is an opaque session id for the cookie and a short product token
// minted by internal/session; the provider's own tokens never leave this process.
//
// The product tenant is decided by our mapping and never by a claim the customer's provider chooses:
// an Entra token's own `tid` must map to an active connection, and an OIDC connection is selected by
// the email domain the vendor registered for the tenant, then pinned to its exact issuer and client
// id. A person with no product role is refused, never defaulted.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

// The refusal codes of the internal API. The four the contract names are tenant_not_onboarded,
// no_role, connection_disabled and session_ended (and no_sso_connection at begin); the rest name
// conditions the dashboard shows as "start again" or that only a misconfigured caller meets.
const (
	CodeBadRequest           = "bad_request"
	CodeNoSSOConnection      = "no_sso_connection"
	CodeAttemptUnknown       = "attempt_unknown"
	CodeAttemptExpired       = "attempt_expired"
	CodeStateMismatch        = "state_mismatch"
	CodeSignInFailed         = "sign_in_failed"
	CodeProviderUnavailable  = "provider_unavailable"
	CodeTenantNotOnboarded   = "tenant_not_onboarded"
	CodeNoRole               = "no_role"
	CodeConnectionDisabled   = "connection_disabled"
	CodeUserDeactivated      = "user_deactivated"
	CodeInviteUnknown        = "invite_unknown"
	CodeInviteUsed           = "invite_used"
	CodeInviteExpired        = "invite_expired"
	CodeOnboardingIncomplete = "onboarding_incomplete"
	CodeSessionEnded         = "session_ended"
	CodeUnavailable          = "unavailable"
)

// Refusal is a decided outcome: an HTTP status and a code for the caller, and a reason for the log
// that is never sent (it can name a claim value or a provider's error).
type Refusal struct {
	Status int
	Code   string
	Reason string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Reason }

func refuse(status int, code, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Code: code, Reason: fmt.Sprintf(format, args...)}
}

const (
	// AttemptTTL is the contract's life of a sign-in attempt and its PKCE/state/nonce.
	AttemptTTL = 10 * time.Minute
	// RefreshInterval is how often a session's IdP refresh token is exercised at most.
	RefreshInterval = 30 * time.Minute
	// entraScopes are what the vendor app's sign-in asks for. offline_access yields the refresh token
	// that lets a disabled Entra account end its product session within RefreshInterval.
	entraScopes = "openid profile email offline_access"
	// attemptSealContext is the Cipher context for the sealed PKCE verifier. An attempt can precede
	// any tenant ("Sign in with Microsoft"), so it is sealed under a fixed context rather than a
	// tenant's key; a tenant id is a uuid, so the two can never derive the same key.
	attemptSealContext = "ops.auth_signin"
)

// ClientAuthenticator authenticates the vendor's Entra app at a token endpoint (internal/entraapp).
type ClientAuthenticator interface {
	ClientAuth(ctx context.Context, tokenEndpoint string) (url.Values, error)
}

// EntraConfig turns on the Entra provider.
type EntraConfig struct {
	ClientID string
	Auth     ClientAuthenticator
	// LoginBaseURL is the identity platform host; default https://login.microsoftonline.com.
	LoginBaseURL string
}

// Config wires the service.
type Config struct {
	Store    Store
	Sessions *session.Manager
	Issuer   *session.Issuer
	// Cipher seals the PKCE verifier, OIDC client secrets and refresh tokens (SAC_DIRECTORY_KEY).
	Cipher *directory.Cipher
	// Entra is nil when the deployment has no Entra app; Entra connections then cannot sign in.
	Entra *EntraConfig
	// HTTPClient makes every outbound call; default NewHTTPClient(AllowInsecureIdP, 10s).
	HTTPClient *http.Client
	// AllowedRedirectURIs, when non-empty, is the exact set of redirect_uri values Begin accepts.
	AllowedRedirectURIs []string
	// AllowInsecureIdP admits http issuers and private addresses: the lab's stand-in provider only.
	AllowInsecureIdP bool
	// JWKSRefetchInterval bounds kid-miss refetches per JWKS; default one minute.
	JWKSRefetchInterval time.Duration
	Logger              *slog.Logger
	Now                 func() time.Time
}

// Service is the relying party.
type Service struct {
	store         Store
	sessions      *session.Manager
	issuer        *session.Issuer
	cipher        *directory.Cipher
	entra         *EntraConfig
	entraBase     string
	client        *http.Client
	keys          *keyCache
	allowInsecure bool
	redirects     []string
	log           *slog.Logger
	now           func() time.Time

	mu        sync.Mutex
	discovery map[string]cachedMeta
	lastSweep time.Time
}

type cachedMeta struct {
	meta ProviderMetadata
	at   time.Time
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("identity: a store is required")
	case cfg.Sessions == nil || cfg.Issuer == nil:
		return nil, errors.New("identity: a session manager and a token issuer are required")
	case cfg.Cipher == nil:
		return nil, errors.New("identity: a cipher (SAC_DIRECTORY_KEY) is required to seal sign-in state")
	}
	if cfg.Entra != nil && (cfg.Entra.ClientID == "" || cfg.Entra.Auth == nil) {
		return nil, errors.New("identity: Entra needs a client id and a credential")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = NewHTTPClient(cfg.AllowInsecureIdP, 10*time.Second)
	}
	if cfg.JWKSRefetchInterval <= 0 {
		cfg.JWKSRefetchInterval = time.Minute
	}
	s := &Service{
		store: cfg.Store, sessions: cfg.Sessions, issuer: cfg.Issuer, cipher: cfg.Cipher, entra: cfg.Entra,
		client: cfg.HTTPClient, allowInsecure: cfg.AllowInsecureIdP, redirects: cfg.AllowedRedirectURIs,
		log: cfg.Logger, now: cfg.Now, discovery: map[string]cachedMeta{},
		keys: newKeyCache(cfg.HTTPClient, cfg.Now, cfg.JWKSRefetchInterval),
	}
	if cfg.Entra != nil {
		s.entraBase = strings.TrimRight(cfg.Entra.LoginBaseURL, "/")
		if s.entraBase == "" {
			s.entraBase = "https://login.microsoftonline.com"
		}
	}
	return s, nil
}

// AttemptHash and ConsentHash are the stored forms of the two handles kept in ops.auth_signin. The
// domain prefix keeps them apart: an onboarding consent state can never be redeemed as a sign-in
// attempt, or the reverse.
func AttemptHash(id string) []byte    { return domainHash("attempt", id) }
func ConsentHash(state string) []byte { return domainHash("consent", state) }

func domainHash(domain, v string) []byte {
	sum := sha256.Sum256([]byte(domain + "\x00" + v))
	return sum[:]
}

// RandomToken is 256 random bits, base64url.
func RandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("identity: no entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------------------------
// Begin
// ---------------------------------------------------------------------------------------------

// BeginRequest is POST /internal/v1/auth/begin.
type BeginRequest struct {
	Email       string `json:"email,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Invite      string `json:"invite,omitempty"`
	RedirectURI string `json:"redirect_uri"`
}

// BeginResult is its answer.
type BeginResult struct {
	Attempt      string `json:"attempt"`
	AuthorizeURL string `json:"authorize_url"`
}

// Begin starts a sign-in. Three ways in: an onboarding invite (the pending connection it created), the
// Microsoft button (no connection yet; the token's tid decides), or a work email (its domain decides
// the tenant, which decides the connection).
func (s *Service) Begin(ctx context.Context, req BeginRequest) (BeginResult, error) {
	s.sweep(ctx)
	if err := s.checkRedirect(req.RedirectURI); err != nil {
		return BeginResult{}, refuse(400, CodeBadRequest, "%v", err)
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider != "" && provider != ProviderEntra && provider != ProviderOIDC {
		return BeginResult{}, refuse(400, CodeBadRequest, "provider %q", req.Provider)
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	var conn *Connection
	var inviteID string
	switch {
	case req.Invite != "":
		inv, err := s.liveInvite(ctx, req.Invite)
		if err != nil {
			return BeginResult{}, err
		}
		c, err := s.onboardingConnection(ctx, inv.TenantID, provider)
		if err != nil {
			return BeginResult{}, err
		}
		conn, inviteID = &c, inv.ID
	case provider == ProviderEntra:
		// The Microsoft button: the connection is found from the token's tid at completion.
	default:
		at := strings.LastIndex(email, "@")
		if at <= 0 || at == len(email)-1 {
			return BeginResult{}, refuse(400, CodeBadRequest, "begin needs an email, a provider or an invite")
		}
		c, err := s.connectionForDomain(ctx, email[at+1:], provider)
		if err != nil {
			return BeginResult{}, err
		}
		conn = &c
	}

	kind := ProviderEntra
	if conn != nil {
		kind = conn.Provider
	}
	var meta ProviderMetadata
	var clientID, scope string
	var err error
	if kind == ProviderEntra {
		if s.entra == nil {
			return BeginResult{}, refuse(404, CodeNoSSOConnection, "Entra sign-in is not configured on this deployment")
		}
		meta, err = s.entraMetadata(ctx)
		clientID, scope = s.entra.ClientID, entraScopes
	} else {
		meta, err = s.oidcMetadata(ctx, conn.Issuer)
		clientID, scope = conn.ClientID, withOpenID(conn.Scopes)
	}
	if err != nil {
		return BeginResult{}, refuse(502, CodeProviderUnavailable, "discovery: %v", err)
	}

	attempt, err1 := RandomToken()
	state, err2 := RandomToken()
	nonce, err3 := RandomToken()
	verifier, err4 := RandomToken()
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return BeginResult{}, err
	}
	sealed, err := s.cipher.Seal(attemptSealContext, verifier)
	if err != nil {
		return BeginResult{}, err
	}
	now := s.now().UTC()
	a := Attempt{
		Hash: AttemptHash(attempt), State: state, Nonce: nonce, VerifierEnc: sealed,
		RedirectURI: req.RedirectURI, InviteID: inviteID, CreatedAt: now, ExpiresAt: now.Add(AttemptTTL),
	}
	if conn != nil {
		a.ConnectionID = conn.ID
	}
	if err := s.store.PutAttempt(ctx, a); err != nil {
		return BeginResult{}, fmt.Errorf("identity: record attempt: %w", err)
	}

	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {req.RedirectURI},
		"scope":                 {scope},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	if kind == ProviderEntra {
		q.Set("response_mode", "query")
	}
	if email != "" {
		q.Set("login_hint", email)
	}
	authorize, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return BeginResult{}, refuse(502, CodeProviderUnavailable, "authorization endpoint: %v", err)
	}
	merged := authorize.Query()
	for k, v := range q {
		merged[k] = v
	}
	authorize.RawQuery = merged.Encode()
	return BeginResult{Attempt: attempt, AuthorizeURL: authorize.String()}, nil
}

func withOpenID(scopes string) string {
	fields := strings.Fields(scopes)
	if !contains(fields, "openid") {
		fields = append([]string{"openid"}, fields...)
	}
	return strings.Join(fields, " ")
}

func (s *Service) checkRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("redirect_uri %q is not an absolute http(s) URL", raw)
	}
	if len(s.redirects) > 0 && !contains(s.redirects, raw) {
		return fmt.Errorf("redirect_uri %q is not one this deployment allows", raw)
	}
	return nil
}

// liveInvite resolves an invite token to its unused, unexpired row. The tenant comes from the token
// and the lookup is tenant-scoped, so a token cannot name another tenant's invite.
func (s *Service) liveInvite(ctx context.Context, token string) (Invite, error) {
	tenant, err := ParseInviteToken(token)
	if err != nil {
		return Invite{}, refuse(404, CodeInviteUnknown, "malformed invite")
	}
	inv, err := s.store.Invite(ctx, tenant, HashToken(token))
	if errors.Is(err, ErrNotFound) {
		return Invite{}, refuse(404, CodeInviteUnknown, "no such invite")
	}
	if err != nil {
		return Invite{}, err
	}
	return inv, s.inviteUsable(inv)
}

func (s *Service) inviteUsable(inv Invite) error {
	if inv.UsedAt != nil {
		return refuse(410, CodeInviteUsed, "invite %s was used", inv.ID)
	}
	if !s.now().Before(inv.ExpiresAt) {
		return refuse(410, CodeInviteExpired, "invite %s expired", inv.ID)
	}
	return nil
}

// onboardingConnection picks the connection an invite's sign-in goes through: the newest pending one
// (of the requested provider), or, for a second invite to a tenant already connected, its active
// one.
func (s *Service) onboardingConnection(ctx context.Context, tenantID, provider string) (Connection, error) {
	conns, err := s.store.TenantConnections(ctx, tenantID)
	if err != nil {
		return Connection{}, err
	}
	var active, disabled *Connection
	for i := range conns {
		c := &conns[i]
		if provider != "" && c.Provider != provider {
			continue
		}
		switch c.Status {
		case StatusPending:
			return *c, nil
		case StatusActive:
			if active == nil {
				active = c
			}
		case StatusDisabled:
			disabled = c
		}
	}
	if active != nil {
		return *active, nil
	}
	if disabled != nil {
		return Connection{}, refuse(403, CodeConnectionDisabled, "the tenant's connection is disabled")
	}
	return Connection{}, refuse(409, CodeOnboardingIncomplete, "no provider has been connected with this invite yet")
}

// connectionForDomain maps a work email's domain to its tenant's active connection.
func (s *Service) connectionForDomain(ctx context.Context, domain, provider string) (Connection, error) {
	tenant, err := s.store.TenantForEmailDomain(ctx, domain)
	if errors.Is(err, ErrNotFound) {
		return Connection{}, refuse(404, CodeNoSSOConnection, "no tenant for domain %q", domain)
	}
	if err != nil {
		return Connection{}, err
	}
	conns, err := s.store.TenantConnections(ctx, tenant)
	if err != nil {
		return Connection{}, err
	}
	var best *Connection
	for i := range conns {
		c := &conns[i]
		if c.Status != StatusActive || (provider != "" && c.Provider != provider) {
			continue
		}
		if best == nil || activatedAfter(c, best) {
			best = c
		}
	}
	if best == nil {
		return Connection{}, refuse(404, CodeNoSSOConnection, "tenant %s has no active connection", tenant)
	}
	return *best, nil
}

func activatedAfter(a, b *Connection) bool {
	if a.ActivatedAt == nil {
		return false
	}
	return b.ActivatedAt == nil || a.ActivatedAt.After(*b.ActivatedAt)
}

// ---------------------------------------------------------------------------------------------
// Complete
// ---------------------------------------------------------------------------------------------

// CompleteRequest is POST /internal/v1/auth/complete.
type CompleteRequest struct {
	Attempt string `json:"attempt"`
	Code    string `json:"code"`
	State   string `json:"state"`
}

// CompleteResult is its answer.
type CompleteResult struct {
	Session     string            `json:"session"`
	AccessToken string            `json:"access_token"`
	ExpiresIn   int               `json:"expires_in"`
	Principal   session.Principal `json:"principal"`
}

// signer is who an id_token says signed in, after verification.
type signer struct {
	subject, actor, tid string
	claims              map[string]json.RawMessage
}

// idpRoles reads the connection's roles claim. It is read only once the connection is known, because
// for the Microsoft button the connection (and so its roles_claim) is decided by the token's tid.
func (w signer) idpRoles(conn *Connection) []string {
	claim := conn.RolesClaim
	if claim == "" {
		claim = "roles"
	}
	return strs(w.claims, claim)
}

// Complete finishes a sign-in. The attempt is consumed first, whatever happens next, so a code, a
// state or an id_token can be tried against it exactly once.
func (s *Service) Complete(ctx context.Context, req CompleteRequest) (CompleteResult, error) {
	if req.Attempt == "" || len(req.Attempt) > 256 {
		return CompleteResult{}, refuse(400, CodeAttemptUnknown, "no attempt")
	}
	a, err := s.store.TakeAttempt(ctx, AttemptHash(req.Attempt))
	if errors.Is(err, ErrNotFound) {
		return CompleteResult{}, refuse(400, CodeAttemptUnknown, "unknown or already used attempt")
	}
	if err != nil {
		return CompleteResult{}, err
	}
	if a.Nonce == "" || len(a.VerifierEnc) == 0 {
		return CompleteResult{}, refuse(400, CodeAttemptUnknown, "the handle is not a sign-in attempt")
	}
	now := s.now().UTC()
	if !now.Before(a.ExpiresAt) {
		return CompleteResult{}, refuse(400, CodeAttemptExpired, "attempt expired at %s", a.ExpiresAt.Format(time.RFC3339))
	}
	if subtle.ConstantTimeCompare([]byte(req.State), []byte(a.State)) != 1 {
		return CompleteResult{}, refuse(400, CodeStateMismatch, "state does not match the attempt")
	}
	if req.Code == "" || len(req.Code) > 4096 {
		return CompleteResult{}, refuse(400, CodeBadRequest, "no authorization code")
	}

	var conn *Connection
	if a.ConnectionID != "" {
		c, err := s.store.ConnectionByID(ctx, a.ConnectionID)
		if errors.Is(err, ErrNotFound) {
			return CompleteResult{}, refuse(403, CodeConnectionDisabled, "connection %s is gone", a.ConnectionID)
		}
		if err != nil {
			return CompleteResult{}, err
		}
		usable := c.Status == StatusActive || (c.Status == StatusPending && a.InviteID != "")
		if !usable {
			return CompleteResult{}, s.refuseAudited(ctx, &c, signer{}, refuse(403, CodeConnectionDisabled, "connection %s is %s", c.ID, c.Status))
		}
		conn = &c
	}
	kind := ProviderEntra
	if conn != nil {
		kind = conn.Provider
	}

	verifier, err := s.cipher.Open(attemptSealContext, a.VerifierEnc)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("identity: open verifier: %w", err)
	}
	tokens, err := s.redeem(ctx, kind, conn, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {a.RedirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return CompleteResult{}, err
	}
	who, err := s.verifyIDToken(ctx, kind, conn, tokens.IDToken, a.Nonce)
	if err != nil {
		return CompleteResult{}, err
	}

	if kind == ProviderEntra {
		if conn != nil {
			if who.tid != conn.EntraTenantID {
				return CompleteResult{}, s.refuseAudited(ctx, conn, who, refuse(403, CodeTenantNotOnboarded,
					"token tid %s is not connection %s's tenant", who.tid, conn.ID))
			}
		} else {
			c, err := s.store.ConnectionForEntraTenant(ctx, who.tid)
			if errors.Is(err, ErrNotFound) {
				return CompleteResult{}, refuse(403, CodeTenantNotOnboarded, "Entra tenant %s maps to no active connection", who.tid)
			}
			if err != nil {
				return CompleteResult{}, err
			}
			if c.Status != StatusActive {
				return CompleteResult{}, s.refuseAudited(ctx, &c, who, refuse(403, CodeConnectionDisabled, "connection %s is %s", c.ID, c.Status))
			}
			conn = &c
		}
	}

	userRef, active, known, err := s.scimPerson(ctx, conn.TenantID, conn.Provider, who.actor, who.subject, "")
	if err != nil {
		return CompleteResult{}, err
	}
	if known && !active {
		return CompleteResult{}, s.refuseAudited(ctx, conn, who, refuse(403, CodeUserDeactivated, "SCIM marks %s inactive", userRef))
	}

	if a.InviteID != "" {
		if err := s.activate(ctx, conn, a.InviteID, who, now); err != nil {
			return CompleteResult{}, err
		}
	}

	grants, err := s.store.RoleGrants(ctx, conn.TenantID, conn.ID, who.subject)
	if err != nil {
		return CompleteResult{}, err
	}
	roles := resolveRoles(who.idpRoles(conn), conn.RoleMap, grants)
	if len(roles) == 0 {
		return CompleteResult{}, s.refuseAudited(ctx, conn, who, refuse(403, CodeNoRole, "no product role"))
	}

	var refreshEnc []byte
	if tokens.RefreshToken != "" {
		if refreshEnc, err = s.cipher.Seal(conn.TenantID, tokens.RefreshToken); err != nil {
			return CompleteResult{}, err
		}
	}
	id, rec, err := s.sessions.Create(ctx, session.Record{
		TenantID: conn.TenantID, ConnectionID: conn.ID, Subject: who.subject, Actor: who.actor,
		Roles: roles, UserRef: userRef, RefreshEnc: refreshEnc,
	})
	if err != nil {
		return CompleteResult{}, err
	}
	// A sign-in with no audit row did not happen: if the row cannot be written, the session ends.
	if err := s.store.Audit(ctx, conn.TenantID, AuditEntry{
		ActorType: "user", ActorID: who.actor, Action: "auth.sign_in", ObjectType: "auth_session", ObjectID: rec.SID(),
		Detail: map[string]any{"idp": conn.Provider, "connection_id": conn.ID, "roles": roles, "onboarding": a.InviteID != ""},
	}); err != nil {
		_, _ = s.sessions.End(ctx, rec)
		return CompleteResult{}, fmt.Errorf("identity: audit sign-in: %w", err)
	}
	tok, expiresIn, p, err := s.mint(rec, conn.Provider)
	if err != nil {
		return CompleteResult{}, err
	}
	s.log.Info("identity: signed in", "tenant", conn.TenantID, "connection", conn.ID, "idp", conn.Provider,
		"sid", rec.SID(), "roles", strings.Join(roles, ","))
	return CompleteResult{Session: id, AccessToken: tok, ExpiresIn: expiresIn, Principal: p}, nil
}

// activate is the onboarding sign-in's commit. It runs after the token is verified and the tenant
// mapping holds, so a connection becomes active only when a real sign-in through it succeeded.
func (s *Service) activate(ctx context.Context, conn *Connection, inviteID string, who signer, now time.Time) error {
	inv, err := s.store.InviteByID(ctx, conn.TenantID, inviteID)
	if errors.Is(err, ErrNotFound) {
		return refuse(404, CodeInviteUnknown, "invite %s is not this tenant's", inviteID)
	}
	if err != nil {
		return err
	}
	if err := s.inviteUsable(inv); err != nil {
		return err
	}
	err = s.store.Activate(ctx, Activation{
		TenantID: conn.TenantID, ConnectionID: conn.ID, InviteID: inviteID,
		Subject: who.subject, Actor: who.actor, At: now,
	})
	switch {
	case errors.Is(err, ErrInviteUsed):
		return refuse(410, CodeInviteUsed, "invite %s was spent concurrently", inviteID)
	case errors.Is(err, ErrConnectionDisabled):
		return refuse(403, CodeConnectionDisabled, "connection %s was disabled", conn.ID)
	case err != nil:
		return err
	}
	conn.Status = StatusActive
	s.log.Info("identity: connection activated by its first sign-in", "tenant", conn.TenantID,
		"connection", conn.ID, "idp", conn.Provider, "invite", inviteID)
	return nil
}

// refuseAudited records a refusal in a tenant we know, then returns it.
func (s *Service) refuseAudited(ctx context.Context, conn *Connection, who signer, r *Refusal) error {
	actorType, actor := "user", who.actor
	if actor == "" {
		actorType, actor = "system", "control-api"
	}
	if err := s.store.Audit(ctx, conn.TenantID, AuditEntry{
		ActorType: actorType, ActorID: actor, Action: "auth.sign_in_refused",
		ObjectType: "identity_connection", ObjectID: conn.ID, Detail: map[string]any{"reason": r.Code},
	}); err != nil {
		s.log.Error("identity: audit refusal", "tenant", conn.TenantID, "error", err)
	}
	return r
}

// redeem calls a provider's token endpoint with the client's own authentication.
func (s *Service) redeem(ctx context.Context, kind string, conn *Connection, form url.Values) (tokenResponse, error) {
	var endpoint string
	var basic *[2]string
	if kind == ProviderEntra {
		if s.entra == nil {
			return tokenResponse{}, refuse(403, CodeConnectionDisabled, "Entra sign-in is not configured on this deployment")
		}
		meta, err := s.entraMetadata(ctx)
		if err != nil {
			return tokenResponse{}, refuse(502, CodeProviderUnavailable, "discovery: %v", err)
		}
		endpoint = meta.TokenEndpoint
		auth, err := s.entra.Auth.ClientAuth(ctx, endpoint)
		if err != nil {
			return tokenResponse{}, refuse(502, CodeProviderUnavailable, "client credential: %v", err)
		}
		for k, v := range auth {
			form[k] = v
		}
		form.Set("client_id", s.entra.ClientID)
	} else {
		meta, err := s.oidcMetadata(ctx, conn.Issuer)
		if err != nil {
			return tokenResponse{}, refuse(502, CodeProviderUnavailable, "discovery: %v", err)
		}
		endpoint = meta.TokenEndpoint
		form.Set("client_id", conn.ClientID)
		secret, err := s.cipher.Open(conn.TenantID, conn.ClientSecretEnc)
		if err != nil {
			return tokenResponse{}, fmt.Errorf("identity: open client secret: %w", err)
		}
		if secret != "" {
			// client_secret_post unless the provider says it only takes basic: the post form is what
			// most providers (and the lab's stand-in) accept, and discovery that lists no methods is the
			// common case.
			if len(meta.TokenAuthMethods) > 0 && !contains(meta.TokenAuthMethods, "client_secret_post") &&
				contains(meta.TokenAuthMethods, "client_secret_basic") {
				basic = &[2]string{conn.ClientID, secret}
			} else {
				form.Set("client_secret", secret)
			}
		}
	}
	tokens, err := postToken(ctx, s.client, endpoint, form, basic)
	if err != nil {
		var oe *oauthError
		if errors.As(err, &oe) {
			return tokenResponse{}, refuse(401, CodeSignInFailed, "%v", err)
		}
		return tokenResponse{}, refuse(502, CodeProviderUnavailable, "%v", err)
	}
	return tokens, nil
}

// verifyIDToken checks an id_token and extracts who signed in. Entra: the issuer must be the
// token's own tenant's issuer; OIDC: the connection's exact issuer. Both: aud is our client id, the
// nonce is this attempt's, and the times hold.
func (s *Service) verifyIDToken(ctx context.Context, kind string, conn *Connection, raw, nonce string) (signer, error) {
	if raw == "" {
		return signer{}, refuse(401, CodeSignInFailed, "the token response carried no id_token")
	}
	parts, err := parseRS256(raw)
	if err != nil {
		return signer{}, refuse(401, CodeSignInFailed, "%v", err)
	}
	var jwksURL, clientID string
	if kind == ProviderEntra {
		jwksURL, clientID = s.entraBase+"/common/discovery/v2.0/keys", s.entra.ClientID
	} else {
		meta, err := s.oidcMetadata(ctx, conn.Issuer)
		if err != nil {
			return signer{}, refuse(502, CodeProviderUnavailable, "discovery: %v", err)
		}
		jwksURL, clientID = meta.JWKSURI, conn.ClientID
	}
	pub, err := s.keys.key(ctx, jwksURL, parts.kid)
	if err != nil {
		return signer{}, refuse(401, CodeSignInFailed, "%v", err)
	}
	if err := parts.verify(pub); err != nil {
		return signer{}, refuse(401, CodeSignInFailed, "%v", err)
	}
	c := parts.claims
	var who signer
	if kind == ProviderEntra {
		who.tid = strings.ToLower(str(c, "tid"))
		if !session.IsUUID(who.tid) {
			return signer{}, refuse(401, CodeSignInFailed, "Entra id_token has no tid")
		}
		if iss := str(c, "iss"); iss != s.entraBase+"/"+who.tid+"/v2.0" {
			return signer{}, refuse(401, CodeSignInFailed, "issuer %q is not the issuer of the token's own tenant", iss)
		}
	} else if iss := str(c, "iss"); iss != conn.Issuer {
		return signer{}, refuse(401, CodeSignInFailed, "issuer %q is not the connection's", iss)
	}
	if err := checkAudience(c, clientID); err != nil {
		return signer{}, refuse(401, CodeSignInFailed, "%v", err)
	}
	if err := checkTimes(c, s.now().UTC(), session.Leeway); err != nil {
		return signer{}, refuse(401, CodeSignInFailed, "%v", err)
	}
	if subtle.ConstantTimeCompare([]byte(str(c, "nonce")), []byte(nonce)) != 1 {
		return signer{}, refuse(401, CodeSignInFailed, "nonce does not match this attempt")
	}

	who.claims = c
	email := str(c, "email")
	if v, ok := c["email_verified"]; ok && strings.Trim(string(v), `"`) == "false" {
		email = "" // an address the provider has not verified does not name the actor
	}
	if kind == ProviderEntra {
		who.subject = strings.ToLower(str(c, "oid"))
		who.actor = firstNonEmpty(str(c, "preferred_username"), str(c, "upn"), email, who.subject)
		if !session.IsUUID(who.subject) {
			return signer{}, refuse(401, CodeSignInFailed, "Entra id_token has no oid")
		}
	} else {
		who.subject = str(c, "sub")
		who.actor = firstNonEmpty(email, str(c, "preferred_username"), who.subject)
		if who.subject == "" {
			return signer{}, refuse(401, CodeSignInFailed, "id_token has no sub")
		}
	}
	return who, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// Token and Revoke
// ---------------------------------------------------------------------------------------------

// TokenResult is POST /internal/v1/auth/token's answer.
type TokenResult struct {
	AccessToken string            `json:"access_token"`
	ExpiresIn   int               `json:"expires_in"`
	Principal   session.Principal `json:"principal"`
}

// Token re-mints a product token for a live session. It re-checks on every call what could have
// changed since the sign-in: the connection is still active, SCIM has not deactivated the person,
// and (at most every RefreshInterval) the provider still honours the refresh token. Any of those
// failing ends the session.
func (s *Service) Token(ctx context.Context, sessionID string) (TokenResult, error) {
	rec, err := s.sessions.Resume(ctx, sessionID)
	if errors.Is(err, session.ErrEnded) {
		return TokenResult{}, refuse(401, CodeSessionEnded, "%v", err)
	}
	if err != nil {
		return TokenResult{}, err
	}
	conn, err := s.store.ConnectionByID(ctx, rec.ConnectionID)
	if errors.Is(err, ErrNotFound) || (err == nil && conn.Status != StatusActive) {
		return TokenResult{}, s.endSession(ctx, rec, CodeConnectionDisabled)
	}
	if err != nil {
		return TokenResult{}, err
	}
	_, active, known, err := s.scimPerson(ctx, rec.TenantID, conn.Provider, rec.Actor, rec.Subject, rec.UserRef)
	if err != nil {
		return TokenResult{}, err
	}
	if (known && !active) || (!known && rec.UserRef != "") {
		return TokenResult{}, s.endSession(ctx, rec, "scim_deactivated")
	}
	if len(rec.RefreshEnc) > 0 {
		if err := s.refresh(ctx, &conn, rec); err != nil {
			s.log.Warn("identity: provider refresh failed; ending the session", "tenant", rec.TenantID,
				"sid", rec.SID(), "error", err)
			return TokenResult{}, s.endSession(ctx, rec, "idp_refresh_failed")
		}
	}
	if err := s.sessions.Touch(ctx, rec); err != nil {
		return TokenResult{}, err
	}
	tok, expiresIn, p, err := s.mint(rec, conn.Provider)
	if err != nil {
		return TokenResult{}, err
	}
	return TokenResult{AccessToken: tok, ExpiresIn: expiresIn, Principal: p}, nil
}

// endSession revokes a session the system ended, audits why, and answers session_ended.
func (s *Service) endSession(ctx context.Context, rec session.Record, reason string) error {
	ended, err := s.sessions.End(ctx, rec)
	if err != nil {
		return err
	}
	if ended {
		if err := s.store.Audit(ctx, rec.TenantID, AuditEntry{
			ActorType: "system", ActorID: "control-api", Action: "auth.session_end", ObjectType: "auth_session",
			ObjectID: rec.SID(), Detail: map[string]any{"reason": reason, "actor": rec.Actor},
		}); err != nil {
			s.log.Error("identity: audit session end", "tenant", rec.TenantID, "error", err)
		}
	}
	return refuse(401, CodeSessionEnded, "%s", reason)
}

// Revoke ends a session at the person's request (sign-out). An unknown or already-ended session is
// not an error: sign-out is idempotent.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	rec, err := s.sessions.Lookup(ctx, sessionID)
	if errors.Is(err, session.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	ended, err := s.sessions.End(ctx, rec)
	if err != nil {
		return err
	}
	if ended {
		if err := s.store.Audit(ctx, rec.TenantID, AuditEntry{
			ActorType: "user", ActorID: rec.Actor, Action: "auth.revoke", ObjectType: "auth_session", ObjectID: rec.SID(),
		}); err != nil {
			return fmt.Errorf("identity: audit revoke: %w", err)
		}
		s.log.Info("identity: session revoked", "tenant", rec.TenantID, "sid", rec.SID())
	}
	return nil
}

func (s *Service) mint(rec session.Record, idp string) (string, int, session.Principal, error) {
	p := session.Principal{
		Tenant: rec.TenantID, Actor: rec.Actor, Roles: rec.Roles, IdP: idp,
		Subject: rec.ConnectionID + ":" + rec.Subject, SessionID: rec.SID(),
	}
	tok, exp, err := s.issuer.Mint(p)
	if err != nil {
		return "", 0, session.Principal{}, err
	}
	p.Roles = session.CanonicalRoles(p.Roles)
	return tok, int(exp.Sub(s.now()).Round(time.Second).Seconds()), p, nil
}

// ---------------------------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------------------------

// refresh exercises the provider refresh token when the last exercise (idp_refreshed_at) is at least
// RefreshInterval old. A provider that refuses — a disabled account, a revoked grant — ends the
// session through the caller.
func (s *Service) refresh(ctx context.Context, conn *Connection, rec session.Record) error {
	if rec.RefreshedAt != nil && s.now().Sub(*rec.RefreshedAt) < RefreshInterval {
		return nil
	}
	token, err := s.cipher.Open(rec.TenantID, rec.RefreshEnc)
	if err != nil || token == "" {
		return errors.New("the sealed refresh token is unreadable")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}
	if conn.Provider == ProviderEntra {
		form.Set("scope", entraScopes)
	}
	tokens, err := s.redeem(ctx, conn.Provider, conn, form)
	if err != nil {
		return err
	}
	if tokens.RefreshToken != "" {
		token = tokens.RefreshToken // a rotating provider invalidates the old one
	}
	enc, err := s.cipher.Seal(rec.TenantID, token)
	if err != nil {
		return err
	}
	return s.sessions.SetRefresh(ctx, rec, enc)
}

// ---------------------------------------------------------------------------------------------
// SCIM linkage
// ---------------------------------------------------------------------------------------------

// scimPerson finds the SCIM-provisioned person behind a sign-in, by the refs the shared derivation
// gives (contract §4): the UPN-ref of the actor and, for Entra, the oid-ref of the subject, each
// resolved through ops.user_ref_alias. known is the ref a session already recorded.
func (s *Service) scimPerson(ctx context.Context, tenantID, provider, actor, subject, known string) (string, bool, bool, error) {
	var refs []string
	if known != "" {
		refs = append(refs, known)
	}
	sealed, err := s.store.UserRefKey(ctx, tenantID)
	if err != nil {
		return "", false, false, err
	}
	if len(sealed) > 0 {
		// Sealed as raw bytes by directory.UserRefKeys, which mints it; sign-in only ever reads it,
		// because a tenant with no key has no SCIM user to find.
		key, err := s.cipher.OpenBytes(tenantID, sealed)
		if err != nil {
			return "", false, false, fmt.Errorf("identity: open user_ref key: %w", err)
		}
		if len(key) != protocol.UserRefKeySize {
			return "", false, false, fmt.Errorf("identity: the tenant's user_ref key is %d bytes, want %d", len(key), protocol.UserRefKeySize)
		}
		if strings.Contains(actor, "@") {
			if ref, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, actor); err == nil {
				refs = append(refs, ref)
			}
		}
		if provider == ProviderEntra && session.IsUUID(subject) {
			if ref, err := protocol.DeriveUserRef(key, protocol.UserRefOID, subject); err == nil {
				refs = append(refs, ref)
			}
		}
	}
	if len(refs) == 0 {
		return "", false, false, nil
	}
	ref, active, err := s.store.ScimUser(ctx, tenantID, refs)
	if errors.Is(err, ErrNotFound) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return ref, active, true, nil
}

// ---------------------------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------------------------

// entraMetadata is the organizations authority's discovery document. Its issuer is the template
// https://login.microsoftonline.com/{tenantid}/v2.0, which is why the issuer check is per token.
func (s *Service) entraMetadata(ctx context.Context) (ProviderMetadata, error) {
	return s.metadata(ctx, s.entraBase+"/organizations/v2.0/.well-known/openid-configuration", func(m ProviderMetadata) error {
		if m.Issuer != s.entraBase+"/{tenantid}/v2.0" {
			return fmt.Errorf("organizations discovery names issuer %q", m.Issuer)
		}
		for _, e := range []string{m.AuthorizationEndpoint, m.TokenEndpoint} {
			if !strings.HasPrefix(e, s.entraBase+"/") {
				return fmt.Errorf("endpoint %q is not on %s", e, s.entraBase)
			}
		}
		return nil
	})
}

func (s *Service) oidcMetadata(ctx context.Context, issuer string) (ProviderMetadata, error) {
	return s.metadata(ctx, DiscoveryURL(issuer), func(m ProviderMetadata) error {
		return checkMetadata(m, issuer, s.allowInsecure)
	})
}

func (s *Service) metadata(ctx context.Context, address string, check func(ProviderMetadata) error) (ProviderMetadata, error) {
	s.mu.Lock()
	c, ok := s.discovery[address]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < time.Hour {
		return c.meta, nil
	}
	var meta ProviderMetadata
	if err := getJSON(ctx, s.client, address, &meta); err != nil {
		return ProviderMetadata{}, err
	}
	if err := check(meta); err != nil {
		return ProviderMetadata{}, err
	}
	s.mu.Lock()
	s.discovery[address] = cachedMeta{meta: meta, at: s.now()}
	s.mu.Unlock()
	return meta, nil
}

// sweep deletes expired attempts, at most once a minute.
func (s *Service) sweep(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	due := now.Sub(s.lastSweep) >= time.Minute
	if due {
		s.lastSweep = now
	}
	s.mu.Unlock()
	if due {
		if err := s.store.SweepAttempts(ctx, now.UTC()); err != nil {
			s.log.Warn("identity: sweeping expired sign-in attempts failed", "error", err)
		}
	}
}
