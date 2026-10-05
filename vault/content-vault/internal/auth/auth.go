// Package auth is the vault's caller identity.
//
// The vault has internal ingress only (D7, docs/02 §11): devices reach content through control-api's
// grant decision and then write ciphertext straight to blob storage, and browsers reach content
// through query-api. Neither a device credential nor a browser session is accepted here, and the
// deployment must not publish this service.
//
// Two facts are established for a request, and they come from different places:
//
//   - WHICH SERVICE is calling (query-api, control-api, ops) is the ingress's assertion, read from
//     X-Sac-Service as described below.
//   - WHICH PERSON a human route serves comes, when an issuer is configured, from the product
//     access token query-api forwards (token.go, TokenAuthenticator): the vault verifies it itself,
//     and an X-Sac-Tenant, X-Sac-Subject or X-Sac-Roles header that disagrees with it is refused.
//     Only with no issuer configured (the lab) are those headers trusted on their own, and the
//     binary says so loudly at startup.
//
// Because the ingress is internal and origin-locked to the two service callers (docs/02 §12's
// "origin lock", 06 §9.3), the identity the vault reads is what the ingress put there after
// authenticating the peer. In a deployment that is the mTLS client certificate's subject and an
// Entra ID token exchanged for a service identity; on this offline host there is no TLS and no
// identity provider, so HeaderAuthenticator reads the same facts from headers and the deployment
// documentation states the dependency in one sentence:
//
//	**The vault trusts these headers only because the only thing that can reach it is the
//	internal ingress, which sets them from an authenticated peer and strips any inbound copy.**
//
// A deployment that publishes the vault, or that lets a caller reach it around the ingress, has
// turned this package into an authentication bypass. cmd/content-vault refuses to start on a
// non-loopback address unless the operator sets an explicit acknowledgement, which is the most a
// process can check about its own ingress.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Principal is an authenticated internal caller.
type Principal struct {
	// Service is the calling component: control-api, query-api or ops. It decides which routes are
	// available, so a device-facing service cannot arrange a retrieval even if it holds a token.
	Service string
	// Subject is the human or service identity the caller vouches for. It becomes the audit row's
	// actor and the principal a retrieval grant is bound to.
	Subject string
	// TenantID is the tenant the caller is acting for. It comes from the authenticated principal
	// and never from the request body (docs/02 §12).
	TenantID string
	// Roles are the analyst-app roles the human session carries. With an issuer configured they
	// come from the token the vault verified; in the header-trust lab, from X-Sac-Roles. They are
	// optional: a service caller that acts without a human (control-api on the device path, ops on
	// the retention path) names none, and the browser-facing read routes refuse it for that.
	Roles []string
	// Session is true when Subject, TenantID and Roles came from a product token this process
	// verified, rather than from headers.
	Session bool
	// SessionID is the token's `sid`, recorded in audit rows. Empty without a token.
	SessionID string
}

// HasAnyRole reports whether the principal carries at least one of the roles named.
func (p Principal) HasAnyRole(allowed ...string) bool {
	for _, have := range p.Roles {
		for _, want := range allowed {
			if have == want {
				return true
			}
		}
	}
	return false
}

// Validate rejects an incomplete principal rather than defaulting any field.
func (p Principal) Validate() error {
	if p.Service == "" {
		return errors.New("auth: principal has no service identity")
	}
	if p.Subject == "" {
		return errors.New("auth: principal has no subject")
	}
	if p.TenantID == "" {
		return errors.New("auth: principal has no tenant")
	}
	return nil
}

// ErrUnauthenticated is returned for any request the caller cannot be identified from.
var ErrUnauthenticated = errors.New("auth: caller is not authenticated")

// ErrPrincipalConflict is returned when a verified token and the X-Sac-* headers beside it name
// different people, tenants or roles. The two are never merged: a disagreement means one of them
// is wrong, and the vault cannot tell which.
var ErrPrincipalConflict = errors.New("auth: the request's headers disagree with its token")

// Authenticator resolves a request to a principal.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// SessionRequirer is implemented by an authenticator under which a human route must be backed by a
// verified token, so a header-only principal is told it is unauthenticated rather than merely
// missing a role.
type SessionRequirer interface {
	SessionRequired() bool
}

// HeaderAuthenticator reads the identity the internal ingress set.
type HeaderAuthenticator struct {
	ServiceHeader string
	SubjectHeader string
	TenantHeader  string
	// RolesHeader carries a comma-separated list of the session's analyst-app roles. It is set by
	// query-api from the verified token; a caller that names none is unaffected unless it asks for
	// a route that needs one.
	RolesHeader string
	// AllowedServices is the closed set of callers. An unknown service is refused rather than
	// trusted: the ingress may have authenticated *something*, and the vault still decides who
	// may read content.
	AllowedServices map[string]bool
}

// NewHeaderAuthenticator returns the deployment authenticator with the conventional header names.
func NewHeaderAuthenticator(allowed ...string) *HeaderAuthenticator {
	set := map[string]bool{}
	for _, s := range allowed {
		set[s] = true
	}
	return &HeaderAuthenticator{
		ServiceHeader:   "X-Sac-Service",
		SubjectHeader:   "X-Sac-Subject",
		TenantHeader:    "X-Sac-Tenant",
		RolesHeader:     "X-Sac-Roles",
		AllowedServices: set,
	}
}

// Authenticate implements Authenticator. Every missing or unknown fact is a refusal; there is no
// anonymous principal and no default tenant.
func (h *HeaderAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	p := Principal{
		Service:  strings.TrimSpace(r.Header.Get(h.ServiceHeader)),
		Subject:  strings.TrimSpace(r.Header.Get(h.SubjectHeader)),
		TenantID: strings.ToLower(strings.TrimSpace(r.Header.Get(h.TenantHeader))),
		Roles:    splitRoles(r.Header.Get(h.RolesHeader)),
	}
	if err := p.Validate(); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if len(h.AllowedServices) > 0 && !h.AllowedServices[p.Service] {
		return Principal{}, fmt.Errorf("%w: service %q is not permitted to call content-vault", ErrUnauthenticated, p.Service)
	}
	if !looksLikeUUID(p.TenantID) {
		return Principal{}, fmt.Errorf("%w: tenant %q is not a uuid", ErrUnauthenticated, p.TenantID)
	}
	return p, nil
}

// TokenAuthenticator is the authenticator when an issuer is configured. The calling service is
// still the ingress's assertion (Headers), but the person is the verified product token's.
type TokenAuthenticator struct {
	Headers  *HeaderAuthenticator
	Verifier *TokenVerifier
}

// SessionRequired implements SessionRequirer: under a token issuer, roles exist only in tokens.
func (a *TokenAuthenticator) SessionRequired() bool { return true }

// Authenticate implements Authenticator.
//
// With no Authorization header the caller is a service acting for no person — control-api on the
// device path, ops on retention — and is read exactly as HeaderAuthenticator reads it, except that
// it may not name roles: a role is a property of a person's session, and under an issuer a session
// is a token. With a bearer, the tenant, subject and roles are the token's, and any X-Sac-* header
// that is present must agree with them.
func (a *TokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		if strings.TrimSpace(r.Header.Get(a.Headers.RolesHeader)) != "" {
			return Principal{}, fmt.Errorf("%w: %s names roles without a session token; roles come only from a verified token", ErrUnauthenticated, a.Headers.RolesHeader)
		}
		return a.Headers.Authenticate(r)
	}
	scheme, token, ok := strings.Cut(authz, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return Principal{}, fmt.Errorf("%w: Authorization is not a Bearer token", ErrUnauthenticated)
	}
	service := strings.TrimSpace(r.Header.Get(a.Headers.ServiceHeader))
	if service == "" {
		return Principal{}, fmt.Errorf("%w: principal has no service identity", ErrUnauthenticated)
	}
	if len(a.Headers.AllowedServices) > 0 && !a.Headers.AllowedServices[service] {
		return Principal{}, fmt.Errorf("%w: service %q is not permitted to call content-vault", ErrUnauthenticated, service)
	}
	claims, err := a.Verifier.Verify(r.Context(), strings.TrimSpace(token))
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	p := Principal{
		Service: service, Subject: claims.Actor, TenantID: claims.TenantID,
		Roles: claims.Roles, Session: true, SessionID: claims.SessionID,
	}
	if h := strings.ToLower(strings.TrimSpace(r.Header.Get(a.Headers.TenantHeader))); h != "" && h != p.TenantID {
		return Principal{}, fmt.Errorf("%w: %s names tenant %q and the token names %q", ErrPrincipalConflict, a.Headers.TenantHeader, h, p.TenantID)
	}
	if h := strings.TrimSpace(r.Header.Get(a.Headers.SubjectHeader)); h != "" && h != p.Subject {
		return Principal{}, fmt.Errorf("%w: %s names %q and the token's actor is %q", ErrPrincipalConflict, a.Headers.SubjectHeader, h, p.Subject)
	}
	if raw := r.Header.Get(a.Headers.RolesHeader); strings.TrimSpace(raw) != "" && !sameRoles(splitRoles(raw), p.Roles) {
		return Principal{}, fmt.Errorf("%w: %s names %v and the token grants %v", ErrPrincipalConflict, a.Headers.RolesHeader, splitRoles(raw), p.Roles)
	}
	if err := p.Validate(); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	return p, nil
}

// sameRoles compares two role lists as sets.
func sameRoles(a, b []string) bool {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, r := range a {
		as[r] = true
	}
	for _, r := range b {
		bs[r] = true
	}
	if len(as) != len(bs) {
		return false
	}
	for r := range as {
		if !bs[r] {
			return false
		}
	}
	return true
}

// StaticAuthenticator returns one principal for every request. It exists for tests and for a local
// development run, and it must never be wired into a deployment: a fixed principal is the absence
// of authentication, and the type says so rather than hiding it behind a name like "dev mode".
type StaticAuthenticator struct{ P Principal }

// Authenticate implements Authenticator.
func (s StaticAuthenticator) Authenticate(*http.Request) (Principal, error) {
	if err := s.P.Validate(); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	return s.P, nil
}

// splitRoles reads the comma-separated roles header. Empty entries are dropped, so a trailing
// comma, or a caller that names no role, yields an empty list rather than a role named "".
func splitRoles(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if role := strings.TrimSpace(part); role != "" {
			out = append(out, role)
		}
	}
	return out
}

func looksLikeUUID(s string) bool {
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
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}
