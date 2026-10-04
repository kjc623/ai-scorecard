// Package content is the grant half of the content path (docs/02-ingest-and-transport.md §3,
// §5.5, §10): control-api decides whether one event's content may be uploaded, relays the object
// key content-vault mints for a granted decision, and finalises a verified upload.
//
// The structural property it keeps is §10.5's: the only way content reaches storage is an upload
// credential minted by a decision about one specific event. There is no endpoint here that accepts
// content, no bulk grant and no tenant-wide credential. control-api mints no key and holds no unwrap
// right; it asks the vault and relays the answer.
package content

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

// Control-plane codes this path adds. They are the device-facing names of §5.5's error rows.
const (
	CodeUnknownEvent  = string(protocol.ReasonUnknownEvent)
	CodeGrantConsumed = string(protocol.ReasonGrantConsumed)
	CodeModeViolation = string(protocol.ReasonModeViolation)
	CodeOversize      = string(protocol.ReasonOversize)
)

// The four denial reasons of §10.2. ops.grant.denial_reason is the same closed set.
const (
	DenyModeNotPermitted = "mode_not_permitted"
	DenyRetentionExpired = "retention_expired"
	DenyOverBudget       = "over_budget"
)

// Grant decisions as ops.grant.decision stores them.
const (
	DecisionGranted = "granted"
	DecisionDenied  = "denied"
	DecisionVoided  = "voided"
)

// RetentionClass is the class a stored content object occupies (ref.retention_class).
const RetentionClass = "content"

// EventContext is the server-side state a decision reads (§10.1). None of it is supplied by the
// device.
type EventContext struct {
	// The event, as the write path recorded it for this device. Found is false when the device
	// has no such observation, which is what makes another device's event a 404.
	Found          bool
	Kind           string
	CollectionMode string
	ExpiresAt      time.Time
	SubmissionID   string
	ContentState   string

	// The tenant's ceiling and its content budget.
	CeilingMode       string
	BudgetBytesPerDay int64
	BytesAddedToday   int64

	// The most recent grant for this event, when there is one.
	Grant *Grant
}

// Grant is one row of ops.grant.
type Grant struct {
	GrantID         string
	EventID         string
	SubmissionID    string
	DeviceID        string
	Decision        string
	DenialReason    string
	ObjectID        string
	UploadExpiresAt time.Time
}

// Store is the database this path needs. The SQL implementation runs every method in the tenant's
// row-level-security session.
type Store interface {
	EventContext(ctx context.Context, tenantID, deviceID, eventID string) (*EventContext, error)
	InsertGrant(ctx context.Context, tenantID string, g Grant) error
	Grant(ctx context.Context, tenantID, grantID string) (*Grant, error)
	VoidGrant(ctx context.Context, tenantID, grantID string) error
	// RecordUpload marks the submission's content as uploaded and adds the object to the tenant's
	// content usage for the day.
	RecordUpload(ctx context.Context, tenantID, submissionID string, bytes int64) error
}

// ErrGrantUnknown is returned by Store.Grant for a grant id the tenant does not have.
var ErrGrantUnknown = errors.New("content: unknown grant")

// PreparedKey is the vault's answer to a prepare: the object key and its wrapped form.
type PreparedKey struct {
	ObjectKeyB64 string
	WrappedDEK   string
	KEKID        string
	KEKVersion   string
}

// StoredObject is what the finaliser tells the vault to record.
type StoredObject struct {
	ObjectID, SubmissionID, EventID string
	BlobPath, CiphertextSHA256      string
	PlaintextSizeBytes              int64
	WrappedDEK, KEKID, KEKVersion   string
	ExpiresAt                       time.Time
}

// Vault is content-vault, seen from the one component allowed to ask it for an object key.
type Vault interface {
	Prepare(ctx context.Context, tenantID, subject, objectID, submissionID, eventID string, expiresAt time.Time, plaintextSize int64) (*PreparedKey, error)
	Finalise(ctx context.Context, tenantID, subject string, obj StoredObject) error
}

// Config is the path's deployment configuration.
type Config struct {
	// UploadSigningKey authorises one upload: it signs the upload URL, and the same key
	// authenticates the upload store's finalise call. The storage layer holds the other copy.
	UploadSigningKey []byte
	// UploadTTL bounds the upload window (§10.3: at most 15 minutes).
	UploadTTL time.Duration
	// MaxObjectBytes caps one object, whatever the budget says.
	MaxObjectBytes int64
}

// Service decides grants and finalises uploads.
type Service struct {
	store Store
	vault Vault
	cfg   Config
	now   func() time.Time
}

// New builds the service. A missing signing key is refused: an upload URL nobody can verify is an
// upload path with no decision behind it.
func New(store Store, vault Vault, cfg Config) (*Service, error) {
	if store == nil || vault == nil {
		return nil, errors.New("content: a store and a vault are both required")
	}
	if len(cfg.UploadSigningKey) < 16 {
		return nil, errors.New("content: the upload signing key must be at least 16 bytes")
	}
	if cfg.UploadTTL <= 0 || cfg.UploadTTL > 15*time.Minute {
		cfg.UploadTTL = 15 * time.Minute
	}
	if cfg.MaxObjectBytes <= 0 {
		cfg.MaxObjectBytes = 64 << 20
	}
	return &Service{store: store, vault: vault, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Decide answers POST /v1/content/grant for an authenticated device. publicBase is the scheme and
// host the device reached this service on; the upload URL is built on it.
func (s *Service) Decide(ctx context.Context, tenantID, deviceID string, req protocol.ContentGrantRequest, publicBase string) (*protocol.ContentGrantResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, err.Error())
	}
	if !isUUID(req.EventID) {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "event_id is not a uuid")
	}
	ec, err := s.store.EventContext(ctx, tenantID, deviceID, req.EventID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if !ec.Found {
		return nil, apierr.New(http.StatusNotFound, CodeUnknownEvent, "this device has no such event")
	}
	// A device holding an M0, M1 or M2 event has no content to upload: refused, not denied.
	if ec.Kind != string(protocol.KindPrompt) || ec.CollectionMode != string(protocol.ModeM3) || req.CollectionMode != protocol.ModeM3 {
		return nil, apierr.New(http.StatusUnprocessableEntity, CodeModeViolation, "the event was not collected at a mode that holds content")
	}
	if ec.ContentState == "uploaded" || ec.ContentState == "shredded" {
		return nil, apierr.New(http.StatusConflict, CodeGrantConsumed, "the grant for this event was already consumed")
	}
	if req.RawSizeBytes > s.cfg.MaxObjectBytes {
		return nil, apierr.New(http.StatusRequestEntityTooLarge, CodeOversize, "the declared object is over the per-object cap")
	}
	now := s.now()

	// A denial is terminal for the event, and a live grant is returned again rather than re-decided.
	if g := ec.Grant; g != nil {
		switch {
		case g.Decision == DecisionDenied:
			return &protocol.ContentGrantResponse{GrantID: g.GrantID, State: protocol.ContentGrantDenied, Reason: g.DenialReason, ExpiresAt: now}, nil
		case g.Decision == DecisionGranted && now.Before(g.UploadExpiresAt):
			return s.granted(ctx, tenantID, deviceID, req, ec, *g, publicBase)
		}
	}

	if reason := s.deny(ec, req, now); reason != "" {
		g := Grant{GrantID: newUUID(), EventID: req.EventID, SubmissionID: ec.SubmissionID, DeviceID: deviceID, Decision: DecisionDenied, DenialReason: reason}
		if err := s.store.InsertGrant(ctx, tenantID, g); err != nil {
			return nil, apierr.Internal(err)
		}
		return &protocol.ContentGrantResponse{GrantID: g.GrantID, State: protocol.ContentGrantDenied, Reason: reason, ExpiresAt: now}, nil
	}

	g := Grant{
		GrantID: newUUID(), EventID: req.EventID, SubmissionID: ec.SubmissionID, DeviceID: deviceID,
		Decision: DecisionGranted, ObjectID: newUUID(), UploadExpiresAt: now.Add(s.cfg.UploadTTL),
	}
	resp, err := s.granted(ctx, tenantID, deviceID, req, ec, g, publicBase)
	if err != nil {
		return nil, err
	}
	if err := s.store.InsertGrant(ctx, tenantID, g); err != nil {
		return nil, apierr.Internal(err)
	}
	return resp, nil
}

// deny applies §10.1's inputs in order and returns the first reason that holds, or "" to grant.
// `not_policy_relevant` is not produced: the tenant retention criteria it is decided on (severity
// floor, allowlisted classes, logging-only tools) have no stored form yet, so every M3 match is
// treated as relevant.
func (s *Service) deny(ec *EventContext, req protocol.ContentGrantRequest, now time.Time) string {
	switch {
	case ec.CeilingMode != string(protocol.ModeM3):
		return DenyModeNotPermitted
	case !ec.ExpiresAt.IsZero() && !now.Before(ec.ExpiresAt):
		return DenyRetentionExpired
	case ec.BytesAddedToday+req.RawSizeBytes > ec.BudgetBytesPerDay:
		return DenyOverBudget
	}
	return ""
}

// granted asks the vault for the object key and assembles the granted response.
func (s *Service) granted(ctx context.Context, tenantID, deviceID string, req protocol.ContentGrantRequest, ec *EventContext, g Grant, publicBase string) (*protocol.ContentGrantResponse, error) {
	key, err := s.vault.Prepare(ctx, tenantID, "device:"+deviceID, g.ObjectID, ec.SubmissionID, req.EventID, ec.ExpiresAt, req.SizeBytes)
	if err != nil {
		return nil, apierr.Internal(fmt.Errorf("content: vault prepare: %w", err))
	}
	return &protocol.ContentGrantResponse{
		GrantID:   g.GrantID,
		State:     protocol.ContentGrantGranted,
		ExpiresAt: g.UploadExpiresAt,
		Upload: &protocol.ContentUpload{
			Method: http.MethodPut,
			URL:    s.uploadURL(publicBase, tenantID, g),
			Headers: map[string]string{
				protocol.HeaderContentGrantID:       g.GrantID,
				protocol.HeaderContentEventID:       req.EventID,
				protocol.HeaderContentWrappedKey:    key.WrappedDEK,
				protocol.HeaderContentKeyID:         key.KEKID,
				protocol.HeaderContentKeyVersion:    key.KEKVersion,
				protocol.HeaderContentPlaintextSize: strconv.FormatInt(req.SizeBytes, 10),
			},
		},
		Key: &protocol.ContentKey{
			ObjectKeyB64: key.ObjectKeyB64, WrappedKeyB64: key.WrappedDEK, KeyID: key.KEKID, Alg: protocol.ContentKeyAlg,
		},
		MaxBytes: req.RawSizeBytes,
	}, nil
}

// uploadURL is the single-object write credential (§10.3): one path, one grant, one expiry, signed
// so the storage layer can verify it without asking. Its shape is not part of the contract.
func (s *Service) uploadURL(publicBase, tenantID string, g Grant) string {
	exp := strconv.FormatInt(g.UploadExpiresAt.Unix(), 10)
	q := url.Values{}
	q.Set("tenant", tenantID)
	q.Set("grant", g.GrantID)
	q.Set("exp", exp)
	q.Set("sig", SignUpload(s.cfg.UploadSigningKey, tenantID, g.ObjectID, g.GrantID, exp))
	return strings.TrimRight(publicBase, "/") + "/v1/content/upload/" + g.ObjectID + "?" + q.Encode()
}

// SignUpload is the upload URL's signature. The storage layer recomputes it with its copy of the
// key.
func SignUpload(key []byte, tenantID, objectID, grantID, exp string) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "sac.upload.v1\n%s\n%s\n%s\n%s", tenantID, objectID, grantID, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignBody authenticates the storage layer's finalise call: an HMAC of the exact body.
func SignBody(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("sac.finalise.v1\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBody checks a finalise call's signature in constant time.
func (s *Service) VerifyBody(body []byte, signature string) bool {
	return hmac.Equal([]byte(SignBody(s.cfg.UploadSigningKey, body)), []byte(signature))
}

// UploadReport is what the storage layer says it received. RawDigest and SizeBytes are what it
// measured; DeclaredRawDigest and the key fields are the metadata the device wrote.
type UploadReport struct {
	TenantID           string `json:"tenant_id"`
	GrantID            string `json:"grant_id"`
	ObjectID           string `json:"object_id"`
	EventID            string `json:"event_id"`
	BlobPath           string `json:"blob_path"`
	RawDigest          string `json:"raw_digest"`
	DeclaredRawDigest  string `json:"declared_raw_digest"`
	SizeBytes          int64  `json:"size_bytes"`
	PlaintextSizeBytes int64  `json:"plaintext_size_bytes"`
	WrappedKeyB64      string `json:"wrapped_key_b64"`
	KeyID              string `json:"key_id"`
	KeyVersion         string `json:"key_version"`
}

// ErrUploadRejected means the staged object must be deleted: it matches no live grant, or it is
// not the bytes the device declared (§10.4). The grant is voided where one exists.
var ErrUploadRejected = errors.New("content: the upload does not match a live grant")

// Finalise promotes a verified upload: the grant must be live and name this object and event, and
// the bytes the store measured must be the bytes the device declared. Only then is the vault told
// to record the object. It returns the submission the object belongs to.
func (s *Service) Finalise(ctx context.Context, r UploadReport) (string, error) {
	g, err := s.store.Grant(ctx, r.TenantID, r.GrantID)
	if errors.Is(err, ErrGrantUnknown) {
		return "", fmt.Errorf("%w: unknown grant", ErrUploadRejected)
	}
	if err != nil {
		return "", err
	}
	if g.Decision != DecisionGranted || g.ObjectID != r.ObjectID || g.EventID != r.EventID {
		return "", fmt.Errorf("%w: the grant does not name this object", ErrUploadRejected)
	}
	ec, err := s.store.EventContext(ctx, r.TenantID, g.DeviceID, g.EventID)
	if err != nil {
		return "", err
	}
	if !ec.Found || ec.ContentState == "uploaded" {
		_ = s.store.VoidGrant(ctx, r.TenantID, r.GrantID)
		return "", fmt.Errorf("%w: a second write for the event", ErrUploadRejected)
	}
	if r.RawDigest == "" || r.RawDigest != r.DeclaredRawDigest || r.SizeBytes <= 0 || r.SizeBytes > s.cfg.MaxObjectBytes {
		_ = s.store.VoidGrant(ctx, r.TenantID, r.GrantID)
		return "", fmt.Errorf("%w: digest or size mismatch", ErrUploadRejected)
	}
	if err := s.vault.Finalise(ctx, r.TenantID, "device:"+g.DeviceID, StoredObject{
		ObjectID: g.ObjectID, SubmissionID: ec.SubmissionID, EventID: g.EventID,
		BlobPath: r.BlobPath, CiphertextSHA256: r.RawDigest, PlaintextSizeBytes: r.PlaintextSizeBytes,
		WrappedDEK: r.WrappedKeyB64, KEKID: r.KeyID, KEKVersion: r.KeyVersion, ExpiresAt: ec.ExpiresAt,
	}); err != nil {
		return "", fmt.Errorf("content: vault finalise: %w", err)
	}
	return ec.SubmissionID, s.store.RecordUpload(ctx, r.TenantID, ec.SubmissionID, r.SizeBytes)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the platform's randomness is gone; nothing issued after this would be safe
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
