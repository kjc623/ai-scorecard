package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

// compactJWS is the minimal reader the test needs to inspect what SignES256 produced, without
// importing a JOSE library (which would be a new dependency).
type compactJWS struct {
	header, payload, sig string
}

func splitJWS(t *testing.T, s string) compactJWS {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS %q is not three parts", s)
	}
	return compactJWS{header: parts[0], payload: parts[1], sig: parts[2]}
}

func b64Decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestSignES256ProducesACompactJWS(t *testing.T) {
	key := testKey(t)
	out, err := SignES256(map[string]any{"alg": "ES256"}, map[string]any{"sub": "device"}, key)
	if err != nil {
		t.Fatalf("SignES256: %v", err)
	}
	j := splitJWS(t, out)
	if len(j.sig) == 0 {
		t.Fatal("signature is empty")
	}
	var header map[string]any
	if err := json.Unmarshal(b64Decode(t, j.header), &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	if header["alg"] != "ES256" {
		t.Fatalf("header alg = %v, want ES256", header["alg"])
	}
	var claims map[string]any
	if err := json.Unmarshal(b64Decode(t, j.payload), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims["sub"] != "device" {
		t.Fatalf("claims sub = %v, want device", claims["sub"])
	}
}

func TestJWKFromPublicIsStableAndP256(t *testing.T) {
	key := testKey(t)
	jwk := JWKFromPublic(&key.PublicKey)
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		t.Fatalf("jwk = %+v, want EC/P-256", jwk)
	}
	if jwk.X == "" || jwk.Y == "" {
		t.Fatal("jwk has no X or Y coordinate")
	}
	again := JWKFromPublic(&key.PublicKey)
	if jwk != again {
		t.Fatal("JWKFromPublic is not deterministic for one key")
	}
	// The thumbprint must be computable from the public half alone.
	if _, err := jwk.Thumbprint(); err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
}

func TestAthOfIsBase64UrlSha256(t *testing.T) {
	token := "access-token-value"
	got := AthOf(token)
	sum := sha256.Sum256([]byte(token))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("AthOf = %q, want %q", got, want)
	}
}

func TestDPoPProofCarriesRequiredClaims(t *testing.T) {
	key := testKey(t)
	proof, err := DPoPProof(key, "POST", "https://ingest.example.com/v1/events", AthOf("tok"))
	if err != nil {
		t.Fatalf("DPoPProof: %v", err)
	}
	j := splitJWS(t, proof)
	var header map[string]any
	if err := json.Unmarshal(b64Decode(t, j.header), &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	if header["typ"] != "dpop+jwt" || header["alg"] != "ES256" {
		t.Fatalf("header = %+v, want dpop+jwt/ES256", header)
	}
	if header["jwk"] == nil {
		t.Fatal("dpop proof has no jwk in its header")
	}
	var claims map[string]any
	if err := json.Unmarshal(b64Decode(t, j.payload), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims["htm"] != "POST" {
		t.Fatalf("htm = %v, want POST", claims["htm"])
	}
	if claims["htu"] != "https://ingest.example.com/v1/events" {
		t.Fatalf("htu = %v, want the full public URL", claims["htu"])
	}
	if claims["jti"] == nil || claims["jti"] == "" {
		t.Fatal("dpop proof has no jti")
	}
	if claims["ath"] == nil {
		t.Fatal("a resource proof carries no ath")
	}
}

func TestTokenAssertionCarriesSubAndTenant(t *testing.T) {
	key := testKey(t)
	a, err := TokenAssertion(key, "device-1", "tenant-1")
	if err != nil {
		t.Fatalf("TokenAssertion: %v", err)
	}
	j := splitJWS(t, a)
	var claims map[string]any
	if err := json.Unmarshal(b64Decode(t, j.payload), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims["sub"] != "device-1" {
		t.Fatalf("sub = %v, want device-1", claims["sub"])
	}
	if claims["tenant_id"] != "tenant-1" {
		t.Fatalf("tenant_id = %v, want tenant-1", claims["tenant_id"])
	}
	if claims["exp"] == nil {
		t.Fatal("assertion has no exp")
	}
}

func TestNewIDIsUniqueAndFormatted(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("NewID = %q, want 36 chars", id)
		}
		if seen[id] {
			t.Fatalf("NewID returned %q twice", id)
		}
		seen[id] = true
	}
}

func TestNewIDFormatsAsV4(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("NewID = %q, not five dash groups", id)
	}
	// The version nibble is 4 in the third group.
	if len(parts[2]) != 4 || parts[2][0] != '4' {
		t.Fatalf("NewID = %q, third group does not carry version 4", id)
	}
	_ = time.Now
}
