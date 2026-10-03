// Package dpop is the device side of the compact-JWS contract (RFC 7515 ES256, RFC 9449
// DPoP, RFC 7638 thumbprint) used to authenticate the device to the cloud without ever
// exporting its private key. It is implemented with the standard library because this is the
// device's own code: the service's JOSE handling is not imported, and a real endpoint agent
// carries an equivalent.
//
// It is the production equivalent of the development client in localdev/authlab/jose.go, lifted
// out of that lab so capture-core can use it without importing a development package.
package dpop

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignES256 builds a compact JWS over header and claims, signed with an ECDSA P-256 key. The
// JOSE signature is the fixed-width R||S encoding, not the ASN.1 form x509 uses.
func SignES256(header, claims any, key *ecdsa.PrivateKey) (string, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("dpop: marshal header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("dpop: marshal claims: %w", err)
	}
	signingInput := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("dpop: sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64(sig), nil
}

// JWKFromPublic renders an EC P-256 public key as the RFC 7517 subset the transport uses. The
// private half never leaves the client.
func JWKFromPublic(pub *ecdsa.PublicKey) protocol.JWK {
	return protocol.JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   b64(pub.X.FillBytes(make([]byte, 32))),
		Y:   b64(pub.Y.FillBytes(make([]byte, 32))),
	}
}

// DPoPProof builds an RFC 9449 proof bound to one method and URL. ath is empty at the token and
// enrolment endpoints (there is no access token yet); a resource request carries
// base64url(sha256(token)).
func DPoPProof(key *ecdsa.PrivateKey, htm, htu, ath string) (string, error) {
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": JWKFromPublic(&key.PublicKey),
	}
	claims := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": time.Now().Unix(),
	}
	jti, err := NewID()
	if err != nil {
		return "", err
	}
	claims["jti"] = jti
	if ath != "" {
		claims["ath"] = ath
	}
	return SignES256(header, claims, key)
}

// AthOf is base64url(sha256(ascii(accessToken))), the RFC 9449 ath value.
func AthOf(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return b64(sum[:])
}

// TokenAssertion is the RFC 7523 assertion the token endpoint verifies: a compact ES256 JWS whose
// sub is the device id and whose tenant_id names the tenant. Only those two claims are read by the
// service, so the rest are conventional.
func TokenAssertion(key *ecdsa.PrivateKey, deviceID, tenantID string) (string, error) {
	now := time.Now().UTC()
	header := map[string]any{"alg": "ES256", "typ": "JWT"}
	jti, err := NewID()
	if err != nil {
		return "", err
	}
	claims := map[string]any{
		"iss":       "capture-core",
		"sub":       deviceID,
		"tenant_id": tenantID,
		"iat":       now.Unix(),
		"exp":       now.Add(5 * time.Minute).Unix(),
		"jti":       jti,
	}
	return SignES256(header, claims, key)
}

// NewID mints a fresh random UUID (version 4, variant 1) as a string. It is used for the DPoP jti
// and the batch_id; a failure to obtain entropy is an error rather than a panic, because this code
// runs in a long-lived service rather than a one-shot lab client.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dpop: entropy: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
