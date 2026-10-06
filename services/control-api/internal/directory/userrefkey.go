package directory

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/shadow-ai-capture/device/protocol"
)

// The tenant's user-reference key (protocol.DeriveUserRef).
//
// Every device in a tenant derives each person's pseudonymous user_ref under this key, and the SCIM
// side derives the same value from what the customer's identity provider sends, so the two meet
// without either sending a name on the event path. That makes the key the one tenant secret that can
// never change: a second key would split every person into two refs. So it is minted exactly once,
// lazily, on the first enrolment or the first SCIM write, whichever comes first, and the minting is
// a conditional write that the database decides: two first callers both propose a key, one proposal
// is stored, and both return the stored one.
//
// It is sealed in ops.tenant.user_ref_key_enc with the directory Cipher, so a database read alone
// does not yield it. Devices receive it in the clear inside the enrolment response; that is the
// design (a device must compute refs offline), and it is why the ref is a pseudonym rather than a
// secret.

// ErrUnknownTenant is returned by a UserRefKeyStore when the tenant does not exist.
var ErrUnknownTenant = errors.New("directory: tenant unknown")

// UserRefKeyStore is the persistence seam for the key. Both methods are tenant-scoped.
type UserRefKeyStore interface {
	// SealedUserRefKey returns the sealed key, or nil when none has been minted. ErrUnknownTenant
	// when the tenant does not exist.
	SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error)
	// InitUserRefKey stores sealed only if no key is stored yet, and returns whichever sealed key is
	// stored afterwards: the caller's on a first write, the earlier writer's otherwise. It must be
	// atomic against a concurrent InitUserRefKey for the same tenant.
	InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error)
}

// UserRefKeys hands out tenant user-reference keys, minting one on first need. Safe for concurrent
// use; a key, once read, is cached for the life of the process, because it never changes.
type UserRefKeys struct {
	store  UserRefKeyStore
	cipher *Cipher
	cache  sync.Map // tenantID -> []byte
}

// NewUserRefKeys wires the key source. Both collaborators are required.
func NewUserRefKeys(store UserRefKeyStore, cipher *Cipher) (*UserRefKeys, error) {
	if store == nil || cipher == nil {
		return nil, fmt.Errorf("directory: user_ref keys need a store and a cipher")
	}
	return &UserRefKeys{store: store, cipher: cipher}, nil
}

// Key returns the tenant's 32-byte user-reference key, minting and sealing it if the tenant has none.
// The returned slice is the caller's own copy.
func (k *UserRefKeys) Key(ctx context.Context, tenantID string) ([]byte, error) {
	if cached, ok := k.cache.Load(tenantID); ok {
		return clone(cached.([]byte)), nil
	}
	sealed, err := k.store.SealedUserRefKey(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("directory: read user_ref key: %w", err)
	}
	if len(sealed) == 0 {
		fresh := make([]byte, protocol.UserRefKeySize)
		if _, err := rand.Read(fresh); err != nil {
			return nil, fmt.Errorf("directory: no entropy for a user_ref key: %w", err)
		}
		proposal, err := k.cipher.SealBytes(tenantID, fresh)
		if err != nil {
			return nil, err
		}
		// The stored key is whatever the database kept, which is this proposal only if no other
		// caller got there first. Using `fresh` here instead would be the race this exists to close.
		if sealed, err = k.store.InitUserRefKey(ctx, tenantID, proposal); err != nil {
			return nil, fmt.Errorf("directory: store user_ref key: %w", err)
		}
	}
	key, err := k.cipher.OpenBytes(tenantID, sealed)
	if err != nil {
		return nil, fmt.Errorf("directory: open user_ref key: %w", err)
	}
	if len(key) != protocol.UserRefKeySize {
		return nil, fmt.Errorf("directory: stored user_ref key is %d bytes, want %d", len(key), protocol.UserRefKeySize)
	}
	k.cache.Store(tenantID, clone(key))
	return key, nil
}

func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
