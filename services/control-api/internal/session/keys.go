package session

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/go-jose/go-jose/v4"
)

// KeySet is the session-signing key and the keys verifiers may still meet.
//
// Rotation is a file edit: the first private key in the file signs; every other key in it, private
// or public-only, is published in the JWKS so a token minted under the previous key verifies until it
// expires. Publishing the next key before it signs is the same edit in the other order.
type KeySet struct {
	signer    *ecdsa.PrivateKey
	signerKID string
	keys      []jose.JSONWebKey
}

// LoadKeyFile reads the session signing key file (SAC_SESSION_SIGNING_KEY_FILE).
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
			key, err := parsePrivateKey(block)
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

func parsePrivateKey(block *pem.Block) (*ecdsa.PrivateKey, error) {
	var key *ecdsa.PrivateKey
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		k, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an ECDSA key")
		}
		key = k
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("curve is %s, want P-256", key.Curve.Params().Name)
	}
	return key, nil
}

// NewKeySet builds a set from a signing key and any extra published keys. Each key's id is its
// RFC 7638 thumbprint, base64url without padding.
func NewKeySet(signer *ecdsa.PrivateKey, published ...*ecdsa.PublicKey) (*KeySet, error) {
	if signer == nil {
		return nil, errors.New("session: a signing key is required")
	}
	ks := &KeySet{signer: signer}
	seen := map[string]bool{}
	for _, pub := range append([]*ecdsa.PublicKey{&signer.PublicKey}, published...) {
		if pub.Curve != elliptic.P256() {
			return nil, errors.New("session: only P-256 keys are supported")
		}
		jwk := jose.JSONWebKey{Key: pub, Algorithm: string(jose.ES256), Use: "sig"}
		sum, err := jwk.Thumbprint(crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("session: key thumbprint: %w", err)
		}
		kid := base64.RawURLEncoding.EncodeToString(sum)
		if seen[kid] {
			continue
		}
		seen[kid] = true
		jwk.KeyID = kid
		ks.keys = append(ks.keys, jwk)
	}
	ks.signerKID = ks.keys[0].KeyID
	return ks, nil
}

// SigningKeyID is the kid of the key that signs.
func (k *KeySet) SigningKeyID() string { return k.signerKID }

// JWKS is the public document verifiers fetch.
func (k *KeySet) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), k.keys...)}
}

func (k *KeySet) publicKey(kid string) *ecdsa.PublicKey {
	for _, v := range k.keys {
		if v.KeyID == kid {
			return v.Key.(*ecdsa.PublicKey)
		}
	}
	return nil
}

func (k *KeySet) signingKey() jose.SigningKey {
	return jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k.signer, KeyID: k.signerKID}}
}
