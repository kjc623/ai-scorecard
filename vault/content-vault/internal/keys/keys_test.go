package keys_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
)

func aad(tenant, object, version string) keys.AAD {
	return keys.AAD{TenantID: tenant, ObjectID: object, KEKID: "kek-1", KEKVersion: version}
}

// TestWrappedKeyIsBoundToItsRow is the hierarchy's per-object property: a sealed data key opens
// only in the row it was sealed for, so a wrapped key lifted from one row into another fails
// authentication rather than decrypting something it should not.
func TestWrappedKeyIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	w := keys.NewLocal()
	if _, err := w.EnsureKEK("kek-1"); err != nil {
		t.Fatal(err)
	}
	dek, err := keys.GenerateDEK(newRand(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := w.Wrap(ctx, "kek-1", aad("tenant-a", "object-1", ""), dek)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.KEKVersion != "1" {
		t.Fatalf("the wrapped key records version %q, want the current version 1", sealed.KEKVersion)
	}
	// A second version exists, so "another version" tests the AAD binding rather than a missing
	// key: the seal was made under version 1 and a version-2 AAD must not open it.
	if _, err := w.NewVersion(ctx, "kek-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Unwrap(ctx, aad("tenant-a", "object-1", "1"), sealed.Bytes); err != nil {
		t.Fatalf("the wrapped key does not open in its own row: %v", err)
	}
	for _, tc := range []struct {
		name string
		aad  keys.AAD
	}{
		{"another tenant", aad("tenant-b", "object-1", "1")},
		{"another object", aad("tenant-a", "object-2", "1")},
		{"another version", aad("tenant-a", "object-1", "2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Unwrap(ctx, tc.aad, sealed.Bytes); !errors.Is(err, keys.ErrAuthentication) {
				t.Fatalf("unwrapping under %s returned %v, want ErrAuthentication", tc.name, err)
			}
		})
	}

	// A truncated or altered blob is refused, not decrypted into garbage.
	if _, err := w.Unwrap(ctx, aad("tenant-a", "object-1", "1"), sealed.Bytes[:8]); !errors.Is(err, keys.ErrMalformedWrapped) {
		t.Errorf("a truncated wrapped key returned %v, want ErrMalformedWrapped", err)
	}
	altered := append([]byte(nil), sealed.Bytes...)
	altered[len(altered)-1] ^= 0x01
	if _, err := w.Unwrap(ctx, aad("tenant-a", "object-1", "1"), altered); !errors.Is(err, keys.ErrAuthentication) {
		t.Errorf("an altered wrapped key returned %v, want ErrAuthentication", err)
	}
}

// TestRotationKeepsOldVersionsUsable is what makes "re-wrap, never re-encrypt" safe: a row that
// still names the old version must remain readable, or a partial rotation is data loss.
func TestRotationKeepsOldVersionsUsable(t *testing.T) {
	ctx := context.Background()
	w := keys.NewLocal()
	if _, err := w.EnsureKEK("kek-1"); err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, keys.DEKSize)
	for i := range dek {
		dek[i] = byte(i)
	}
	v1, err := w.Wrap(ctx, "kek-1", aad("t", "o", ""), dek)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := w.NewVersion(ctx, "kek-1")
	if err != nil {
		t.Fatal(err)
	}
	if newVersion != "2" {
		t.Fatalf("new version is %q, want 2", newVersion)
	}
	// The current version changed, which is what stops new writes being sealed under the old one.
	if cur, err := w.CurrentVersion(ctx, "kek-1"); err != nil || cur != "2" {
		t.Fatalf("current version is %q (%v), want 2", cur, err)
	}
	// The old row still opens under the version it names...
	opened, err := w.Unwrap(ctx, aad("t", "o", "1"), v1.Bytes)
	if err != nil || string(opened) != string(dek) {
		t.Fatalf("the version-1 row no longer opens: %v", err)
	}
	// ...and a new wrap uses version 2.
	v2, err := w.Wrap(ctx, "kek-1", aad("t", "o2", ""), dek)
	if err != nil {
		t.Fatal(err)
	}
	if v2.KEKVersion != "2" {
		t.Errorf("a wrap after rotation used version %q, want 2", v2.KEKVersion)
	}
	if _, err := w.Unwrap(ctx, aad("t", "o2", "2"), v2.Bytes); err != nil {
		t.Errorf("the version-2 row does not open: %v", err)
	}
	versions := w.Versions("kek-1")
	if len(versions) != 2 || versions[0] != "1" || versions[1] != "2" {
		t.Errorf("key versions are %v, want both 1 and 2", versions)
	}
}

// TestDestroyIsTerminalAndRecorded: every version goes, and the wrapper reports why. This is the
// operation that makes §6.4's "destroying the key destroys the content" true.
func TestDestroyIsTerminalAndRecorded(t *testing.T) {
	ctx := context.Background()
	w := keys.NewLocal()
	if _, err := w.EnsureKEK("kek-1"); err != nil {
		t.Fatal(err)
	}
	dek, err := keys.GenerateDEK(newRand(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := w.Wrap(ctx, "kek-1", aad("t", "o", ""), dek)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Destroy(ctx, "kek-1", "tenant_offboarded"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := w.Unwrap(ctx, aad("t", "o", "1"), sealed.Bytes); !errors.Is(err, keys.ErrKeyDestroyed) {
		t.Fatalf("unwrapping after destruction returned %v, want ErrKeyDestroyed", err)
	}
	if reason, ok := w.DestroyedReason("kek-1"); !ok || reason != "tenant_offboarded" {
		t.Errorf("the destruction reason is %q (%v)", reason, ok)
	}
	if _, err := w.Wrap(ctx, "kek-1", aad("t", "o2", ""), dek); !errors.Is(err, keys.ErrKeyDestroyed) {
		t.Errorf("wrapping after destruction returned %v, want ErrKeyDestroyed", err)
	}
	// Destroying again is idempotent, so an offboarding retry does not fail the receipt.
	if err := w.Destroy(ctx, "kek-1", "tenant_offboarded"); err != nil {
		t.Errorf("a second destroy returned %v", err)
	}
	// A KEK that never existed is a different fact from one that was destroyed.
	if err := w.Destroy(ctx, "kek-never", "x"); !errors.Is(err, keys.ErrUnknownKEK) {
		t.Errorf("destroying an unknown KEK returned %v, want ErrUnknownKEK", err)
	}
}

// TestWrapRefusesAnIncompleteAAD: a sealed key that is not bound to a row is the thing the AAD
// exists to prevent, so the wrapper refuses rather than sealing one.
func TestWrapRefusesAnIncompleteAAD(t *testing.T) {
	ctx := context.Background()
	w := keys.NewLocal()
	if _, err := w.EnsureKEK("kek-1"); err != nil {
		t.Fatal(err)
	}
	dek, err := keys.GenerateDEK(newRand(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []keys.AAD{
		{ObjectID: "o", KEKID: "kek-1", KEKVersion: "1"},
		{TenantID: "t", KEKID: "kek-1", KEKVersion: "1"},
	} {
		if _, err := w.Wrap(ctx, "kek-1", bad, dek); err == nil {
			t.Errorf("wrapping with an incomplete AAD (%+v) succeeded", bad)
		}
	}
	if _, err := w.Wrap(ctx, "kek-1", aad("t", "o", ""), make([]byte, 16)); err == nil {
		t.Error("wrapping a 128-bit data key succeeded; the DEK is AES-256")
	}
}

// TestLocalPersistenceRoundTrip is the local development path: keys survive a restart, still bound
// to their rows.
func TestLocalPersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local-kek.json")
	w, err := keys.OpenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.EnsureKEK("kek-1"); err != nil {
		t.Fatal(err)
	}
	dek, err := keys.GenerateDEK(newRand(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := w.Wrap(ctx, "kek-1", aad("t", "o", ""), dek)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	reopened, err := keys.OpenLocal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	opened, err := reopened.Unwrap(ctx, aad("t", "o", "1"), sealed.Bytes)
	if err != nil || string(opened) != string(dek) {
		t.Fatalf("a persisted key did not reopen its row: %v", err)
	}
}

// TestUnimplementedCloudBackendFailsLoudly is the honesty requirement: the KMS backend exists as an
// interface and a refusal, so no configuration can select it and believe it works.
func TestUnimplementedCloudBackendFailsLoudly(t *testing.T) {
	ctx := context.Background()
	k := keys.NewKMS("https://example.vault.azure.net", "customer_managed")
	if k.Kind() != keys.KindAzureKeyVault {
		t.Errorf("Kind() is %q", k.Kind())
	}
	if health := k.Health(); !contains(health, "NOT IMPLEMENTED") {
		t.Errorf("Health() does not say the backend is unimplemented: %q", health)
	}
	dek, err := keys.GenerateDEK(newRand(t))
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		name string
		err  error
	}
	calls := []call{
		{"CurrentVersion", errOf(func() error { _, e := k.CurrentVersion(ctx, "kek"); return e })},
		{"Wrap", errOf(func() error { _, e := k.Wrap(ctx, "kek", aad("t", "o", ""), dek); return e })},
		{"Unwrap", errOf(func() error { _, e := k.Unwrap(ctx, aad("t", "o", "1"), []byte{1}); return e })},
		{"NewVersion", errOf(func() error { _, e := k.NewVersion(ctx, "kek"); return e })},
		{"Destroy", errOf(func() error { return k.Destroy(ctx, "kek", "erasure") })},
	}
	for _, c := range calls {
		if !errors.Is(c.err, keys.ErrNotImplemented) {
			t.Errorf("KMS.%s returned %v, want ErrNotImplemented", c.name, c.err)
		}
	}
}

// TestKeyWrapperCannotBeAskedForKeyMaterial is the §5.3 invariant "the KEK never leaves the key
// store" expressed as an interface property: there is no method that could return a KEK, so no
// implementation can offer one by accident and no caller can ask for one.
func TestKeyWrapperCannotBeAskedForKeyMaterial(t *testing.T) {
	typ := reflect.TypeOf((*keys.KeyWrapper)(nil)).Elem()
	allowed := map[string]bool{
		"Kind": true, "CurrentVersion": true, "Wrap": true, "Unwrap": true,
		"NewVersion": true, "Destroy": true,
	}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !allowed[name] {
			t.Errorf("KeyWrapper grew a method %q; anything beyond wrap/unwrap/rotate/destroy is an operation the vault must not be able to perform", name)
		}
	}
	if typ.NumMethod() != len(allowed) {
		t.Errorf("KeyWrapper has %d methods, want %d", typ.NumMethod(), len(allowed))
	}
	for _, forbidden := range []string{"GetKey", "ExportKey", "ListKeys", "ImportKey", "Plaintext"} {
		if _, ok := typ.MethodByName(forbidden); ok {
			t.Errorf("KeyWrapper exposes %s, which would let the KEK leave the key store", forbidden)
		}
	}
}

func errOf(f func() error) error { return f() }

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
