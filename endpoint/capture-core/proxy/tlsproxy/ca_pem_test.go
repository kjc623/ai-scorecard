package tlsproxy

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// A CA that survives a PEM round-trip (mint -> PEM/KeyPEM -> NewCAFromPEM) must be the same CA:
// the same fingerprint, and a leaf minted by the reloaded CA verifies against the reloaded pool.
func TestCA_PEMRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", nil, now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	certPEM := ca.PEM()
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}

	reloaded, err := NewCAFromPEM(certPEM, keyPEM, now)
	if err != nil {
		t.Fatalf("NewCAFromPEM: %v", err)
	}
	if reloaded.Info().Fingerprint != ca.Info().Fingerprint {
		t.Fatalf("reloaded fingerprint = %q, want %q", reloaded.Info().Fingerprint, ca.Info().Fingerprint)
	}

	leaf, err := reloaded.Leaf("api.example.invalid", now)
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:       reloaded.Pool(),
		DNSName:     "api.example.invalid",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: now,
	}); err != nil {
		t.Fatalf("minted leaf does not verify against the loaded pool: %v", err)
	}
}

// A SEC1 ("EC PRIVATE KEY") key is accepted alongside PKCS#8.
func TestNewCAFromPEM_AcceptsSEC1Key(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", nil, now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	sec1, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})
	if _, err := NewCAFromPEM(ca.PEM(), sec1PEM, now); err != nil {
		t.Fatalf("SEC1 key was refused: %v", err)
	}
}

// A mismatched certificate/key pair, a non-EC key and a multi-block PEM are all refused, because a
// CA the proxy cannot actually use must not be accepted silently.
func TestNewCAFromPEM_RejectsUnusableInputs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", nil, now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM := ca.PEM()
	keyPEM, _ := ca.KeyPEM()

	other, err := NewCA("device-2", nil, now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	otherKeyPEM, _ := other.KeyPEM()

	if _, err := NewCAFromPEM(certPEM, otherKeyPEM, now); err == nil {
		t.Fatal("a mismatched cert/key pair was accepted")
	}

	// A PKCS#8 block that does not hold an EC key must fail to parse as EC.
	garbage := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x30, 0x03, 0x02, 0x01, 0x01}})
	if _, err := NewCAFromPEM(certPEM, garbage, now); err == nil {
		t.Fatal("a non-EC PKCS#8 key was accepted")
	}

	// Two certificates are refused: exactly one is required.
	if _, err := NewCAFromPEM(append(append([]byte(nil), certPEM...), certPEM...), keyPEM, now); err == nil {
		t.Fatal("two concatenated certificates were accepted")
	}
}

// Provider.Start must treat a partially configured PEM CA (exactly one of the pair) as an error,
// never a silent generated fallback; a full pair is loaded and becomes the provider's CA.
func TestProvider_CAPEMConfig(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", nil, now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM := ca.PEM()
	keyPEM, _ := ca.KeyPEM()

	t.Run("cert only is refused", func(t *testing.T) {
		p := New(Config{CACertPEM: certPEM})
		if err := p.Start(context.Background()); err == nil {
			t.Fatal("Start accepted a cert-only CA config, which would silently fall back to a generated CA")
		}
	})

	t.Run("key only is refused", func(t *testing.T) {
		p := New(Config{CAKeyPEM: keyPEM})
		if err := p.Start(context.Background()); err == nil {
			t.Fatal("Start accepted a key-only CA config, which would silently fall back to a generated CA")
		}
	})

	t.Run("full pair is loaded", func(t *testing.T) {
		p := New(Config{CACertPEM: certPEM, CAKeyPEM: keyPEM, Log: testLogger{t}})
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start with a full PEM pair: %v", err)
		}
		defer p.Stop(context.Background())
		got := p.CA()
		if got == nil {
			t.Fatal("no CA was loaded from the PEM pair")
		}
		if got.Info().Fingerprint != ca.Info().Fingerprint {
			t.Fatalf("loaded fingerprint = %q, want %q", got.Info().Fingerprint, ca.Info().Fingerprint)
		}
	})
}
