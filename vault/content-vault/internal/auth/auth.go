// Package auth is the vault's caller identity.
//
// The vault has internal ingress only (D7, docs/02 §11): devices reach content through control-api's
// grant decision and then write ciphertext straight to blob storage, and browsers reach content
// through query-api. Neither a device credential nor a browser session is accepted here, and the
// deployment must not publish this service.
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
	// Roles are the analyst-app roles the human session carries, as the caller (query-api)
	// verified from the signed token. They are optional: a service caller that acts without a
	// human (control-api on the device path, ops on the retention path) names none, and the
	// browser-facing read routes refuse it for that. The vault checks them again on those routes
	// because it is the component that returns content.
	Roles []string
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

// Authenticator resolves a request to a principal.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
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
