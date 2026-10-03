package auth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// credentialProof is what a credential path has cryptographically established before the store is
// consulted: who the credential claims to be, which mode presented it, and the transport binding
// the request proved possession of. Tenant, device and credential are the claims the credential
// itself carries; a body or header value never reaches this struct.
type credentialProof struct {
	TenantID     string
	DeviceID     string
	CredentialID string
	Mode         protocol.AuthMode
	// Thumbprint is the binding value derived from what was presented: SHA-256 over the certificate
	// SPKI for x509, the RFC 7638 JWK thumbprint for dpop. The store compares it with
	// ops.device_credential.public_key_thumbprint.
	Thumbprint string
}

// nowFrom resolves an optional injected clock, so every authenticator reads time the same way and a
// test can make expiry and replay windows deterministic.
func nowFrom(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// resolveCredential runs the §2.3 status check shared by every production authenticator. It is one
// function so the two certificate paths and the DPoP path cannot diverge: a check added here is a
// check every mode makes, and none can skip the transport-binding comparison.
func resolveCredential(ctx context.Context, s store.Store, region string, now time.Time, p credentialProof) (Principal, error) {
	st, err := s.PrincipalStatus(ctx, p.TenantID, p.DeviceID, p.CredentialID)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: credential status: %w", err)
	}
	if !st.TenantKnown {
		return Principal{}, ErrUnknownTenant
	}
	if !st.IngestEnabled || st.TenantStatus == "closed" {
		return Principal{}, ErrTenantSuspended
	}
	if region != "" && st.TenantRegion != "" && st.TenantRegion != region {
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
	// The transport binding and the credential's mode are checked once the row is known to exist,
	// before the revocation and expiry states are reported: a key that does not match is refused as
	// a bad credential, not as a revoked one.
	if err := st.CheckBinding(string(p.Mode), p.Thumbprint); err != nil {
		switch {
		case errors.Is(err, store.ErrCredentialTypeMismatch):
			return Principal{}, fmt.Errorf("%w: %v", ErrCredentialTypeMismatch, err)
		case errors.Is(err, store.ErrCredentialThumbprintMismatch):
			return Principal{}, fmt.Errorf("%w: %v", ErrThumbprintMismatch, err)
		default:
			return Principal{}, fmt.Errorf("%w: %v", ErrBadCredential, err)
		}
	}
	if st.CredentialRevoked != nil {
		return Principal{}, ErrCredentialRevoked
	}
	if !st.CredentialExpiry.IsZero() && !st.CredentialExpiry.After(now) {
		return Principal{}, ErrCredentialExpired
	}

	return Principal{
		TenantID:     p.TenantID,
		DeviceID:     p.DeviceID,
		CredentialID: p.CredentialID,
		NotAfter:     st.CredentialExpiry,
	}, nil
}

// certificateSPKIThumbprint is the single transport binding for x509 (ADR 0020 §4): SHA-256 over the
// certificate's SubjectPublicKeyInfo, base64url without padding. Base64url is deliberate: it is the
// same representation the RFC 7638 JWK thumbprint uses, so the one public_key_thumbprint column
// holds one kind of value for both modes.
func certificateSPKIThumbprint(leaf *x509.Certificate) (string, error) {
	if leaf == nil || len(leaf.RawSubjectPublicKeyInfo) == 0 {
		return "", fmt.Errorf("%w: certificate carries no subject public key info", ErrBadCredential)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// parseCertificateChain reads the PEM certificates a forwarded header carries. The first
// CERTIFICATE is the leaf; any remaining certificates are intermediates used to build the chain to
// the configured roots.
//
// The header may carry a raw PEM (as a direct-TLS lab listener or a unit test writes it) or a
// URL-encoded PEM. The encoded form is the only one a multi-line value can take inside a
// single-line HTTP header, and it is the form an L7 gateway actually forwards (Application Gateway,
// and AWS ALB's X-Amzn-Mtls-Clientcert). Both are accepted; anything else is refused rather than
// treated as "no credential", because the header's presence is a claim and a malformed one is a bad
// credential.
func parseCertificateChain(raw []byte) ([]*x509.Certificate, error) {
	certs, err := decodePEMCertificates(raw)
	if err == nil {
		return certs, nil
	}
	// Go's HTTP stack refuses to write a header value containing a raw newline, which is why the
	// gateway must percent-encode the PEM; try that form before giving up.
	if unescaped, uerr := url.QueryUnescape(string(raw)); uerr == nil && unescaped != string(raw) {
		if decoded, derr := decodePEMCertificates([]byte(unescaped)); derr == nil {
			return decoded, nil
		}
	}
	return nil, err
}

// decodePEMCertificates decodes every CERTIFICATE block in a byte slice and returns them in order.
func decodePEMCertificates(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := raw
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
			return nil, fmt.Errorf("parse a forwarded certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("forwarded certificate header carries no CERTIFICATE block")
	}
	return certs, nil
}
