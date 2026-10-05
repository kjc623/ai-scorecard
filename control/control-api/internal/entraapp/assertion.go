package entraapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // x5t is defined as the SHA-1 thumbprint: it names the certificate, it protects nothing
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"
)

// certCredential signs RFC 7523 client assertions with a certificate's key. Entra matches the
// assertion to the uploaded certificate by the x5t header, so the certificate (public half) is what
// the owner uploads to the app registration and the key never leaves this process.
type certCredential struct {
	clientID string
	key      *rsa.PrivateKey
	x5t      string
	now      func() time.Time
}

// loadCertCredential reads a PEM file holding the certificate and its RSA private key (in either
// order). Entra's certificate credentials are RSA, so an EC key is refused here rather than at the
// first sign-in.
func loadCertCredential(path, clientID string, now func() time.Time) (*certCredential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("entraapp: read %s: %w", EnvCertFile, err)
	}
	return parseCertCredential(raw, clientID, now)
}

func parseCertCredential(raw []byte, clientID string, now func() time.Time) (*certCredential, error) {
	var cert *x509.Certificate
	var key *rsa.PrivateKey
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			if cert == nil {
				c, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, fmt.Errorf("entraapp: certificate: %w", err)
				}
				cert = c
			}
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("entraapp: private key: %w", err)
			}
			key = k
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("entraapp: private key: %w", err)
			}
			k, ok := parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, errors.New("entraapp: the certificate key must be RSA (Entra certificate credentials are RSA)")
			}
			key = k
		}
	}
	if cert == nil || key == nil {
		return nil, fmt.Errorf("entraapp: %s must hold a CERTIFICATE and its RSA PRIVATE KEY", EnvCertFile)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.Cmp(key.N) != 0 || pub.E != key.E {
		return nil, errors.New("entraapp: the private key does not belong to the certificate")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("entraapp: the certificate key is shorter than 2048 bits")
	}
	sum := sha1.Sum(cert.Raw)
	return &certCredential{
		clientID: clientID, key: key, now: now,
		x5t: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

func (c *certCredential) kind() string { return "certificate" }

func (c *certCredential) params(_ context.Context, aud string) (url.Values, error) {
	assertion, err := c.assertion(aud)
	if err != nil {
		return nil, err
	}
	return url.Values{"client_assertion_type": {AssertionType}, "client_assertion": {assertion}}, nil
}

// assertion is the client-assertion JWT: RS256, x5t, aud = the token endpoint, iss = sub = the client
// id, a fresh jti, and a five-minute life (Entra refuses one valid for longer than ten).
func (c *certCredential) assertion(aud string) (string, error) {
	now := c.now().UTC()
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("entraapp: no entropy: %w", err)
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT", "x5t": c.x5t}
	claims := map[string]any{
		"aud": aud, "iss": c.clientID, "sub": c.clientID, "jti": hex.EncodeToString(jti[:]),
		"nbf": now.Unix(), "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("entraapp: sign client assertion: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
