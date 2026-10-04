package tlsproxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// NewCAFromPEM rebuilds a device CA from a certificate and an EC private key in PEM form. It
// exists so an offline generator (cmd/sac-bundle) can mint the per-device root and hand the proxy
// the same CA the device already trusts, instead of the proxy minting a second one at first start.
//
// The key is accepted in PKCS#8 ("PRIVATE KEY") or SEC1 ("EC PRIVATE KEY") form; anything else is
// refused, because a key that is not EC cannot mint the P-256 leaves this package signs. No sealer
// is attached: a loaded key was already in the clear at rest, so there is nothing new to seal, and
// Sealed is deliberately left empty for the caller to detect rather than a silent no-op.
func NewCAFromPEM(certPEM, keyPEM []byte, now time.Time) (*CA, error) {
	cert, der, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA certificate: %w", err)
	}
	key, err := parseECKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA key: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != key.Curve ||
		pub.X.Cmp(key.X) != 0 || pub.Y.Cmp(key.Y) != 0 {
		return nil, errors.New("tlsproxy: CA certificate and key do not match")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{
		key:     key,
		cert:    cert,
		der:     der,
		pool:    pool,
		leaves:  map[string]*tls.Certificate{},
		leafTTL: LeafTTL,
	}, nil
}

// PEM returns the CA certificate as a PEM block. It is the public half and safe to install into
// the trust store.
func (c *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}

// KeyPEM returns the CA private key as a PKCS#8 PEM block. This is an interception capability:
// the caller (an offline generator) writes it to a mode-0600 file, and nothing in this package
// writes it anywhere.
func (c *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: marshalling CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parseCertPEM parses exactly one PEM x509 certificate.
func parseCertPEM(data []byte) (*x509.Certificate, []byte, error) {
	block, err := singlePEMBlock(data)
	if err != nil {
		return nil, nil, err
	}
	if block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("PEM block is %q, not a certificate", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("not a parseable x509 certificate: %w", err)
	}
	return cert, block.Bytes, nil
}

// parseECKeyPEM parses exactly one EC private key in PKCS#8 or SEC1 form.
func parseECKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, err := singlePEMBlock(data)
	if err != nil {
		return nil, err
	}
	switch block.Type {
	case "PRIVATE KEY": // PKCS#8
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PKCS#8 key: %w", err)
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, not an EC key", k)
		}
		return ec, nil
	case "EC PRIVATE KEY": // SEC1
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("PEM block is %q, not an EC private key (PKCS#8 or SEC1)", block.Type)
	}
}

// singlePEMBlock requires exactly one PEM block and no trailing data, because a CA key or
// certificate handed to the proxy must be unambiguous about which one it is.
func singlePEMBlock(data []byte) (*pem.Block, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("multiple PEM blocks or trailing data; exactly one block is required")
	}
	return block, nil
}
