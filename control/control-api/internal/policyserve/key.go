package policyserve

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultKeyID is the key id a bundle names when none is configured; it is the agent's default.
const DefaultKeyID = "policy-key-1"

// LoadSigningKey reads the policy signing key file: a PKCS#8 PEM Ed25519 private key
// (`openssl genpkey -algorithm ed25519`), or hex of the 64-byte private key or its 32-byte seed. The
// key is never logged; only its public half (PublicKeyHex) may be.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policyserve: read signing key: %w", err)
	}
	return ParseSigningKey(raw)
}

// ParseSigningKey is LoadSigningKey's parser, for a key held in memory.
func ParseSigningKey(raw []byte) (ed25519.PrivateKey, error) {
	if bytes.Contains(raw, []byte("-----BEGIN")) {
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, errors.New("policyserve: signing key PEM is not a PKCS#8 PRIVATE KEY block")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("policyserve: signing key is not PKCS#8: %w", err)
		}
		priv, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("policyserve: signing key is %T, want an Ed25519 key", k)
		}
		return priv, nil
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, errors.New("policyserve: signing key is neither PEM nor hex")
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		priv := ed25519.PrivateKey(b)
		// A 64-byte key whose public half does not follow from its seed would sign bundles no
		// pinned public key verifies; refuse it at boot rather than at the first device.
		if !bytes.Equal(ed25519.NewKeyFromSeed(priv.Seed()), priv) {
			return nil, errors.New("policyserve: signing key's public half does not match its seed")
		}
		return priv, nil
	default:
		return nil, fmt.Errorf("policyserve: hex signing key is %d bytes, want 32 or 64", len(b))
	}
}

// PublicKeyHex is the public half as the device pins it (capture-core --policy-key, SAC_POLICY_KEY).
func PublicKeyHex(priv ed25519.PrivateKey) string {
	pub, _ := priv.Public().(ed25519.PublicKey)
	return hex.EncodeToString(pub)
}
