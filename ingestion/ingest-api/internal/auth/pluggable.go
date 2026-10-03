package auth

import (
	"context"
	"errors"
	"net/http"
)

// presenter is implemented by every authenticator that can tell from the request alone whether it
// is the right mode, before it performs any verification. The selector uses it so a request that
// presents no credential at all is refused as absent rather than as a failed verification, and so a
// credential present in one form is never silently re-interpreted as another.
type presenter interface {
	Presents(r *http.Request) bool
}

// Pluggable is the single seam ADR 0020 decision 2 calls for: it selects the authenticator by what
// the request presents. A deployment configures the production modes it serves; the development
// authenticator is optional and only ever set behind the existing acknowledgement.
type Pluggable struct {
	Direct    *MTLSAuthenticator
	Forwarded *ForwardedCertAuthenticator
	DPoP      *DPoPAuthenticator
	Dev       Authenticator
}

// NewPluggable validates the selection at construction. A process with no production mode and no
// acknowledged development mode must not serve, which is the same refusal main.go makes with its
// dedicated message; catching it here keeps the rule with the code that selects.
func NewPluggable(p Pluggable) (*Pluggable, error) {
	if p.Direct == nil && p.Forwarded == nil && p.DPoP == nil && p.Dev == nil {
		return nil, errors.New("auth: no device authenticator is configured (no x509, dpop, or dev mode)")
	}
	out := p
	return &out, nil
}

// Authenticate implements Authenticator. The order is the order of the wire modes: a certificate on
// the connection, a certificate forwarded by the edge, a DPoP-bound request, then the development
// escape hatch. A request that presents none of them is ErrNoCredential, not a failed credential.
func (p *Pluggable) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	switch {
	case p.Direct != nil && p.Direct.Presents(r):
		return p.Direct.Authenticate(ctx, r)
	case p.Forwarded != nil && p.Forwarded.Presents(r):
		return p.Forwarded.Authenticate(ctx, r)
	case p.DPoP != nil && p.DPoP.Presents(r):
		return p.DPoP.Authenticate(ctx, r)
	}
	if p.Dev != nil {
		// A dev authenticator without a presence test (a test double) is called unconditionally;
		// the real DevHeader carries one so it only answers its own headers.
		if pr, ok := p.Dev.(presenter); !ok || pr.Presents(r) {
			return p.Dev.Authenticate(ctx, r)
		}
	}
	return Principal{}, ErrNoCredential
}
