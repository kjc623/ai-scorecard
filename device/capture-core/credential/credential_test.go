package credential

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// testCredential is a credential with a real self-signed leaf, so KeyPair can be exercised.
func testCredential(t *testing.T) *Credential {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device-1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &Credential{
		DeviceID:             "device-1",
		TenantID:             "tenant-1",
		Region:               "eu",
		HardwareIdentityHash: "sha256:abc",
		PrivateKey:           string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CertPEM:              string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		NotAfter:             notAfter,
	}
}

func TestStoreRoundTripsAndProtectsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sealed")
	store, err := Open(path, testKey(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := testCredential(t)
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DeviceID != c.DeviceID || got.TenantID != c.TenantID || got.CertPEM != c.CertPEM || !got.NotAfter.Equal(c.NotAfter) {
		t.Fatalf("round trip changed the credential: %+v", got)
	}
	if _, err := got.KeyPair(); err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if err := state.CheckFile(path); err != nil {
		t.Fatalf("the credential file is not protected: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("device-1")) {
		t.Fatal("the credential file holds plaintext")
	}
}

func TestLoadWithoutAFileIsNotEnrolled(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "nope"), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing file = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadRefusesTamperingAndTheWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sealed")
	store, err := Open(path, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testCredential(t)); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Load(); err == nil {
		t.Fatal("a credential opened under another key")
	}
	raw, _ := os.ReadFile(path)
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("a tampered credential was accepted")
	}
}

func TestValidateRefusesAnUnusableCredential(t *testing.T) {
	for name, mutate := range map[string]func(*Credential){
		"no device":  func(c *Credential) { c.DeviceID = "" },
		"no tenant":  func(c *Credential) { c.TenantID = "" },
		"no key":     func(c *Credential) { c.PrivateKey = "" },
		"no leaf":    func(c *Credential) { c.CertPEM = "" },
		"bad leaf":   func(c *Credential) { c.CertPEM = "not a certificate" },
		"wrong pair": func(c *Credential) { c.PrivateKey = testCredential(t).PrivateKey },
	} {
		c := testCredential(t)
		mutate(c)
		if c.Validate() == nil {
			if _, err := c.KeyPair(); err == nil {
				t.Errorf("%s: an unusable credential validated and built a key pair", name)
			}
		}
	}
}

func TestExpired(t *testing.T) {
	c := testCredential(t)
	if c.Expired(c.NotAfter.Add(-time.Second)) || !c.Expired(c.NotAfter) {
		t.Fatal("Expired disagrees with NotAfter")
	}
	c.NotAfter = time.Time{}
	if c.Expired(time.Now()) {
		t.Fatal("a zero NotAfter was treated as expired")
	}
}
