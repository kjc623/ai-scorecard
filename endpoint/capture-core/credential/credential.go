// Package credential is the device credential at rest: the issued X.509 leaf and its chain, plus
// the EC private key that never leaves the device. The file is sealed with AES-256-GCM under the
// spool key and written with the state directory's protection.
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
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// Credential is the device credential as it is sealed on disk.
type Credential struct {
	DeviceID string `json:"device_id"`
	TenantID string `json:"tenant_id"`
	Region   string `json:"region,omitempty"`

	// HardwareIdentityHash is the enrolment idempotency key, kept so a re-enrolment presents the
	// value the device was first issued under.
	HardwareIdentityHash string `json:"hardware_identity_hash"`

	// DeviceIdentity is the tenant's identity setting as the server stated it at enrolment, so a
	// restart keeps acting on it. Empty means the server did not state one.
	DeviceIdentity protocol.DeviceIdentity `json:"device_identity,omitempty"`

	// UserRefKey is the tenant's user-reference key as the enrolment response carried it
	// (base64url, unpadded). A restart must derive the same user_ref, so it is kept with the
	// credential it was issued with. Empty means the server issued none.
	UserRefKey string `json:"user_ref_key,omitempty"`

	// PrivateKey is the EC private key in PEM "EC PRIVATE KEY" form.
	PrivateKey string `json:"private_key"`

	CertPEM  string    `json:"cert_pem"`
	ChainPEM []string  `json:"chain_pem,omitempty"`
	NotAfter time.Time `json:"not_after"`
}

// Validate rejects a credential the transport could not use, so a corrupt or half-written
// credential is refused at load rather than failing mid-request.
func (c *Credential) Validate() error {
	switch {
	case c == nil:
		return errors.New("credential: nil credential")
	case c.DeviceID == "":
		return errors.New("credential: device_id is required")
	case c.TenantID == "":
		return errors.New("credential: tenant_id is required")
	case c.PrivateKey == "":
		return errors.New("credential: private_key is required")
	case c.CertPEM == "":
		return errors.New("credential: cert_pem is required")
	}
	return nil
}

// ECPrivateKey parses the stored private key.
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

// KeyPair builds the client certificate the transport presents: the issued leaf, its chain and
// the stored private key.
func (c *Credential) KeyPair() (tls.Certificate, error) {
	certPEM := c.CertPEM
	for _, p := range c.ChainPEM {
		certPEM += "\n" + p
	}
	return tls.X509KeyPair([]byte(certPEM), []byte(c.PrivateKey))
}

// Expired reports whether the leaf has passed its NotAfter at now. A zero NotAfter is no stated
// expiry, not an expiry at the epoch.
func (c *Credential) Expired(now time.Time) bool {
	return c != nil && !c.NotAfter.IsZero() && !now.Before(c.NotAfter)
}

// RenewAt is when the device rotates its certificate: two thirds of the way through the leaf's
// validity, while the current certificate can still authenticate the rotation. A leaf that cannot
// be parsed renews a day before NotAfter.
func (c *Credential) RenewAt() time.Time {
	if block, _ := pem.Decode([]byte(c.CertPEM)); block != nil {
		if leaf, err := x509.ParseCertificate(block.Bytes); err == nil && leaf.NotAfter.After(leaf.NotBefore) {
			return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
		}
	}
	return c.NotAfter.Add(-24 * time.Hour)
}

// Store seals the credential file.
type Store struct {
	path string
	aead cipher.AEAD
}

const (
	magic     = "SACR"
	versionV1 = 1
	// headerLen is magic(4) + version(1) + nonce(12).
	headerLen = 4 + 1 + 12
)

// Open builds a Store for path under key (32 bytes). It does not read the file: Load and Save are
// the I/O, so a caller can decide whether to load or to enrol and save.
func Open(path string, key []byte) (*Store, error) {
	if path == "" {
		return nil, errors.New("credential: path is required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential: AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: GCM: %w", err)
	}
	return &Store{path: path, aead: aead}, nil
}

// Path returns the credential file path.
func (s *Store) Path() string { return s.path }

// Load reads and unseals the credential. It returns an error satisfying
// errors.Is(err, fs.ErrNotExist) when there is no file yet, which a caller treats as not
// enrolled.
func (s *Store) Load() (*Credential, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	return s.unseal(raw)
}

// Save seals the credential and atomically replaces the file, so a crash leaves the old
// credential or the new one, never a torn one.
func (s *Store) Save(c *Credential) error {
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
	if err := state.WriteFile(s.path, sealed); err != nil {
		return fmt.Errorf("credential: writing %s: %w", s.path, err)
	}
	return nil
}

func aad() []byte { return append([]byte(magic), versionV1) }

func (s *Store) seal(plain []byte) ([]byte, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("credential: nonce: %w", err)
	}
	out := make([]byte, 0, headerLen+len(plain)+s.aead.Overhead())
	out = append(out, magic...)
	out = append(out, versionV1)
	out = append(out, nonce[:]...)
	return s.aead.Seal(out, nonce[:], plain, aad()), nil
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
	plain, err := s.aead.Open(nil, raw[5:17], raw[17:], aad())
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
