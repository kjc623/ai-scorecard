package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// These tests pin the device-authentication contract (ADR 0020; docs/02-ingest-and-transport.md
// §2, §5.1). The RFC 7638 vector is the one external authority for Thumbprint: a home-grown
// expectation could agree with a buggy canonical form.

// ADR 0020 decision 2: the wire modes are a closed set. An unknown mode must be refused, never
// defaulted to x509, because a default is a silent downgrade of an unknown credential type.
func TestAuthModeIsClosed(t *testing.T) {
	for _, m := range []AuthMode{AuthModeX509, AuthModeDPoP} {
		if !m.Valid() {
			t.Fatalf("mode %q is a production mode but reported itself invalid", m)
		}
	}
	for _, m := range []AuthMode{"", "x509 ", "X509", "mtls", "dev", "bearer"} {
		if m.Valid() {
			t.Fatalf("mode %q is outside the closed set but reported itself valid", m)
		}
	}
	if AuthModeX509 != "x509" || AuthModeDPoP != "dpop" {
		t.Fatalf("mode values drifted: x509=%q dpop=%q", AuthModeX509, AuthModeDPoP)
	}
}

// The header names and version constants are a wire contract other slices import, so a rename is
// a breaking change and is caught here rather than in the field.
func TestAuthHeaderAndVersionConstants(t *testing.T) {
	headers := map[string]string{
		"HeaderClientCert":    HeaderClientCert,
		"HeaderDPoP":          HeaderDPoP,
		"HeaderAuthorization": HeaderAuthorization,
	}
	want := map[string]string{
		"HeaderClientCert":    "X-Client-Cert",
		"HeaderDPoP":          "DPoP",
		"HeaderAuthorization": "Authorization",
	}
	for name, got := range headers {
		if got != want[name] {
			t.Fatalf("%s = %q, want %q", name, got, want[name])
		}
	}
	if EnrolmentSchemaVersion != "1.0" {
		t.Fatalf("EnrolmentSchemaVersion = %q, want %q", EnrolmentSchemaVersion, "1.0")
	}
	if TokenTypeDPoP != "DPoP" {
		t.Fatalf("TokenTypeDPoP = %q, want %q", TokenTypeDPoP, "DPoP")
	}
}

// RFC 7638 §3.1 is an RSA key, despite being widely mislabelled an EC example; the thumbprint
// NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs is that RSA key's. The optional alg and kid members
// are present in the RFC's JWK on purpose: the vector also proves they are excluded from the
// hash, which is why a later kid cannot change a key's identity.
func TestJWKThumbprintRFC7638Vector(t *testing.T) {
	key := JWK{
		Kty: "RSA",
		N: "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAt" +
			"VT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn6" +
			"4tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FD" +
			"W2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n9" +
			"1CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINH" +
			"aQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw",
		E:   "AQAB",
		Alg: "RS256",
		Kid: "2011-04-29",
	}
	const want = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"
	got, err := key.Thumbprint()
	if err != nil {
		t.Fatalf("Thumbprint of the RFC 7638 §3.1 key: %v", err)
	}
	if got != want {
		t.Fatalf("Thumbprint = %q, want %q (RFC 7638 §3.1)", got, want)
	}
}

// The EC canonical form (crv, kty, x, y) is pinned against an independent computation of the
// well-known P-256 public key in RFC 7515 Appendix A.3. RFC 7638 publishes no EC vector, so this
// value was derived from the canonical JSON with a separate implementation rather than from this
// package.
func TestJWKThumbprintECVector(t *testing.T) {
	key := JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU",
		Y:   "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0",
	}
	const want = "oKIywvGUpTVTyxMQ3bwIIeQUudfr_CkLMjCE19ECD-U"
	got, err := key.Thumbprint()
	if err != nil {
		t.Fatalf("Thumbprint of the RFC 7515 A.3 key: %v", err)
	}
	if got != want {
		t.Fatalf("Thumbprint = %q, want %q", got, want)
	}
}

// A partial key must fail closed: hashing whatever members happen to be present would produce a
// well-formed but wrong thumbprint, binding a credential to a key nobody holds.
func TestJWKThumbprintRefusesIncompleteOrUnknown(t *testing.T) {
	cases := map[string]JWK{
		"no kty":         {X: "a", Y: "b"},
		"unknown kty":    {Kty: "oct", N: "a"},
		"EC missing crv": {Kty: "EC", X: "a", Y: "b"},
		"EC missing x":   {Kty: "EC", Crv: "P-256", Y: "b"},
		"EC missing y":   {Kty: "EC", Crv: "P-256", X: "a"},
		"RSA missing e":  {Kty: "RSA", N: "a"},
		"RSA missing n":  {Kty: "RSA", E: "AQAB"},
		"lowercase kty":  {Kty: "ec", Crv: "P-256", X: "a", Y: "b"},
	}
	for name, key := range cases {
		got, err := key.Thumbprint()
		if err == nil {
			t.Fatalf("%s: Thumbprint returned %q with no error, want a refusal", name, got)
		}
		if got != "" {
			t.Fatalf("%s: Thumbprint returned a value %q alongside an error", name, got)
		}
	}
}

// The enquiry and response shapes round-trip unchanged, and omitempty actually omits the fields a
// mode does not use, so an x509 request does not carry a stray jwk and vice versa.
func TestEnrolmentRoundTrip(t *testing.T) {
	x509Req := EnrolmentRequest{
		SchemaVersion:  EnrolmentSchemaVersion,
		EnrolmentToken: "bootstrap-token",
		Mode:           AuthModeX509,
		CSR:            "-----BEGIN CERTIFICATE REQUEST-----\nMIIB...\n-----END CERTIFICATE REQUEST-----",
		Device: DeviceInfo{
			OS:                   "windows",
			OSVersion:            "11.0.26100",
			AgentVersion:         "1.4.2",
			MDMID:                "intune-abc",
			HardwareIdentityHash: "sha256:9f2c",
		},
		ClaimedRegion: "eu",
	}
	assertJSONRoundTrip(t, x509Req)

	ecKey := &JWK{Kty: "EC", Crv: "P-256", X: "xf", Y: "yg"}
	dpopReq := EnrolmentRequest{
		SchemaVersion: EnrolmentSchemaVersion,
		Mode:          AuthModeDPoP,
		JWK:           ecKey,
		Device:        DeviceInfo{OS: "linux", AgentVersion: "1.4.2", HardwareIdentityHash: "sha256:abcd"},
	}
	b, err := json.Marshal(dpopReq)
	if err != nil {
		t.Fatalf("marshal dpop request: %v", err)
	}
	if _, present := decodeKeys(t, b)["csr"]; present {
		t.Fatalf("a dpop request serialised a csr: %s", b)
	}
	assertJSONRoundTrip(t, dpopReq)

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	resp := EnrolmentResponse{
		SchemaVersion: EnrolmentSchemaVersion,
		DeviceID:      "d-1",
		TenantID:      "t-1",
		Region:        "eu",
		Reenrolled:    true,
		Credential: IssuedCredential{
			Mode:     AuthModeX509,
			CertPEM:  "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----",
			ChainPEM: []string{"-----BEGIN CERTIFICATE-----\nCA...\n-----END CERTIFICATE-----"},
			NotAfter: now.Add(90 * 24 * time.Hour),
		},
		PolicyETag: "bundle-7",
		ServerTime: now,
	}
	assertJSONRoundTrip(t, resp)
}

// The token endpoint's token_type is a one-value closed vocabulary: anything but DPoP would be a
// replayable bearer token, which ADR 0005 forbids.
func TestTokenResponseValidatesClosedTokenType(t *testing.T) {
	good := TokenResponse{
		AccessToken: "at-1",
		TokenType:   TokenTypeDPoP,
		ExpiresIn:   300,
		ServerTime:  time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed DPoP token response was refused: %v", err)
	}
	assertJSONRoundTrip(t, good)

	bearer := good
	bearer.TokenType = "Bearer"
	if err := bearer.Validate(); err == nil {
		t.Fatal("a Bearer token response was accepted; a token that is not DPoP-bound is replayable")
	}
	empty := good
	empty.TokenType = ""
	if err := empty.Validate(); err == nil {
		t.Fatal("a token response with no token_type was accepted rather than refused")
	}
	noToken := good
	noToken.AccessToken = ""
	if err := noToken.Validate(); err == nil {
		t.Fatal("a token response with no access_token was accepted")
	}
	noLife := good
	noLife.ExpiresIn = 0
	if err := noLife.Validate(); err == nil {
		t.Fatal("a token response with no lifetime was accepted")
	}
}

func assertJSONRoundTrip[T any](t *testing.T, v T) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var back T
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	if !reflect.DeepEqual(v, back) {
		t.Fatalf("%T did not round-trip:\n before: %#v\n after:  %#v\n json: %s", v, v, back, b)
	}
}

func decodeKeys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	return m
}
