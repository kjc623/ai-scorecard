package signer

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotConfigured is returned by every KeyVaultSigner operation. It is deliberately a hard refusal
// rather than a fallback to a local authority: a deployment that asked for Key Vault and silently
// got an in-process CA would have a trust root nobody chose and no audit trail in the HSM.
var ErrNotConfigured = errors.New("signer: Key Vault is not configured in this build")

// KeyVaultSigner is the production signing seam (ADR 0020, Consequences: "a certificate-signing
// service behind a CertificateSigner interface (local CA in development, Key Vault in production)").
//
// The interface is ready for it and the wiring selects it when a vault URI is configured, but this
// build does not implement the Key Vault REST call: it takes a dependency this offline module cannot
// vendor (a managed-identity token source and a signing client), and faking a signature with a local
// key while claiming Key Vault custody is exactly the dishonesty the ADR's "do not fake it" rule
// forbids. It therefore refuses clearly and names the missing configuration.
type KeyVaultSigner struct {
	// VaultURI is the Key Vault / Managed HSM URI from SAC_KEYVAULT_URI. It is recorded, never a
	// credential, and is included in the refusal so an operator can see what was configured.
	VaultURI string
}

// Name implements CertificateSigner.
func (s *KeyVaultSigner) Name() string {
	if s.VaultURI == "" {
		return "keyvault (unconfigured)"
	}
	return "keyvault"
}

// Sign implements CertificateSigner. It always refuses until the vault client lands.
func (s *KeyVaultSigner) Sign(context.Context, Request) (Issued, error) {
	if s.VaultURI == "" {
		return Issued{}, fmt.Errorf("%w: no vault URI was configured (SAC_KEYVAULT_URI)", ErrNotConfigured)
	}
	return Issued{}, fmt.Errorf("%w: vault %q is configured but this build carries no Key Vault signing client; "+
		"a certificate-only deployment cannot issue leaves until it does, and refusing is safer than signing "+
		"with a key the HSM was supposed to hold", ErrNotConfigured, s.VaultURI)
}
