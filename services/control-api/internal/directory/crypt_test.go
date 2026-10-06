package directory

import "testing"

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestCipherRoundTripsAndIsTenantScoped(t *testing.T) {
	c := testCipher(t)
	sealed, err := c.Seal("tenant-1", "dir-abc")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := c.Open("tenant-1", sealed); err != nil || got != "dir-abc" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := c.Open("tenant-2", sealed); err == nil {
		t.Fatal("a sealed identifier opened under another tenant's key")
	}
	if empty, err := c.Seal("tenant-1", ""); err != nil || empty != nil {
		t.Fatalf("Seal(\"\") = %v, %v; want nil", empty, err)
	}
	if _, err := NewCipher([]byte("short")); err == nil {
		t.Fatal("a short master key was accepted")
	}
}
