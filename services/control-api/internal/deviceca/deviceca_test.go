package deviceca_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/deviceca"
	"github.com/shadow-ai-capture/control-api/internal/deviceca/devicecatest"
)

const (
	deviceID = "33333333-3333-4333-8333-333333333333"
	tenantID = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
)

func csrOf(t *testing.T) *x509.CertificateRequest {
	t.Helper()
	dk := devicecatest.NewDeviceKey(t)
	block, _ := pem.Decode([]byte(dk.CSRPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func TestSignIssuesAVerifiableClientCertificate(t *testing.T) {
	a := devicecatest.New(t)
	now := time.Now()
	issued, err := a.CA.Sign(csrOf(t), tenantID, deviceID, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(issued.DER)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != deviceID {
		t.Errorf("CN = %q, want %q", leaf.Subject.CommonName, deviceID)
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != tenantID {
		t.Errorf("OU = %v, want [%s]", leaf.Subject.OrganizationalUnit, tenantID)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("EKU = %v, want [clientAuth]", leaf.ExtKeyUsage)
	}
	if !issued.NotAfter.Equal(leaf.NotAfter) || leaf.NotAfter.Sub(now) > deviceca.Validity {
		t.Errorf("NotAfter = %v, leaf %v", issued.NotAfter, leaf.NotAfter)
	}
	if len(issued.ChainPEM) != 1 || issued.ChainPEM[0] != string(a.CertPEM) {
		t.Errorf("chain is not the CA certificate")
	}
	if err := a.CA.Verify([]*x509.Certificate{leaf}, now); err != nil {
		t.Fatalf("the issued certificate does not verify: %v", err)
	}
}

func TestVerifyRefusesWhatTheCADidNotIssue(t *testing.T) {
	a := devicecatest.New(t)
	other := devicecatest.New(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	good := func() *x509.Certificate {
		return &x509.Certificate{
			Subject:     pkix.Name{CommonName: deviceID, OrganizationalUnit: []string{tenantID}},
			NotBefore:   now.Add(-time.Hour),
			NotAfter:    now.Add(time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
	}
	expired := good()
	expired.NotBefore, expired.NotAfter = now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	serverAuth := good()
	serverAuth.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	for name, leaf := range map[string]*x509.Certificate{
		"another CA":       other.Leaf(t, good(), &key.PublicKey),
		"expired":          a.Leaf(t, expired, &key.PublicKey),
		"not client auth":  a.Leaf(t, serverAuth, &key.PublicKey),
		"self-signed leaf": selfSigned(t, key),
	} {
		if err := a.CA.Verify([]*x509.Certificate{leaf}, now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if err := a.CA.Verify(nil, now); err == nil {
		t.Error("an empty chain verified")
	}
	if err := a.CA.Verify([]*x509.Certificate{a.Leaf(t, good(), &key.PublicKey)}, now); err != nil {
		t.Errorf("a good leaf did not verify: %v", err)
	}
}

func selfSigned(t *testing.T, key *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: deviceID},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestNewRefusesMismatchedOrIncompleteMaterial(t *testing.T) {
	a := devicecatest.New(t)
	b := devicecatest.New(t)
	for name, c := range map[string][2][]byte{
		"key of another CA": {a.CertPEM, b.KeyPEM},
		"no key":            {a.CertPEM, nil},
		"no certificate":    {nil, a.KeyPEM},
		"key as cert":       {a.KeyPEM, a.KeyPEM},
	} {
		if _, err := deviceca.New(c[0], c[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notCA := a.Leaf(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "leaf"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
	}, &leafKey.PublicKey)
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: notCA.Raw})
	der, _ := x509.MarshalECPrivateKey(leafKey)
	if _, err := deviceca.New(leafPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err == nil ||
		!strings.Contains(err.Error(), "not a CA") {
		t.Errorf("a leaf certificate was accepted as the CA: %v", err)
	}
}

func TestSignRefusesAForgedCSR(t *testing.T) {
	a := devicecatest.New(t)
	csr := csrOf(t)
	csr.Signature[len(csr.Signature)-1] ^= 0xff
	if _, err := a.CA.Sign(csr, tenantID, deviceID, time.Now()); err == nil {
		t.Fatal("a CSR whose signature does not verify was signed")
	}
}
