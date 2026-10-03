package credential

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

func testKeyProvider(t *testing.T) capturespool.KeyProvider {
	t.Helper()
	k, err := capturespool.NewRandomMemoryKeyProvider()
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return k
}

func testECPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func testCredential(t *testing.T, mode protocol.AuthMode) *Credential {
	t.Helper()
	return &Credential{
		Mode:                 mode,
		DeviceID:             "device-1",
		TenantID:             "tenant-1",
		Region:               "eu",
		HardwareIdentityHash: "sha256:abc",
		PrivateKey:           testECPrivateKeyPEM(t),
	}
}

func TestStoreRoundTrips(t *testing.T) {
	for _, mode := range []protocol.AuthMode{protocol.AuthModeX509, protocol.AuthModeDPoP} {
		t.Run(string(mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential.sealed")
			store, err := Open(path, testKeyProvider(t))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			c := &Credential{
				Mode:                 mode,
				DeviceID:             "device-1",
				TenantID:             "tenant-1",
				Region:               "eu",
				HardwareIdentityHash: "sha256:abc",
				PrivateKey:           testECPrivateKeyPEM(t),
			}
			if mode == protocol.AuthModeX509 {
				c.CertPEM = "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"
			} else {
				c.JWK = &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}
			}
			if err := store.Save(c); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := store.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Mode != mode || got.DeviceID != "device-1" || got.TenantID != "tenant-1" {
				t.Fatalf("round trip changed identity: %+v", got)
			}
			if got.PrivateKey != c.PrivateKey {
				t.Fatal("round trip changed the private key")
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if _, err := got.ECPrivateKey(); err != nil {
				t.Fatalf("ECPrivateKey: %v", err)
			}
		})
	}
}

func TestStoreLoadMissingIsNotExist(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "nope"), testKeyProvider(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load of a missing file = %v, want os.ErrNotExist", err)
	}
}

func TestStoreRefusesTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sealed")
	store, err := Open(path, testKeyProvider(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := testCredential(t, protocol.AuthModeDPoP)
	c.JWK = &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	raw[len(raw)/2] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("a tampered credential was accepted")
	}
}

func TestStoreRefusesWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sealed")
	store, err := Open(path, testKeyProvider(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := testCredential(t, protocol.AuthModeDPoP)
	c.JWK = &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	other, err := Open(path, testKeyProvider(t))
	if err != nil {
		t.Fatalf("Open other: %v", err)
	}
	if _, err := other.Load(); err == nil {
		t.Fatal("a credential read under the wrong key was accepted")
	}
}

func TestSealedReflectsProvider(t *testing.T) {
	kp := testKeyProvider(t)
	store, err := Open(filepath.Join(t.TempDir(), "c"), kp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// MemoryKeyProvider is unsealed by definition.
	if store.Sealed() {
		t.Fatal("Sealed() is true for a MemoryKeyProvider, which is never platform-protected")
	}
}
