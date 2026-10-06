// Package session is the product's own token authority and its server-side session store.
//
// control-api is the relying party for every customer identity provider (internal/identity), and
// what it hands the rest of the product is never the provider's token: it is a short-lived product
// access token this package mints, ES256 under a dedicated session-signing key, verified by
// query-api, content-vault and control-api's own admin API against one issuer and one JWKS.
//
// The browser never holds a product token. The dashboard server holds an opaque session id (the
// cookie) and exchanges it here for a token of a few minutes, which is what makes a revoked session,
// a SCIM deactivation or a disabled connection take effect within one token lifetime.
//
// The same authority signs the service token control-api presents to content-vault: a token with no
// tenant or person, naming the calling service in its svc claim.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The product roles, in the canonical order roles are written in, so two tokens for one person
// compare equal.
const (
	RoleViewer        = "viewer"
	RoleAnalyst       = "analyst"
	RoleContentReader = "content_reader"
	RoleAdmin         = "admin"
)

var roleOrder = []string{RoleViewer, RoleAnalyst, RoleContentReader, RoleAdmin}

// ValidRole reports whether r is a product role.
func ValidRole(r string) bool { return slices.Contains(roleOrder, r) }

// CanonicalRoles drops unknown and repeated roles and returns the rest in canonical order.
func CanonicalRoles(in []string) []string {
	out := []string{}
	for _, r := range roleOrder {
		if slices.Contains(in, r) {
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

const (
	// TokenTTL is the life of a product token.
	TokenTTL = 5 * time.Minute
	// MaxTokenTTL is the ceiling verifiers may assume for a product token's life.
	MaxTokenTTL = 10 * time.Minute
	// ServiceTokenTTL is the life of a service token.
	ServiceTokenTTL = 5 * time.Minute
	// Leeway is the clock skew a verifier tolerates on exp, nbf and iat.
	Leeway = 60 * time.Second
	// typAccessToken is the JOSE typ of every token this authority signs (RFC 9068).
	typAccessToken = "at+jwt"
)

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
	for _, want := range roles {
		if slices.Contains(p.Roles, want) {
			return true
		}
	}
	return false
}

// productClaims are the private claims of a product token; the registered ones are jwt.Claims.
type productClaims struct {
	Tenant string   `json:"sac_tenant"`
	Actor  string   `json:"actor"`
	Roles  []string `json:"roles"`
	IdP    string   `json:"idp"`
	SID    string   `json:"sid"`
}

// serviceClaims is the private claim of a service token.
type serviceClaims struct {
	Service string `json:"svc"`
}

// IssuerConfig configures the token authority.
type IssuerConfig struct {
	// Issuer is SAC_AUTH_ISSUER, the exact `iss` every verifier pins (e.g. http://control-api:8080).
	Issuer string
	Now    func() time.Time
}

// Issuer mints product access tokens and service tokens.
type Issuer struct {
	keys   *KeySet
	issuer string
	signer jose.Signer
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	signer, err := jose.NewSigner(keys.signingKey(), (&jose.SignerOptions{}).WithType(typAccessToken))
	if err != nil {
		return nil, fmt.Errorf("session: signer: %w", err)
	}
	return &Issuer{keys: keys, issuer: iss, signer: signer, now: cfg.Now}, nil
}

// Issuer is the `iss` this authority writes.
func (i *Issuer) Issuer() string { return i.issuer }

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
	registered, err := i.registered(jwt.Audience{AudienceQuery, AudienceVault, AudienceControl}, p.Subject, TokenTTL)
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := jwt.Signed(i.signer).Claims(registered).Claims(productClaims{
		Tenant: strings.ToLower(p.Tenant), Actor: p.Actor, Roles: roles, IdP: p.IdP, SID: p.SessionID,
	}).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("session: sign: %w", err)
	}
	return tok, registered.Expiry.Time().UTC(), nil
}

// ServiceToken signs a token that authenticates this service to another one: aud is audience, sub
// and svc name the calling service, and it lives ServiceTokenTTL. It carries no tenant, actor or
// role, so it can never pass as a person's product token.
func (i *Issuer) ServiceToken(audience, service string) (string, error) {
	if strings.TrimSpace(audience) == "" || strings.TrimSpace(service) == "" {
		return "", errors.New("session: a service token needs an audience and a service")
	}
	registered, err := i.registered(jwt.Audience{audience}, service, ServiceTokenTTL)
	if err != nil {
		return "", err
	}
	tok, err := jwt.Signed(i.signer).Claims(registered).Claims(serviceClaims{Service: service}).Serialize()
	if err != nil {
		return "", fmt.Errorf("session: sign: %w", err)
	}
	return tok, nil
}

func (i *Issuer) registered(aud jwt.Audience, subject string, ttl time.Duration) (jwt.Claims, error) {
	jti, err := NewUUID()
	if err != nil {
		return jwt.Claims{}, err
	}
	now := i.now().UTC()
	return jwt.Claims{
		Issuer: i.issuer, Audience: aud, Subject: subject, ID: jti,
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(ttl)),
	}, nil
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

// Verify checks a compact token for one audience: alg pinned to ES256, typ at+jwt, a kid this set
// publishes, the exact issuer, the audience, and exp/nbf/iat within Leeway.
func (v *Verifier) Verify(token, audience string) (Principal, error) {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return Principal{}, invalid("%v", err)
	}
	if len(parsed.Headers) != 1 {
		return Principal{}, invalid("not a single-signature token")
	}
	h := parsed.Headers[0]
	if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != typAccessToken {
		return Principal{}, invalid("typ %q", typ)
	}
	pub := v.keys.publicKey(h.KeyID)
	if pub == nil {
		return Principal{}, invalid("unknown kid")
	}
	var registered jwt.Claims
	var c productClaims
	if err := parsed.Claims(pub, &registered, &c); err != nil {
		return Principal{}, invalid("%v", err)
	}
	if registered.Expiry == nil {
		return Principal{}, invalid("no exp")
	}
	if err := registered.ValidateWithLeeway(jwt.Expected{
		Issuer: v.issuer, AnyAudience: jwt.Audience{audience}, Time: v.now().UTC(),
	}, Leeway); err != nil {
		return Principal{}, invalid("%v", err)
	}
	roles := CanonicalRoles(c.Roles)
	if !IsUUID(c.Tenant) || c.Actor == "" || len(roles) == 0 || registered.Subject == "" {
		return Principal{}, invalid("missing tenant, actor, role or subject")
	}
	if c.IdP != IdPEntra && c.IdP != IdPOIDC {
		return Principal{}, invalid("idp %q", c.IdP)
	}
	return Principal{
		Tenant: strings.ToLower(c.Tenant), Actor: c.Actor, Roles: roles, IdP: c.IdP,
		Subject: registered.Subject, SessionID: c.SID, TokenID: registered.ID, ExpiresAt: registered.Expiry.Time().UTC(),
	}, nil
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
