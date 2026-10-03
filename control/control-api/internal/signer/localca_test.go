package signer_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/signer"
)

const (
	deviceID = "33333333-3333-7333-8333-333333333333"
	tenantID = "11111111-1111-7111-8111-111111111111"
)

func makeCSR(t *testing.T) *x509.CertificateRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	return csr
}

// TestLocalCASignsAVerifiableClientLeaf is the x509 contract of ADR 0020 decision 3.
func TestLocalCASignsAVerifiableClientLeaf(t *testing.T) {
	ca, err := signer.NewLocalCA(nil, nil, 30*24*time.Hour, []string{"ingest.eu.example.com"})
	if err != nil {
		t.Fatalf("NewLocalCA: %v", err)
	}
	issued, err := ca.Sign(context.Background(), signer.Request{CSR: makeCSR(t), DeviceID: deviceID, TenantID: tenantID})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	block, _ := pem.Decode([]byte(issued.CertPEM))
	if block == nil {
		t.Fatal("issued credential is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
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
	if len(issued.ChainPEM) != 1 {
		t.Fatalf("chain = %d certificates, want 1", len(issued.ChainPEM))
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(issued.ChainPEM[0])) {
		t.Fatal("chain does not parse")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("leaf does not verify: %v", err)
	}
}

// TestLocalCAGeneratesAFreshKeyEachTime is the "never reuse a fixed key" rule: two generated CAs must
// have different public keys and different certificate bodies.
func TestLocalCAGeneratesAFreshKeyEachTime(t *testing.T) {
	a, err := signer.NewLocalCA(nil, nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewLocalCA a: %v", err)
	}
	b, err := signer.NewLocalCA(nil, nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewLocalCA b: %v", err)
	}
	if string(a.CACertPEM()) == string(b.CACertPEM()) {
		t.Fatal("two generated CAs are byte-identical: the key was reused")
	}
	pa, _ := x509.ParseCertificate(mustBlock(t, a.CACertPEM()))
	pb, _ := x509.ParseCertificate(mustBlock(t, b.CACertPEM()))
	ka := pa.PublicKey.(*ecdsa.PublicKey)
	kb := pb.PublicKey.(*ecdsa.PublicKey)
	if ka.Equal(kb) {
		t.Fatal("two generated CAs share a public key")
	}
}

// TestKeyVaultSignerRefusesRatherThanFakingIt proves the production seam is not a local key wearing
// a vault URI: every Sign call is a clear refusal.
func TestKeyVaultSignerRefusesRatherThanFakingIt(t *testing.T) {
	kv := &signer.KeyVaultSigner{VaultURI: "https://kv.example/keys/leaf"}
	if _, err := kv.Sign(context.Background(), signer.Request{CSR: makeCSR(t), DeviceID: deviceID, TenantID: tenantID}); err == nil {
		t.Fatal("the Key Vault signer produced a credential")
	} else if !contains(err.Error(), "Key Vault") {
		t.Fatalf("refusal does not name Key Vault: %v", err)
	}
}

func mustBlock(t *testing.T, pemBytes []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block")
	}
	return block.Bytes
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
