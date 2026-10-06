package vault

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keyring"
	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// RetrievalPath is where minted retrieval URLs are served: RetrievalPath + tenant + "/" + grant.
const RetrievalPath = "/v1/content/retrieval/"

// RetrieveRequest is an analyst's request to read one event's content.
type RetrieveRequest struct {
	TenantID string
	EventID  string
	// Principal is the analyst, from their verified token.
	Principal string
	// CaseReference and SecondApprover are recorded when given. A second approver must be
	// someone other than the requester.
	CaseReference  string
	SecondApprover string
	Justification  string
	// SessionID is the token's sid, recorded so a read can be tied to the sign-in that made it.
	SessionID string
}

// RetrieveResult is a scheduled retrieval: a single-use URL, not content.
type RetrieveResult struct {
	GrantID      string
	ExpiresAt    time.Time
	RawDigest    string // digest of exactly the bytes the URL will return
	RetrievalURL string
}

// Retrieve authorises one read and mints its single-use retrieval URL. The outcome, granted or
// refused, is audited in the same transaction.
func (s *Service) Retrieve(ctx context.Context, req RetrieveRequest) (RetrieveResult, error) {
	now := s.now().UTC()
	var res RetrieveResult
	var outcome error
	err := s.store.InTenant(ctx, req.TenantID, func(tx store.Tx) error {
		t, err := tenant(ctx, tx)
		if err != nil {
			return err
		}
		if t.Status == "closed" || !t.ReadEnabled {
			return deny(ReasonRetrievalDisabled, "content reads are disabled for tenant %s", t.TenantID)
		}
		refuse := func(objectType, objectID string, result error, detail map[string]any) error {
			outcome = result
			return tx.AppendAudit(ctx, s.retrievalAudit(req, ActionRetrievalRefused, objectType, objectID, now, detail))
		}
		if req.SecondApprover != "" && req.SecondApprover == req.Principal {
			d := deny(ReasonSecondApproverNotDistinct, "the second approver must be someone other than the requester")
			return refuse("event", req.EventID, d, map[string]any{"reason": string(d.Reason)})
		}
		c, err := tx.ContentForEvent(ctx, req.EventID)
		if errors.Is(err, store.ErrNotFound) {
			d := deny(ReasonNoContentObject, "no content is stored for event %s", req.EventID)
			return refuse("event", req.EventID, d, map[string]any{"reason": string(d.Reason)})
		}
		if err != nil {
			return err
		}
		if u := s.unavailable(ctx, tx, c, now); u != nil {
			return refuse("content", c.ObjectID, u, map[string]any{"reason": string(u.Reason), "receipt_ref": u.ReceiptRef})
		}
		g := store.RetrievalGrant{
			GrantID: uuid.New().String(), EventID: c.EventID, ObjectID: c.ObjectID, SubmissionID: c.SubmissionID,
			Principal: req.Principal, CaseReference: req.CaseReference, SecondApprover: req.SecondApprover,
			IssuedAt: now, ExpiresAt: now.Add(s.retrievalTTL), RawDigest: c.RawDigest,
		}
		if err := tx.InsertRetrievalGrant(ctx, g); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, s.retrievalAudit(req, ActionRetrievalGranted, "content", c.ObjectID, now, map[string]any{
			"grant_id": g.GrantID, "expires_at": g.ExpiresAt.Format(time.RFC3339), "raw_digest": g.RawDigest,
		})); err != nil {
			return err
		}
		res = RetrieveResult{GrantID: g.GrantID, ExpiresAt: g.ExpiresAt, RawDigest: g.RawDigest, RetrievalURL: s.retrievalURL(req.TenantID, g.GrantID)}
		return nil
	})
	if err != nil {
		return RetrieveResult{}, err
	}
	return res, outcome
}

func (s *Service) retrievalAudit(req RetrieveRequest, action, objectType, objectID string, now time.Time, detail map[string]any) store.AuditEntry {
	detail["event_id"] = req.EventID
	if req.SecondApprover != "" {
		detail["second_approver"] = req.SecondApprover
	}
	if req.Justification != "" {
		detail["justification"] = req.Justification
	}
	if req.SessionID != "" {
		detail["sid"] = req.SessionID
	}
	return store.AuditEntry{
		ActorType: "user", ActorID: req.Principal, Action: action, ObjectType: objectType, ObjectID: objectID,
		CaseReference: req.CaseReference, Detail: detail, OccurredAt: now,
	}
}

func (s *Service) retrievalURL(tenantID, grantID string) string {
	return s.retrievalURLBase + RetrievalPath + tenantID + "/" + grantID
}

// unavailable reports content that exists as a row but cannot be served: past its retention, or
// sealed under a master key the keyring no longer holds.
func (s *Service) unavailable(ctx context.Context, tx store.Tx, c store.Content, now time.Time) *Unavailable {
	switch {
	case !now.Before(c.ExpiresAt):
		return s.gone(ctx, tx, UnavailableRetentionExpired, "the content passed its retention")
	case !s.keys.Has(c.KeyVersion):
		return s.gone(ctx, tx, UnavailableKeyUnavailable, "the content is sealed under key version "+c.KeyVersion+", which this service no longer holds")
	}
	return nil
}

func (s *Service) gone(ctx context.Context, tx store.Tx, reason UnavailableReason, detail string) *Unavailable {
	ref, err := tx.LatestReceiptID(ctx)
	if err != nil {
		s.log.Warn("content-vault: reading the latest erasure receipt", "error", err)
	}
	return &Unavailable{Reason: reason, ReceiptRef: ref, Detail: detail}
}

// RedeemResult is the content a retrieval URL serves.
type RedeemResult struct {
	GrantID   string
	EventID   string
	RawDigest string
	Plaintext []byte
}

// Redeem serves a minted retrieval URL. The URL is the credential: it names an opaque tenant and an
// opaque single-use grant that expires within minutes, so no caller identity is needed. The claim,
// the decryption and the audit row commit together before any byte is returned.
func (s *Service) Redeem(ctx context.Context, tenantID, grantID string) (RedeemResult, error) {
	now := s.now().UTC()
	var res RedeemResult
	var outcome error
	err := s.store.InTenant(ctx, tenantID, func(tx store.Tx) error {
		t, err := tx.Tenant(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return deny(ReasonGrantRequired, "no retrieval grant %s exists", grantID)
		}
		if err != nil {
			return err
		}
		if t.Status == "closed" || !t.ReadEnabled {
			return deny(ReasonRetrievalDisabled, "content reads are disabled for tenant %s", t.TenantID)
		}
		g, err := tx.RetrievalGrant(ctx, grantID)
		if errors.Is(err, store.ErrNotFound) {
			return deny(ReasonGrantRequired, "no retrieval grant %s exists", grantID)
		}
		if err != nil {
			return err
		}
		if !g.UsedAt.IsZero() {
			return deny(ReasonGrantAlreadyUsed, "the retrieval grant was redeemed at %s", g.UsedAt.UTC().Format(time.RFC3339))
		}
		if !now.Before(g.ExpiresAt) {
			return deny(ReasonGrantExpired, "the retrieval grant expired at %s", g.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if _, err := tx.ClaimRetrievalGrant(ctx, grantID, g.Principal, now); errors.Is(err, store.ErrNotFound) {
			return deny(ReasonGrantAlreadyUsed, "the retrieval grant was redeemed by a concurrent request")
		} else if err != nil {
			return err
		}
		audit := func(detail map[string]any) error {
			detail["grant_id"], detail["event_id"] = g.GrantID, g.EventID
			return tx.AppendAudit(ctx, store.AuditEntry{
				ActorType: "user", ActorID: g.Principal, Action: ActionRetrievalRedeemed, ObjectType: "content",
				ObjectID: g.ObjectID, CaseReference: g.CaseReference, Detail: detail, OccurredAt: now,
			})
		}
		c, err := tx.ContentForObject(ctx, g.ObjectID)
		if errors.Is(err, store.ErrNotFound) {
			u := s.gone(ctx, tx, UnavailableErasure, "the content was deleted after the grant was issued")
			outcome = u
			return audit(map[string]any{"outcome": "no_longer_available", "reason": string(u.Reason)})
		}
		if err != nil {
			return err
		}
		if u := s.unavailable(ctx, tx, c, now); u != nil {
			outcome = u
			return audit(map[string]any{"outcome": "no_longer_available", "reason": string(u.Reason)})
		}
		plaintext, err := s.keys.Open(c.TenantID, c.ObjectID, c.KeyVersion, c.Ciphertext)
		if errors.Is(err, keyring.ErrUnknownVersion) {
			u := s.gone(ctx, tx, UnavailableKeyUnavailable, err.Error())
			outcome = u
			return audit(map[string]any{"outcome": "no_longer_available", "reason": string(u.Reason)})
		}
		if err != nil {
			return err
		}
		if protocol.RawDigest(plaintext) != c.RawDigest {
			return errors.New("vault: decrypted content does not match its recorded digest")
		}
		if err := audit(map[string]any{"outcome": "served", "raw_digest": c.RawDigest, "bytes": len(plaintext)}); err != nil {
			return err
		}
		res = RedeemResult{GrantID: g.GrantID, EventID: g.EventID, RawDigest: c.RawDigest, Plaintext: plaintext}
		return nil
	})
	if err != nil {
		return RedeemResult{}, err
	}
	return res, outcome
}
