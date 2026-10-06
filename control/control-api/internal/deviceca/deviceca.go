// Package deviceca is the device certificate authority. It signs the client certificate a device
// receives at enrolment and verifies the certificates devices present on later requests.
//
// A device certificate names the device in its subject common name and the tenant in its
// organisational unit, carries the clientAuth extended key usage, and is signed by the CA whose
// certificate and key the deployment supplies. Application Gateway forwards the certificate a
// device presented without validating its chain, so every origin that trusts the forwarded
// certificate verifies it here first.
package deviceca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Validity is the life of an issued device certificate. The device renews before it ends.
const Validity = 90 * 24 * time.Hour

// Issued is a signed device certificate and the chain a verifier needs.
type Issued struct {
	// DER is the certificate's encoding; the credential id is derived from it.
	DER      []byte
	CertPEM  string
	ChainPEM []string
	NotAfter time.Time
}

// CA signs and verifies device certificates.
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
	roots   *x509.CertPool
}

// New loads the CA from its PEM certificate and PEM private key (PKCS#8, SEC 1 or PKCS#1). The key
// must belong to the certificate, and the certificate must be a CA.
func New(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, errors.New("deviceca: the CA certificate is not a PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("deviceca: parse CA certificate: %w", err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, errors.New("deviceca: the CA certificate is not a CA certificate")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("deviceca: the CA key is not PEM")
	}
	key, err := parseKey(keyBlock)
	if err != nil {
		return nil, err
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(cert.PublicKey) {
		return nil, errors.New("deviceca: the CA key does not belong to the CA certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}),
		roots:   roots,
	}, nil
}

func parseKey(block *pem.Block) (crypto.Signer, error) {
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("deviceca: parse CA key: %w", err)
		}
		return k, nil
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("deviceca: parse CA key: %w", err)
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("deviceca: parse CA key: %w", err)
		}
		switch k := k.(type) {
		case *ecdsa.PrivateKey:
			return k, nil
		case *rsa.PrivateKey:
			return k, nil
		case ed25519.PrivateKey:
			return k, nil
		}
		return nil, fmt.Errorf("deviceca: unsupported CA key type %T", k)
	}
	return nil, fmt.Errorf("deviceca: unexpected PEM block %q for the CA key", block.Type)
}

// CertificatePEM is the CA certificate, the chain a device and a verifier are given.
func (c *CA) CertificatePEM() []byte { return append([]byte(nil), c.certPEM...) }

// Sign issues a device certificate for the public key in csr, valid from now for Validity. The
// CSR's signature must verify: it is the device's proof that it holds the private key.
func (c *CA) Sign(csr *x509.CertificateRequest, tenantID, deviceID string, now time.Time) (Issued, error) {
	if csr == nil {
		return Issued{}, errors.New("deviceca: no CSR")
	}
	if err := csr.CheckSignature(); err != nil {
		return Issued{}, fmt.Errorf("deviceca: CSR signature does not verify: %w", err)
	}
	if tenantID == "" || deviceID == "" {
		return Issued{}, errors.New("deviceca: tenant and device are both required")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Issued{}, fmt.Errorf("deviceca: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: deviceID, OrganizationalUnit: []string{tenantID}},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return Issued{}, fmt.Errorf("deviceca: sign device certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Issued{}, fmt.Errorf("deviceca: parse device certificate: %w", err)
	}
	return Issued{
		DER:      der,
		CertPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ChainPEM: []string{string(c.certPEM)},
		NotAfter: leaf.NotAfter,
	}, nil
}

// Verify checks a presented chain (leaf first, then any intermediates) against the CA certificate:
// the signature chain, the validity window at now, and the clientAuth key usage.
func (c *CA) Verify(chain []*x509.Certificate, now time.Time) error {
	if len(chain) == 0 {
		return errors.New("deviceca: no certificate presented")
	}
	intermediates := x509.NewCertPool()
	for _, ic := range chain[1:] {
		intermediates.AddCert(ic)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots:         c.roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return fmt.Errorf("deviceca: %w", err)
	}
	return nil
}
