// Package auth authenticates a device from the client certificate Application Gateway forwards.
//
// The gateway terminates the device's TLS connection and passes the presented leaf in
// X-Client-Cert. The gateway is a filter, not the authority: the chain is verified here against the
// device CA, and the credential's status is read from the database on every request. Tenant and
// device come from the certificate and never from the request body.
package auth

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Principal is the authenticated device.
type Principal struct {
	TenantID     string
	DeviceID     string
	CredentialID string
}

// Authentication failures that are not a credential's database status. Those are the store's
// errors (store.ErrCredentialRevoked and the rest), returned unwrapped.
var (
	ErrNoCredential  = errors.New("auth: no client certificate presented")
	ErrBadCredential = errors.New("auth: the client certificate is not a device certificate issued by the device CA")
)

// Statuses reads a credential's state.
type Statuses interface {
	PrincipalStatus(ctx context.Context, tenantID, deviceID, credentialID string) (store.PrincipalStatus, error)
}

// Certificates authenticates the certificate forwarded in protocol.HeaderClientCert.
type Certificates struct {
	Store Statuses
	// Roots is the device CA: the certificate control-api signs device leaves with.
	Roots *x509.CertPool
	// Region is the deployment's region; a tenant pinned to another region is refused.
	Region string
	Now    func() time.Time
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Authenticate verifies the forwarded chain, reads the identity from the leaf, and checks the
// credential's status.
func (a *Certificates) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	header := strings.TrimSpace(r.Header.Get(protocol.HeaderClientCert))
	if header == "" {
		return Principal{}, ErrNoCredential
	}
	chain, err := parseChain(header)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrBadCredential, err)
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	leaf := chain[0]
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         a.Roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrBadCredential, err)
	}

	p, err := identity(leaf)
	if err != nil {
		return Principal{}, err
	}
	st, err := a.Store.PrincipalStatus(ctx, p.TenantID, p.DeviceID, p.CredentialID)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: credential status: %w", err)
	}
	if err := st.Check(now, a.Region); err != nil {
		return Principal{}, err
	}
	return p, nil
}

// identity reads the device from the subject common name and the tenant from the organisational
// unit, as control-api issues them. The credential id is a digest of the whole certificate, so the
// credential row it names binds this exact certificate, key included.
func identity(leaf *x509.Certificate) (Principal, error) {
	device := leaf.Subject.CommonName
	if !uuidRE.MatchString(device) {
		return Principal{}, fmt.Errorf("%w: the subject common name is not a device id", ErrBadCredential)
	}
	for _, ou := range leaf.Subject.OrganizationalUnit {
		if uuidRE.MatchString(ou) {
			return Principal{TenantID: ou, DeviceID: device, CredentialID: protocol.CredentialID(leaf.Raw)}, nil
		}
	}
	return Principal{}, fmt.Errorf("%w: the subject carries no tenant id", ErrBadCredential)
}

// parseChain decodes the PEM certificates in the header: the leaf first, then any intermediates.
// The gateway URL-encodes the PEM, because a header value cannot carry its newlines. Both percent
// encodings are tried (a space as %20 keeps a literal '+' of base64 intact; a space as '+' needs
// form decoding), and a raw PEM is accepted too.
func parseChain(header string) ([]*x509.Certificate, error) {
	candidates := []string{header}
	if s, err := url.PathUnescape(header); err == nil && s != header {
		candidates = append(candidates, s)
	}
	if s, err := url.QueryUnescape(header); err == nil && s != header {
		candidates = append(candidates, s)
	}
	var err error
	for _, text := range candidates {
		var chain []*x509.Certificate
		if chain, err = decodePEM(text); err == nil {
			return chain, nil
		}
	}
	return nil, err
}

func decodePEM(text string) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := []byte(text)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return nil, errors.New("the header carries no certificate")
	}
	return chain, nil
}
