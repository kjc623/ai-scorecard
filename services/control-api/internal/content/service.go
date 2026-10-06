// Package content decides whether one event's prompt content may be uploaded, and hands a granted
// upload to content-vault.
//
// The device asks for a grant for one event (POST /v1/content/grant). The decision reads only
// server-side state: the device's own observation of that event, the tenant's ceiling, retention
// and daily content budget (the content stored today plus this object's declared size). A granted device then sends the content once (POST /v1/content);
// control-api checks the grant and the body's digest and forwards the body to content-vault, which
// claims the grant, encrypts the content and stores it. There is no bulk grant and no tenant-wide
// upload credential: every upload is bound to a decision about one event.
package content

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Denial reasons, the closed set ops.grant.denial_reason holds.
const (
	DenyModeNotPermitted = "mode_not_permitted"
	DenyRetentionExpired = "retention_expired"
	DenyOverBudget       = "over_budget"
)

// Decisions as ops.grant.decision stores them.
const (
	DecisionGranted = "granted"
	DecisionDenied  = "denied"
)

// GrantTTL is how long a granted upload stays open.
const GrantTTL = 15 * time.Minute

// EventContext is the server-side state a decision reads. None of it comes from the device.
type EventContext struct {
	// Found is false when this device has no observation of the event, which is what makes another
	// device's event unknown.
	Found          bool
	Kind           string
	CollectionMode string
	ExpiresAt      time.Time
	SubmissionID   string
	ContentState   string

	CeilingMode       string
	BudgetBytesPerDay int64
	BytesAddedToday   int64

	// Grant is the most recent grant for the event, when there is one.
	Grant *Grant
}

// Grant is one ops.grant row.
type Grant struct {
	GrantID      string
	EventID      string
	SubmissionID string
	DeviceID     string
	Decision     string
	DenialReason string
	ExpiresAt    time.Time
	UsedAt       *time.Time
}

// Store is the database this path needs. The SQL implementation runs every method in the tenant's
// row-level-security session.
type Store interface {
	EventContext(ctx context.Context, tenantID, deviceID, eventID string) (*EventContext, error)
	InsertGrant(ctx context.Context, tenantID string, g Grant) error
	Grant(ctx context.Context, tenantID, grantID string) (*Grant, error)
	// AddContentUsage adds stored content bytes to the tenant's usage for the day.
	AddContentUsage(ctx context.Context, tenantID string, bytes int64) error
}

// ErrGrantUnknown is returned by Store.Grant for a grant the tenant does not have.
var ErrGrantUnknown = errors.New("content: unknown grant")

// Vault stores granted content. stored is true when this call stored the content and false when
// it was already stored by an earlier attempt with the same grant and digest. A refusal is a
// *VaultError.
type Vault interface {
	Put(ctx context.Context, tenantID, eventID, grantID, rawDigest string, body []byte) (stored bool, err error)
}

// VaultError is content-vault refusing an upload: its HTTP status and error code.
type VaultError struct {
	Status int
	Code   string
}

func (e *VaultError) Error() string {
	return fmt.Sprintf("content-vault answered %d %s", e.Status, e.Code)
}

// Service decides grants and forwards uploads.
type Service struct {
	store Store
	vault Vault
	now   func() time.Time
}

// New builds the service.
func New(st Store, vault Vault) (*Service, error) {
	if st == nil || vault == nil {
		return nil, errors.New("content: a store and a vault are both required")
	}
	return &Service{store: st, vault: vault, now: time.Now}, nil
}

// Decide answers POST /v1/content/grant for an authenticated device.
func (s *Service) Decide(ctx context.Context, tenantID, deviceID string, req protocol.ContentGrantRequest) (*protocol.ContentGrantResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, err.Error())
	}
	if !store.IsUUID(req.EventID) {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "event_id is not a uuid")
	}
	ec, err := s.store.EventContext(ctx, tenantID, deviceID, req.EventID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if !ec.Found {
		return nil, apierr.New(http.StatusNotFound, string(protocol.ReasonUnknownEvent), "this device has no such event")
	}
	// An event not collected at M3 has no content to upload: refused, not denied.
	if ec.Kind != string(protocol.KindPrompt) || ec.CollectionMode != string(protocol.ModeM3) || req.CollectionMode != protocol.ModeM3 {
		return nil, apierr.New(http.StatusUnprocessableEntity, string(protocol.ReasonModeViolation),
			"the event was not collected at a mode that holds content")
	}
	if ec.ContentState == "uploaded" || ec.ContentState == "shredded" {
		return nil, apierr.New(http.StatusConflict, string(protocol.ReasonGrantConsumed), "the content of this event was already uploaded")
	}
	if req.RawSizeBytes > protocol.MaxContentObjectBytes {
		return nil, apierr.New(http.StatusRequestEntityTooLarge, string(protocol.ReasonOversize),
			"the declared object is over the per-object cap")
	}
	now := s.now().UTC()

	// A denial is terminal for the event, and an open grant is returned again rather than decided
	// a second time.
	if g := ec.Grant; g != nil {
		switch {
		case g.Decision == DecisionDenied:
			return denied(*g, now), nil
		case g.Decision == DecisionGranted && g.UsedAt != nil:
			return nil, apierr.New(http.StatusConflict, string(protocol.ReasonGrantConsumed), "the grant for this event was already used")
		case g.Decision == DecisionGranted && now.Before(g.ExpiresAt):
			return granted(*g), nil
		}
	}

	grantID, err := store.NewUUID()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	g := Grant{GrantID: grantID, EventID: req.EventID, SubmissionID: ec.SubmissionID, DeviceID: deviceID}
	if reason := deny(ec, req, now); reason != "" {
		g.Decision, g.DenialReason = DecisionDenied, reason
	} else {
		g.Decision, g.ExpiresAt = DecisionGranted, now.Add(GrantTTL)
	}
	if err := s.store.InsertGrant(ctx, tenantID, g); err != nil {
		return nil, apierr.Internal(err)
	}
	if g.Decision == DecisionDenied {
		return denied(g, now), nil
	}
	return granted(g), nil
}

// deny applies the decision's inputs in order and returns the first reason that holds, or "" to
// grant.
func deny(ec *EventContext, req protocol.ContentGrantRequest, now time.Time) string {
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

func denied(g Grant, now time.Time) *protocol.ContentGrantResponse {
	return &protocol.ContentGrantResponse{GrantID: g.GrantID, State: protocol.ContentGrantDenied, Reason: g.DenialReason, ExpiresAt: now}
}

func granted(g Grant) *protocol.ContentGrantResponse {
	return &protocol.ContentGrantResponse{
		GrantID: g.GrantID, State: protocol.ContentGrantGranted, ExpiresAt: g.ExpiresAt,
		MaxBytes: protocol.MaxContentObjectBytes,
	}
}

// Upload is one POST /v1/content: the content object and the headers that name its grant.
type Upload struct {
	GrantID   string
	EventID   string
	RawDigest string
	Body      []byte
}

// Upload checks that the grant is this device's grant for this event, granted and still open, and
// that the body is the bytes the device declared; then it hands the body to content-vault. A grant
// already used is still forwarded: content-vault accepts a retry of the same content and refuses
// anything else, so a device whose first answer was lost can safely send again.
func (s *Service) Upload(ctx context.Context, tenantID, deviceID string, u Upload) error {
	if !store.IsUUID(u.GrantID) || !store.IsUUID(u.EventID) {
		return apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation,
			"the grant id and event id headers must both be uuids")
	}
	if len(u.Body) == 0 || len(u.Body) > protocol.MaxContentObjectBytes {
		return apierr.New(http.StatusRequestEntityTooLarge, string(protocol.ReasonOversize),
			"a content object is between one byte and the per-object cap")
	}
	if u.RawDigest != protocol.RawDigest(u.Body) {
		return apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "the body does not match its raw digest")
	}
	g, err := s.store.Grant(ctx, tenantID, u.GrantID)
	if errors.Is(err, ErrGrantUnknown) || (err == nil && (g.DeviceID != deviceID || g.EventID != u.EventID)) {
		return apierr.New(http.StatusNotFound, apierr.CodeNotFound, "this device has no such grant for this event")
	}
	if err != nil {
		return apierr.Internal(err)
	}
	if g.Decision != DecisionGranted {
		return apierr.New(http.StatusForbidden, apierr.CodeForbidden, "the grant was not granted")
	}
	if g.UsedAt == nil && !s.now().Before(g.ExpiresAt) {
		return apierr.New(http.StatusGone, string(protocol.ReasonGrantExpired), "the grant has expired; ask for a new one")
	}
	stored, err := s.vault.Put(ctx, tenantID, u.EventID, u.GrantID, u.RawDigest, u.Body)
	if err != nil {
		return vaultRefusal(err)
	}
	// Usage is counted once, when the content is first stored: a retry the vault answers as a
	// replay adds nothing.
	if stored {
		if err := s.store.AddContentUsage(ctx, tenantID, int64(len(u.Body))); err != nil {
			return apierr.Internal(err)
		}
	}
	return nil
}

// vaultRefusal maps content-vault's answer to the device's. A conflict means the event's content
// is already stored, so the device stops sending it; a refusal of the grant is final; anything else
// is retryable.
func vaultRefusal(err error) error {
	var ve *VaultError
	if !errors.As(err, &ve) {
		return apierr.Internal(fmt.Errorf("content-vault: %w", err))
	}
	switch {
	case ve.Status == http.StatusConflict:
		return apierr.New(http.StatusConflict, string(protocol.ReasonGrantConsumed), "the content of this event is already stored")
	case ve.Status == http.StatusForbidden && ve.Code == string(protocol.ReasonGrantExpired):
		return apierr.New(http.StatusGone, string(protocol.ReasonGrantExpired), "the grant has expired; ask for a new one")
	case ve.Status == http.StatusForbidden:
		return apierr.New(http.StatusForbidden, apierr.CodeForbidden, "content-vault refused the grant")
	case ve.Status == http.StatusBadRequest || ve.Status == http.StatusRequestEntityTooLarge:
		return apierr.New(ve.Status, apierr.CodeSchemaViolation, "content-vault refused the content object")
	}
	return apierr.Internal(err)
}
