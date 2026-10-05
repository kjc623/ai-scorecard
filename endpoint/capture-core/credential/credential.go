// Package credential is the device's per-device credential at rest: the issued x509 leaf (or the
// registered DPoP public key) plus the EC private key that never leaves the device. The file is
// sealed with AES-256-GCM under the key a capturespool.KeyProvider supplies — the same key and
// AEAD approach the spool uses, so a credential file and a spool directory share one
// key-protection story (ADR 0020 decision 3: the private key never leaves the device).
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// Credential is the device credential as it is sealed on disk. Mode decides which of CertPEM /
// ChainPEM / NotAfter (x509) or JWK (dpop) are populated; PrivateKey is present for both.
type Credential struct {
	Mode     protocol.AuthMode `json:"mode"`
	DeviceID string            `json:"device_id"`
	TenantID string            `json:"tenant_id"`
	Region   string            `json:"region,omitempty"`

	// HardwareIdentityHash is the enrolment idempotency key (C11), stored so a re-enrolment uses
	// the same value it was first issued under.
	HardwareIdentityHash string `json:"hardware_identity_hash"`

	// DeviceIdentity is the tenant's identity setting as the server stated it at enrolment
	// (ADR 0021). It is stored with the credential so a restart keeps acting on the setting the
	// tenant chose; an empty value means the server did not state one.
	DeviceIdentity protocol.DeviceIdentity `json:"device_identity,omitempty"`

	// UserRefKey is the tenant's user-reference key as the enrolment response carried it
	// (base64url, unpadded; contract §4). It is sealed with the credential because it is issued with
	// it and a restart must derive the same user_ref; empty means the server issued none.
	UserRefKey string `json:"user_ref_key,omitempty"`

	// PrivateKey is the EC private key in PEM "EC PRIVATE KEY" form. It never leaves the device.
	PrivateKey string `json:"private_key"`

	// x509 mode.
	CertPEM  string    `json:"cert_pem,omitempty"`
	ChainPEM []string  `json:"chain_pem,omitempty"`
	NotAfter time.Time `json:"not_after,omitempty"`

	// dpop mode.
	JWK *protocol.JWK `json:"jwk,omitempty"`
}

// Validate rejects a credential the transport could not use, so a corrupt or half-written
// credential is refused at load rather than failing mid-request.
func (c *Credential) Validate() error {
	if c == nil {
		return errors.New("credential: nil credential")
	}
	if !c.Mode.Valid() {
		return fmt.Errorf("credential: mode %q outside the closed set {x509,dpop}", c.Mode)
	}
	if c.DeviceID == "" {
		return errors.New("credential: device_id is required")
	}
	if c.TenantID == "" {
		return errors.New("credential: tenant_id is required")
	}
	if c.PrivateKey == "" {
		return errors.New("credential: private_key is required")
	}
	switch c.Mode {
	case protocol.AuthModeX509:
		if c.CertPEM == "" {
			return errors.New("credential: x509 credential has no cert_pem")
		}
	case protocol.AuthModeDPoP:
		if c.JWK == nil {
			return errors.New("credential: dpop credential has no jwk")
		}
	}
	return nil
}

// ECPrivateKey parses the stored private key. The private half never leaves the process.
func (c *Credential) ECPrivateKey() (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(c.PrivateKey))
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("credential: private_key is not an EC PRIVATE KEY PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("credential: parsing private key: %w", err)
	}
	return key, nil
}

// KeyPair builds the tls.Certificate the x509 transport presents, from the issued leaf and the
// stored private key.
func (c *Credential) KeyPair() (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(c.CertPEM), []byte(c.PrivateKey))
}

// Store seals the credential file. It is opened with the same KeyProvider the spool uses, so the
// credential is protected by exactly the key-protection story the spool reports.
type Store struct {
	path   string
	aead   cipher.AEAD
	sealed bool
}

const (
	magic     = "SACR"
	versionV1 = 1
	// headerLen is magic(4) + version(1) + nonce(12).
	headerLen = 4 + 1 + 12
)

// Open builds a Store for path under the key supplied by keys. It does not read the file: Load and
// Save are the I/O, so a caller can decide whether to load or to enrol-and-save.
func Open(path string, keys spool.KeyProvider) (*Store, error) {
	if path == "" {
		return nil, errors.New("credential: path is required")
	}
	if keys == nil {
		return nil, errors.New("credential: keys is required")
	}
	key, err := keys.Key()
	if err != nil {
		return nil, fmt.Errorf("credential: obtaining key: %w", err)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, aead: aead, sealed: keys.Sealed()}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential: AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: GCM: %w", err)
	}
	return aead, nil
}

// Sealed reports whether the key material is platform-protected (mirrors KeyProvider.Sealed).
func (s *Store) Sealed() bool { return s.sealed }

// Path returns the credential file path.
func (s *Store) Path() string { return s.path }

// Load reads and unseals the credential. It returns os.ErrNotExist when there is no file yet,
// which a caller treats as "not enrolled".
func (s *Store) Load() (*Credential, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	return s.unseal(raw)
}

// Save seals and atomically replaces the credential file, so a crash leaves either the old file or
// the new one, never a torn credential.
func (s *Store) Save(c *Credential) error {
	if c == nil {
		return errors.New("credential: nil credential")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("credential: encoding: %w", err)
	}
	sealed, err := s.seal(plain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("credential: creating directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cred-*")
	if err != nil {
		return fmt.Errorf("credential: temp file: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		return fmt.Errorf("credential: writing: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("credential: flushing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credential: closing: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("credential: renaming into place: %w", err)
	}
	return nil
}

func (s *Store) seal(plain []byte) ([]byte, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("credential: nonce: %w", err)
	}
	aad := append([]byte(nil), magic...)
	aad = append(aad, versionV1)
	out := make([]byte, 0, headerLen+len(plain)+s.aead.Overhead())
	out = append(out, magic...)
	out = append(out, versionV1)
	out = append(out, nonce[:]...)
	return s.aead.Seal(out, nonce[:], plain, aad), nil
}

func (s *Store) unseal(raw []byte) (*Credential, error) {
	if len(raw) < headerLen+s.aead.Overhead() {
		return nil, errors.New("credential: file is too short to be a sealed credential")
	}
	if string(raw[:4]) != magic {
		return nil, errors.New("credential: bad magic: not a credential file")
	}
	if raw[4] != versionV1 {
		return nil, fmt.Errorf("credential: unsupported version %d", raw[4])
	}
	var nonce [12]byte
	copy(nonce[:], raw[5:17])
	aad := append([]byte(nil), magic...)
	aad = append(aad, versionV1)
	plain, err := s.aead.Open(nil, nonce[:], raw[17:], aad)
	if err != nil {
		return nil, errors.New("credential: failed authentication (tampered, or the wrong key)")
	}
	var c Credential
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, fmt.Errorf("credential: decoding: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
