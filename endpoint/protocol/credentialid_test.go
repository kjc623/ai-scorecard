package protocol

import "testing"

// CredentialID is the one value control-api (issuer) and ingest-api (verifier) must agree on
// without sharing state, so its determinism and shape are asserted here (ADR 0020 decision 4).
func TestCredentialIDIsDeterministicAndVersioned(t *testing.T) {
	der := []byte("a certificate's DER bytes")

	first := CredentialID(der)
	if first != CredentialID(der) {
		t.Fatalf("CredentialID is not deterministic: %q then %q", first, CredentialID(der))
	}
	if first == CredentialID([]byte("another certificate's DER bytes")) {
		t.Fatalf("CredentialID collides on different input")
	}
	// A version-4-shaped uuid: 36 chars with the version nibble at index 14.
	if len(first) != 36 || first[14] != '4' {
		t.Fatalf("CredentialID %q is not a version-4-shaped uuid", first)
	}
}
