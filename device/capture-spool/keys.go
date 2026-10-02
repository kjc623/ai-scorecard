package spool

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the spool key length: 32 bytes, AES-256.
const KeySize = 32

// KeyProvider supplies the per-device spool key (docs/01-collectors.md §12). The key is
// never written into the spool directory by this package, and no implementation here
// derives it from anything inside the spool.
//
// The production wrapping is the platform's key protection — DPAPI scoped to the service
// account on Windows, Keychain on macOS (§12, ASSUMPTION A1). That is the seam FuncKeyProvider
// exists for: a capture-core build supplies a function that unwraps the key with the platform
// protector, and reports Sealed() == true. This package deliberately ships no DPAPI or
// Keychain call, because the offline host cannot exercise either; shipping an untested
// platform call and reporting EncryptionKeySealed would be a claim, not a fact.
type KeyProvider interface {
	// Key returns the current 32-byte spool key. It is called on Open; the returned slice
	// is copied and the caller may reuse it.
	Key() ([]byte, error)

	// Sealed reports whether the key material is protected by the platform's key
	// protection rather than readable as a plaintext key file. It is surfaced as
	// SpoolStats.EncryptionKeySealed. A plaintext key file and an in-memory test key are
	// both unsealed, and saying so is the point of the field.
	Sealed() bool
}

// MemoryKeyProvider holds a key in process memory. It is for tests and for a device whose
// key is unwrapped from platform protection at startup; the key is never persisted.
type MemoryKeyProvider struct {
	key []byte
}

// NewMemoryKeyProvider copies key, which must be KeySize bytes.
func NewMemoryKeyProvider(key []byte) (*MemoryKeyProvider, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("spool: key must be %d bytes, got %d", KeySize, len(key))
	}
	k := make([]byte, KeySize)
	copy(k, key)
	return &MemoryKeyProvider{key: k}, nil
}

// NewRandomMemoryKeyProvider mints a fresh random key.
func NewRandomMemoryKeyProvider() (*MemoryKeyProvider, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("spool: minting spool key: %w", err)
	}
	return NewMemoryKeyProvider(k)
}

func (p *MemoryKeyProvider) Key() ([]byte, error) {
	k := make([]byte, KeySize)
	copy(k, p.key)
	return k, nil
}

// Sealed reports false: an in-memory key is not platform-protected, and the health report
// should say so rather than imply protection that does not exist.
func (p *MemoryKeyProvider) Sealed() bool { return false }

// FileKeyProvider reads the spool key from a file that must live outside the spool
// directory. It is the offline development and test provider; it is *not* the production
// wrapping, and it reports Sealed() == false for that reason.
type FileKeyProvider struct {
	path string
}

// NewFileKeyProvider opens or creates a key file at path, refusing any path that resolves
// inside spoolDir. The refusal is the requirement being enforced ("a key that is never
// written to the spool directory"), not advice: a key beside its ciphertext protects
// nothing.
func NewFileKeyProvider(path, spoolDir string) (*FileKeyProvider, error) {
	absKey, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("spool: resolving key path: %w", err)
	}
	absSpool, err := filepath.Abs(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("spool: resolving spool directory: %w", err)
	}
	if within(absSpool, absKey) {
		return nil, fmt.Errorf("spool: refusing key file %s: it is inside the spool directory %s, and a key beside its ciphertext is not encryption at rest", absKey, absSpool)
	}
	p := &FileKeyProvider{path: absKey}
	key, err := p.load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := p.create(); err != nil {
			return nil, err
		}
		return p, nil
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("spool: key file %s holds %d bytes, want %d", p.path, len(key), KeySize)
	}
	return p, nil
}

// Path returns the absolute key file path, for a test that has to prove the key is not in
// the spool directory.
func (p *FileKeyProvider) Path() string { return p.path }

func (p *FileKeyProvider) load() ([]byte, error) {
	return os.ReadFile(p.path)
}

func (p *FileKeyProvider) create() error {
	if dir := filepath.Dir(p.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("spool: creating key directory: %w", err)
		}
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("spool: minting spool key: %w", err)
	}
	// O_EXCL: two processes racing to enrol must not end up with two keys and one spool.
	f, err := os.OpenFile(p.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // another process won the race; Key() will read its key
		}
		return fmt.Errorf("spool: creating key file: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return fmt.Errorf("spool: writing key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("spool: flushing key file: %w", err)
	}
	return f.Close()
}

func (p *FileKeyProvider) Key() ([]byte, error) {
	key, err := os.ReadFile(p.path)
	if err != nil {
		return nil, fmt.Errorf("spool: reading spool key: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("spool: key file holds %d bytes, want %d", len(key), KeySize)
	}
	return key, nil
}

// Sealed reports false: a plaintext key file is not platform key protection.
func (p *FileKeyProvider) Sealed() bool { return false }

// FuncKeyProvider adapts a function to a KeyProvider. It is the seam for the platform
// wrapping of §12 (DPAPI / Keychain): the function unwraps the key with the platform
// protector and passes sealed=true only when that is genuinely what happened.
type FuncKeyProvider struct {
	Fn     func() ([]byte, error)
	IsSealed bool
}

// Key calls the underlying function and checks the length.
func (p FuncKeyProvider) Key() ([]byte, error) {
	if p.Fn == nil {
		return nil, errors.New("spool: nil key function")
	}
	k, err := p.Fn()
	if err != nil {
		return nil, err
	}
	if len(k) != KeySize {
		return nil, fmt.Errorf("spool: key must be %d bytes, got %d", KeySize, len(k))
	}
	return k, nil
}

// Sealed reports what the provider was told about its own key protection.
func (p FuncKeyProvider) Sealed() bool { return p.IsSealed }

// within reports whether path is inside dir (or equal to it), comparing cleaned absolute
// paths. It is used to refuse a key file inside the spool directory.
func within(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if strings.EqualFold(dir, path) {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return false
	}
	return !filepath.IsAbs(rel)
}
