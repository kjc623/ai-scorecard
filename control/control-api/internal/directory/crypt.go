package directory

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// Cipher seals the product's `*_enc` columns. It began as the seal for directory_object_id_enc,
// the one column that maps a pseudonymous user_ref to a real person; the enterprise-onboarding
// contract made it the scheme for every sealed column (the tenant's user-reference key, a SCIM
// resource as last provisioned, an OIDC client secret), so a deployment holds one directory key
// rather than one per column.
//
// The master key is per deployment, and each tenant gets its own key derived from it, so a row
// copied from one tenant's partition cannot be opened with another's derived key even if RLS were
// bypassed. AES-256-GCM gives confidentiality and an integrity tag: a tampered ciphertext fails to
// open rather than decrypting to a wrong value. The stored form is nonce || ciphertext.
//
// The key is a deployment secret. A key that is lost makes every sealed value unreadable: the
// mapping to a real person (subject export and erasure), the SCIM resources (an identity provider
// re-sends them on its next full cycle) and the tenant's user-reference key, which is the one loss
// that cannot be repaired by re-provisioning, because every device derives its user_ref from it.
type Cipher struct {
	master []byte
}

// NewCipher takes the deployment's 32-byte master key.
func NewCipher(master []byte) (*Cipher, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("directory: the directory key must be 32 bytes, got %d", len(master))
	}
	cp := make([]byte, len(master))
	copy(cp, master)
	return &Cipher{master: cp}, nil
}

// DecodeKey reads SAC_DIRECTORY_KEY's spelling: standard base64 of 32 bytes. It is here rather than
// in the binary so the HTTP service and the sync subcommand read the secret the same way.
func DecodeKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("directory: no directory key: set SAC_DIRECTORY_KEY to a base64 32-byte value")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("directory: the directory key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("directory: the directory key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// tenantKey derives a per-tenant key. HMAC-SHA256 is the KDF rather than HKDF so the build stays on
// the standard library's most basic primitives; the master key is already full entropy, which is
// what makes a single HMAC a sound KDF here. The label predates the other sealed columns and is
// kept, because changing it would orphan every value already sealed.
func (c *Cipher) tenantKey(tenantID string) []byte {
	mac := hmac.New(sha256.New, c.master)
	mac.Write([]byte("sac-directory-object-id\x00"))
	mac.Write([]byte(tenantID))
	return mac.Sum(nil)
}

func (c *Cipher) aead(tenantID string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(c.tenantKey(tenantID))
	if err != nil {
		return nil, fmt.Errorf("directory: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("directory: gcm: %w", err)
	}
	return aead, nil
}

// Seal encrypts one value for one tenant. An empty plaintext seals to nil, so a provider that has no
// identifier stores NULL rather than an encrypted empty string.
func (c *Cipher) Seal(tenantID, plaintext string) ([]byte, error) {
	return c.SealBytes(tenantID, []byte(plaintext))
}

// SealBytes is Seal for a binary value, such as a key.
func (c *Cipher) SealBytes(tenantID string, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	aead, err := c.aead(tenantID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("directory: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal.
func (c *Cipher) Open(tenantID string, sealed []byte) (string, error) {
	plaintext, err := c.OpenBytes(tenantID, sealed)
	return string(plaintext), err
}

// OpenBytes reverses SealBytes.
func (c *Cipher) OpenBytes(tenantID string, sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	aead, err := c.aead(tenantID)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, fmt.Errorf("directory: sealed value is shorter than a nonce")
	}
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("directory: open sealed value: %w", err)
	}
	return plaintext, nil
}
