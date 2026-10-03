package signer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// LocalCA is a development and test certificate authority. It either loads an existing CA key pair
// or generates one in process, and it signs a PKCS#10 CSR into a leaf with CN=device_id,
// OU=tenant_id, EKU=clientAuth and the configured validity and SANs.
//
// A generated CA is ephemeral: it lives only in the process and is regenerated on every start, which
// is exactly what makes a laptop or a test need no PKI. A deployment uses the loaded form or Key
// Vault, because an ephemeral authority would invalidate every device credential on restart.
type LocalCA struct {
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
	certPEM  []byte
	validity time.Duration
	sans     []string
	now      func() time.Time
}

// NewLocalCA loads the CA from PEM material, or generates a fresh one when both arguments are empty.
// It refuses a half-supplied pair: a certificate without its key cannot sign, and surfacing that as
// a signing failure on the first enrolment is worse than refusing to start.
func NewLocalCA(certPEM, keyPEM []byte, validity time.Duration, sans []string) (*LocalCA, error) {
	if validity <= 0 {
		validity = 90 * 24 * time.Hour
	}
	ca := &LocalCA{validity: validity, sans: sans, now: time.Now}
	switch {
	case len(certPEM) == 0 && len(keyPEM) == 0:
		if err := ca.generate(); err != nil {
			return nil, err
		}
	case len(certPEM) == 0 || len(keyPEM) == 0:
		return nil, fmt.Errorf("signer: CA certificate and key must both be supplied or both omitted")
	default:
		if err := ca.load(certPEM, keyPEM); err != nil {
			return nil, err
		}
	}
	return ca, nil
}

// Name implements CertificateSigner.
func (c *LocalCA) Name() string { return "local-ca" }

// CACertPEM returns the CA certificate, which is the chain a device is given. It is not secret.
func (c *LocalCA) CACertPEM() []byte { return append([]byte(nil), c.certPEM...) }

// SetNow overrides the clock for tests.
func (c *LocalCA) SetNow(now func() time.Time) { c.now = now }

func (c *LocalCA) generate() error {
	// A fresh P-256 key on every call. The invariant the task states -- never a fixed key -- is
	// enforced by construction here and asserted by a test that generates two CAs and checks the
	// public keys differ.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("signer: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := c.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Shadow AI Capture Local CA", Organization: []string{"Shadow AI Capture"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("signer: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("signer: parse generated CA certificate: %w", err)
	}
	c.cert, c.key = cert, key
	c.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return nil
}

func (c *LocalCA) load(certPEM, keyPEM []byte) error {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return fmt.Errorf("signer: CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("signer: parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return fmt.Errorf("signer: CA certificate is not a CA")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("signer: CA key is not PEM")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParseECPrivateKey(keyBlock.Bytes); err == nil {
		key = k
	} else if parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
		ec, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return fmt.Errorf("signer: CA key is not ECDSA")
		}
		key = ec
	} else {
		return fmt.Errorf("signer: parse CA key: %w", err)
	}
	c.cert, c.key = cert, key
	c.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return nil
}

// Sign implements CertificateSigner.
func (c *LocalCA) Sign(_ context.Context, req Request) (Issued, error) {
	if req.CSR == nil {
		return Issued{}, fmt.Errorf("signer: no CSR")
	}
	// The CSR must prove possession of the private key; signing a request whose signature does not
	// verify would issue a certificate for a key nobody has demonstrated.
	if err := req.CSR.CheckSignature(); err != nil {
		return Issued{}, fmt.Errorf("signer: CSR signature does not verify: %w", err)
	}
	if req.DeviceID == "" || req.TenantID == "" {
		return Issued{}, fmt.Errorf("signer: device and tenant are both required")
	}
	serial, err := randomSerial()
	if err != nil {
		return Issued{}, err
	}
	now := c.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         req.DeviceID,
			OrganizationalUnit: []string{req.TenantID},
		},
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    now.Add(c.validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	for _, san := range c.sans {
		if ip := net.ParseIP(san); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if san != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, san)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, req.CSR.PublicKey, c.key)
	if err != nil {
		return Issued{}, fmt.Errorf("signer: create leaf certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Issued{}, fmt.Errorf("signer: parse leaf certificate: %w", err)
	}
	return Issued{
		CertPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ChainPEM: []string{string(c.certPEM)},
		NotAfter: leaf.NotAfter,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("signer: generate serial: %w", err)
	}
	return serial, nil
}
