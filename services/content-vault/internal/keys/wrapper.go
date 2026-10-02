// Package keys is the content key hierarchy of docs/06-security-and-threat-model.md §5.3 as this
// service implements it.
//
//	Object plaintext
//	    │  AES-256-GCM under a per-object data key (the DEK), fresh per object
//	    ▼
//	Per-object DEK ──wrapped by──► per-tenant key-encryption key (the KEK), by version
//	    │
//	    ├─ mode 1 vendor:         vendor Key Vault / Managed HSM
//	    ├─ mode 2 customer_managed: the customer's vault, via federated workload identity
//	    └─ mode 3 customer_held:  the customer's HSM; the KEK never leaves it
//
// The three invariants that shape this package:
//
//   - **The KEK never leaves the key store.** Wrap and Unwrap are the only operations, and only
//     the *result* of an unwrap crosses the boundary (§5.3, B12). The local software wrapper
//     holds key bytes in this process because it *is* the key store for tests and local dev; the
//     cloud wrapper's job is to hold them somewhere else entirely.
//   - **A wrapped data key is bound to the row it lives in.** The GCM additional authenticated
//     data is the tenant, the object, the KEK id and the KEK version, so a wrapped key copied
//     into another tenant's or another object's row does not decrypt. That turns "the database
//     row and the blob are in different systems" (§5.3) into "and the two halves are
//     interchangeable only in the row they were made for".
//   - **Destruction is a real operation, not a state flag.** Destroy removes every version of a
//     tenant's KEK, after which no unwrap succeeds: that is what makes brief §4.4's "destroying
//     the key destroys the content" true, and it is why the read path must report the result as
//     *destroyed* rather than as an error (§6.4).
//
// The interface is deliberately five methods wide. Anything richer — getKey, listKeys, import —
// is an operation the vault must never be able to perform, so it is not on the interface at all.
package keys

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// KeyKind identifies which backend is in use, for the health channel and for the honesty of a
// deployment report: a service that says "azure-key-vault" while running the local software
// wrapper is lying about the one fact this component exists to provide.
type KeyKind string

const (
	KindLocalSoftware KeyKind = "local-software"
	KindAzureKeyVault KeyKind = "azure-key-vault"
	KindCustomerHSM   KeyKind = "customer-hsm"
)

// Errors callers branch on. Everything else is an infrastructure failure.
var (
	// ErrKeyDestroyed is returned by Unwrap after the KEK has been destroyed. It is a *terminal
	// fact about the content*, not a transient failure: the caller must report the object as
	// destroyed (§6.4), never as a retryable error.
	ErrKeyDestroyed = errors.New("keys: the tenant key has been destroyed")
	// ErrUnknownKEK is returned for a key id this store has never held. It is distinct from
	// ErrKeyDestroyed because "never existed" and "existed and was destroyed" produce different
	// receipts.
	ErrUnknownKEK = errors.New("keys: unknown key encryption key")
	// ErrUnknownVersion is returned when a row names a KEK version this store does not hold.
	ErrUnknownVersion = errors.New("keys: unknown key version")
	// ErrNotImplemented is returned by every method of a backend this build has not exercised.
	// It exists so an unexercised cloud backend cannot be mistaken for a working one.
	ErrNotImplemented = errors.New("keys: backend not implemented in this build")
	// ErrMalformedWrapped is returned when a wrapped key is not a well-formed sealed blob.
	ErrMalformedWrapped = errors.New("keys: wrapped data key is malformed")
	// ErrAuthentication is returned when a wrapped key does not authenticate under the AAD it was
	// presented with: the row was moved, the version is wrong, or the bytes were altered.
	ErrAuthentication = errors.New("keys: wrapped data key does not authenticate against this row")
)

// DEKSize is the per-object data key length: AES-256 (§5.3).
const DEKSize = 32

// KEKSize is the per-tenant key length: AES-256 (§5.3). 6.4 point 3 requires the wrap to be
// unbroken at this size, which is why it is 256 bits and not a derived or memorable value.
const KEKSize = 32

// AAD is the additional authenticated data a wrapped DEK is bound to. Every field is read from
// the content_object row that holds the wrapped key, so unwrapping is possible only in that row:
// a wrapped key lifted into another tenant's, another object's or another version's row fails
// authentication rather than decrypting something it should not.
type AAD struct {
	TenantID   string
	ObjectID   string
	KEKID      string
	KEKVersion string
}

// Canonical serialises the AAD unambiguously: length-prefixed UTF-8 fields, so
// ("ab","c") and ("a","bc") cannot collide, plus a domain-separation label so these bytes can
// never be confused with another protocol's AAD.
func (a AAD) Canonical() []byte {
	var b strings.Builder
	b.WriteString("sac.dek.v1")
	var lenBuf [4]byte
	for _, f := range []string{a.TenantID, a.ObjectID, a.KEKID, a.KEKVersion} {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(f)))
		b.Write(lenBuf[:])
		b.WriteString(f)
	}
	return []byte(b.String())
}

// Complete reports whether every AAD field is present. An incomplete AAD is refused rather than
// sealed, because a wrapped key that is not bound to a row is the thing this type exists to
// prevent.
func (a AAD) Complete() bool {
	return a.TenantID != "" && a.ObjectID != "" && a.KEKID != "" && a.KEKVersion != ""
}

// Wrapped is a sealed DEK together with the key it was sealed under. The version travels with the
// value so a caller cannot record the wrong one in the row it writes.
type Wrapped struct {
	Bytes      []byte
	KEKID      string
	KEKVersion string
}

// KeyWrapper is the whole surface the vault has against a key store.
//
// Note what is *not* here: no GetKey, no ExportKey, no ListKeys, and no way to ask for a KEK's
// bytes. §5.3 requires that the KEK never leaves the key store, and an interface that cannot
// express the request is the only version of that requirement a reviewer can check.
type KeyWrapper interface {
	// Kind names the backend truthfully.
	Kind() KeyKind

	// CurrentVersion returns the version new wraps must use. It is what makes "the old key
	// version is not silently used for new writes" testable rather than assumed.
	CurrentVersion(ctx context.Context, kekID string) (string, error)

	// Wrap seals a DEK under the current version of kekID, bound to aad. aad.KEKVersion is
	// ignored on input (the current version wins) and set on the returned Wrapped.
	Wrap(ctx context.Context, kekID string, aad AAD, dek []byte) (Wrapped, error)

	// Unwrap opens a DEK sealed under the version named in aad. It returns ErrKeyDestroyed when
	// the KEK has been destroyed and ErrAuthentication when the AAD does not match the seal.
	Unwrap(ctx context.Context, aad AAD, wrapped []byte) ([]byte, error)

	// NewVersion creates the next KEK version and makes it current. It does not touch any
	// wrapped key: re-wrapping is the caller's job and is what keeps rotation from re-encrypting
	// content (§6.4 point 1 — there is one copy of the plaintext, and rotation does not make a
	// second one).
	NewVersion(ctx context.Context, kekID string) (string, error)

	// Destroy removes every version of kekID. reason is recorded for the receipt (A4: the
	// receipt states what actually happened, including a recovery window, rather than claiming
	// immediate destruction that is not true).
	Destroy(ctx context.Context, kekID, reason string) error
}

// GenerateKEK returns a fresh 256-bit KEK from r.
func GenerateKEK(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	k := make([]byte, KEKSize)
	if _, err := readFull(r, k); err != nil {
		return nil, fmt.Errorf("keys: generating a KEK: %w", err)
	}
	return k, nil
}

// GenerateDEK returns a fresh 256-bit per-object data key from r.
func GenerateDEK(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	k := make([]byte, DEKSize)
	if _, err := readFull(r, k); err != nil {
		return nil, fmt.Errorf("keys: generating a DEK: %w", err)
	}
	return k, nil
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, errors.New("keys: random source returned no bytes")
		}
	}
	return total, nil
}
