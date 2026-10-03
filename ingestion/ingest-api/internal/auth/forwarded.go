package auth

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// ForwardedCertAuthenticator authenticates the x509 mode when the edge, not this process,
// terminated the device TLS handshake: Application Gateway forwards the client certificate as PEM
// in X-Client-Cert (ADR 0020 decision 2). The edge is a filter, never the authority, so the chain
// is re-verified here against ClientCAs with the clientAuth EKU before any identity is read.
//
// A forwarded certificate is only trustworthy when the origin is reachable solely through the edge
// (private network, source restricted to the gateway subnet). That is a deployment property this
// code cannot check; what it can do, and does, is refuse anything the CA bundle did not sign.
type ForwardedCertAuthenticator struct {
	Store store.Store
	// ClientCAs is the trust bundle the forwarded chain must verify against. Required: a forwarded
	// path with no roots would accept any certificate the header carried.
	ClientCAs *x509.CertPool
	// Region is the region this deployment serves, for the §12 fail-closed check. Empty disables only
	// that check, visibly: the deployment is then single-region.
	Region string
	// Header is the edge-forwarded certificate header. Empty means protocol.HeaderClientCert.
	Header string
	Now    func() time.Time
}

// Presents reports whether the request carries a forwarded certificate header.
func (a *ForwardedCertAuthenticator) Presents(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(a.header())) != ""
}

func (a *ForwardedCertAuthenticator) header() string {
	if a.Header != "" {
		return a.Header
	}
	return protocol.HeaderClientCert
}

// Authenticate implements Authenticator.
func (a *ForwardedCertAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	raw := r.Header.Get(a.header())
	if strings.TrimSpace(raw) == "" {
		return Principal{}, ErrNoCredential
	}
	if a.ClientCAs == nil {
		// A configuration defect, not a device one: refuse rather than trust an unverified chain.
		return Principal{}, fmt.Errorf("%w: no client CA bundle is configured for the forwarded certificate path", ErrBadCredential)
	}
	certs, err := parseCertificateChain([]byte(raw))
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrBadCredential, err)
	}
	leaf := certs[0]

	roots := a.ClientCAs
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	now := nowFrom(a.Now)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		// The certificate must be usable for client authentication; a server certificate that
		// happened to chain to the CA is not a device credential.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return Principal{}, fmt.Errorf("%w: forwarded certificate chain: %v", ErrBadCredential, err)
	}

	tenantID, deviceID, credentialID, err := identityFromCertificate(leaf)
	if err != nil {
		return Principal{}, err
	}
	thumbprint, err := certificateSPKIThumbprint(leaf)
	if err != nil {
		return Principal{}, err
	}
	return resolveCredential(ctx, a.Store, a.Region, now, credentialProof{
		TenantID: tenantID, DeviceID: deviceID, CredentialID: credentialID,
		Mode: protocol.AuthModeX509, Thumbprint: thumbprint,
	})
}
