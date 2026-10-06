// Package keyring encrypts and decrypts stored content.
//
// The keyring is a list of versioned 32-byte master keys, configured as
//
//	v2:<base64 key>,v1:<base64 key>
//
// The first entry encrypts; every entry decrypts, so a new master key is introduced by prepending
// it and an old one is retired only once no stored row names its version.
//
// Each tenant has its own content key, derived from a master key with HKDF-SHA256 (no salt, info
// "sac/content/v1/" followed by the tenant id). A stored object is AES-256-GCM under that key, with
// a random 12-byte nonce prepended to the GCM output and "<tenant id>/<object id>" as additional
// authenticated data, so ciphertext moved to another row or tenant does not open.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// KeySize is the length of a master key and of a derived tenant key.
const KeySize = 32

const (
	nonceSize  = 12
	infoPrefix = "sac/content/v1/"
)

// ErrUnknownVersion is returned by Open when the ciphertext names a master key the keyring does
// not hold.
var ErrUnknownVersion = errors.New("keyring: unknown key version")

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// Keyring holds the master keys. It is safe for concurrent use.
type Keyring struct {
	current string
	keys    map[string][]byte
}

// Parse reads a keyring specification. It refuses an empty keyring, a malformed entry, a key that
// is not exactly 32 bytes, and a version named twice.
func Parse(spec string) (*Keyring, error) {
	k := &Keyring{keys: map[string][]byte{}}
	for i, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("keyring: entry %d is empty", i+1)
		}
		version, encoded, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("keyring: entry %d is not <version>:<base64 key>", i+1)
		}
		version = strings.TrimSpace(version)
		if !versionPattern.MatchString(version) {
			return nil, fmt.Errorf("keyring: entry %d has version %q; a version is 1-32 of [A-Za-z0-9._-]", i+1, version)
		}
		if _, dup := k.keys[version]; dup {
			return nil, fmt.Errorf("keyring: version %q is named twice", version)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("keyring: key %q is not standard base64", version)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("keyring: key %q is %d bytes, not %d", version, len(key), KeySize)
		}
		if i == 0 {
			k.current = version
		}
		k.keys[version] = key
	}
	return k, nil
}

// Current is the version new content is encrypted under.
func (k *Keyring) Current() string { return k.current }

// Versions lists every version the keyring holds, current first and the rest sorted.
func (k *Keyring) Versions() []string {
	rest := slices.DeleteFunc(slices.Sorted(maps.Keys(k.keys)), func(v string) bool { return v == k.current })
	return append([]string{k.current}, rest...)
}

// Has reports whether the keyring can decrypt content stored under version.
func (k *Keyring) Has(version string) bool {
	_, ok := k.keys[version]
	return ok
}

// Seal encrypts plaintext for one object under the current master key and returns the version it
// used with the ciphertext (nonce followed by the GCM output).
func (k *Keyring) Seal(tenantID, objectID string, plaintext []byte) (string, []byte, error) {
	aead, err := k.aead(k.current, tenantID)
	if err != nil {
		return "", nil, err
	}
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return "", nil, fmt.Errorf("keyring: nonce: %w", err)
	}
	return k.current, aead.Seal(out, out, plaintext, aad(tenantID, objectID)), nil
}

// Open decrypts one object's ciphertext. It returns ErrUnknownVersion when the keyring does not
// hold version, and an error when the ciphertext does not authenticate for this tenant and object.
func (k *Keyring) Open(tenantID, objectID, version string, ciphertext []byte) ([]byte, error) {
	aead, err := k.aead(version, tenantID)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < nonceSize+aead.Overhead() {
		return nil, errors.New("keyring: ciphertext is shorter than a nonce and a tag")
	}
	plaintext, err := aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], aad(tenantID, objectID))
	if err != nil {
		return nil, errors.New("keyring: ciphertext does not authenticate for this tenant and object")
	}
	return plaintext, nil
}

func (k *Keyring) aead(version, tenantID string) (cipher.AEAD, error) {
	master, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownVersion, version)
	}
	if tenantID == "" {
		return nil, errors.New("keyring: no tenant")
	}
	key, err := TenantKey(master, tenantID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// TenantKey derives one tenant's content key from a master key.
func TenantKey(master []byte, tenantID string) ([]byte, error) {
	return hkdf.Key(sha256.New, master, nil, infoPrefix+tenantID, KeySize)
}

func aad(tenantID, objectID string) []byte {
	return []byte(tenantID + "/" + objectID)
}
