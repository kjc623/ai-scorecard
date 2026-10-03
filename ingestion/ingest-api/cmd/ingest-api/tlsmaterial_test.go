package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issueCAAndLeaf builds a CA and a server certificate signed by it, as PEM.
func issueCAAndLeaf(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sac-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ingest.eu.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"ingest.eu.example.com"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func clearTLSEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvTLSCertPEM, EnvTLSKeyPEM, EnvTLSClientCAPEM} {
		t.Setenv(name, "")
	}
}

// TestTLSMaterialFromTheEnvironment is the F5 path: the material a deployment injects from Key Vault
// as environment variables, because the Container Apps module has no command/args and no volume mount.
func TestTLSMaterialFromTheEnvironment(t *testing.T) {
	caPEM, certPEM, keyPEM := issueCAAndLeaf(t)
	t.Setenv(EnvTLSCertPEM, string(certPEM))
	t.Setenv(EnvTLSKeyPEM, string(keyPEM))
	t.Setenv(EnvTLSClientCAPEM, string(caPEM))

	m, err := loadTLSMaterial(options{})
	if err != nil {
		t.Fatalf("loadTLSMaterial: %v", err)
	}
	if m == nil {
		t.Fatal("no material loaded from a complete environment set")
	}
	if !strings.Contains(m.source, "environment") {
		t.Errorf("source = %q, want it to name the environment", m.source)
	}
	cfg := m.serverTLSConfig()
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3 (§2.1)", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert (§2.1)", cfg.ClientAuth)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("certificates = %d, want the one from the environment", len(cfg.Certificates))
	}
	if cfg.ClientCAs == nil {
		t.Error("no client CA pool: device certificates could not be verified")
	}
}

// TestTLSMaterialFromFiles covers the laptop form, and the precedence rule: a flag wins.
func TestTLSMaterialFromFiles(t *testing.T) {
	clearTLSEnv(t)
	caPEM, certPEM, keyPEM := issueCAAndLeaf(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	for _, f := range []struct {
		path string
		data []byte
	}{{caPath, caPEM}, {certPath, certPEM}, {keyPath, keyPEM}} {
		if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
	}

	m, err := loadTLSMaterial(options{tlsCert: certPath, tlsKey: keyPath, tlsClientCA: caPath})
	if err != nil {
		t.Fatalf("loadTLSMaterial: %v", err)
	}
	if m == nil || m.source != "files" {
		t.Fatalf("material = %+v, want the file form", m)
	}

	t.Run("a flag wins over the environment", func(t *testing.T) {
		// The other CA in the environment is a different one; the file form must be the one used.
		otherCA, otherCert, otherKey := issueCAAndLeaf(t)
		t.Setenv(EnvTLSCertPEM, string(otherCert))
		t.Setenv(EnvTLSKeyPEM, string(otherKey))
		t.Setenv(EnvTLSClientCAPEM, string(otherCA))
		m, err := loadTLSMaterial(options{tlsCert: certPath, tlsKey: keyPath, tlsClientCA: caPath})
		if err != nil {
			t.Fatalf("loadTLSMaterial: %v", err)
		}
		if m.source != "files" {
			t.Errorf("source = %q, want files: a flag wins over the environment", m.source)
		}
	})
}

// TestTLSMaterialIncompleteIsAnError: a partial set must fail at startup rather than surface as a
// handshake failure on the first device request.
func TestTLSMaterialIncompleteIsAnError(t *testing.T) {
	caPEM, certPEM, _ := issueCAAndLeaf(t)

	t.Run("environment with two of three", func(t *testing.T) {
		t.Setenv(EnvTLSCertPEM, string(certPEM))
		t.Setenv(EnvTLSKeyPEM, "")
		t.Setenv(EnvTLSClientCAPEM, string(caPEM))
		if _, err := loadTLSMaterial(options{}); err == nil {
			t.Fatal("a partial environment set was accepted")
		} else if !strings.Contains(err.Error(), EnvTLSKeyPEM) {
			t.Errorf("error = %v, want it to name the missing variable", err)
		}
	})

	t.Run("flags with two of three", func(t *testing.T) {
		clearTLSEnv(t)
		if _, err := loadTLSMaterial(options{tlsCert: "cert.pem", tlsKey: "key.pem"}); err == nil {
			t.Fatal("a partial flag set was accepted")
		}
	})

	t.Run("nothing configured is not an error here", func(t *testing.T) {
		clearTLSEnv(t)
		m, err := loadTLSMaterial(options{})
		if err != nil {
			t.Fatalf("loadTLSMaterial: %v", err)
		}
		if m != nil {
			t.Error("no material was configured, so none should be returned; the caller refuses to serve")
		}
	})
}
