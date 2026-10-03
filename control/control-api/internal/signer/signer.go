// Package signer issues the leaf credential an x509 enrolment receives (ADR 0020 decision 3).
//
// The interface exists because the development and production implementations are different
// authorities: a local CA that the repository can stand up with no PKI, and Azure Key Vault /
// Managed HSM in a deployment. The service depends on the interface, never on either concrete
// type, so the two are interchangeable without a branch in the request path.
package signer

import (
	"context"
	"crypto/x509"
	"time"
)

// Request is one leaf to issue. The private key is the device's and never appears here: the CSR
// carries only the public key and its proof of possession.
type Request struct {
	// CSR is the device's PKCS#10 request, already parsed and signature-checked by the caller.
	CSR *x509.CertificateRequest
	// DeviceID becomes the leaf's subject CN.
	DeviceID string
	// TenantID becomes the leaf's organisational unit, the only place the tenant is read from at
	// authentication time (docs/02-ingest-and-transport.md §2.2).
	TenantID string
}

// Issued is the leaf and its chain. NotAfter is materialised on the credential row so the expiry
// watch (docs/02 §2.2) is a query rather than a scan of certificates.
type Issued struct {
	CertPEM  string
	ChainPEM []string
	NotAfter time.Time
}

// CertificateSigner turns a PKCS#10 CSR into a leaf. Sign must never reuse key material: a local CA
// may be generated fresh, and a Key Vault signer must delegate to the HSM.
type CertificateSigner interface {
	Sign(ctx context.Context, req Request) (Issued, error)
	// Name identifies the authority in the startup log; it never carries key material.
	Name() string
}
