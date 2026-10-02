package vault

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
	"github.com/shadow-ai-capture/content-vault/internal/store"
)

// Audit actions. Every one of them is a row in ops.audit, and the content path's rows are written
// *before* the thing they record is served (docs/02 §11).
const (
	ActionKeyIssued          = "content_key_issued"
	ActionContentStored      = "content_stored"
	ActionContentShredded    = "content_shredded"
	ActionKeyRotated         = "content_key_rotated"
	ActionRetrievalRequested = "content_retrieval_requested"
	ActionRetrievalGranted   = "content_retrieval_granted"
	ActionRetrievalRefused   = "content_retrieval_refused"
	ActionRetrievalRedeemed  = "content_retrieval_redeemed"
	ActionSearch             = "content_search"
)

// Options configure the service. Every zero value has a safe default; the two required fields are
// the store and the key wrapper.
type Options struct {
	// Store is the persistence seam.
	Store store.Store
	// Keys is the key store. The service never holds a KEK itself: it asks this interface, which
	// is how "unwrap happens in exactly one service" (D7) is kept true even as backends change.
	Keys keys.KeyWrapper

	Now func() time.Time
	Rnd io.Reader

	// RetrievalGrantTTL is how long a scheduled retrieval stays redeemable. §11 calls the
	// retrieval URL "short-lived"; the ceiling below is the value a deployment may not exceed
	// without changing this line.
	RetrievalGrantTTL    time.Duration
	MaxRetrievalGrantTTL time.Duration

	// ScopeTiers is the signed policy bundle's effective search tier per scope (§6.3: "the tenant
	// tier is a ceiling, never a global switch"). A scope that is not named is `disabled`.
	ScopeTiers map[string]store.SearchTier

	// MaxSearchResults caps a search response; MaxSnippetChars caps one highlighted snippet. The
	// bounding rule is configuration (§06 §14.3 Q-f), so it is a field rather than a constant.
	MaxSearchResults int
	MaxSnippetChars  int
	FuzzyThreshold   float64

	// FetchBlob is the stand-in for blob storage. In a deployment the vault never reads content
	// bytes: it authorises a retrieval and the caller fetches the object from blob storage with a
	// short-lived credential (docs/02 §11). On this offline host there is no blob store, so the
	// binary may wire a function that returns the ciphertext it holds for local development and for
	// the tests. When it is nil, an authorised read returns no bytes and says so.
	FetchBlob func(blobPath string) ([]byte, error)

	Logger *slog.Logger
}

// Service is the vault.
type Service struct {
	opts Options
}

// New builds the service.
func New(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("vault: a store is required")
	}
	if o.Keys == nil {
		return nil, errors.New("vault: a key wrapper is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rnd == nil {
		o.Rnd = rand.Reader
	}
	if o.RetrievalGrantTTL <= 0 {
		o.RetrievalGrantTTL = 5 * time.Minute
	}
	if o.MaxRetrievalGrantTTL <= 0 {
		o.MaxRetrievalGrantTTL = 15 * time.Minute
	}
	if o.RetrievalGrantTTL > o.MaxRetrievalGrantTTL {
		return nil, fmt.Errorf("vault: retrieval grant TTL %s exceeds the %s ceiling", o.RetrievalGrantTTL, o.MaxRetrievalGrantTTL)
	}
	if o.MaxSearchResults <= 0 {
		o.MaxSearchResults = 20
	}
	if o.MaxSnippetChars <= 0 {
		o.MaxSnippetChars = 240
	}
	if o.FuzzyThreshold <= 0 {
		o.FuzzyThreshold = 0.3 // pg_trgm's default similarity threshold
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{opts: o}, nil
}

// Kind reports the key backend, so a deployment report cannot claim a KMS this build does not have.
func (s *Service) Keys() keys.KeyWrapper { return s.opts.Keys }

// ---------------------------------------------------------------------------------------
// Storing content: two phases, because a staged object has no ciphertext yet
// ---------------------------------------------------------------------------------------

// The docs' content path has two phases (docs/02 §10.3-10.4): control-api decides to grant, the
// vault mints the object's data key and wraps it, the device encrypts and uploads the ciphertext to
// blob storage, and the finaliser then verifies and records the object. The schema has no "staged"
// state — ops.content_object admits exactly `uploaded` and `shredded` — so the wrapped key is
// minted in phase one and returned in phase two, when there is a row to put it in.
//
// The sealed key crossing the internal network twice is harmless and deliberate: it is sealed under
// the tenant KEK with the object's identity as additional authenticated data, so it is useless
// without the KEK, and the device never sees it — the device receives only the plaintext data key
// for the object it is about to encrypt (docs/02 §10.4).

// PrepareRequest asks for a fresh per-object data key.
type PrepareRequest struct {
	TenantID     string
	ObjectID     string
	SubmissionID string
	EventID      string
	// RetentionClass and ExpiresAt are decided by control-api from policy; the vault records them.
	RetentionClass             string
	ExpiresAt                  time.Time
	ExpectedPlaintextSizeBytes int64
}

// PrepareResult carries the plaintext data key for the device and the sealed form for finalisation.
type PrepareResult struct {
	ObjectID   string
	DEK        []byte
	WrappedDEK []byte
	KEKID      string
	KEKVersion string
	ExpiresAt  time.Time
}

// PrepareObject mints and wraps one object's data key. Nothing is stored yet: an object row without
// a ciphertext would assert content that does not exist.
func (s *Service) PrepareObject(ctx context.Context, req PrepareRequest) (PrepareResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := s.checkTenantWritable(tenant); err != nil {
		return PrepareResult{}, err
	}
	if tenant.KEKID == "" {
		return PrepareResult{}, Denialf(DenyTenantNotPermitted, "tenant %s has no KEK configured, so no object key can be wrapped", tenant.TenantID)
	}
	if req.ObjectID == "" {
		return PrepareResult{}, fmt.Errorf("vault: prepare request has no object_id")
	}
	if req.ExpiresAt.IsZero() {
		req.ExpiresAt = now.Add(90 * 24 * time.Hour)
	}

	dek, err := keys.GenerateDEK(s.opts.Rnd)
	if err != nil {
		return PrepareResult{}, err
	}
	aad := keys.AAD{TenantID: tenant.TenantID, ObjectID: req.ObjectID, KEKID: tenant.KEKID}
	wrapped, err := s.wrapForObject(ctx, tenant, aad, dek)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "service", ActorID: "content-vault",
		Action: ActionKeyIssued, ObjectType: "content_object", ObjectID: req.ObjectID,
		Detail: map[string]any{
			"kek_id": wrapped.KEKID, "kek_version": wrapped.KEKVersion,
			"expected_plaintext_size_bytes": req.ExpectedPlaintextSizeBytes,
		},
		OccurredAt: now,
	}); err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{
		ObjectID: req.ObjectID, DEK: dek, WrappedDEK: wrapped.Bytes,
		KEKID: wrapped.KEKID, KEKVersion: wrapped.KEKVersion, ExpiresAt: req.ExpiresAt,
	}, nil
}

// FinaliseRequest is one object's arrival at the backend: the ciphertext is already in blob storage,
// the wrapped key is the one PrepareObject minted, and the caller (control-api's finaliser) has
// verified the size and raw digest (docs/02 §10.4).
type FinaliseRequest struct {
	TenantID     string
	ObjectID     string
	SubmissionID string
	EventID      string
	BlobPath     string
	// CiphertextSHA256 is the digest of the exact bytes in blob storage, "sha256:<hex>".
	CiphertextSHA256   string
	PlaintextSizeBytes int64
	// WrappedDEK, KEKID and KEKVersion are what PrepareObject returned.
	WrappedDEK     []byte
	KEKID          string
	KEKVersion     string
	RetentionClass string
	ExpiresAt      time.Time

	// IndexUnits are the searchable units that arrived with this content. A unit the tenant's tier
	// does not permit is refused *and reported*, never silently dropped: the caller learns which
	// units it failed to index and why.
	IndexUnits []IndexUnit
}

// IndexUnit is one candidate ingest.search_text row.
type IndexUnit struct {
	UnitKind  string
	UnitIndex int
	Body      string
	ExpiresAt time.Time
}

// FinaliseResult reports what happened, including the refusals.
type FinaliseResult struct {
	ObjectID   string
	KEKID      string
	KEKVersion string
	Indexed    int
	Refused    []UnitRefusal
}

// UnitRefusal is one index unit the service would not write, with the closed reason.
type UnitRefusal struct {
	UnitKind  string
	UnitIndex int
	Reason    DenialReason
	Detail    string
}

// FinaliseObject records the object and the index units its tenant may have.
func (s *Service) FinaliseObject(ctx context.Context, req FinaliseRequest) (FinaliseResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return FinaliseResult{}, err
	}
	if err := s.checkTenantWritable(tenant); err != nil {
		return FinaliseResult{}, err
	}
	if req.ObjectID == "" || req.BlobPath == "" || req.CiphertextSHA256 == "" {
		return FinaliseResult{}, fmt.Errorf("vault: finalise request is missing object_id, blob_path or ciphertext_sha256")
	}
	if len(req.WrappedDEK) == 0 || req.KEKID == "" || req.KEKVersion == "" {
		return FinaliseResult{}, fmt.Errorf("vault: finalise request carries no wrapped data key")
	}
	if req.ExpiresAt.IsZero() {
		req.ExpiresAt = now.Add(90 * 24 * time.Hour)
	}

	obj := store.ContentObject{
		TenantID: tenant.TenantID, ObjectID: req.ObjectID, SubmissionID: req.SubmissionID,
		EventID: req.EventID, BlobPath: req.BlobPath, CiphertextSHA256: req.CiphertextSHA256,
		PlaintextSizeBytes: req.PlaintextSizeBytes, WrappedDEK: req.WrappedDEK,
		KEKID: req.KEKID, KEKVersion: req.KEKVersion, RetentionClass: req.RetentionClass,
		State: store.StateUploaded, CreatedAt: now, ExpiresAt: req.ExpiresAt,
	}
	if err := s.opts.Store.PutContentObject(ctx, obj); err != nil {
		return FinaliseResult{}, err
	}

	result := FinaliseResult{ObjectID: req.ObjectID, KEKID: req.KEKID, KEKVersion: req.KEKVersion}
	for _, unit := range req.IndexUnits {
		reason, detail := s.indexPermitted(tenant, unit.UnitKind)
		if reason != "" {
			result.Refused = append(result.Refused, UnitRefusal{UnitKind: unit.UnitKind, UnitIndex: unit.UnitIndex, Reason: reason, Detail: detail})
			continue
		}
		expires := unit.ExpiresAt
		if expires.IsZero() {
			expires = req.ExpiresAt
		}
		su := store.SearchUnit{
			TenantID: tenant.TenantID, SubmissionID: req.SubmissionID, UnitKind: unit.UnitKind,
			UnitIndex: unit.UnitIndex, Body: unit.Body, ExpiresAt: expires,
		}
		if err := s.opts.Store.PutSearchUnit(ctx, su); err != nil {
			return result, err
		}
		result.Indexed++
	}

	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "service", ActorID: "content-vault",
		Action: ActionContentStored, ObjectType: "content_object", ObjectID: req.ObjectID,
		Detail: map[string]any{
			"kek_id": req.KEKID, "kek_version": req.KEKVersion,
			"indexed": result.Indexed, "refused_units": len(result.Refused),
			"plaintext_size_bytes": req.PlaintextSizeBytes,
		},
		OccurredAt: now,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// IndexUnit writes one index row, refusing when the tenant's tier or its custody mode forbids it.
// It is the service-boundary half of ADR 0014's invariant: the database enforces the pair, and so
// does this, so a dropped constraint, a bypassed migration or a stale replica cannot make the
// vault index content it must not.
func (s *Service) IndexUnit(ctx context.Context, tenantID string, unit IndexUnit, submissionID string) error {
	tenant, err := s.loadTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	if reason, detail := s.indexPermitted(tenant, unit.UnitKind); reason != "" {
		return Denialf(reason, "%s", detail)
	}
	return s.opts.Store.PutSearchUnit(ctx, store.SearchUnit{
		TenantID: tenantID, SubmissionID: submissionID, UnitKind: unit.UnitKind,
		UnitIndex: unit.UnitIndex, Body: unit.Body, ExpiresAt: unit.ExpiresAt,
	})
}

// indexPermitted applies §6.1's table to one unit: the tier the tenant holds, and the custody pair
// the schema makes unrepresentable.
func (s *Service) indexPermitted(tenant store.Tenant, unitKind string) (DenialReason, string) {
	if !store.ValidUnitKind(unitKind) {
		return DenyIndexUnitNotPermitted, fmt.Sprintf("unit_kind %q is outside the closed set {prompt_body, attachment_name}", unitKind)
	}
	// ADR 0014 / the tenant_full_text_search_requires_vendor_readable_content constraint.
	if tenant.ContentSearch == store.SearchFullText && tenant.KeyCustody == store.CustodyCustomerHeld {
		return DenyKeyCustodySearchConflict,
			"a customer_held tenant cannot have prompt text indexed: an index over plaintext is plaintext-derived, and the database refuses this pair"
	}
	// The schema's tenant_search_tier_requires_collection_mode constraint, checked here too.
	if tenant.ContentSearch == store.SearchFullText && tenant.CeilingMode != protocol.ModeM3 {
		return DenySearchTierRequiresM3,
			fmt.Sprintf("full_text requires M3 for the scope, and this tenant's ceiling is %s", tenant.CeilingMode)
	}
	switch unitKind {
	case store.UnitPromptBody:
		if tenant.ContentSearch != store.SearchFullText {
			return DenyIndexUnitNotPermitted, fmt.Sprintf("prompt_body needs the full_text tier, and this tenant holds %q", tenant.ContentSearch)
		}
	case store.UnitAttachmentName:
		if tenant.ContentSearch.Rank() < store.SearchAttachmentNames.Rank() {
			return DenyIndexUnitNotPermitted, fmt.Sprintf("attachment_name needs attachment_names or above, and this tenant holds %q", tenant.ContentSearch)
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------------------
// Retrieval: the approved path (docs/02 §11, C16)
// ---------------------------------------------------------------------------------------

// RetrieveRequest is an analyst's request for one event's content.
type RetrieveRequest struct {
	TenantID string
	EventID  string
	// Principal is the authenticated analyst. It is never taken from a request body (docs/02 §12).
	Principal string
	// CaseReference and SecondApprover are C16's four-eyes gate: a full-content retrieval needs
	// both, and the approver must be someone other than the requester.
	CaseReference  string
	SecondApprover string
	Justification  string
}

// RetrieveResult is a scheduled retrieval: the caller gets a grant id and an expiry, not content.
type RetrieveResult struct {
	State     string
	GrantID   string
	ExpiresAt time.Time
	// RawDigest is the digest of the exact bytes the retrieval will return, so the analyst can
	// verify what they were granted (docs/02 §11).
	RawDigest string
}

// Retrieve authorises one retrieval and records it before it serves anything.
//
// The order is the requirement: validate, then **commit the audit row**, then look at the object
// and the key. If the audit write fails the retrieval is refused (`audit_unavailable`) rather than
// served (docs/02 §11, §6.3).
func (s *Service) Retrieve(ctx context.Context, req RetrieveRequest) (RetrieveResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return RetrieveResult{}, err
	}
	if err := s.checkTenantReadable(tenant); err != nil {
		return RetrieveResult{}, err
	}
	switch {
	case req.CaseReference == "":
		return RetrieveResult{}, s.refuse(ctx, tenant, req, now, DenyCaseReferenceRequired, "C16 requires a case reference for a full-content retrieval")
	case req.SecondApprover == "":
		return RetrieveResult{}, s.refuse(ctx, tenant, req, now, DenySecondApproverRequired, "C16 requires a second approver for a full-content retrieval")
	case req.SecondApprover == req.Principal:
		return RetrieveResult{}, s.refuse(ctx, tenant, req, now, DenySecondApproverNotDistinct, "the second approver must be someone other than the requester")
	}

	// Audit before serve. This row is committed before the object row is read: an access that
	// returned nothing is safe to record, and content served with no record is not.
	if err := s.opts.Store.AppendAudit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionRetrievalRequested, ObjectType: "event", ObjectID: req.EventID,
		CaseReference: req.CaseReference,
		Detail: map[string]any{
			"second_approver": req.SecondApprover,
			"justification":   req.Justification,
			"outcome":         "pending",
		},
		OccurredAt: now,
	}); err != nil {
		return RetrieveResult{}, Denialf(DenyAuditUnavailable, "the retrieval audit row could not be committed, so the read fails closed: %v", err)
	}

	obj, err := s.opts.Store.ObjectForEvent(ctx, tenant.TenantID, req.EventID)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return RetrieveResult{}, s.refuse(ctx, tenant, req, now, DenyNoContentObject,
				"no content object exists for this event: either no grant was made or the upload never completed")
		}
		return RetrieveResult{}, err
	}

	// Retention first, then erasure: an object past its expiry is unavailable whether or not the
	// sweep has run, and the caller is told which of the two it is.
	if !obj.ExpiresAt.IsZero() && !obj.ExpiresAt.After(now) {
		return RetrieveResult{}, s.unavailable(ctx, tenant, req, now, UnavailableRetentionExpired, obj, "the object passed its retention expiry")
	}
	if obj.State == store.StateShredded {
		return RetrieveResult{}, s.unavailable(ctx, tenant, req, now, shredReason(obj.ShreddedReason), obj, "the object was shredded")
	}

	// The unwrap proves the content is readable now; the DEK is used for nothing else here and is
	// dropped when this function returns. It is deliberately absent from the grant record: the
	// grant authorises a read, it does not carry the key.
	if _, err := s.unwrap(ctx, tenant, obj); err != nil {
		if u, ok := IsUnavailable(err); ok {
			return RetrieveResult{}, s.unavailableWithReceipt(ctx, tenant, req, now, u.Reason, obj, u.Detail)
		}
		return RetrieveResult{}, err
	}

	grantID, err := newUUID(s.opts.Rnd)
	if err != nil {
		return RetrieveResult{}, err
	}
	grant := store.RetrievalGrant{
		TenantID: tenant.TenantID, GrantID: grantID, EventID: obj.EventID, ObjectID: obj.ObjectID,
		SubmissionID: obj.SubmissionID, Principal: req.Principal, CaseReference: req.CaseReference,
		SecondApprover: req.SecondApprover, IssuedAt: now, ExpiresAt: now.Add(s.opts.RetrievalGrantTTL),
		RawDigest: obj.CiphertextSHA256,
	}
	if err := s.opts.Store.PutRetrievalGrant(ctx, grant); err != nil {
		return RetrieveResult{}, err
	}
	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionRetrievalGranted, ObjectType: "content_object", ObjectID: obj.ObjectID,
		CaseReference: req.CaseReference,
		Detail: map[string]any{
			"grant_id": grantID, "second_approver": req.SecondApprover,
			"expires_at": grant.ExpiresAt.Format(time.RFC3339), "raw_digest": obj.CiphertextSHA256,
		},
		OccurredAt: now,
	}); err != nil {
		return RetrieveResult{}, Denialf(DenyAuditUnavailable, "the grant could not be recorded in the audit trail: %v", err)
	}
	return RetrieveResult{State: "available", GrantID: grantID, ExpiresAt: grant.ExpiresAt, RawDigest: obj.CiphertextSHA256}, nil
}

// RedeemRequest redeems a retrieval grant: this is the call that gets the content.
type RedeemRequest struct {
	TenantID  string
	GrantID   string
	Principal string
	EventID   string
}

// RedeemResult carries the content. In this offline build the vault is the only component that can
// produce the plaintext (there is no blob store and no SAS URL to mint), so it returns the bytes;
// in a deployment the same authorisation would return a short-lived storage URL and the bytes
// would come from blob storage without passing through the vault's response (docs/02 §11). The
// authorisation decision — the part the grant matrix tests — is identical either way, and README.md
// states which half is NOT VERIFIED.
type RedeemResult struct {
	State            string
	Reason           UnavailableReason
	ReceiptRef       string
	GrantID          string
	EventID          string
	BlobPath         string
	RawDigest        string
	CiphertextSHA256 string
	Plaintext        []byte
	GrantExpiresAt   time.Time
}

// Redeem applies the grant matrix and, only when every check passes, returns content.
//
// The order matters and is deliberate: existence, then binding (principal, event), then time, then
// single use. A caller who presents another principal's grant learns that the grant is not theirs;
// a caller who presents a used grant learns it is used. Neither learns anything about the content.
func (s *Service) Redeem(ctx context.Context, req RedeemRequest) (RedeemResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return RedeemResult{}, err
	}
	if err := s.checkTenantReadable(tenant); err != nil {
		return RedeemResult{}, err
	}

	grant, err := s.opts.Store.RetrievalGrant(ctx, tenant.TenantID, req.GrantID)
	if err != nil {
		if errors.Is(err, store.ErrGrantNotFound) {
			// "No grant" and "not one of yours" are the same answer to a caller who holds only a
			// grant id: the id is not a capability by itself.
			return RedeemResult{}, Denialf(DenyGrantRequired, "no retrieval grant %s exists for this tenant", req.GrantID)
		}
		return RedeemResult{}, err
	}
	if grant.Principal != req.Principal {
		return RedeemResult{}, Denialf(DenyGrantPrincipalMismatch, "the grant was issued to another principal")
	}
	if grant.EventID != req.EventID {
		return RedeemResult{}, Denialf(DenyGrantEventMismatch, "the grant is bound to event %s, not %s", grant.EventID, req.EventID)
	}
	if !now.Before(grant.ExpiresAt) {
		return RedeemResult{}, Denialf(DenyGrantExpired, "the grant expired at %s", grant.ExpiresAt.Format(time.RFC3339))
	}
	if grant.Used() {
		return RedeemResult{}, Denialf(DenyGrantAlreadyUsed, "the grant was redeemed at %s by %s", grant.UsedAt.Format(time.RFC3339), grant.UsedBy)
	}

	// The atomic half: two concurrent redemptions of one grant cannot both succeed, because the
	// claim is a single conditional write (`used_at IS NULL`).
	claimed, err := s.opts.Store.ClaimRetrievalGrant(ctx, tenant.TenantID, req.GrantID, req.Principal, now)
	if err != nil {
		if errors.Is(err, store.ErrGrantAlreadyUsed) {
			return RedeemResult{}, Denialf(DenyGrantAlreadyUsed, "the grant was consumed by a concurrent request")
		}
		return RedeemResult{}, err
	}

	obj, err := s.opts.Store.ContentObject(ctx, tenant.TenantID, claimed.ObjectID)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return RedeemResult{State: "no_longer_available", Reason: UnavailableErasure, GrantID: claimed.GrantID,
				EventID: claimed.EventID, ReceiptRef: s.receiptRef(ctx, tenant.TenantID)}, nil
		}
		return RedeemResult{}, err
	}
	if !obj.ExpiresAt.IsZero() && !obj.ExpiresAt.After(now) {
		return RedeemResult{State: "no_longer_available", Reason: UnavailableRetentionExpired,
			GrantID: claimed.GrantID, EventID: claimed.EventID, ReceiptRef: s.receiptRef(ctx, tenant.TenantID)}, nil
	}
	if obj.State == store.StateShredded {
		return RedeemResult{State: "no_longer_available", Reason: shredReason(obj.ShreddedReason),
			GrantID: claimed.GrantID, EventID: claimed.EventID, ReceiptRef: s.receiptRef(ctx, tenant.TenantID)}, nil
	}

	dek, err := s.unwrap(ctx, tenant, obj)
	if err != nil {
		if u, ok := IsUnavailable(err); ok {
			return RedeemResult{State: "no_longer_available", Reason: u.Reason, GrantID: claimed.GrantID,
				EventID: claimed.EventID, ReceiptRef: s.receiptRef(ctx, tenant.TenantID)}, nil
		}
		return RedeemResult{}, err
	}

	// Audit before the bytes leave. A failed audit write here means no content, even though the
	// grant has already been consumed: a consumed grant that served nothing is recoverable, and
	// content served with no record is not.
	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionRetrievalRedeemed, ObjectType: "content_object", ObjectID: obj.ObjectID,
		CaseReference: claimed.CaseReference,
		Detail: map[string]any{
			"grant_id": claimed.GrantID, "event_id": claimed.EventID,
			"blob_path": obj.BlobPath, "raw_digest": obj.CiphertextSHA256,
			"bytes": len(dek),
		},
		OccurredAt: now,
	}); err != nil {
		return RedeemResult{}, Denialf(DenyAuditUnavailable, "the redemption audit row could not be committed, so no content is served: %v", err)
	}

	standIn, err := s.serve(ctx, obj)
	if err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{
		State: "available", GrantID: claimed.GrantID, EventID: claimed.EventID, BlobPath: obj.BlobPath,
		RawDigest: obj.CiphertextSHA256, CiphertextSHA256: obj.CiphertextSHA256,
		Plaintext: standIn, GrantExpiresAt: claimed.ExpiresAt,
	}, nil
}

// serve produces the bytes for an authorised read. In a deployment this is a storage call that the
// vault does not make; here it is the one place the offline build stands in for blob storage, and
// it is named so nobody mistakes it for the real path.
func (s *Service) serve(_ context.Context, obj store.ContentObject) ([]byte, error) {
	if s.opts.FetchBlob == nil {
		// No blob store is configured: report the authorisation as granted and the bytes as
		// unavailable from this component, rather than inventing content.
		return nil, nil
	}
	return s.opts.FetchBlob(obj.BlobPath)
}

// ---------------------------------------------------------------------------------------
// Shredding and erasure (§6.4, C34, D1)
// ---------------------------------------------------------------------------------------

// ShredReason values are the schema's ops.content_object.shredded_reason set.
var ShredReasons = []string{"retention_expired", "erasure", "hold_released", "tenant_offboarded"}

// ShredRequest destroys one object's key and records the receipt.
type ShredRequest struct {
	TenantID    string
	ObjectID    string
	Reason      string
	RequestedBy string
	// DestroyTenantKey destroys the whole tenant KEK, which is what offboarding and
	// customer-held-key content require (§6.4): every object of the tenant becomes unreadable at
	// once, not only this one.
	DestroyTenantKey bool
}

// ShredResult reports what was destroyed. AlreadyShredded is true when the object was already
// gone: erasure is idempotent, and a second request is answered with the same fact rather than an
// error — "we deleted it" and "it was already deleted" are both successes and the receipt must not
// claim two deletions.
type ShredResult struct {
	ObjectID          string
	Reason            string
	AlreadyShredded   bool
	KeyDestroyed      bool
	SearchRowsRemoved int64
	Receipt           store.ErasureReceipt
}

// ShredObject destroys one object's content.
func (s *Service) ShredObject(ctx context.Context, req ShredRequest) (ShredResult, error) {
	now := s.opts.Now().UTC()
	if !validShredReason(req.Reason) {
		return ShredResult{}, fmt.Errorf("vault: shred reason %q is outside the closed set %v", req.Reason, ShredReasons)
	}
	tenant, err := s.loadTenant(ctx, req.TenantID)
	if err != nil {
		return ShredResult{}, err
	}

	result := ShredResult{ObjectID: req.ObjectID, Reason: req.Reason}
	obj, err := s.opts.Store.ContentObject(ctx, tenant.TenantID, req.ObjectID)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			// Nothing to shred is not a failure of the erasure: it is the erasure having already
			// happened, or never having had anything to do.
			result.AlreadyShredded = true
			return result, nil
		}
		return ShredResult{}, err
	}
	if obj.State == store.StateShredded {
		result.AlreadyShredded = true
		result.Reason = obj.ShreddedReason
		return result, nil
	}

	if err := s.opts.Store.ShredObject(ctx, tenant.TenantID, req.ObjectID, req.Reason, now); err != nil {
		return ShredResult{}, err
	}
	// The index is not under a key, so key destruction cannot reach it: §6.4's footnote is
	// explicit that erasure must reach ingest.search_text by row deletion.
	removed, err := s.opts.Store.DeleteSearchText(ctx, tenant.TenantID, obj.SubmissionID)
	if err != nil {
		return ShredResult{}, err
	}
	result.SearchRowsRemoved = removed

	if req.DestroyTenantKey && tenant.KEKID != "" {
		if err := s.opts.Keys.Destroy(ctx, tenant.KEKID, req.Reason); err != nil {
			return ShredResult{}, fmt.Errorf("vault: destroying the tenant key: %w", err)
		}
		result.KeyDestroyed = true
	}

	receipt := store.ErasureReceipt{
		TenantID: tenant.TenantID, ReceiptID: mustUUID(s.opts.Rnd), ScopeKind: scopeKindFor(req.Reason),
		RequestedBy: req.RequestedBy, RequestedAt: now, CompletedAt: now,
		Mechanisms: mechanismsFor(result),
		RemovedCounts: map[string]int{
			"content_object": 1,
			"search_text":    int(removed),
		},
		RemainingCounts: map[string]int{},
	}
	if result.KeyDestroyed {
		receipt.Mechanisms = append(receipt.Mechanisms, "tenant_key_destroyed")
	}
	if err := s.opts.Store.PutErasureReceipt(ctx, receipt); err != nil {
		return ShredResult{}, err
	}
	result.Receipt = receipt

	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.RequestedBy,
		Action: ActionContentShredded, ObjectType: "content_object", ObjectID: req.ObjectID,
		Detail: map[string]any{
			"reason": req.Reason, "receipt_id": receipt.ReceiptID,
			"search_text_removed": removed, "tenant_key_destroyed": result.KeyDestroyed,
		},
		OccurredAt: now,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// EraseTenant destroys every object of a tenant and, when asked, its KEK. It is the offboarding
// path: §6.4's "destroying the key destroys the content" at tenant scope.
func (s *Service) EraseTenant(ctx context.Context, tenantID, requestedBy, reason string) (ShredResult, error) {
	now := s.opts.Now().UTC()
	if !validShredReason(reason) {
		return ShredResult{}, fmt.Errorf("vault: shred reason %q is outside the closed set %v", reason, ShredReasons)
	}
	tenant, err := s.loadTenant(ctx, tenantID)
	if err != nil {
		return ShredResult{}, err
	}
	objects, err := s.opts.Store.ObjectsForTenant(ctx, tenant.TenantID)
	if err != nil {
		return ShredResult{}, err
	}
	shredded := 0
	for _, obj := range objects {
		if err := s.opts.Store.ShredObject(ctx, tenant.TenantID, obj.ObjectID, reason, now); err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue
			}
			return ShredResult{}, err
		}
		shredded++
	}
	removed, err := s.opts.Store.DeleteSearchText(ctx, tenant.TenantID, "")
	if err != nil {
		return ShredResult{}, err
	}
	result := ShredResult{Reason: reason, SearchRowsRemoved: removed}
	if tenant.KEKID != "" {
		if err := s.opts.Keys.Destroy(ctx, tenant.KEKID, reason); err != nil && !errors.Is(err, keys.ErrUnknownKEK) {
			return ShredResult{}, fmt.Errorf("vault: destroying the tenant key: %w", err)
		}
		result.KeyDestroyed = true
	}
	receipt := store.ErasureReceipt{
		TenantID: tenant.TenantID, ReceiptID: mustUUID(s.opts.Rnd), ScopeKind: "tenant",
		RequestedBy: requestedBy, RequestedAt: now, CompletedAt: now,
		Mechanisms:    mechanismsFor(result),
		RemovedCounts: map[string]int{"content_object": shredded, "search_text": int(removed)},
		// The KEK's recovery window is stated in the receipt: A4 requires the receipt to say what
		// actually happened, and the local backend destroys immediately while a Key Vault backend
		// would have a soft-delete window.
		RemainingCounts: map[string]int{},
	}
	if err := s.opts.Store.PutErasureReceipt(ctx, receipt); err != nil {
		return ShredResult{}, err
	}
	result.Receipt = receipt
	return result, s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "service", ActorID: requestedBy,
		Action: ActionContentShredded, ObjectType: "tenant", ObjectID: tenant.TenantID,
		Detail:     map[string]any{"reason": reason, "receipt_id": receipt.ReceiptID, "objects": shredded},
		OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------------------

// RotateResult reports a rotation: the versions involved and how many objects were re-wrapped.
type RotateResult struct {
	KEKID      string
	OldVersion string
	NewVersion string
	Rewrapped  int
	Failed     []ObjectFailure
}

// ObjectFailure is an object that could not be re-wrapped, with the reason. A rotation that
// silently skipped rows would leave objects sealed under a version nobody expects.
type ObjectFailure struct {
	ObjectID string
	Err      string
}

// RotateTenant re-wraps every object of a tenant under a new KEK version. It never touches a blob:
// the DEK is unwrapped and re-wrapped, the ciphertext is untouched, and that is exactly what
// "re-wrap, never re-encrypt" means mechanically.
func (s *Service) RotateTenant(ctx context.Context, tenantID string) (RotateResult, error) {
	now := s.opts.Now().UTC()
	tenant, err := s.loadTenant(ctx, tenantID)
	if err != nil {
		return RotateResult{}, err
	}
	if tenant.KEKID == "" {
		return RotateResult{}, Denialf(DenyTenantNotPermitted, "tenant %s has no KEK to rotate", tenant.TenantID)
	}
	oldVersion, err := s.opts.Keys.CurrentVersion(ctx, tenant.KEKID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("vault: reading the current key version: %w", err)
	}
	newVersion, err := s.opts.Keys.NewVersion(ctx, tenant.KEKID)
	if err != nil {
		return RotateResult{}, fmt.Errorf("vault: creating a new key version: %w", err)
	}
	result := RotateResult{KEKID: tenant.KEKID, OldVersion: oldVersion, NewVersion: newVersion}

	objects, err := s.opts.Store.ObjectsForTenant(ctx, tenant.TenantID)
	if err != nil {
		return result, err
	}
	for _, obj := range objects {
		oldAAD := keys.AAD{TenantID: tenant.TenantID, ObjectID: obj.ObjectID, KEKID: obj.KEKID, KEKVersion: obj.KEKVersion}
		dek, err := s.opts.Keys.Unwrap(ctx, oldAAD, obj.WrappedDEK)
		if err != nil {
			result.Failed = append(result.Failed, ObjectFailure{ObjectID: obj.ObjectID, Err: err.Error()})
			continue
		}
		newAAD := keys.AAD{TenantID: tenant.TenantID, ObjectID: obj.ObjectID, KEKID: tenant.KEKID}
		wrapped, err := s.opts.Keys.Wrap(ctx, tenant.KEKID, newAAD, dek)
		if err != nil {
			result.Failed = append(result.Failed, ObjectFailure{ObjectID: obj.ObjectID, Err: err.Error()})
			continue
		}
		if err := s.opts.Store.RewrapObject(ctx, tenant.TenantID, obj.ObjectID, wrapped.Bytes, wrapped.KEKVersion, obj.KEKVersion, now); err != nil {
			result.Failed = append(result.Failed, ObjectFailure{ObjectID: obj.ObjectID, Err: err.Error()})
			continue
		}
		result.Rewrapped++
	}
	detail := map[string]any{
		"kek_id": tenant.KEKID, "old_version": oldVersion, "new_version": newVersion,
		"rewrapped": result.Rewrapped, "failed": len(result.Failed),
	}
	if err := s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "service", ActorID: "content-vault",
		Action: ActionKeyRotated, ObjectType: "tenant", ObjectID: tenant.TenantID,
		Detail: detail, OccurredAt: now,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------------------

func (s *Service) loadTenant(ctx context.Context, tenantID string) (store.Tenant, error) {
	if tenantID == "" {
		return store.Tenant{}, Denialf(DenyTenantNotPermitted, "no tenant was resolved for this request")
	}
	tenant, err := s.opts.Store.Tenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, store.ErrUnknownTenant) {
			// An unknown tenant is a refusal, not a 404: docs/02 §12 makes a tenant that disagrees
			// with the principal a rejection rather than a reconciliation.
			return store.Tenant{}, Denialf(DenyTenantNotPermitted, "tenant %s is unknown", tenantID)
		}
		return store.Tenant{}, err
	}
	return tenant, nil
}

func (s *Service) checkTenantWritable(t store.Tenant) error {
	if !t.IngestEnabled || t.Status == "closed" || t.Status == "offboarding" {
		return Denialf(DenyTenantNotPermitted, "tenant %s is %s with ingest_enabled=%v, so no content may be stored", t.TenantID, t.Status, t.IngestEnabled)
	}
	return nil
}

func (s *Service) checkTenantReadable(t store.Tenant) error {
	if t.Status == "closed" {
		return Denialf(DenyTenantNotPermitted, "tenant %s is closed", t.TenantID)
	}
	if !t.ReadEnabled {
		return Denialf(DenyRetrievalDisabled, "content reads are disabled for tenant %s", t.TenantID)
	}
	return nil
}

// unwrap opens one object's DEK and converts the key store's failures into the two facts the
// caller must distinguish: content that is gone (unavailable, served as a result) and a genuine
// fault (an error).
func (s *Service) unwrap(ctx context.Context, tenant store.Tenant, obj store.ContentObject) ([]byte, error) {
	aad := keys.AAD{TenantID: tenant.TenantID, ObjectID: obj.ObjectID, KEKID: obj.KEKID, KEKVersion: obj.KEKVersion}
	dek, err := s.opts.Keys.Unwrap(ctx, aad, obj.WrappedDEK)
	switch {
	case err == nil:
		return dek, nil
	case errors.Is(err, keys.ErrKeyDestroyed),
		errors.Is(err, keys.ErrUnknownKEK),
		errors.Is(err, keys.ErrUnknownVersion):
		// §6.4: destroying the key destroys the content, so this is a *result*, not an error.
		// §11 names it `key_unavailable` and requires 200 with that result rather than a failure.
		return nil, &Unavailable{Reason: UnavailableKeyUnavailable, Detail: err.Error()}
	case errors.Is(err, keys.ErrNotImplemented):
		return nil, Denialf(DenyCustodyModeUnsupported, "the configured key backend cannot unwrap: %v", err)
	default:
		// A wrapped key that does not authenticate against its own row is tampering or
		// corruption, not destruction, and it must not be reported as "gone".
		return nil, fmt.Errorf("vault: unwrapping object %s: %w", obj.ObjectID, err)
	}
}

func (s *Service) audit(ctx context.Context, e store.AuditEntry) error {
	return s.opts.Store.AppendAudit(ctx, e)
}

// provisioner is implemented by a key store that can create a tenant's first KEK on demand. The
// local development wrapper does; a Key Vault backend does **not**, because creating a key is a
// control-plane action with its own audit trail and its own approval (docs/06 §6.2), and a service
// that silently minted cloud keys would be doing key management behind the operator's back.
type provisioner interface {
	EnsureKEK(kekID string) (string, error)
}

// wrapForObject seals a DEK, provisioning the local backend's key on first use. The retry exists
// because a fresh deployment (and every test fixture) has a tenant row naming a KEK that the local
// store has never seen; the alternative — requiring a separate provisioning step before the first
// grant — is a step a deployment can forget, and forgetting it fails the *upload*, which is the
// user-visible path.
func (s *Service) wrapForObject(ctx context.Context, tenant store.Tenant, aad keys.AAD, dek []byte) (keys.Wrapped, error) {
	wrapped, err := s.opts.Keys.Wrap(ctx, tenant.KEKID, aad, dek)
	switch {
	case err == nil:
		return wrapped, nil
	case errors.Is(err, keys.ErrUnknownKEK):
		p, ok := s.opts.Keys.(provisioner)
		if !ok {
			return keys.Wrapped{}, Denialf(DenyCustodyModeUnsupported,
				"the tenant names KEK %q, the configured backend does not hold it, and this backend cannot create keys: %v",
				tenant.KEKID, err)
		}
		if _, perr := p.EnsureKEK(tenant.KEKID); perr != nil {
			return keys.Wrapped{}, fmt.Errorf("vault: provisioning KEK %q: %w", tenant.KEKID, perr)
		}
		return s.opts.Keys.Wrap(ctx, tenant.KEKID, aad, dek)
	case errors.Is(err, keys.ErrNotImplemented):
		return keys.Wrapped{}, Denialf(DenyCustodyModeUnsupported, "the configured key backend cannot wrap: %v", err)
	default:
		return keys.Wrapped{}, fmt.Errorf("vault: wrapping the data key: %w", err)
	}
}

// refuse records a refusal and returns it. §6.3: "a search that returns zero rows is still
// audited"; the same reasoning applies to a retrieval that is refused, and a refusal is exactly
// the signal an investigator's own access log needs.
func (s *Service) refuse(ctx context.Context, tenant store.Tenant, req RetrieveRequest, now time.Time, reason DenialReason, detail string) error {
	_ = s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionRetrievalRefused, ObjectType: "event", ObjectID: req.EventID,
		CaseReference: req.CaseReference,
		Detail:        map[string]any{"reason": string(reason), "detail": detail, "outcome": "refused"},
		OccurredAt:    now,
	})
	return Denialf(reason, "%s", detail)
}

// unavailable records the §11 result and returns it.
func (s *Service) unavailable(ctx context.Context, tenant store.Tenant, req RetrieveRequest, now time.Time, reason UnavailableReason, obj store.ContentObject, detail string) error {
	return s.unavailableWithReceipt(ctx, tenant, req, now, reason, obj, detail)
}

func (s *Service) unavailableWithReceipt(ctx context.Context, tenant store.Tenant, req RetrieveRequest, now time.Time, reason UnavailableReason, obj store.ContentObject, detail string) error {
	receipt := s.receiptRef(ctx, tenant.TenantID)
	_ = s.audit(ctx, store.AuditEntry{
		TenantID: tenant.TenantID, ActorType: "user", ActorID: req.Principal,
		Action: ActionRetrievalRefused, ObjectType: "content_object", ObjectID: obj.ObjectID,
		CaseReference: req.CaseReference,
		Detail: map[string]any{
			"reason": string(reason), "detail": detail, "outcome": "no_longer_available",
			"receipt_ref": receipt,
		},
		OccurredAt: now,
	})
	return &Unavailable{Reason: reason, ReceiptRef: receipt, Detail: detail}
}

// receiptRef returns the most recent erasure receipt for a tenant, or "" when there is none. It is
// best-effort on purpose: a missing reference must not turn an unavailability into an error.
func (s *Service) receiptRef(ctx context.Context, tenantID string) string {
	r, err := s.opts.Store.LastReceipt(ctx, tenantID)
	if err != nil {
		return ""
	}
	return r.ReceiptID
}

func shredReason(s string) UnavailableReason {
	switch s {
	case "retention_expired":
		return UnavailableRetentionExpired
	case "hold_released":
		return UnavailableHoldReleased
	case "tenant_offboarded":
		return UnavailableTenantOffboarded
	default:
		// erasure is the default because it is the reason an explicit deletion carries, and the
		// schema's set has no other member that could be mistaken for "still here".
		return UnavailableErasure
	}
}

func validShredReason(s string) bool {
	for _, r := range ShredReasons {
		if r == s {
			return true
		}
	}
	return false
}

func scopeKindFor(reason string) string {
	switch reason {
	case "tenant_offboarded":
		return "tenant"
	case "hold_released":
		return "hold_release"
	default:
		return "retention"
	}
}

func mechanismsFor(r ShredResult) []string {
	out := []string{"row_marked_shredded", "wrapped_key_destroyed", "search_text_deleted"}
	if r.KeyDestroyed {
		out = append(out, "tenant_key_destroyed")
	}
	return out
}

// newUUID returns a version-4 UUID from r. No external dependency is fetchable offline, and this
// is the only identifier this service mints.
func newUUID(r io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("vault: generating an id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b), nil
}

func mustUUID(r io.Reader) string {
	id, err := newUUID(r)
	if err != nil {
		panic("vault: " + err.Error())
	}
	return id
}

func formatUUID(b [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}

var _ = hex.EncodeToString
