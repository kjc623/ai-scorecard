package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// The device side of the compact-JWS contract (RFC 7515 ES256, RFC 9449 DPoP, RFC 7638 thumbprint).
// It is implemented here with the standard library because this is the device's own code: the
// service's jose package is not imported, and a real endpoint agent would carry an equivalent.

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signES256 builds a compact JWS over header and claims, signed with an ECDSA P-256 key. The JOSE
// signature is the fixed-width R||S encoding, not the ASN.1 form x509 uses.
func signES256(header, claims any, key *ecdsa.PrivateKey) (string, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signingInput := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64(sig), nil
}

// jwkFromPublic renders an EC P-256 public key as the RFC 7517 subset the transport uses. The
// private half never leaves the client.
func jwkFromPublic(pub *ecdsa.PublicKey) protocolJWK {
	return protocolJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   b64(pub.X.FillBytes(make([]byte, 32))),
		Y:   b64(pub.Y.FillBytes(make([]byte, 32))),
	}
}

// protocolJWK mirrors endpoint/protocol.JWK's JSON wire shape. It exists only so this file does not
// have to construct a protocol.JWK by hand for the proof header; the client's request bodies still
// use the real protocol types.
type protocolJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// dpopProof builds an RFC 9449 proof bound to one method and URL. ath is empty at the token
// endpoint (there is no access token yet); a resource request carries base64url(sha256(token)).
func dpopProof(key *ecdsa.PrivateKey, htm, htu, ath string) (string, error) {
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": jwkFromPublic(&key.PublicKey),
	}
	claims := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": newUUID(),
	}
	if ath != "" {
		claims["ath"] = ath
	}
	return signES256(header, claims, key)
}

// athOf is base64url(sha256(ascii(accessToken))), the RFC 9449 ath value.
func athOf(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return b64(sum[:])
}

// tokenAssertion is the RFC 7523 assertion the token endpoint verifies: a compact ES256 JWS whose
// sub is the device id and whose tenant_id names the tenant. Only those two claims are read by the
// service, so the rest are conventional.
func tokenAssertion(key *ecdsa.PrivateKey, deviceID, tenantID string) (string, error) {
	now := time.Now().UTC()
	header := map[string]any{"alg": "ES256", "typ": "JWT"}
	claims := map[string]any{
		"iss":       "authlab",
		"sub":       deviceID,
		"tenant_id": tenantID,
		"iat":       now.Unix(),
		"exp":       now.Add(5 * time.Minute).Unix(),
		"jti":       newUUID(),
	}
	return signES256(header, claims, key)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
