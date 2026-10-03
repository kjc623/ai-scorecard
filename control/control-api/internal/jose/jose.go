// Package jose is the small, stdlib-only JOSE subset the device transport needs: compact JWS with
// ES256 (RFC 7515), the EC public JWK (RFC 7517) whose RFC 7638 thumbprint is the DPoP binding, and
// the header/claims helpers the RFC 9449 proof and the access token share.
//
// It is deliberately not a general JOSE library. It supports exactly the algorithm the contract
// names -- ES256 -- and refuses anything else, because an "alg" that can be selected by the caller
// is the classic JWT confusion. The access token header is `{"alg":"ES256","typ":"at+jwt","kid":...}`
// and the DPoP proof header is `{"typ":"dpop+jwt","alg":"ES256","jwk":{...}}`.
package jose

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// AlgES256 is the only signature algorithm this package will verify or produce.
const AlgES256 = "ES256"

// HeaderDPoPType and HeaderAccessTokenType are the `typ` values of the two JWT classes in this
// transport. They are pinned so a token cannot be replayed as a proof or the reverse.
const (
	TypDPoP        = "dpop+jwt"
	TypAccessToken = "at+jwt"
)

// Errors the callers distinguish. Everything here is a malformed or unverifiable token, never an
// infrastructure failure.
var (
	ErrMalformed = errors.New("jose: malformed compact JWS")
	ErrAlgorithm = errors.New("jose: unsupported JWS algorithm")
	ErrSignature = errors.New("jose: signature does not verify")
	ErrJWK       = errors.New("jose: unusable JWK")
)

// Claims is the decoded payload. Raw values are kept so a caller reads only the members it names,
// and a missing member is distinguishable from a present one.
type Claims map[string]json.RawMessage

// String returns the named string claim, or "" and false when it is absent or not a string.
func (c Claims) String(name string) (string, bool) {
	raw, ok := c[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// Int64 returns the named numeric claim. JSON numbers arrive as json.Number here.
func (c Claims) Int64(name string) (int64, bool) {
	raw, ok := c[name]
	if !ok {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

// Has reports whether the claim is present.
func (c Claims) Has(name string) bool { _, ok := c[name]; return ok }

// JWS is a parsed compact JWS. Its header and claims are available before verification so a caller
// can select a key; Verify must still be called before acting on any claim.
type JWS struct {
	Header    map[string]json.RawMessage
	Claims    Claims
	signingIn string
	signature []byte
}

// HeaderString reads a string member of the protected header.
func (j *JWS) HeaderString(name string) (string, bool) {
	raw, ok := j.Header[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// HeaderJWK decodes the `jwk` member of the protected header, which the DPoP proof carries.
func (j *JWS) HeaderJWK() (protocol.JWK, error) {
	raw, ok := j.Header["jwk"]
	if !ok {
		return protocol.JWK{}, fmt.Errorf("%w: header carries no jwk", ErrMalformed)
	}
	var k protocol.JWK
	if err := json.Unmarshal(raw, &k); err != nil {
		return protocol.JWK{}, fmt.Errorf("%w: decode jwk: %v", ErrMalformed, err)
	}
	return k, nil
}

// Parse splits and decodes a compact JWS without verifying it. The signature is retained for
// Verify. The protected header must be the ES256 algorithm.
func Parse(compact string) (*JWS, error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: %d segments, want 3", ErrMalformed, len(parts))
	}
	headerBytes, err := unb64(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrMalformed, err)
	}
	payloadBytes, err := unb64(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	sig, err := unb64(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: header is not JSON: %v", ErrMalformed, err)
	}
	var alg string
	if raw, ok := header["alg"]; !ok || json.Unmarshal(raw, &alg) != nil {
		return nil, fmt.Errorf("%w: header has no alg", ErrMalformed)
	}
	if alg != AlgES256 {
		return nil, fmt.Errorf("%w: alg %q, want %q", ErrAlgorithm, alg, AlgES256)
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: payload is not a JSON object: %v", ErrMalformed, err)
	}
	return &JWS{
		Header:    header,
		Claims:    claims,
		signingIn: parts[0] + "." + parts[1],
		signature: sig,
	}, nil
}

// Verify checks the signature against pub. It never trusts the header's own key; the caller supplies
// the key that the credential registered.
func (j *JWS) Verify(pub *ecdsa.PublicKey) error {
	if pub == nil {
		return fmt.Errorf("%w: no public key", ErrSignature)
	}
	if len(j.signature) != 64 {
		return fmt.Errorf("%w: ES256 signature is %d bytes, want 64", ErrMalformed, len(j.signature))
	}
	r := new(big.Int).SetBytes(j.signature[:32])
	s := new(big.Int).SetBytes(j.signature[32:])
	sum := sha256.Sum256([]byte(j.signingIn))
	if !ecdsa.Verify(pub, sum[:], r, s) {
		return ErrSignature
	}
	return nil
}

// VerifyES256 parses and verifies in one step against an explicit key.
func VerifyES256(compact string, pub *ecdsa.PublicKey) (*JWS, error) {
	j, err := Parse(compact)
	if err != nil {
		return nil, err
	}
	if err := j.Verify(pub); err != nil {
		return nil, err
	}
	return j, nil
}

// SignES256 produces a compact JWS. header is marshalled as the protected header and claims as the
// payload; both are compact JSON with no whitespace, as RFC 7515 requires.
func SignES256(header map[string]any, claims any, key *ecdsa.PrivateKey) (string, error) {
	if key == nil {
		return "", errors.New("jose: no signing key")
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("jose: marshal header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("jose: marshal claims: %w", err)
	}
	signingIn := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signingIn))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("jose: sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingIn + "." + b64(sig), nil
}

// JWKFromPublic renders an EC P-256 public key as the RFC 7517 subset this transport uses.
func JWKFromPublic(pub *ecdsa.PublicKey) (protocol.JWK, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return protocol.JWK{}, fmt.Errorf("%w: only P-256 is supported", ErrJWK)
	}
	return protocol.JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   b64(pub.X.FillBytes(make([]byte, 32))),
		Y:   b64(pub.Y.FillBytes(make([]byte, 32))),
	}, nil
}

// PublicFromJWK reconstructs the EC public key from a JWK. It requires P-256 and a point on the
// curve; an off-curve point is refused rather than handed to the verifier.
func PublicFromJWK(k protocol.JWK) (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" {
		return nil, fmt.Errorf("%w: kty %q, want EC", ErrJWK, k.Kty)
	}
	if k.Crv != "P-256" {
		return nil, fmt.Errorf("%w: crv %q, want P-256", ErrJWK, k.Crv)
	}
	xb, err := unb64(k.X)
	if err != nil {
		return nil, fmt.Errorf("%w: x: %v", ErrJWK, err)
	}
	yb, err := unb64(k.Y)
	if err != nil {
		return nil, fmt.Errorf("%w: y: %v", ErrJWK, err)
	}
	x := new(big.Int).SetBytes(xb)
	y := new(big.Int).SetBytes(yb)
	if !elliptic.P256().IsOnCurve(x, y) {
		return nil, fmt.Errorf("%w: point is not on P-256", ErrJWK)
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

// ParseECPrivateKeyPEM accepts a SEC1 ("EC PRIVATE KEY") or PKCS#8 ("PRIVATE KEY") P-256 key.
func ParseECPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("jose: no PEM block in the signing key")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("jose: signing key curve is %s, want P-256", key.Curve.Params().Name)
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("jose: parse signing key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("jose: signing key is not a P-256 ECDSA key")
	}
	return key, nil
}

// Thumbprint is the RFC 7638 thumbprint of a public JWK, delegated to the protocol package so this
// service and the device compute the same value from the same code.
func Thumbprint(k protocol.JWK) (string, error) { return k.Thumbprint() }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
