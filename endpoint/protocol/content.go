package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// The content grant of docs/02-ingest-and-transport.md §5.5 and §10: the device asks whether one
// event's content may be uploaded, and a granted decision carries the only means to do it — one
// upload URL and one object key. These shapes are shared by the device, which sends the request
// and seals the object, control-api, which decides, and content-vault, which opens the object for
// an approved retrieval.

// ContentGrantSchemaVersion is the grant request's schema_version.
const ContentGrantSchemaVersion = "1.0"

// Grant states. A denial is a successful decision and travels in a 200 (§5.5).
const (
	ContentGrantGranted = "granted"
	ContentGrantDenied  = "denied"
)

// ContentKeyAlg is the only object-key algorithm (§10.4).
const ContentKeyAlg = "A256GCM"

// Upload metadata headers. The grant response names the ones the server decided, and the device
// repeats them verbatim; HeaderContentRawDigest is the one the device computes, because only it
// has seen the bytes it is about to write.
const (
	HeaderContentGrantID       = "X-Sac-Grant-Id"
	HeaderContentEventID       = "X-Sac-Event-Id"
	HeaderContentWrappedKey    = "X-Sac-Wrapped-Key"
	HeaderContentKeyID         = "X-Sac-Key-Id"
	HeaderContentKeyVersion    = "X-Sac-Key-Version"
	HeaderContentPlaintextSize = "X-Sac-Plaintext-Size"
	HeaderContentRawDigest     = "X-Sac-Raw-Digest"
)

// Reason codes the grant endpoint adds to the §5 error envelope.
const (
	ReasonUnknownEvent  ReasonCode = "unknown_event"
	ReasonGrantConsumed ReasonCode = "grant_consumed"
	ReasonGrantExpired  ReasonCode = "grant_expired"
)

// ContentGrantRequest is the body of POST /v1/content/grant. It carries no tenant, no device and
// no case reference: the first two come from the credential, and the third is not the device's to
// supply.
type ContentGrantRequest struct {
	SchemaVersion   string         `json:"schema_version"`
	EventID         string         `json:"event_id"`
	CollectionMode  CollectionMode `json:"collection_mode"`
	ContentDigest   string         `json:"content_digest"`
	SizeBytes       int64          `json:"size_bytes"`
	AttachmentCount int            `json:"attachment_count"`
	RawSizeBytes    int64          `json:"raw_size_bytes"`
	PolicyRuleID    string         `json:"policy_rule_id"`
}

// Validate checks the request's own shape; whether the event exists and what the tenant permits
// are the server's decisions.
func (r ContentGrantRequest) Validate() error {
	switch {
	case r.SchemaVersion != ContentGrantSchemaVersion:
		return fmt.Errorf("protocol: content grant schema_version %q is not %q", r.SchemaVersion, ContentGrantSchemaVersion)
	case r.EventID == "":
		return errors.New("protocol: content grant names no event_id")
	case !r.CollectionMode.Valid():
		return fmt.Errorf("protocol: content grant collection_mode %q is outside the closed set", r.CollectionMode)
	case r.SizeBytes < 0 || r.RawSizeBytes < 0 || r.AttachmentCount < 0:
		return errors.New("protocol: content grant sizes must not be negative")
	}
	return nil
}

// ContentGrantResponse is the 200 body. Upload and Key are present only when State is granted.
type ContentGrantResponse struct {
	GrantID   string         `json:"grant_id"`
	State     string         `json:"state"`
	Reason    string         `json:"reason,omitempty"`
	ExpiresAt time.Time      `json:"expires_at"`
	Upload    *ContentUpload `json:"upload,omitempty"`
	Key       *ContentKey    `json:"key,omitempty"`
	MaxBytes  int64          `json:"max_bytes,omitempty"`
}

// ContentUpload is the single write the grant permits. The URL is opaque and used verbatim.
type ContentUpload struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ContentKey is the object key: plaintext for the device to seal with, and the wrapped form the
// server keeps. The plaintext key is not a disclosure — the device already holds the plaintext it
// is about to encrypt (§10.4).
type ContentKey struct {
	ObjectKeyB64  string `json:"object_key_b64"`
	WrappedKeyB64 string `json:"wrapped_key_b64"`
	KeyID         string `json:"key_id"`
	Alg           string `json:"alg"`
}

// SealContent encrypts one event's content under its object key: AES-256-GCM, a fresh nonce
// prepended to the ciphertext, and the event id as additional data so an object served against
// another event fails to open.
func SealContent(objectKey []byte, eventID string, plaintext []byte) ([]byte, error) {
	gcm, err := contentAEAD(objectKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, []byte(eventID)), nil
}

// OpenContent is the inverse of SealContent.
func OpenContent(objectKey []byte, eventID string, sealed []byte) ([]byte, error) {
	gcm, err := contentAEAD(objectKey)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("protocol: sealed content is shorter than its nonce")
	}
	return gcm.Open(nil, sealed[:n], sealed[n:], []byte(eventID))
}

// SealedContentSize is the size of the object SealContent produces for a plaintext of n bytes.
func SealedContentSize(n int64) int64 { return n + 12 + 16 }

// RawDigest is the upload's raw_digest: the digest of the exact bytes written (§10.4), in the one
// spelling the repository uses for digests.
func RawDigest(sealed []byte) string {
	sum := sha256.Sum256(sealed)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func contentAEAD(objectKey []byte) (cipher.AEAD, error) {
	if len(objectKey) != 32 {
		return nil, fmt.Errorf("protocol: object key is %d bytes, want 32", len(objectKey))
	}
	block, err := aes.NewCipher(objectKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
