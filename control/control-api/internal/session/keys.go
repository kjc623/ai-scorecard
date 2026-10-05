package session

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/jose"
)

// KeySet is the session-signing key and the keys verifiers may still meet. It is deliberately not the
// device token key (SAC_DPOP_TOKEN_KEY_PEM): a product token and a device token are different
// authorities, and one key signing both would let a token of one class be presented as the other.
//
// Rotation is a file edit: the first private key in the file signs; every other key in it, private
// or public-only, is published in the JWKS so a token minted under the previous key verifies until it
// expires (at most ten minutes). Publishing the next key before it signs is the same edit in the
// other order.
type KeySet struct {
	signer    *ecdsa.PrivateKey
	signerKID string
	keys      []verificationKey
}

type verificationKey struct {
	kid string
	pub *ecdsa.PublicKey
	jwk protocol.JWK
}

// LoadKeyFile reads SAC_SESSION_SIGNING_KEY_FILE.
func LoadKeyFile(path string) (*KeySet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session: read signing key file: %w", err)
	}
	return ParseKeys(b)
}

// ParseKeys reads one or more PEM blocks: P-256 private keys ("EC PRIVATE KEY" or "PRIVATE KEY") and
// public keys ("PUBLIC KEY"). The first private key signs.
func ParseKeys(pemBytes []byte) (*KeySet, error) {
	var signer *ecdsa.PrivateKey
	var pubs []*ecdsa.PublicKey
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "EC PRIVATE KEY", "PRIVATE KEY":
			key, err := jose.ParseECPrivateKeyPEM(pem.EncodeToMemory(block))
			if err != nil {
				return nil, fmt.Errorf("session: signing key: %w", err)
			}
			if signer == nil {
				signer = key
			}
			pubs = append(pubs, &key.PublicKey)
		case "PUBLIC KEY":
			parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("session: published key: %w", err)
			}
			pub, ok := parsed.(*ecdsa.PublicKey)
			if !ok || pub.Curve != elliptic.P256() {
				return nil, errors.New("session: a published key is not a P-256 ECDSA key")
			}
			pubs = append(pubs, pub)
		case "EC PARAMETERS":
			// `openssl ecparam -genkey` writes the curve name ahead of the key; the key names it too.
		default:
			return nil, fmt.Errorf("session: unexpected PEM block %q in the signing key file", block.Type)
		}
	}
	if signer == nil {
		return nil, errors.New("session: the signing key file holds no P-256 private key")
	}
	return NewKeySet(signer, pubs...)
}

// NewKeySet builds a set from a signing key and any extra published keys.
func NewKeySet(signer *ecdsa.PrivateKey, published ...*ecdsa.PublicKey) (*KeySet, error) {
	if signer == nil {
		return nil, errors.New("session: a signing key is required")
	}
	ks := &KeySet{signer: signer}
	seen := map[string]bool{}
	for _, pub := range append([]*ecdsa.PublicKey{&signer.PublicKey}, published...) {
		jwk, err := jose.JWKFromPublic(pub)
		if err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
		kid, err := jwk.Thumbprint()
		if err != nil {
			return nil, fmt.Errorf("session: key thumbprint: %w", err)
		}
		if seen[kid] {
			continue
		}
		seen[kid] = true
		jwk.Kid, jwk.Alg, jwk.Use = kid, jose.AlgES256, "sig"
		ks.keys = append(ks.keys, verificationKey{kid: kid, pub: pub, jwk: jwk})
	}
	ks.signerKID = ks.keys[0].kid
	return ks, nil
}

// SigningKeyID is the kid of the key that signs.
func (k *KeySet) SigningKeyID() string { return k.signerKID }

// JWKS is the public document verifiers fetch.
func (k *KeySet) JWKS() map[string]any {
	keys := make([]protocol.JWK, 0, len(k.keys))
	for _, v := range k.keys {
		keys = append(keys, v.jwk)
	}
	return map[string]any{"keys": keys}
}

func (k *KeySet) publicKey(kid string) *ecdsa.PublicKey {
	for _, v := range k.keys {
		if v.kid == kid {
			return v.pub
		}
	}
	return nil
}
