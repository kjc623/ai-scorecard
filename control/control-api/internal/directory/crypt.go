package directory

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// Cipher seals directory_object_id_enc. The schema reserves exactly one column that maps a
// pseudonymous user_ref to a real person and says it is encrypted; this is the encryption.
//
// The master key is per deployment, and each tenant gets its own key derived from it, so a row
// copied from one tenant's partition cannot be opened with another's derived key even if RLS were
// bypassed. AES-256-GCM gives confidentiality and an integrity tag: a tampered ciphertext fails to
// open rather than decrypting to a wrong identifier. The stored form is nonce || ciphertext.
//
// The key is a deployment secret. A key that is lost makes every stored directory identifier
// unreadable, which only the subject-export and erasure paths use (task 13); the sync itself never
// reads the column back, so a lost key does not stop the sync, it only breaks the mapping to a real
// person.
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

// tenantKey derives a per-tenant key. HMAC-SHA256 is the KDF rather than HKDF so the build stays on
// the standard library's most basic primitives; the master key is already full entropy, which is
// what makes a single HMAC a sound KDF here.
func (c *Cipher) tenantKey(tenantID string) []byte {
	mac := hmac.New(sha256.New, c.master)
	mac.Write([]byte("sac-directory-object-id\x00"))
	mac.Write([]byte(tenantID))
	return mac.Sum(nil)
}

// Seal encrypts one directory identifier for one tenant. An empty plaintext seals to nil, so a
// provider that has no identifier stores NULL rather than an encrypted empty string.
func (c *Cipher) Seal(tenantID, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	block, err := aes.NewCipher(c.tenantKey(tenantID))
	if err != nil {
		return nil, fmt.Errorf("directory: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("directory: gcm: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("directory: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open reverses Seal. It exists for the subject-export and erasure paths this task does not build,
// and for the test that proves a sealed identifier round-trips.
func (c *Cipher) Open(tenantID string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(c.tenantKey(tenantID))
	if err != nil {
		return "", fmt.Errorf("directory: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("directory: gcm: %w", err)
	}
	if len(sealed) < aead.NonceSize() {
		return "", fmt.Errorf("directory: sealed value is shorter than a nonce")
	}
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("directory: open sealed value: %w", err)
	}
	return string(plaintext), nil
}
