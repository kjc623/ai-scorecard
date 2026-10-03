package keys

import (
	"context"
	"fmt"
)

// KMSKeyWrapper is the cloud backend, and **it is not implemented in this build**.
//
// TOOLCHAIN-DECISION.md §3 records the decision and its consequence in one line: a local AES-256-GCM
// software implementation for tests and local development, and "an **explicitly unimplemented**
// Azure Key Vault / Managed HSM backend whose interface documents what the cloud must provide. Any
// claim that the cloud path works is false on this host."
//
// The type exists for three reasons, and all three are about not lying:
//
//  1. **The interface is the specification.** What a deployment must supply is written here, in the
//     shape the caller already uses, so the gap is a piece of work rather than a paragraph in a
//     design document.
//  2. **Every method fails loudly.** There is no partially-working cloud path that a configuration
//     mistake could select: the wrapper reports ErrNotImplemented and the service refuses to start
//     with it (cmd/content-vault checks Kind() at startup).
//  3. **Health says so.** Kind() returns azure-key-vault only in a build that has one; this build's
//     Health() reports the backend as unimplemented, so a deployment report cannot accidentally
//     claim a KMS it does not have.
//
// What the cloud backend must provide, operation by operation. This is the contract, and it is
// deliberately stricter than "call wrapKey":
//
//	CurrentVersion(kekID)
//	  Key Vault: GET {vault}/keys/{name} — read the current version only. The response's key
//	  material must NOT be used; only the version identifier is.
//
//	Wrap(kekID, aad, dek) -> Wrapped
//	  Key Vault: POST {vault}/keys/{name}/{version}/wrapkey with alg RSA-OAEP-256 (or A256KW for an
//	  HSM-backed symmetric key), value = base64(aad || dek). The AAD must be *inside* the wrapped
//	  plaintext, because Key Vault's wrapKey has no AAD parameter — so the local wrapper's "seal
//	  with AAD" and the cloud's "wrap a blob that contains the AAD" are the same construction at
//	  different layers, and the version recorded on the row is the one Key Vault returns.
//
//	Unwrap(aad, wrapped)
//	  Key Vault: POST .../unwrapkey, then verify the AAD that comes back matches the row's tenant,
//	  object, key id and version. A mismatch is ErrAuthentication, not a decryption of the wrong
//	  object. The unwrap result exists only in this process's memory for the duration of one
//	  operation (§5.3).
//
//	NewVersion(kekID)
//	  Key Vault: POST {vault}/keys/{name}/rotate, or create a new version. Rotation must be
//	  customer-visible under mode 2 (§6.2: "the vendor and the customer, in the customer's control
//	  plane").
//
//	Destroy(kekID, reason)
//	  Key Vault: DELETE {vault}/keys/{name} (soft delete) followed by purge once the recovery window
//	  closes. **A4 applies**: the receipt must state the actual recovery window, and the service
//	  must not claim immediate destruction while a soft-deleted key is still recoverable. Under
//	  mode 2 and mode 3 the customer performs this, and the vendor's receipt records that the
//	  customer did.
//
// Modes 2 and 3 need more than the API surface:
//
//	customer_managed  a federated workload identity the customer can revoke, scoped to
//	                  wrapKey/unwrapKey on one key (never key read, never purge); every unwrap
//	                  recorded in the customer's own control plane, because that record is the
//	                  whole guarantee of §6.2 ("the vendor cannot read content without an unwrap
//	                  that the customer's own key store records").
//	customer_held     an HSM the key never leaves. Wrap and unwrap are *operations the customer's
//	                  HSM performs*; the vendor's process sends a blob and receives a blob. Key
//	                  destruction is the customer's act, and the vendor cannot prevent it.
//
// Until that exists and is exercised against a real vault, this type must not be wired into a
// deployment, and the service's health output must keep saying so.
type KMSKeyWrapper struct {
	// Endpoint is the vault URI the deployment would use. It is recorded so an operator sees which
	// vault was *requested*, and never as evidence that one was reached.
	Endpoint string

	// Mode is the custody mode this wrapper would serve: vendor, customer_managed or customer_held.
	Mode string
}

// NewKMS records an intended cloud backend without pretending to have one.
func NewKMS(endpoint, mode string) *KMSKeyWrapper {
	return &KMSKeyWrapper{Endpoint: endpoint, Mode: mode}
}

// Kind implements KeyWrapper. It names the backend the deployment asked for; Health() is what
// reports that it is not implemented.
func (k *KMSKeyWrapper) Kind() KeyKind { return KindAzureKeyVault }

// Health is the honest one-line status of this backend.
func (k *KMSKeyWrapper) Health() string {
	return fmt.Sprintf("azure-key-vault backend NOT IMPLEMENTED (endpoint=%q mode=%q): no network and no cloud SDK on this build host",
		k.Endpoint, k.Mode)
}

func (k *KMSKeyWrapper) unimplemented(op string) error {
	return fmt.Errorf("%w: %s (%s, endpoint %q)", ErrNotImplemented, op, k.Mode, k.Endpoint)
}

// CurrentVersion implements KeyWrapper by refusing.
func (k *KMSKeyWrapper) CurrentVersion(context.Context, string) (string, error) {
	return "", k.unimplemented("current version")
}

// Wrap implements KeyWrapper by refusing.
func (k *KMSKeyWrapper) Wrap(context.Context, string, AAD, []byte) (Wrapped, error) {
	return Wrapped{}, k.unimplemented("wrap")
}

// Unwrap implements KeyWrapper by refusing.
func (k *KMSKeyWrapper) Unwrap(context.Context, AAD, []byte) ([]byte, error) {
	return nil, k.unimplemented("unwrap")
}

// NewVersion implements KeyWrapper by refusing.
func (k *KMSKeyWrapper) NewVersion(context.Context, string) (string, error) {
	return "", k.unimplemented("rotate")
}

// Destroy implements KeyWrapper by refusing.
func (k *KMSKeyWrapper) Destroy(context.Context, string, string) error {
	return k.unimplemented("destroy")
}
