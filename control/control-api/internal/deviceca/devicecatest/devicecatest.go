// Package devicecatest generates device CA material and device keys for tests.
package devicecatest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/deviceca"
)

// Authority is a generated CA: its PEM material, its parsed parts, and the loaded deviceca.CA.
type Authority struct {
	CertPEM []byte
	KeyPEM  []byte
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CA      *deviceca.CA
}

// New generates a fresh P-256 CA valid from a day ago for ten years.
func New(t testing.TB) *Authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Test Device CA"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	a := &Authority{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Cert:    cert,
		Key:     key,
	}
	if a.CA, err = deviceca.New(a.CertPEM, a.KeyPEM); err != nil {
		t.Fatal(err)
	}
	return a
}

// Leaf signs an arbitrary certificate for pub with this CA, for tests that need a certificate the
// enrolment path would never issue (expired, wrong key usage).
func (a *Authority) Leaf(t testing.TB, tmpl *x509.Certificate, pub any) *x509.Certificate {
	t.Helper()
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, pub, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// DeviceKey is a device's key pair and a CSR for it, PEM-encoded as the enrolment request carries it.
type DeviceKey struct {
	Key    *ecdsa.PrivateKey
	CSRPEM string
}

// NewDeviceKey generates a P-256 device key and a CSR for it.
func NewDeviceKey(t testing.TB) DeviceKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "device"}, SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return DeviceKey{Key: key, CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))}
}
