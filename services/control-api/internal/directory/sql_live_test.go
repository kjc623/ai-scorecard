package directory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
)

// The live tests run the key store as sac_control against the database SAC_TEST_PG_DSN names.

func TestKeyStoreStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	for i, q := range Statements {
		name := fmt.Sprintf("sac_dir_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+q); err != nil {
			t.Errorf("statement %d does not prepare: %v", i, err)
			continue
		}
		_, _ = db.ExecContext(ctx, "DEALLOCATE "+name)
	}
}

// TestKeyStoreMintsOnceUnderARace drives two first callers through the real conditional write: the
// barrier makes both read "no key" before either writes, so only the database decides, and both
// must come back with the key it kept.
func TestKeyStoreMintsOnceUnderARace(t *testing.T) {
	owner := pgtest.Open(t)
	db := pgtest.OpenAs(t, "sac_control")
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eu-west")

	store := &sqlBarrierKeyStore{inner: NewKeyStore(db)}
	store.arrived.Add(2)
	var keys [2][]byte
	var errs [2]error
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		ks, err := NewUserRefKeys(store, testCipher(t))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = ks.Key(ctx, tenant)
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Key: %v / %v", errs[0], errs[1])
	}
	if !bytes.Equal(keys[0], keys[1]) {
		t.Fatal("two first callers ended with different keys")
	}
	var sealed []byte
	if err := owner.QueryRowContext(ctx, `SELECT user_ref_key_enc FROM ops.tenant WHERE tenant_id = $1::uuid`, tenant).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if opened, err := testCipher(t).OpenBytes(tenant, sealed); err != nil || !bytes.Equal(opened, keys[0]) {
		t.Fatalf("the stored key is not the returned one: %v", err)
	}
	if _, err := (&UserRefKeys{store: NewKeyStore(db), cipher: testCipher(t)}).Key(ctx, pgtest.UUID(t)); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("unknown tenant = %v", err)
	}
}

type sqlBarrierKeyStore struct {
	inner   *KeyStore
	arrived sync.WaitGroup
}

func (b *sqlBarrierKeyStore) SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	sealed, err := b.inner.SealedUserRefKey(ctx, tenantID)
	b.arrived.Done()
	b.arrived.Wait()
	return sealed, err
}

func (b *sqlBarrierKeyStore) InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error) {
	return b.inner.InitUserRefKey(ctx, tenantID, sealed)
}
