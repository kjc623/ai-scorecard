package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// barrierKeyStore makes the race deterministic: no SealedUserRefKey call returns until `callers`
// of them have arrived, so every caller sees "no key yet" and proposes its own. Only the store's
// conditional write may then decide.
type barrierKeyStore struct {
	inner   *MemoryKeyStore
	arrived sync.WaitGroup
	inits   atomic.Int32
}

func (b *barrierKeyStore) SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	sealed, err := b.inner.SealedUserRefKey(ctx, tenantID)
	b.arrived.Done()
	b.arrived.Wait()
	return sealed, err
}

func (b *barrierKeyStore) InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error) {
	b.inits.Add(1)
	return b.inner.InitUserRefKey(ctx, tenantID, sealed)
}

func TestUserRefKeysTwoFirstCallersEndWithOneKey(t *testing.T) {
	const callers = 2
	store := &barrierKeyStore{inner: NewMemoryKeyStore()}
	store.arrived.Add(callers)
	// Two processes, not one: a shared cache would hide the race this test is about.
	var keys [callers][]byte
	var errs [callers]error
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		ks, err := NewUserRefKeys(store, testCipher(t))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = ks.Key(context.Background(), "tenant-race")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if store.inits.Load() != callers {
		t.Fatalf("%d callers proposed a key, want %d (the barrier should have made both see none)", store.inits.Load(), callers)
	}
	if !bytes.Equal(keys[0], keys[1]) {
		t.Fatal("two first callers ended with different keys; every person would be split in two")
	}
	if len(keys[0]) != protocol.UserRefKeySize {
		t.Fatalf("key is %d bytes", len(keys[0]))
	}
	stored, _ := store.inner.SealedUserRefKey(context.Background(), "tenant-race")
	opened, err := testCipher(t).OpenBytes("tenant-race", stored)
	if err != nil || !bytes.Equal(opened, keys[0]) {
		t.Fatalf("the stored key is not the one both callers returned: %v", err)
	}
}

func TestUserRefKeysMintOnceThenCache(t *testing.T) {
	store := &countingKeyStore{inner: NewMemoryKeyStore()}
	ks, err := NewUserRefKeys(store, testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := ks.Key(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	first[0] ^= 0xff // the caller's copy is its own
	second, err := ks.Key(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("mutating a returned key changed the cached one")
	}
	if store.reads != 1 || store.inits != 1 {
		t.Fatalf("reads=%d inits=%d, want one of each (the second call is served from cache)", store.reads, store.inits)
	}
	// A new process reads the stored key and does not mint another.
	again, _ := NewUserRefKeys(store, testCipher(t))
	third, err := again.Key(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(third, second) || store.inits != 1 {
		t.Fatal("a second process minted a new key instead of reading the stored one")
	}
}

func TestUserRefKeysAreTenantScoped(t *testing.T) {
	store := NewMemoryKeyStore()
	ks, _ := NewUserRefKeys(store, testCipher(t))
	a, err := ks.Key(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ks.Key(context.Background(), "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two tenants share a user_ref key")
	}
	// A sealed key copied into another tenant's row does not open there.
	sealedA, _ := store.SealedUserRefKey(context.Background(), "tenant-a")
	moved := NewMemoryKeyStore()
	_, _ = moved.InitUserRefKey(context.Background(), "tenant-b", sealedA)
	other, _ := NewUserRefKeys(moved, testCipher(t))
	if _, err := other.Key(context.Background(), "tenant-b"); err == nil {
		t.Fatal("tenant a's sealed key opened as tenant b's")
	}
}

func TestUserRefKeysRefuseAnUnknownTenant(t *testing.T) {
	store := NewMemoryKeyStore()
	store.Known = map[string]bool{"tenant-known": true}
	ks, _ := NewUserRefKeys(store, testCipher(t))
	if _, err := ks.Key(context.Background(), "tenant-other"); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("unknown tenant: err = %v, want ErrUnknownTenant", err)
	}
	if _, err := NewUserRefKeys(nil, testCipher(t)); err == nil {
		t.Fatal("a key source with no store was built")
	}
}

// The stored key is the one devices derive with: the protocol vectors come out of it unchanged.
func TestUserRefKeysServeTheKeyDevicesDeriveWith(t *testing.T) {
	vectorKey, err := protocol.DecodeUserRefKey("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryKeyStore()
	sealed, _ := testCipher(t).SealBytes("tenant-v", vectorKey)
	_, _ = store.InitUserRefKey(context.Background(), "tenant-v", sealed)
	ks, _ := NewUserRefKeys(store, testCipher(t))
	key, err := ks.Key(context.Background(), "tenant-v")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := protocol.DeriveUserRef(key, protocol.UserRefUPN, "Ada.Lovelace@Contoso.com ")
	if got != "u_ed0bf663359a09dab5105ed60a56e835" {
		t.Fatalf("upn ref = %s", got)
	}
	if enc := base64.RawURLEncoding.EncodeToString(key); enc != "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" {
		t.Fatalf("the enrolment encoding of the key = %s", enc)
	}
}

func TestDecodeKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if k, err := DecodeKey(" " + good + "\n"); err != nil || len(k) != 32 {
		t.Fatalf("DecodeKey(good) = %d bytes, %v", len(k), err)
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := DecodeKey(bad); err == nil {
			t.Errorf("DecodeKey(%q) accepted", bad)
		}
	}
}

type countingKeyStore struct {
	inner        *MemoryKeyStore
	reads, inits int
}

func (c *countingKeyStore) SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	c.reads++
	return c.inner.SealedUserRefKey(ctx, tenantID)
}

func (c *countingKeyStore) InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error) {
	c.inits++
	return c.inner.InitUserRefKey(ctx, tenantID, sealed)
}
