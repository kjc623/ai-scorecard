// Package auth resolves the authenticated principal for a device-facing request.
//
// Two rules from docs/02-ingest-and-transport.md §2 and §5.3 are load-bearing here:
//
//   - tenant_id, device_id and region come from the authenticated principal and never from the
//     request body (C32). The body's tenant_id is checked against this principal and a
//     disagreement is rejected, never honoured.
//   - The edge (Front Door mTLS) is a filter, never the authority: the origin re-validates the
//     certificate and, decisively, the per-device credential status on every request (§2.2), with
//     no cache, because the check is a point lookup at 0.14 events/s mean.
package auth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Principal is who the request is from. Everything here is derived from the credential.
type Principal struct {
	TenantID     string
	DeviceID     string
	CredentialID string
	NotAfter     time.Time
}

// Authenticator turns a request into a Principal.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// Authentication failures. They are distinct because the operator-facing response distinguishes
// them, even where §7's closed code set has only one code for the class.
var (
	ErrNoCredential      = errors.New("auth: no client credential presented")
	ErrBadCredential     = errors.New("auth: client credential is not a device credential")
	ErrCredentialUnknown = errors.New("auth: credential unknown or not issued by this deployment")
	ErrCredentialRevoked = errors.New("auth: credential revoked")
	ErrCredentialExpired = errors.New("auth: credential expired")
	ErrDeviceRevoked     = errors.New("auth: device revoked")
	ErrUnknownTenant     = errors.New("auth: tenant unknown or inactive")
	ErrTenantSuspended   = errors.New("auth: tenant ingest is disabled")
	ErrRegionMismatch    = errors.New("auth: deployment region is not the tenant's pinned region")
)

// MTLSAuthenticator authenticates a request from its TLS client certificate and the credential
// table. It performs the admission-time status check; the authoritative check happens again inside
// the write transaction (store.WriteBatch), because a credential can be revoked while a batch is
// being validated (§2.3).
type MTLSAuthenticator struct {
	Store store.Store
	// Region is the region this deployment serves, used for the §12 fail-closed region check.
	// Empty disables only that check, and does so visibly: the deployment is then single-region.
	Region string
	Now    func() time.Time
}

// Authenticate implements Authenticator.
func (a *MTLSAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Principal{}, ErrNoCredential
	}
	leaf := r.TLS.PeerCertificates[0]
	tenantID, deviceID, credentialID, err := identityFromCertificate(leaf)
	if err != nil {
		return Principal{}, err
	}

	now := time.Now
	if a.Now != nil {
		now = a.Now
	}

	st, err := a.Store.PrincipalStatus(ctx, tenantID, deviceID, credentialID)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: credential status: %w", err)
	}
	if !st.TenantKnown {
		return Principal{}, ErrUnknownTenant
	}
	if !st.IngestEnabled || st.TenantStatus == "closed" {
		return Principal{}, ErrTenantSuspended
	}
	if a.Region != "" && st.TenantRegion != "" && st.TenantRegion != a.Region {
		return Principal{}, ErrRegionMismatch
	}
	if !st.DeviceKnown {
		return Principal{}, ErrCredentialUnknown
	}
	if st.DeviceRevokedAt != nil {
		return Principal{}, ErrDeviceRevoked
	}
	if !st.CredentialKnown {
		return Principal{}, ErrCredentialUnknown
	}
	if st.CredentialRevoked != nil {
		return Principal{}, ErrCredentialRevoked
	}
	if !st.CredentialExpiry.IsZero() && !st.CredentialExpiry.After(now()) {
		return Principal{}, ErrCredentialExpired
	}

	return Principal{
		TenantID:     tenantID,
		DeviceID:     deviceID,
		CredentialID: credentialID,
		NotAfter:     st.CredentialExpiry,
	}, nil
}

// identityFromCertificate reads the identity §2.2 puts in the certificate: device_id as the
// subject CN, the tenant in an organisational attribute. The tenant is NOT taken from any header:
// a header is caller-controlled, and tenant from the principal is what makes cross-tenant writes
// structurally impossible rather than merely unauthorised.
func identityFromCertificate(leaf *x509.Certificate) (tenantID, deviceID, credentialID string, err error) {
	deviceID = leaf.Subject.CommonName
	if deviceID == "" {
		return "", "", "", fmt.Errorf("%w: certificate has no subject CN", ErrBadCredential)
	}
	if !contract.IsUUID(deviceID) {
		return "", "", "", fmt.Errorf("%w: subject CN %q is not a device uuid", ErrBadCredential, deviceID)
	}
	for _, ou := range leaf.Subject.OrganizationalUnit {
		if contract.IsUUID(ou) {
			tenantID = ou
			break
		}
	}
	if tenantID == "" {
		return "", "", "", fmt.Errorf("%w: certificate carries no tenant organisational unit", ErrBadCredential)
	}
	// The credential id is derived from the certificate itself, so a re-issued certificate is a
	// different row and revocation granularity is per credential, not per device.
	sum := leaf.Raw
	credentialID = contract.DeterministicUUID("sac-credential\x1f", sum)
	return tenantID, deviceID, credentialID, nil
}

// Static is a test authenticator: it returns a fixed principal or a fixed error. It is used by the
// service's tests in place of a TLS handshake, never by the binary.
type Static struct {
	P   Principal
	Err error
}

// Authenticate implements Authenticator.
func (s Static) Authenticate(context.Context, *http.Request) (Principal, error) { return s.P, s.Err }
