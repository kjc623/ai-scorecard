// Package auth verifies the bearer tokens content-vault accepts.
//
// Both kinds are JWTs signed ES256 by the product issuer (control-api) and published in its JWKS,
// and both must name this service's audience:
//
//   - a person's access token, which query-api forwards on a search or a retrieval. It carries the
//     tenant (sac_tenant), the person (actor), their product roles and a session id (sid);
//   - a service token, which control-api mints for itself to upload content. It carries
//     svc = "control-api" and no tenant: the tenant is the one named in the upload path.
//
// Tokens are verified with go-oidc against the issuer's JWKS, which is cached and re-fetched when a
// token names a key id the cache does not hold.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// DefaultAudience is this service's audience in every token it accepts.
	DefaultAudience = "sac-vault"
	// MaxTokenAge bounds how long after issue a token is accepted, whatever its exp says.
	MaxTokenAge = 10 * time.Minute
	// ClockLeeway is the clock skew tolerated on iat.
	ClockLeeway = 60 * time.Second
	// maxTokenBytes bounds what is parsed at all; a product token is a few hundred bytes.
	maxTokenBytes = 8 << 10
	maxActorRunes = 256
)

// Product roles. A role outside this set is ignored.
const (
	RoleViewer        = "viewer"
	RoleAnalyst       = "analyst"
	RoleContentReader = "content_reader"
	RoleAdmin         = "admin"
)

var productRoles = []string{RoleViewer, RoleAnalyst, RoleContentReader, RoleAdmin}

// ErrUnauthenticated wraps every reason a request carries no acceptable token.
var ErrUnauthenticated = errors.New("auth: not authenticated")

// Claims are the facts a verified token carries.
type Claims struct {
	Subject string
	// Service is the svc claim. It is set on a service token and empty on a person's token.
	Service   string
	TenantID  string
	Actor     string
	Roles     []string
	SessionID string
}

// Person is who a person's access token speaks for.
type Person struct {
	TenantID  string
	Actor     string
	Roles     []string
	SessionID string
}

// HasAnyRole reports whether the person holds at least one of roles.
func (p Person) HasAnyRole(roles ...string) bool {
	for _, r := range roles {
		if slices.Contains(p.Roles, r) {
			return true
		}
	}
	return false
}

// Person returns the person a token speaks for. A service token, or a token without a tenant, an
// actor and a product role, speaks for nobody.
func (c Claims) Person() (Person, error) {
	switch {
	case c.Service != "":
		return Person{}, fmt.Errorf("%w: a service token does not speak for a person", ErrUnauthenticated)
	case !IsUUID(c.TenantID):
		return Person{}, fmt.Errorf("%w: the token's sac_tenant is not a tenant id", ErrUnauthenticated)
	case c.Actor == "" || utf8.RuneCountInString(c.Actor) > maxActorRunes || hasControl(c.Actor):
		return Person{}, fmt.Errorf("%w: the token's actor is missing or not a printable name", ErrUnauthenticated)
	case len(c.Roles) == 0:
		return Person{}, fmt.Errorf("%w: the token names no product role", ErrUnauthenticated)
	}
	return Person{TenantID: c.TenantID, Actor: c.Actor, Roles: c.Roles, SessionID: c.SessionID}, nil
}

// Config configures a Verifier.
type Config struct {
	// Issuer is the exact iss every token must carry (SAC_AUTH_ISSUER).
	Issuer string
	// Audience is this service's audience; DefaultAudience when empty.
	Audience string
	// JWKSURL is where the issuer publishes its keys; <Issuer>/.well-known/jwks.json when empty.
	JWKSURL string
	// Client fetches the JWKS; a client with a ten-second timeout that follows no redirect when nil.
	Client *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Verifier verifies bearer tokens. It is safe for concurrent use.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
	now      func() time.Time
	// Audience and JWKSURL are the resolved settings, for the startup log.
	Audience string
	JWKSURL  string
}

// NewVerifier builds a verifier. The JWKS is fetched on first use, not here, so a service can start
// before its issuer.
func NewVerifier(cfg Config) (*Verifier, error) {
	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer == "" {
		return nil, errors.New("auth: no issuer")
	}
	if cfg.Audience == "" {
		cfg.Audience = DefaultAudience
	}
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = strings.TrimRight(issuer, "/") + "/.well-known/jwks.json"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), cfg.Client), cfg.JWKSURL)
	v := oidc.NewVerifier(issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.ES256},
		Now:                  cfg.Now,
	})
	return &Verifier{verifier: v, now: cfg.Now, Audience: cfg.Audience, JWKSURL: cfg.JWKSURL}, nil
}

// Verify checks a token's signature, issuer, audience and lifetime and returns its claims. The
// error never contains the token.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	if raw == "" || len(raw) > maxTokenBytes {
		return Claims{}, fmt.Errorf("%w: the bearer is empty or longer than any product token", ErrUnauthenticated)
	}
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	now := v.now()
	if tok.IssuedAt.IsZero() {
		return Claims{}, fmt.Errorf("%w: the token has no iat", ErrUnauthenticated)
	}
	if tok.IssuedAt.After(now.Add(ClockLeeway)) {
		return Claims{}, fmt.Errorf("%w: the token was issued in the future", ErrUnauthenticated)
	}
	if now.Sub(tok.IssuedAt) > MaxTokenAge+ClockLeeway {
		return Claims{}, fmt.Errorf("%w: the token is older than %s", ErrUnauthenticated, MaxTokenAge)
	}
	var extra struct {
		Svc       string   `json:"svc"`
		SacTenant string   `json:"sac_tenant"`
		Actor     string   `json:"actor"`
		Roles     []string `json:"roles"`
		Sid       string   `json:"sid"`
	}
	if err := tok.Claims(&extra); err != nil {
		return Claims{}, fmt.Errorf("%w: the token's claims are malformed: %v", ErrUnauthenticated, err)
	}
	c := Claims{
		Subject:  tok.Subject,
		Service:  strings.TrimSpace(extra.Svc),
		TenantID: strings.ToLower(strings.TrimSpace(extra.SacTenant)),
		Actor:    strings.TrimSpace(extra.Actor),
	}
	for _, r := range extra.Roles {
		if slices.Contains(productRoles, r) && !slices.Contains(c.Roles, r) {
			c.Roles = append(c.Roles, r)
		}
	}
	if isHex(extra.Sid) && len(extra.Sid) >= 8 && len(extra.Sid) <= 64 {
		c.SessionID = strings.ToLower(extra.Sid)
	}
	return c, nil
}

// Bearer returns the token in a request's Authorization header.
func Bearer(r *http.Request) (string, error) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, ok := strings.Cut(h, " ")
	if h == "" || !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("%w: no Authorization: Bearer token", ErrUnauthenticated)
	}
	return strings.TrimSpace(token), nil
}

// IsUUID reports whether s is a canonical lower-case UUID.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range strings.ToLower(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

func hasControl(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
