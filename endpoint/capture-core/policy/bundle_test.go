package policy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testRootCA mints a throwaway self-signed cert and returns its PEM and lowercase-hex sha256
// fingerprint, so the validation rules can be exercised without importing tlsproxy (which would
// be an import cycle).
func testRootCA(t *testing.T) (pemStr, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1_800_000_000, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), fmt.Sprintf("%x", sum[:])
}

// §13.2's "schema valid?" step now also covers the carried device CA and the CLI shim: a bundle
// naming a root the device cannot check, or a shim target it cannot parse, is refused rather
// than partly applied.
func TestBundle_ValidateRootCA(t *testing.T) {
	caPEM, fp := testRootCA(t)

	// Accept: a PEM with no fingerprint (the fingerprint is optional).
	b := testBundle("42")
	b.Interception.RootCAPEM = caPEM
	if err := b.Validate(); err != nil {
		t.Fatalf("a single well-formed root_ca_pem was rejected: %v", err)
	}

	// Accept: a PEM with a matching fingerprint.
	b.Interception.RootCAFingerprint = fp
	if err := b.Validate(); err != nil {
		t.Fatalf("a matching root_ca_fingerprint was rejected: %v", err)
	}

	// Reject: a fingerprint that does not match the DER.
	mismatch := testBundle("42")
	mismatch.Interception.RootCAPEM = caPEM
	mismatch.Interception.RootCAFingerprint = strings.Repeat("0", 64)
	if err := mismatch.Validate(); err == nil {
		t.Fatal("a fingerprint that does not match the DER was accepted")
	}

	// Reject: a fingerprint with no PEM to check it against.
	noPEM := testBundle("42")
	noPEM.Interception.RootCAFingerprint = fp
	if err := noPEM.Validate(); err == nil {
		t.Fatal("a fingerprint without a root_ca_pem was accepted")
	}

	// Reject: not a PEM at all.
	garbage := testBundle("42")
	garbage.Interception.RootCAPEM = "this is not a PEM certificate"
	if err := garbage.Validate(); err == nil {
		t.Fatal("a non-PEM root_ca_pem was accepted")
	}

	// Reject: two certificates where exactly one is required.
	two := testBundle("42")
	two.Interception.RootCAPEM = caPEM + caPEM
	if err := two.Validate(); err == nil {
		t.Fatal("two concatenated certificates were accepted")
	}

	// Reject: a PEM block that is not a certificate.
	notCert := testBundle("42")
	notCert.Interception.RootCAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}))
	if err := notCert.Validate(); err == nil {
		t.Fatal("a non-certificate PEM block was accepted")
	}
}

func TestBundle_ValidateProxyAndCLIShim(t *testing.T) {
	// Accept: a well-formed proxy listen address and shim target with a closed-set runtime list.
	b := testBundle("42")
	b.Interception.ProxyListen = "127.0.0.1:8843"
	b.CLIShim = CLIShimPolicy{Enabled: true, ProxyAddr: "127.0.0.1:8843", Runtimes: []string{"go", "node", "python"}}
	if err := b.Validate(); err != nil {
		t.Fatalf("a well-formed proxy/shim was rejected: %v", err)
	}

	// Reject: a proxy_listen that is not host:port.
	badListen := testBundle("42")
	badListen.Interception.ProxyListen = "not-a-host-port"
	if err := badListen.Validate(); err == nil {
		t.Fatal("a malformed proxy_listen was accepted")
	}

	// Reject: a cli_shim proxy_addr that is not host:port.
	badAddr := testBundle("42")
	badAddr.CLIShim.ProxyAddr = "no-port-here"
	if err := badAddr.Validate(); err == nil {
		t.Fatal("a malformed cli_shim proxy_addr was accepted")
	}

	// Reject: a shim runtime outside the closed set.
	badRuntime := testBundle("42")
	badRuntime.CLIShim.Runtimes = []string{"go", "ruby"}
	if err := badRuntime.Validate(); err == nil {
		t.Fatal("a runtime outside {go,node,python} was accepted")
	}

	// Reject: an empty runtime name (a bare comma).
	emptyRuntime := testBundle("42")
	emptyRuntime.CLIShim.Runtimes = []string{"go", ""}
	if err := emptyRuntime.Validate(); err == nil {
		t.Fatal("an empty runtime name was accepted")
	}
}
