package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"
)

// TestLoadSignerPrefersConfiguredCAOverVaultURI pins the Shape A selection rule. A deployment always
// passes SAC_KEYVAULT_URI, and before this rule existed that pushed the signer onto the unimplemented
// KeyVaultSigner even when a device CA key pair was configured, so every certificate enrolment was
// refused. The configured CA material must win; the vault signer is only the fallback when none is.
func TestLoadSignerPrefersConfiguredCAOverVaultURI(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Device CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	t.Setenv(EnvCACertPEM, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	t.Setenv(EnvCAKeyPEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const kv = "https://example.vault.azure.net/"

	sg, err := loadSigner(options{credentialTTL: 24 * time.Hour}, kv, logger)
	if err != nil {
		t.Fatalf("loadSigner with CA material: %v", err)
	}
	if sg.Name() != "local-ca" {
		t.Fatalf("signer = %q, want local-ca: configured CA material must win over the vault URI", sg.Name())
	}

	// No CA material: the vault URI selects the vault signer (which refuses until it is implemented),
	// which is the correct refusal for a deployment that configured no CA.
	t.Setenv(EnvCACertPEM, "")
	t.Setenv(EnvCAKeyPEM, "")
	sg, err = loadSigner(options{credentialTTL: 24 * time.Hour}, kv, logger)
	if err != nil {
		t.Fatalf("loadSigner without CA material: %v", err)
	}
	if sg.Name() == "local-ca" {
		t.Fatal("no CA material selected the local CA anyway")
	}
}
