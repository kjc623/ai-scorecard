// Package vault holds content-vault's rules: storing content under a single-use upload grant,
// minting and redeeming single-use retrieval grants, and searching the content index.
//
// Content is encrypted before it reaches the database (package keyring), and this service is the
// only component that can decrypt it. Every operation runs in one tenant-scoped transaction, and
// every read of content or of the index writes its audit row in that same transaction, so nothing
// is served unless its audit row commits.
package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// Audit actions this service writes.
const (
	ActionContentStored      = "content_stored"
	ActionRetrievalGranted   = "content_retrieval_granted"
	ActionRetrievalRefused   = "content_retrieval_refused"
	ActionRetrievalRedeemed  = "content_retrieval_redeemed"
	ActionSearch             = "content_search"
	ActionSubjectExport      = "content_subject_export"
	retentionClassContent    = "content"
	defaultRetrievalGrantTTL = 5 * time.Minute
)

// Config configures a Service.
type Config struct {
	Store store.Store
	Keys  *keyring.Keyring
	// RetrievalURLBase is the origin a browser redeems a retrieval URL on. Empty mints a path the
	// caller resolves against its own origin.
	RetrievalURLBase string
	// RetrievalGrantTTL is how long a minted retrieval URL stays redeemable; five minutes when zero.
	RetrievalGrantTTL time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
}

// Service is the vault.
type Service struct {
	store            store.Store
	keys             *keyring.Keyring
	retrievalURLBase string
	retrievalTTL     time.Duration
	now              func() time.Time
	log              *slog.Logger
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Keys == nil {
		return nil, errors.New("vault: a store and a keyring are required")
	}
	if cfg.RetrievalGrantTTL <= 0 {
		cfg.RetrievalGrantTTL = defaultRetrievalGrantTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{
		store: cfg.Store, keys: cfg.Keys, retrievalURLBase: strings.TrimRight(cfg.RetrievalURLBase, "/"),
		retrievalTTL: cfg.RetrievalGrantTTL, now: cfg.Now, log: cfg.Logger,
	}, nil
}

// UploadRequest is one content object control-api forwards under an upload grant.
type UploadRequest struct {
	TenantID  string
	EventID   string
	GrantID   string
	RawDigest string // protocol.RawDigest of Content, as the device declared it
	Content   []byte
}

// UploadResult describes the stored object.
type UploadResult struct {
	ObjectID  string
	EventID   string
	GrantID   string
	RawDigest string
	SizeBytes int
	ExpiresAt time.Time
	// IndexedPrompt and IndexedAttachmentNames count the search rows written.
	IndexedPrompt          int
	IndexedAttachmentNames int
	// Replayed is true when this exact upload was already stored and nothing new was written.
	Replayed bool
}

// Upload stores one content object. In one transaction it claims the grant, encrypts and inserts
// the object, marks the submission's content as uploaded, writes the search rows the tenant's tier
// permits, and writes the audit row. Repeating
// an upload that was already stored under the same grant with the same digest succeeds without
// writing anything.
func (s *Service) Upload(ctx context.Context, req UploadRequest) (UploadResult, error) {
	if len(req.Content) > protocol.MaxContentObjectBytes {
		return UploadResult{}, deny(ReasonInvalidRequest, "the content object is %d bytes, over the %d-byte limit", len(req.Content), protocol.MaxContentObjectBytes)
	}
	if got := protocol.RawDigest(req.Content); got != req.RawDigest {
		return UploadResult{}, deny(ReasonInvalidRequest, "the body's digest is %s, not the declared %s", got, req.RawDigest)
	}
	now := s.now().UTC()
	var res UploadResult
	err := s.store.InTenant(ctx, req.TenantID, func(tx store.Tx) error {
		t, err := tenant(ctx, tx)
		if err != nil {
			return err
		}
		if !t.IngestEnabled || t.Status == "closed" || t.Status == "offboarding" {
			return deny(ReasonTenantNotPermitted, "tenant %s is %s with ingest disabled or closing; no content is accepted", t.TenantID, t.Status)
		}
		g, err := tx.ClaimGrant(ctx, req.GrantID, req.EventID, now)
		if errors.Is(err, store.ErrNotFound) {
			res, err = s.unclaimable(ctx, tx, req, now)
			return err
		}
		if err != nil {
			return err
		}
		uc, err := tx.UploadContext(ctx, g.SubmissionID)
		if err != nil {
			return err
		}
		c := store.Content{
			TenantID: req.TenantID, ObjectID: uuid.New().String(), EventID: req.EventID,
			SubmissionID: g.SubmissionID, GrantID: req.GrantID, PlaintextSizeBytes: len(req.Content),
			RawDigest: req.RawDigest, RetentionClass: retentionClassContent, PromptKind: uc.PromptKind,
			CreatedAt: now, ExpiresAt: now.AddDate(0, 0, uc.RetentionDays),
		}
		if c.KeyVersion, c.Ciphertext, err = s.keys.Seal(c.TenantID, c.ObjectID, req.Content); err != nil {
			return err
		}
		if err := tx.InsertContent(ctx, c); errors.Is(err, store.ErrContentExists) {
			return deny(ReasonAlreadyStored, "event %s already has stored content", req.EventID)
		} else if err != nil {
			return err
		}
		if c.SubmissionID != "" {
			if err := tx.MarkUploaded(ctx, c.SubmissionID); err != nil {
				return err
			}
		}
		prompt, names, err := s.index(ctx, tx, t.ContentSearch, c, req.Content)
		if err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, store.AuditEntry{
			ActorType: "device", ActorID: g.DeviceID, Action: ActionContentStored,
			ObjectType: "content", ObjectID: c.ObjectID, OccurredAt: now,
			Detail: map[string]any{
				"event_id": c.EventID, "grant_id": c.GrantID, "submission_id": c.SubmissionID,
				"raw_digest": c.RawDigest, "plaintext_size_bytes": c.PlaintextSizeBytes,
				"key_version": c.KeyVersion, "expires_at": c.ExpiresAt.Format(time.RFC3339),
				"indexed_prompt": prompt, "indexed_attachment_names": names, "via": "control-api",
			},
		}); err != nil {
			return err
		}
		res = UploadResult{
			ObjectID: c.ObjectID, EventID: c.EventID, GrantID: c.GrantID, RawDigest: c.RawDigest,
			SizeBytes: c.PlaintextSizeBytes, ExpiresAt: c.ExpiresAt,
			IndexedPrompt: prompt, IndexedAttachmentNames: names,
		}
		return nil
	})
	return res, err
}

// unclaimable explains a grant that could not be claimed. The one success is a retry of an upload
// that is already stored under this grant with this digest.
func (s *Service) unclaimable(ctx context.Context, tx store.Tx, req UploadRequest, now time.Time) (UploadResult, error) {
	stored, err := tx.ContentForEvent(ctx, req.EventID)
	switch {
	case err == nil && stored.GrantID == req.GrantID && stored.RawDigest == req.RawDigest:
		return UploadResult{
			ObjectID: stored.ObjectID, EventID: stored.EventID, GrantID: stored.GrantID, RawDigest: stored.RawDigest,
			SizeBytes: stored.PlaintextSizeBytes, ExpiresAt: stored.ExpiresAt, Replayed: true,
		}, nil
	case err == nil:
		return UploadResult{}, deny(ReasonAlreadyStored, "event %s already has stored content that this upload does not match", req.EventID)
	case !errors.Is(err, store.ErrNotFound):
		return UploadResult{}, err
	}
	g, err := tx.Grant(ctx, req.GrantID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && g.EventID != req.EventID:
		return UploadResult{}, deny(ReasonGrantUnknown, "no grant %s exists for event %s", req.GrantID, req.EventID)
	case err != nil:
		return UploadResult{}, err
	case g.Decision != "granted":
		return UploadResult{}, deny(ReasonGrantNotGranted, "grant %s is %s", req.GrantID, g.Decision)
	case !g.UsedAt.IsZero():
		return UploadResult{}, deny(ReasonGrantConsumed, "grant %s was used at %s", req.GrantID, g.UsedAt.UTC().Format(time.RFC3339))
	case !now.Before(g.ExpiresAt):
		return UploadResult{}, deny(ReasonGrantExpired, "grant %s expired at %s", req.GrantID, g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return UploadResult{}, fmt.Errorf("vault: grant %s could not be claimed and is not explained", req.GrantID)
}

// index writes the search rows the tier permits for one stored object: the typed prompt at
// full_text, attachment names at attachment_names and above. A client-generated request is never
// indexed, and an object whose submission is not known has nothing to attach rows to.
func (s *Service) index(ctx context.Context, tx store.Tx, tier store.SearchTier, c store.Content, content []byte) (prompt, names int, err error) {
	if c.SubmissionID == "" || c.PromptKind == string(protocol.PromptKindClientGenerated) {
		return 0, 0, nil
	}
	text, attachments := indexUnits(content)
	if tier.Allows(store.UnitPromptBody) && text != "" {
		if err := tx.PutSearchText(ctx, store.SearchUnit{
			SubmissionID: c.SubmissionID, UnitKind: store.UnitPromptBody, Body: text, ExpiresAt: c.ExpiresAt,
		}); err != nil {
			return 0, 0, err
		}
		prompt = 1
	}
	if tier.Allows(store.UnitAttachmentName) {
		for i, name := range attachments {
			if err := tx.PutSearchText(ctx, store.SearchUnit{
				SubmissionID: c.SubmissionID, UnitKind: store.UnitAttachmentName, UnitIndex: i, Body: name, ExpiresAt: c.ExpiresAt,
			}); err != nil {
				return 0, 0, err
			}
			names++
		}
	}
	return prompt, names, nil
}

// parseUUID returns the canonical lower-case form of a UUID.
func parseUUID(s string) (string, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// tenant reads the tenant row, refusing an unknown tenant.
func tenant(ctx context.Context, tx store.Tx) (store.Tenant, error) {
	t, err := tx.Tenant(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return store.Tenant{}, deny(ReasonTenantNotPermitted, "the tenant is unknown")
	}
	return t, err
}
