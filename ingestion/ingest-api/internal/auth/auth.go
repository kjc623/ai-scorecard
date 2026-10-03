// Package auth resolves the authenticated principal for a device-facing request.
//
// Two rules from docs/02-ingest-and-transport.md §2 and §5.3 are load-bearing here:
//
//   - tenant_id, device_id and region come from the authenticated principal and never from the
//     request body (C32). The body's tenant_id is checked against this principal and a
//     disagreement is rejected, never honoured.
//   - The edge (Application Gateway, or a lab proxy speaking the same X-Client-Cert interface) is a
//     filter, never the authority: the origin re-validates the credential and, decisively, the
//     per-device credential status on every request (§2.2), with no cache, because the check is a
//     point lookup at 0.14 events/s mean. The mode is selected by what the request presents, and
//     every production mode funnels through resolveCredential so none can skip a check another makes.
package auth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

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
	// The DPoP mode's failures, kept apart so writeAuthError can name the stage (ADR 0020 §2). A
	// replay is an authentication failure, not a permissions failure: the proof authenticated a
	// different request and must not authenticate this one.
	ErrBadAccessToken         = errors.New("auth: the DPoP access token is missing, malformed, or not issued for this deployment")
	ErrBadProof               = errors.New("auth: the DPoP proof is not a valid sender-constrained proof for this request")
	ErrReplay                 = errors.New("auth: the DPoP proof was already presented (jti replay)")
	ErrThumbprintMismatch     = errors.New("auth: the presented key is not the credential's transport binding")
	ErrCredentialTypeMismatch = errors.New("auth: the credential type does not match the presented authentication mode")
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

// Presents reports whether the request carries the credential this authenticator consumes: a client
// certificate on the connection. The pluggable selector uses it so a request without one is not
// mistaken for a failed certificate.
func (a *MTLSAuthenticator) Presents(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.PeerCertificates) > 0
}

// Authenticate implements Authenticator. Go's TLS handshake has already verified the chain against
// the listener's ClientCAs (RequireAndVerifyClientCert), so what remains is to read the identity out
// of the leaf, compute the transport binding, and run the shared status check.
func (a *MTLSAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if !a.Presents(r) {
		return Principal{}, ErrNoCredential
	}
	leaf := r.TLS.PeerCertificates[0]
	tenantID, deviceID, credentialID, err := identityFromCertificate(leaf)
	if err != nil {
		return Principal{}, err
	}
	thumbprint, err := certificateSPKIThumbprint(leaf)
	if err != nil {
		return Principal{}, err
	}
	return resolveCredential(ctx, a.Store, a.Region, nowFrom(a.Now), credentialProof{
		TenantID: tenantID, DeviceID: deviceID, CredentialID: credentialID,
		Mode: protocol.AuthModeX509, Thumbprint: thumbprint,
	})
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
	// different row and revocation granularity is per credential, not per device. The derivation is
	// shared (protocol.CredentialID) so control-api, which issues the certificate, and this service,
	// which only ever sees it, compute the same id without sharing state.
	credentialID = protocol.CredentialID(leaf.Raw)
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
