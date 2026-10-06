package keyring

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const (
	tenantA = "7d3c6a52-0b8e-4f0e-9a51-2a4c1f6b9e01"
	tenantB = "c2b1f0e4-5d6a-4b7c-8e9f-0a1b2c3d4e5f"
	object1 = "0f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	object2 = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

func key(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, KeySize))
}

func mustParse(t *testing.T, spec string) *Keyring {
	t.Helper()
	k, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := mustParse(t, "v1:"+key(1))
	plaintext := []byte(`{"messages":[{"role":"user","content":"quarterly numbers"}]}`)
	version, ct, err := k.Seal(tenantA, object1, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1" {
		t.Fatalf("version = %q, want v1", version)
	}
	if bytes.Contains(ct, plaintext) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if len(ct) != nonceSize+len(plaintext)+16 {
		t.Fatalf("ciphertext is %d bytes, want nonce + plaintext + tag = %d", len(ct), nonceSize+len(plaintext)+16)
	}
	got, err := k.Open(tenantA, object1, version, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open = %q, want %q", got, plaintext)
	}
}

func TestEmptyPlaintextRoundTrips(t *testing.T) {
	k := mustParse(t, "v1:"+key(1))
	version, ct, err := k.Seal(tenantA, object1, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.Open(tenantA, object1, version, ct)
	if err != nil || len(got) != 0 {
		t.Fatalf("Open = %q, %v; want empty, nil", got, err)
	}
}

func TestEachSealUsesAFreshNonce(t *testing.T) {
	k := mustParse(t, "v1:"+key(1))
	_, a, _ := k.Seal(tenantA, object1, []byte("same"))
	_, b, _ := k.Seal(tenantA, object1, []byte("same"))
	if bytes.Equal(a[:nonceSize], b[:nonceSize]) || bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext produced the same nonce or ciphertext")
	}
}

func TestCiphertextIsBoundToItsTenantAndObject(t *testing.T) {
	k := mustParse(t, "v1:"+key(1))
	version, ct, err := k.Seal(tenantA, object1, []byte("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"another tenant": {tenantB, object1},
		"another object": {tenantA, object2},
		"both":           {tenantB, object2},
	}
	for name, ids := range cases {
		if _, err := k.Open(ids[0], ids[1], version, ct); err == nil {
			t.Errorf("%s: ciphertext opened under the wrong identity", name)
		}
	}
}

func TestTamperingIsDetected(t *testing.T) {
	k := mustParse(t, "v1:"+key(1))
	version, ct, _ := k.Seal(tenantA, object1, []byte("prompt"))
	for _, i := range []int{0, nonceSize, len(ct) - 1} {
		bad := bytes.Clone(ct)
		bad[i] ^= 0x01
		if _, err := k.Open(tenantA, object1, version, bad); err == nil {
			t.Errorf("flipping byte %d went undetected", i)
		}
	}
	if _, err := k.Open(tenantA, object1, version, ct[:nonceSize+15]); err == nil {
		t.Error("a truncated ciphertext opened")
	}
}

func TestRotation(t *testing.T) {
	old := mustParse(t, "v1:"+key(1))
	v1, ct1, err := old.Seal(tenantA, object1, []byte("before rotation"))
	if err != nil {
		t.Fatal(err)
	}

	rotated := mustParse(t, "v2:"+key(2)+", v1:"+key(1))
	if rotated.Current() != "v2" {
		t.Fatalf("Current = %q, want v2 (the first entry)", rotated.Current())
	}
	got, err := rotated.Open(tenantA, object1, v1, ct1)
	if err != nil || string(got) != "before rotation" {
		t.Fatalf("the rotated keyring did not open v1 content: %q, %v", got, err)
	}
	v2, ct2, err := rotated.Seal(tenantA, object2, []byte("after rotation"))
	if err != nil {
		t.Fatal(err)
	}
	if v2 != "v2" {
		t.Fatalf("new content sealed under %q, want v2", v2)
	}
	if _, err := old.Open(tenantA, object2, v2, ct2); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("a keyring without v2 opened v2 content: %v", err)
	}

	retired := mustParse(t, "v2:"+key(2))
	if _, err := retired.Open(tenantA, object1, v1, ct1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("Open with a retired version = %v, want ErrUnknownVersion", err)
	}
	if !rotated.Has("v1") || !rotated.Has("v2") || retired.Has("v1") {
		t.Fatal("Has does not reflect the configured versions")
	}
	if v := rotated.Versions(); len(v) != 2 || v[0] != "v2" || v[1] != "v1" {
		t.Fatalf("Versions = %v, want [v2 v1]", v)
	}
}

func TestTheSameVersionLabelUnderAnotherKeyDoesNotOpen(t *testing.T) {
	a := mustParse(t, "v1:"+key(1))
	b := mustParse(t, "v1:"+key(9))
	version, ct, _ := a.Seal(tenantA, object1, []byte("prompt"))
	if _, err := b.Open(tenantA, object1, version, ct); err == nil {
		t.Fatal("content opened under a different master key with the same label")
	}
}

// TestFormat decrypts with the standard library alone, so the stored format is exactly the one the
// package documents: HKDF-SHA256(master, nil, "sac/content/v1/"+tenant), AES-256-GCM, a 12-byte
// nonce prefix and "<tenant>/<object>" as additional data.
func TestFormat(t *testing.T) {
	master := bytes.Repeat([]byte{7}, KeySize)
	k := mustParse(t, "v7:"+base64.StdEncoding.EncodeToString(master))
	_, ct, err := k.Seal(tenantA, object1, []byte("format"))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := hkdf.Key(sha256.New, master, nil, "sac/content/v1/"+tenantA, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(derived)
	gcm, _ := cipher.NewGCM(block)
	got, err := gcm.Open(nil, ct[:12], ct[12:], []byte(tenantA+"/"+object1))
	if err != nil || string(got) != "format" {
		t.Fatalf("independent decryption = %q, %v", got, err)
	}
}

func TestTenantKeysDiffer(t *testing.T) {
	master := bytes.Repeat([]byte{3}, KeySize)
	a, _ := TenantKey(master, tenantA)
	b, _ := TenantKey(master, tenantB)
	if bytes.Equal(a, b) || bytes.Equal(a, master) || len(a) != KeySize {
		t.Fatal("tenant keys are not distinct 32-byte derivations")
	}
}

func TestBadKeyringsAreRefused(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	long := base64.StdEncoding.EncodeToString(make([]byte, 33))
	cases := map[string]string{
		"empty":             "",
		"blank":             "   ",
		"no colon":          key(1),
		"empty version":     ":" + key(1),
		"bad version":       "v 1:" + key(1),
		"bad base64":        "v1:not*base64",
		"short key":         "v1:" + short,
		"long key":          "v1:" + long,
		"16-byte key":       "v1:" + base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"duplicate version": "v1:" + key(1) + ",v1:" + key(2),
		"trailing comma":    "v1:" + key(1) + ",",
		"empty middle":      "v2:" + key(2) + ",,v1:" + key(1),
	}
	for name, spec := range cases {
		if _, err := Parse(spec); err == nil {
			t.Errorf("%s: Parse(%q) accepted a bad keyring", name, spec)
		}
	}
}

func TestParseErrorsNeverContainKeyMaterial(t *testing.T) {
	secret := key(5)
	_, err := Parse("v1:" + secret + ",v1:" + secret)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error %v leaks the key", err)
	}
}
