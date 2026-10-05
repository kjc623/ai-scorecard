// Package session is the product's own token authority and its server-side session store
// (the shared contract §2 and §3; docs/06 §4.1).
//
// control-api is the relying party for every customer identity provider (internal/identity), and
// what it hands the rest of the product is never the provider's token: it is a short-lived product
// access token this package mints, ES256 under a dedicated session-signing key, verified by
// query-api, content-vault and control-api's own admin API against one issuer and one JWKS. A Google
// or Okta access token is opaque or not meant for us, so forwarding provider tokens could never work
// for "any provider"; one issuer can.
//
// The browser never holds a product token. The dashboard server holds an opaque session id (the
// cookie) and exchanges it here for a token of at most ten minutes, which is what makes a revoked
// session, a SCIM deactivation or a disabled connection take effect within one token lifetime.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/jose"
)

// The product roles (docs/06 §4.1, reconciled with the owner for task 11). The order is the
// canonical order roles are written in, so two tokens for one person compare equal.
const (
	RoleViewer        = "viewer"
	RoleAnalyst       = "analyst"
	RoleContentReader = "content_reader"
	RoleAdmin         = "admin"
)

var roleOrder = []string{RoleViewer, RoleAnalyst, RoleContentReader, RoleAdmin}

// ValidRole reports whether r is a product role.
func ValidRole(r string) bool {
	for _, v := range roleOrder {
		if r == v {
			return true
		}
	}
	return false
}

// CanonicalRoles drops unknown and repeated roles and returns the rest in canonical order.
func CanonicalRoles(in []string) []string {
	have := map[string]bool{}
	for _, r := range in {
		have[r] = true
	}
	out := []string{}
	for _, r := range roleOrder {
		if have[r] {
			out = append(out, r)
		}
	}
	return out
}

// The audiences of a product token. Every token names all three, and each verifier requires its own:
// the dashboard forwards one token to query-api, which forwards it to the vault, so the same token
// must be acceptable to both, and control-api's admin API is the third reader.
const (
	AudienceQuery   = "sac-query"
	AudienceVault   = "sac-vault"
	AudienceControl = "sac-control"
)

// The identity providers a token can name.
const (
	IdPEntra = "entra"
	IdPOIDC  = "oidc"
)

// MaxTokenTTL is the contract's ceiling on a product token's life.
const MaxTokenTTL = 10 * time.Minute

// Leeway is the clock skew a verifier tolerates on exp, nbf and iat.
const Leeway = 60 * time.Second

// Principal is who a product token speaks for. The JSON form is the `principal` object of the
// internal sign-in API; the other fields are for this service's own use.
type Principal struct {
	Tenant string   `json:"tenant"`
	Actor  string   `json:"actor"`
	Roles  []string `json:"roles"`
	IdP    string   `json:"idp"`

	// Subject is `<connection_id>:<idp subject>`, the token's sub.
	Subject string `json:"-"`
	// SessionID is the sid claim: a hex prefix of the session hash, for audit correlation only.
	SessionID string    `json:"-"`
	TokenID   string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

// HasRole reports whether the principal holds any of roles.
func (p Principal) HasRole(roles ...string) bool {
	for _, have := range p.Roles {
		for _, want := range roles {
			if have == want {
				return true
			}
		}
	}
	return false
}

// claims is the payload of a product token, exactly the contract's member list.
type claims struct {
	Iss    string   `json:"iss"`
	Aud    []string `json:"aud"`
	Sub    string   `json:"sub"`
	Tenant string   `json:"sac_tenant"`
	Actor  string   `json:"actor"`
	Roles  []string `json:"roles"`
	IdP    string   `json:"idp"`
	SID    string   `json:"sid"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
	Jti    string   `json:"jti"`
}

// IssuerConfig configures the token authority.
type IssuerConfig struct {
	// Issuer is SAC_AUTH_ISSUER, the exact `iss` every verifier pins (e.g. http://control-api:8080).
	Issuer string
	// TTL is the token life; it is clamped to MaxTokenTTL. Default 5 minutes.
	TTL time.Duration
	Now func() time.Time
}

// Issuer mints product access tokens.
type Issuer struct {
	keys   *KeySet
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

// NewIssuer builds the authority. The issuer must be an absolute URL, because verifiers derive the
// JWKS address from it and pin it exactly.
func NewIssuer(keys *KeySet, cfg IssuerConfig) (*Issuer, error) {
	if keys == nil {
		return nil, errors.New("session: a key set is required")
	}
	iss := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if !strings.HasPrefix(iss, "https://") && !strings.HasPrefix(iss, "http://") {
		return nil, fmt.Errorf("session: issuer %q is not an absolute http(s) URL", cfg.Issuer)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.TTL > MaxTokenTTL {
		cfg.TTL = MaxTokenTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Issuer{keys: keys, issuer: iss, ttl: cfg.TTL, now: cfg.Now}, nil
}

// Issuer is the `iss` this authority writes.
func (i *Issuer) Issuer() string { return i.issuer }

// TTL is the life of a minted token.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Keys is the key set, for the verifier and the JWKS handler.
func (i *Issuer) Keys() *KeySet { return i.keys }

// Mint signs a token for p. Tenant, actor, at least one role, the idp and the subject are required:
// a token that names no role would be refused by every verifier anyway, and minting one would hide
// the defect upstream.
func (i *Issuer) Mint(p Principal) (string, time.Time, error) {
	roles := CanonicalRoles(p.Roles)
	switch {
	case !IsUUID(p.Tenant):
		return "", time.Time{}, errors.New("session: a token needs a tenant uuid")
	case strings.TrimSpace(p.Actor) == "":
		return "", time.Time{}, errors.New("session: a token needs an actor")
	case len(roles) == 0:
		return "", time.Time{}, errors.New("session: a token needs at least one product role")
	case p.IdP != IdPEntra && p.IdP != IdPOIDC:
		return "", time.Time{}, fmt.Errorf("session: idp %q is not entra or oidc", p.IdP)
	case p.Subject == "":
		return "", time.Time{}, errors.New("session: a token needs a subject")
	}
	jti, err := NewUUID()
	if err != nil {
		return "", time.Time{}, err
	}
	now := i.now().UTC()
	exp := now.Add(i.ttl)
	c := claims{
		Iss: i.issuer, Aud: []string{AudienceQuery, AudienceVault, AudienceControl},
		Sub: p.Subject, Tenant: strings.ToLower(p.Tenant), Actor: p.Actor, Roles: roles, IdP: p.IdP,
		SID: p.SessionID, Iat: now.Unix(), Exp: exp.Unix(), Jti: jti,
	}
	header := map[string]any{"alg": jose.AlgES256, "typ": jose.TypAccessToken, "kid": i.keys.signerKID}
	tok, err := jose.SignES256(header, c, i.keys.signer)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, time.Unix(c.Exp, 0).UTC(), nil
}

// Verifier checks product tokens against the local key set. control-api's admin API uses it; the
// other services verify the same tokens against the published JWKS.
type Verifier struct {
	keys   *KeySet
	issuer string
	now    func() time.Time
}

// NewVerifier builds a verifier for tokens this process's issuer minted.
func NewVerifier(iss *Issuer) *Verifier {
	return &Verifier{keys: iss.keys, issuer: iss.issuer, now: iss.now}
}

// ErrInvalidToken is every reason a product token is refused. The reason is in the wrapped text for
// the log; a caller answers 401 whatever it is.
var ErrInvalidToken = errors.New("session: invalid product token")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(format, args...))
}

// Verify checks a compact token for one audience: alg pinned to ES256 (jose refuses any other), typ
// at+jwt, a kid this set publishes, the exact issuer, the audience, and exp/nbf/iat within Leeway.
func (v *Verifier) Verify(token, audience string) (Principal, error) {
	j, err := jose.Parse(token)
	if err != nil {
		return Principal{}, invalid("%v", err)
	}
	if typ, _ := j.HeaderString("typ"); typ != jose.TypAccessToken {
		return Principal{}, invalid("typ %q", typ)
	}
	kid, _ := j.HeaderString("kid")
	pub := v.keys.publicKey(kid)
	if pub == nil {
		return Principal{}, invalid("unknown kid")
	}
	if err := j.Verify(pub); err != nil {
		return Principal{}, invalid("%v", err)
	}
	if iss, _ := j.Claims.String("iss"); iss != v.issuer {
		return Principal{}, invalid("issuer %q", iss)
	}
	if !audienceHas(j.Claims["aud"], audience) {
		return Principal{}, invalid("audience does not include %q", audience)
	}
	now := v.now().UTC()
	exp, ok := j.Claims.Int64("exp")
	if !ok {
		return Principal{}, invalid("no exp")
	}
	if !now.Before(time.Unix(exp, 0).Add(Leeway)) {
		return Principal{}, invalid("expired")
	}
	if nbf, ok := j.Claims.Int64("nbf"); ok && now.Add(Leeway).Before(time.Unix(nbf, 0)) {
		return Principal{}, invalid("not yet valid")
	}
	if iat, ok := j.Claims.Int64("iat"); ok && now.Add(Leeway).Before(time.Unix(iat, 0)) {
		return Principal{}, invalid("issued in the future")
	}
	var c claims
	raw, _ := json.Marshal(j.Claims)
	if err := json.Unmarshal(raw, &c); err != nil {
		return Principal{}, invalid("claims: %v", err)
	}
	roles := CanonicalRoles(c.Roles)
	if !IsUUID(c.Tenant) || c.Actor == "" || len(roles) == 0 || c.Sub == "" {
		return Principal{}, invalid("missing tenant, actor, role or subject")
	}
	if c.IdP != IdPEntra && c.IdP != IdPOIDC {
		return Principal{}, invalid("idp %q", c.IdP)
	}
	return Principal{
		Tenant: strings.ToLower(c.Tenant), Actor: c.Actor, Roles: roles, IdP: c.IdP,
		Subject: c.Sub, SessionID: c.SID, TokenID: c.Jti, ExpiresAt: time.Unix(exp, 0).UTC(),
	}, nil
}

func audienceHas(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

type principalKey struct{}

// FromContext returns the principal Require stored on the request context.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal stores p on ctx, for a handler test that bypasses Require.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Require is middleware for control-api's admin API: a bearer product token for audience, and, when
// roles are named, at least one of them. The principal is on the request context for the handler,
// which is where the audit actor comes from — never from a header the caller wrote.
func (v *Verifier) Require(audience string, roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if len(auth) < 7 || !strings.EqualFold(auth[:7], "bearer ") {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sac-control"`)
				writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			p, err := v.Verify(strings.TrimSpace(auth[7:]), audience)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sac-control", error="invalid_token"`)
				writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if len(roles) > 0 && !p.HasRole(roles...) {
				writeJSONError(w, http.StatusForbidden, "forbidden_role")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// NewUUID returns a random version-4 UUID.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: no entropy: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// IsUUID reports whether s is the canonical 8-4-4-4-12 hex form the database casts with ::uuid.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}
