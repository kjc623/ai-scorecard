// Package httpapi is the vault's one HTTP surface.
//
// **Internal ingress only (D6/D7).** The service is the only component that can unwrap a content
// key, so it has no user-facing endpoint and no device-facing endpoint: docs/02 §11 says devices
// reach content through control-api's grant decision and then write ciphertext straight to blob
// storage, and browsers reach content through query-api, which calls the vault over the internal
// network under its own service identity.
//
// The routes here are therefore exactly the internal calls, and the device-facing paths — including
// `POST /v1/content/grant`, which belongs to control-api (docs/02 §5.5) — are deliberately absent.
// A test asserts they 404 rather than assuming nobody will add them: "the router rejects the
// public/edge routes" is a property this package can hold, and holding it is cheaper than
// remembering it.
//
// Errors come in two kinds, kept apart on purpose:
//
//   - a **content refusal** carries one of vault's closed denial reasons and status 403, because a
//     refusal is an authorisation fact (or, for `no_longer_available`, status 200 with the explicit
//     §11 result — never 404 and never an empty body);
//   - a **transport error** (malformed JSON, a body over the cap) carries `bad_request` and status
//     400, because it is not a statement about content at all.
package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
	"github.com/shadow-ai-capture/device/protocol"
)

// Server is the HTTP surface.
type Server struct {
	Vault  *vault.Service
	Auth   auth.Authenticator
	Logger *slog.Logger
	Now    func() time.Time

	// MaxBodyBytes bounds one internal request. The largest body is a finalisation carrying a
	// bounded prompt body and a few filenames, not content bytes: the ciphertext went to blob
	// storage, not here.
	MaxBodyBytes int64
}

// New builds a server.
func New(v *vault.Service, a auth.Authenticator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Vault: v, Auth: a, Logger: logger, Now: time.Now, MaxBodyBytes: 1 << 20}
}

// Handler returns the routes. One surface, internal only, no public paths.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/content/object", s.guard(s.handlePrepare))
	mux.HandleFunc("POST /v1/content/object/finalise", s.guard(s.handleFinalise))
	mux.HandleFunc("POST /v1/content/retrieval", s.guard(s.handleRetrieval))
	mux.HandleFunc("POST /v1/content/redeem", s.guard(s.handleRedeem))
	mux.HandleFunc("POST /v1/content/shred", s.guard(s.handleShred))
	mux.HandleFunc("POST /v1/content/rotate", s.guard(s.handleRotate))
	mux.HandleFunc("POST /v1/content-search", s.guard(s.handleSearch))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"key_backend":    string(s.Vault.Keys().Kind()),
			"ingress":        "internal-only",
			"denial_reasons": vault.SortedDenialReasons(),
		})
	})
	return mux
}

// guard authenticates, bounds the body and hands the handler a principal.
func (s *Server) guard(next func(http.ResponseWriter, *http.Request, auth.Principal, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Auth.Authenticate(r)
		if err != nil {
			s.writeTransportError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
			return
		}
		if s.MaxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.MaxBodyBytes)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeTransportError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
			return
		}
		next(w, r, p, body)
	}
}

// ---------------------------------------------------------------------------------------
// Requests and responses
// ---------------------------------------------------------------------------------------

type indexUnitJSON struct {
	UnitKind  string    `json:"unit_kind"`
	UnitIndex int       `json:"unit_index"`
	Body      string    `json:"body"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type prepareRequestJSON struct {
	TenantID                   string    `json:"tenant_id,omitempty"`
	ObjectID                   string    `json:"object_id"`
	SubmissionID               string    `json:"submission_id"`
	EventID                    string    `json:"event_id"`
	RetentionClass             string    `json:"retention_class"`
	ExpiresAt                  time.Time `json:"expires_at,omitempty"`
	ExpectedPlaintextSizeBytes int64     `json:"expected_plaintext_size_bytes,omitempty"`
}

type finaliseRequestJSON struct {
	TenantID           string          `json:"tenant_id,omitempty"`
	ObjectID           string          `json:"object_id"`
	SubmissionID       string          `json:"submission_id"`
	EventID            string          `json:"event_id"`
	PromptKind         string          `json:"prompt_kind"`
	BlobPath           string          `json:"blob_path"`
	CiphertextSHA256   string          `json:"ciphertext_sha256"`
	PlaintextSizeBytes int64           `json:"plaintext_size_bytes"`
	WrappedDEKB64      string          `json:"wrapped_dek_b64"`
	KEKID              string          `json:"kek_id"`
	KEKVersion         string          `json:"kek_version"`
	RetentionClass     string          `json:"retention_class"`
	ExpiresAt          time.Time       `json:"expires_at,omitempty"`
	IndexUnits         []indexUnitJSON `json:"index_units,omitempty"`
}

type retrievalRequestJSON struct {
	TenantID       string `json:"tenant_id,omitempty"`
	EventID        string `json:"event_id"`
	CaseReference  string `json:"case_reference"`
	SecondApprover string `json:"second_approver"`
	Justification  string `json:"justification"`
}

type redeemRequestJSON struct {
	TenantID string `json:"tenant_id,omitempty"`
	GrantID  string `json:"grant_id"`
	EventID  string `json:"event_id"`
}

type shredRequestJSON struct {
	TenantID         string `json:"tenant_id,omitempty"`
	ObjectID         string `json:"object_id"`
	Reason           string `json:"reason"`
	RequestedBy      string `json:"requested_by"`
	DestroyTenantKey bool   `json:"destroy_tenant_key,omitempty"`
}

type rotateRequestJSON struct {
	TenantID string `json:"tenant_id,omitempty"`
}

type searchRequestJSON struct {
	TenantID      string    `json:"tenant_id,omitempty"`
	Scope         string    `json:"scope"`
	Form          string    `json:"form"`
	Query         string    `json:"query"`
	Limit         int       `json:"limit,omitempty"`
	CaseReference string    `json:"case_reference,omitempty"`
	Cursor        string    `json:"cursor,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	Tool          string    `json:"tool,omitempty"`
	Device        string    `json:"device,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	ReceivedFrom  time.Time `json:"received_from,omitempty"`
	ReceivedTo    time.Time `json:"received_to,omitempty"`
}

// ---------------------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------------------

func (s *Server) handlePrepare(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req prepareRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{"object_id": req.ObjectID}) {
		return
	}
	res, err := s.Vault.PrepareObject(r.Context(), vault.PrepareRequest{
		TenantID: p.TenantID, ObjectID: req.ObjectID, SubmissionID: req.SubmissionID,
		EventID: req.EventID, RetentionClass: req.RetentionClass, ExpiresAt: req.ExpiresAt,
		ExpectedPlaintextSizeBytes: req.ExpectedPlaintextSizeBytes,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"object_id": res.ObjectID,
		// The plaintext data key, for the device that is about to encrypt this one object
		// (docs/02 §10.4). It is returned here and nowhere else, over the internal channel.
		"object_key_b64":  base64.StdEncoding.EncodeToString(res.DEK),
		"wrapped_dek_b64": base64.StdEncoding.EncodeToString(res.WrappedDEK),
		"kek_id":          res.KEKID,
		"kek_version":     res.KEKVersion,
		"expires_at":      res.ExpiresAt,
	})
}

func (s *Server) handleFinalise(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req finaliseRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{
		"object_id": req.ObjectID, "blob_path": req.BlobPath,
		"ciphertext_sha256": req.CiphertextSHA256, "wrapped_dek_b64": req.WrappedDEKB64,
		"kek_id": req.KEKID, "kek_version": req.KEKVersion,
	}) {
		return
	}
	wrapped, err := base64.StdEncoding.DecodeString(req.WrappedDEKB64)
	if err != nil {
		s.writeTransportError(w, http.StatusBadRequest, "bad_request", "wrapped_dek_b64 is not base64")
		return
	}
	units := make([]vault.IndexUnit, 0, len(req.IndexUnits))
	for _, u := range req.IndexUnits {
		units = append(units, vault.IndexUnit{UnitKind: u.UnitKind, UnitIndex: u.UnitIndex, Body: u.Body, ExpiresAt: u.ExpiresAt})
	}
	res, err := s.Vault.FinaliseObject(r.Context(), vault.FinaliseRequest{
		TenantID: p.TenantID, ObjectID: req.ObjectID, SubmissionID: req.SubmissionID,
		EventID: req.EventID, BlobPath: req.BlobPath, CiphertextSHA256: req.CiphertextSHA256,
		PlaintextSizeBytes: req.PlaintextSizeBytes, WrappedDEK: wrapped, KEKID: req.KEKID,
		KEKVersion: req.KEKVersion, RetentionClass: req.RetentionClass, ExpiresAt: req.ExpiresAt,
		PromptKind: protocol.PromptKind(req.PromptKind), IndexUnits: units,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	refused := make([]map[string]any, 0, len(res.Refused))
	for _, r := range res.Refused {
		refused = append(refused, map[string]any{
			"unit_kind": r.UnitKind, "unit_index": r.UnitIndex,
			"reason": string(r.Reason), "detail": r.Detail,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"object_id": res.ObjectID, "indexed": res.Indexed, "refused_units": refused,
	})
}

func (s *Server) handleRetrieval(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req retrievalRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{"event_id": req.EventID}) {
		return
	}
	res, err := s.Vault.Retrieve(r.Context(), vault.RetrieveRequest{
		TenantID: p.TenantID, EventID: req.EventID, Principal: p.Subject,
		CaseReference: req.CaseReference, SecondApprover: req.SecondApprover,
		Justification: req.Justification,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"state": "available", "grant_id": res.GrantID, "expires_at": res.ExpiresAt,
		"raw_digest": res.RawDigest,
	})
}

func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req redeemRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{"grant_id": req.GrantID, "event_id": req.EventID}) {
		return
	}
	res, err := s.Vault.Redeem(r.Context(), vault.RedeemRequest{
		TenantID: p.TenantID, GrantID: req.GrantID, Principal: p.Subject, EventID: req.EventID,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	if res.State == "no_longer_available" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"state": "no_longer_available", "reason": string(res.Reason), "receipt_ref": res.ReceiptRef,
		})
		return
	}
	// The offline stand-in: with no blob store wired, the vault returns the object's reference and
	// digests and no bytes. A deployment returns a short-lived storage URL here and the bytes never
	// transit this response (docs/02 §11).
	out := map[string]any{
		"state": "available", "grant_id": res.GrantID, "event_id": res.EventID,
		"blob_path": res.BlobPath, "raw_digest": res.RawDigest,
		"ciphertext_sha256": res.CiphertextSHA256, "expires_at": res.GrantExpiresAt,
	}
	if res.Plaintext != nil {
		out["content_b64"] = base64.StdEncoding.EncodeToString(res.Plaintext)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleShred(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req shredRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{"object_id": req.ObjectID, "reason": req.Reason}) {
		return
	}
	requestedBy := req.RequestedBy
	if requestedBy == "" {
		requestedBy = p.Subject
	}
	res, err := s.Vault.ShredObject(r.Context(), vault.ShredRequest{
		TenantID: p.TenantID, ObjectID: req.ObjectID, Reason: req.Reason,
		RequestedBy: requestedBy, DestroyTenantKey: req.DestroyTenantKey,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"state": "destroyed", "object_id": res.ObjectID, "reason": res.Reason,
		"already_shredded": res.AlreadyShredded, "key_destroyed": res.KeyDestroyed,
		"search_text_removed": res.SearchRowsRemoved,
		"receipt_id":          res.Receipt.ReceiptID,
		"mechanisms":          res.Receipt.Mechanisms,
		"removed_counts":      res.Receipt.RemovedCounts,
	})
}

func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req rotateRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	res, err := s.Vault.RotateTenant(r.Context(), p.TenantID)
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"kek_id": res.KEKID, "old_version": res.OldVersion, "new_version": res.NewVersion,
		"rewrapped": res.Rewrapped, "failed": len(res.Failed),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, p auth.Principal, body []byte) {
	var req searchRequestJSON
	if !s.decode(w, p, body, &req) {
		return
	}
	if !s.requireFields(w, map[string]string{"scope": req.Scope, "form": req.Form, "query": req.Query}) {
		return
	}
	res, err := s.Vault.Search(r.Context(), vault.SearchRequest{
		TenantID: p.TenantID, Principal: p.Subject, Scope: req.Scope,
		Form: store.SearchForm(req.Form), Query: req.Query, Limit: req.Limit, CaseReference: req.CaseReference,
		Cursor: req.Cursor,
		Filters: store.SearchFilters{
			Subject: req.Subject, Tool: req.Tool, Device: req.Device, Mode: req.Mode,
			ReceivedFrom: req.ReceivedFrom, ReceivedTo: req.ReceivedTo,
		},
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	hits := make([]map[string]any, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, map[string]any{
			"submission_id": h.SubmissionID, "unit_kind": h.UnitKind, "unit_index": h.UnitIndex,
			"snippet": h.Snippet, "rank": h.Rank,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"state": "available", "effective_tier": string(res.Effective),
		"unit_kinds": res.UnitKinds, "hits": hits, "truncated": res.Truncated,
		"next_cursor": res.NextCursor,
	})
}

// ---------------------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------------------

// decode unmarshals a body strictly and refuses a tenant that disagrees with the principal.
func (s *Server) decode(w http.ResponseWriter, p auth.Principal, body []byte, into any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		s.writeTransportError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	if dec.More() {
		s.writeTransportError(w, http.StatusBadRequest, "bad_request", "trailing content after the JSON object")
		return false
	}
	if got := bodyTenant(into); got != "" && !equalFold(got, p.TenantID) {
		// docs/02 §12: a body tenant_id that disagrees with the authenticated principal is
		// rejected, not reconciled.
		s.writeVaultError(w, vault.Denialf(vault.DenyTenantMismatch,
			"the body names tenant %s and the authenticated principal is tenant %s", got, p.TenantID))
		return false
	}
	return true
}

// writeVaultError renders a refusal or an unavailability.
func (s *Server) writeVaultError(w http.ResponseWriter, err error) {
	if u, ok := vault.IsUnavailable(err); ok {
		// §11: 200 with an explicit result, never 404 and never an empty body.
		s.writeJSON(w, http.StatusOK, map[string]any{
			"state": "no_longer_available", "reason": string(u.Reason),
			"receipt_ref": u.ReceiptRef, "detail": u.Detail,
		})
		return
	}
	if d, ok := vault.IsDenial(err); ok {
		s.writeJSON(w, http.StatusForbidden, errorBody{
			Error:      errorDetail{Code: string(d.Reason), Detail: d.Detail, Closed: d.Reason.Valid()},
			ServerTime: s.now(),
		})
		return
	}
	// Anything else is this service failing, and it says so rather than dressing it as a refusal.
	s.Logger.Error("content-vault: internal error", "error", err)
	s.writeTransportError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

type errorBody struct {
	Error      errorDetail `json:"error"`
	ServerTime time.Time   `json:"server_time"`
}

type errorDetail struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
	// Closed reports whether the code is a member of the documented refusal set. It is present so a
	// reader of a response can tell a refusal from a transport failure without a lookup table.
	Closed bool `json:"closed"`
}

func (s *Server) writeTransportError(w http.ResponseWriter, status int, code, detail string) {
	s.writeJSON(w, status, errorBody{
		Error:      errorDetail{Code: code, Detail: detail},
		ServerTime: s.now(),
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		s.Logger.Error("content-vault: writing a response", "error", err)
	}
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func formOf(s string) store.SearchForm { return store.SearchForm(s) }

// bodyTenant reads the optional body tenant_id so decode can reject a body that disagrees with the
// authenticated principal (docs/02 §12). Each request type carries the field; the interface keeps
// the check in one place instead of seven.
type tenantNamer interface{ tenantID() string }

func (r prepareRequestJSON) tenantID() string   { return r.TenantID }
func (r finaliseRequestJSON) tenantID() string  { return r.TenantID }
func (r retrievalRequestJSON) tenantID() string { return r.TenantID }
func (r redeemRequestJSON) tenantID() string    { return r.TenantID }
func (r shredRequestJSON) tenantID() string     { return r.TenantID }
func (r rotateRequestJSON) tenantID() string    { return r.TenantID }
func (r searchRequestJSON) tenantID() string    { return r.TenantID }

func bodyTenant(v any) string {
	if t, ok := v.(tenantNamer); ok {
		return t.tenantID()
	}
	return ""
}
