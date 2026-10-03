package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

// loadTokenPublicKey parses the deployment's DPoP access-token signing public key. The DPoP mode
// verifies ES256 tokens, so anything that is not an ECDSA key is refused at startup rather than
// failing every request. A "PUBLIC KEY" (PKIX) block and a CERTIFICATE block are both accepted: a
// deployment may publish the signing key directly or hand over the certificate that carries it, and
// both name the same key.
//
// The value may be PEM text (the form a deployment injects from Key Vault) or a path to a PEM file
// (the form a laptop or a lab uses). The two are the same material, and accepting both is the same
// rule control-api's --dpop-token-key-pem already follows: a flag wins over the environment, and the
// environment holds either the text or the path, never a second variable for the same key.
func loadTokenPublicKey(pemText string) (crypto.PublicKey, error) {
	text := strings.TrimSpace(pemText)
	if text == "" {
		return nil, nil
	}
	if !strings.Contains(text, "-----BEGIN") {
		b, err := os.ReadFile(text)
		if err != nil {
			return nil, fmt.Errorf("%s: value is neither PEM text nor a readable file: %w", EnvDPoPTokenPublicPEM, err)
		}
		text = string(b)
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("DPoP token public key (%s) is not PEM", EnvDPoPTokenPublicPEM)
	}
	var key crypto.PublicKey
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse the DPoP token certificate: %w", err)
		}
		key = cert.PublicKey
	default:
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse the DPoP token public key: %w", err)
		}
		key = pub
	}
	if _, ok := key.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("DPoP token public key is %T, want an ECDSA key for ES256", key)
	}
	return key, nil
}
