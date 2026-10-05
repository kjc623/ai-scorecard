package protocol

import (
	"strings"
	"testing"
)

// The vectors were computed outside Go (Node's crypto.createHmac) so this test pins the
// derivation itself, not this implementation of it. control-api's SCIM side and capture-core
// must both reproduce them.
func TestDeriveUserRefVectors(t *testing.T) {
	key, err := DecodeUserRefKey("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind  UserRefKind
		value string
		want  string
	}{
		{UserRefUPN, "Ada.Lovelace@Contoso.com ", "u_ed0bf663359a09dab5105ed60a56e835"},
		{UserRefUPN, "ada.lovelace@contoso.com", "u_ed0bf663359a09dab5105ed60a56e835"},
		{UserRefOID, "6F9619FF-8B86-D011-B42D-00C04FC964FF", "u_a504db1d5d49e461d150819d7d9286e8"},
		{UserRefAccount, `DESKTOP-01\Kyle`, "u_07eff34d88f27c2bd74a0274623831b7"},
	} {
		got, err := DeriveUserRef(key, tc.kind, tc.value)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.kind, tc.value, err)
		}
		if got != tc.want {
			t.Errorf("%s %q = %s, want %s", tc.kind, tc.value, got, tc.want)
		}
	}
}

func TestDeriveUserRefKindsDoNotCollide(t *testing.T) {
	key := make([]byte, UserRefKeySize)
	upn, _ := DeriveUserRef(key, UserRefUPN, "same")
	oid, _ := DeriveUserRef(key, UserRefOID, "same")
	if upn == oid {
		t.Fatal("a upn and an oid with the same text must not derive the same reference")
	}
}

func TestDeriveUserRefRefusals(t *testing.T) {
	key := make([]byte, UserRefKeySize)
	if _, err := DeriveUserRef(key[:16], UserRefUPN, "a@b"); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := DeriveUserRef(key, UserRefKind("email"), "a@b"); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := DeriveUserRef(key, UserRefUPN, "   "); err == nil {
		t.Error("an empty value was accepted")
	}
	if _, err := DecodeUserRefKey(strings.Repeat("A", 10)); err == nil {
		t.Error("a short encoded key was accepted")
	}
}
