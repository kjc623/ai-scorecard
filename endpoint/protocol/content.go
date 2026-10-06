package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Content upload at collection mode M3.
//
// Prompt content stays on the device until the server grants its upload for one event. The device
// asks with POST /v1/content/grant; a granted decision names a grant id, and the device then
// sends the content once with POST /v1/content over its authenticated connection. control-api
// checks the grant and hands the content to content-vault, which encrypts and stores it.

// ContentGrantSchemaVersion is the grant request's schema_version.
const ContentGrantSchemaVersion = "1.0"

// Grant states. A denial is a successful decision and travels in a 200.
const (
	ContentGrantGranted = "granted"
	ContentGrantDenied  = "denied"
)

// ContentUploadPath is where the device sends granted content.
const ContentUploadPath = "/v1/content"

// MaxContentObjectBytes caps one uploaded content object: the prompt text the device extracted,
// or the request body as observed when it could not extract one. The device holds no content
// larger than this. Attachment bytes are never part of the object; each attachment's name, size
// and digest travel in the event envelope.
const MaxContentObjectBytes = 16 << 20

// Upload headers. HeaderContentRawDigest is the digest of the exact body bytes (RawDigest).
const (
	HeaderContentGrantID   = "X-Sac-Grant-Id"
	HeaderContentEventID   = "X-Sac-Event-Id"
	HeaderContentRawDigest = "X-Sac-Raw-Digest"
)

// Reason codes the content endpoints add to the error envelope.
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

// ContentGrantResponse is the 200 body of POST /v1/content/grant. GrantID and MaxBytes matter only
// when State is granted.
type ContentGrantResponse struct {
	GrantID   string    `json:"grant_id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxBytes  int64     `json:"max_bytes,omitempty"`
}

// RawDigest is the digest of an upload body, in the repository's digest spelling.
func RawDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
