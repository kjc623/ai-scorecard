// Package httpapi is content-vault's HTTP surface. The service has internal ingress only, so every
// caller is another service inside the environment:
//
//	PUT  /internal/v1/tenants/{tenant}/content/{event}  control-api's service token
//	POST /v1/content/retrieval                          a person's token (content_reader), forwarded by query-api
//	POST /v1/content-search                             a person's token (analyst or content_reader), forwarded by query-api
//	GET  /v1/content/retrieval/{tenant}/{grant}         no token: the single-use URL is the credential;
//	                                                    the dashboard server forwards it from the browser
//	GET  /healthz, GET /readyz
//
// A refusal is {"error":{"code","detail"}} with a code from the vault's closed set. Content that no
// longer exists is an explicit {"state":"no_longer_available","reason",...} answer, never a 404.
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
	"github.com/shadow-ai-capture/content-vault/internal/store"
	"github.com/shadow-ai-capture/content-vault/internal/vault"
)

// UploadService is the only caller allowed to store content.
const UploadService = "control-api"

// maxJSONBytes bounds a search or retrieval request body.
const maxJSONBytes = 64 << 10

var rawDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TokenVerifier verifies a bearer token.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (auth.Claims, error)
}

// Server serves the routes.
type Server struct {
	vault  *vault.Service
	tokens TokenVerifier
	ready  func(context.Context) error
	log    *slog.Logger
}

// New builds a server. ready is the readiness check, a database round trip.
func New(v *vault.Service, tokens TokenVerifier, ready func(context.Context) error, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{vault: v, tokens: tokens, ready: ready, log: log}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /internal/v1/tenants/{tenant}/content/{event}", s.upload)
	mux.HandleFunc("POST /v1/content/retrieval", s.retrieve)
	mux.HandleFunc("GET "+vault.RetrievalPath+"{tenant}/{grant}", s.redeem)
	mux.HandleFunc("POST /v1/content-search", s.search)
	mux.HandleFunc("POST /v1/content/subject-export", s.subjectExport)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		s.log.Warn("content-vault: not ready", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// claims verifies the request's bearer token, answering 401 when there is none or it is invalid.
func (s *Server) claims(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	raw, err := auth.Bearer(r)
	if err == nil {
		var c auth.Claims
		if c, err = s.tokens.Verify(r.Context(), raw); err == nil {
			return c, true
		}
	}
	s.log.Info("content-vault: unauthenticated request", "path", r.URL.Path, "error", err)
	writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token from the product issuer is required")
	return auth.Claims{}, false
}

// person authenticates a person's token holding one of roles.
func (s *Server) person(w http.ResponseWriter, r *http.Request, roles ...string) (auth.Person, bool) {
	c, ok := s.claims(w, r)
	if !ok {
		return auth.Person{}, false
	}
	p, err := c.Person()
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "this route serves a person and the token does not speak for one")
		return auth.Person{}, false
	}
	if !p.HasAnyRole(roles...) {
		writeError(w, http.StatusForbidden, "forbidden", "this route needs the role "+strings.Join(roles, " or "))
		return auth.Person{}, false
	}
	return p, true
}

// uploadResponse is the answer to a stored upload.
type uploadResponse struct {
	State     string         `json:"state"`
	ObjectID  string         `json:"object_id"`
	EventID   string         `json:"event_id"`
	GrantID   string         `json:"grant_id"`
	RawDigest string         `json:"raw_digest"`
	SizeBytes int            `json:"size_bytes"`
	ExpiresAt time.Time      `json:"expires_at"`
	Indexed   map[string]int `json:"indexed"`
	Replayed  bool           `json:"replayed"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	c, ok := s.claims(w, r)
	if !ok {
		return
	}
	if c.Service != UploadService {
		writeError(w, http.StatusForbidden, "forbidden", "only control-api's service token may upload content")
		return
	}
	tenantID, err1 := canonicalUUID(r.PathValue("tenant"))
	eventID, err2 := canonicalUUID(r.PathValue("event"))
	grantID, err3 := canonicalUUID(r.Header.Get(protocol.HeaderContentGrantID))
	digest := r.Header.Get(protocol.HeaderContentRawDigest)
	switch {
	case err1 != nil || err2 != nil:
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), "the path does not name a tenant id and an event id")
		return
	case err3 != nil:
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), protocol.HeaderContentGrantID+" is not a grant id")
		return
	case !rawDigestPattern.MatchString(digest):
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), protocol.HeaderContentRawDigest+" is not sha256:<64 lower-case hex>")
		return
	case r.ContentLength > protocol.MaxContentObjectBytes:
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a content object is at most 16 MiB")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxContentObjectBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a content object is at most 16 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), "the body could not be read")
		return
	}
	res, err := s.vault.Upload(r.Context(), vault.UploadRequest{
		TenantID: tenantID, EventID: eventID, GrantID: grantID, RawDigest: digest, Content: body,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, uploadResponse{
		State: "stored", ObjectID: res.ObjectID, EventID: res.EventID, GrantID: res.GrantID,
		RawDigest: res.RawDigest, SizeBytes: res.SizeBytes, ExpiresAt: res.ExpiresAt, Replayed: res.Replayed,
		Indexed: map[string]int{store.UnitPromptBody: res.IndexedPrompt, store.UnitAttachmentName: res.IndexedAttachmentNames},
	})
}

type retrievalRequest struct {
	EventID        string `json:"event_id"`
	CaseReference  string `json:"case_reference"`
	SecondApprover string `json:"second_approver"`
	Justification  string `json:"justification"`
}

func (s *Server) retrieve(w http.ResponseWriter, r *http.Request) {
	p, ok := s.person(w, r, auth.RoleContentReader)
	if !ok {
		return
	}
	var req retrievalRequest
	if !decode(w, r, &req) {
		return
	}
	eventID, err := canonicalUUID(req.EventID)
	if err != nil {
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), "event_id is not an event id")
		return
	}
	res, err := s.vault.Retrieve(r.Context(), vault.RetrieveRequest{
		TenantID: p.TenantID, EventID: eventID, Principal: p.Actor, CaseReference: req.CaseReference,
		SecondApprover: req.SecondApprover, Justification: req.Justification, SessionID: p.SessionID,
	})
	if u, ok := vault.AsUnavailable(err); ok {
		writeJSON(w, http.StatusOK, unavailableBody(u, true))
		return
	}
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": "available", "grant_id": res.GrantID, "expires_at": res.ExpiresAt,
		"raw_digest": res.RawDigest, "retrieval_url": res.RetrievalURL,
	})
}

func (s *Server) redeem(w http.ResponseWriter, r *http.Request) {
	tenantID, err1 := canonicalUUID(r.PathValue("tenant"))
	grantID, err2 := canonicalUUID(r.PathValue("grant"))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), "the retrieval URL does not name a tenant and a grant")
		return
	}
	res, err := s.vault.Redeem(r.Context(), tenantID, grantID)
	if u, ok := vault.AsUnavailable(err); ok {
		// A byte response cannot carry both content and a result, so the result has its own status.
		writeJSON(w, http.StatusGone, unavailableBody(u, false))
		return
	}
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set(protocol.HeaderContentGrantID, res.GrantID)
	h.Set(protocol.HeaderContentRawDigest, res.RawDigest)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Plaintext)
}

type searchRequest struct {
	Form          string    `json:"form"`
	Query         string    `json:"query"`
	Limit         int       `json:"limit"`
	CaseReference string    `json:"case_reference"`
	Cursor        string    `json:"cursor"`
	Subject       string    `json:"subject"`
	Tool          string    `json:"tool"`
	Device        string    `json:"device"`
	Mode          string    `json:"mode"`
	ReceivedFrom  time.Time `json:"received_from"`
	ReceivedTo    time.Time `json:"received_to"`
}

type searchHit struct {
	SubmissionID string  `json:"submission_id"`
	UnitKind     string  `json:"unit_kind"`
	UnitIndex    int     `json:"unit_index"`
	Snippet      string  `json:"snippet"`
	Rank         float64 `json:"rank"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	p, ok := s.person(w, r, auth.RoleAnalyst, auth.RoleContentReader)
	if !ok {
		return
	}
	var req searchRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.vault.Search(r.Context(), vault.SearchRequest{
		TenantID: p.TenantID, Principal: p.Actor, SessionID: p.SessionID,
		Form: store.SearchForm(req.Form), Query: req.Query, Limit: req.Limit,
		CaseReference: req.CaseReference, Cursor: req.Cursor,
		Filters: store.SearchFilters{
			Subject: req.Subject, Tool: req.Tool, Device: req.Device, Mode: req.Mode,
			ReceivedFrom: req.ReceivedFrom, ReceivedTo: req.ReceivedTo,
		},
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	hits := make([]searchHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, searchHit{SubmissionID: h.SubmissionID, UnitKind: h.UnitKind, UnitIndex: h.UnitIndex, Snippet: h.Snippet, Rank: h.Rank})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": "available", "effective_tier": string(res.Tier), "unit_kinds": res.UnitKinds,
		"hits": hits, "truncated": res.NextCursor != "", "next_cursor": res.NextCursor,
	})
}

type subjectExportRequest struct {
	SubjectRef string `json:"subject_ref"`
}

type subjectPrompt struct {
	EventID      string `json:"event_id"`
	SubmissionID string `json:"submission_id"`
	PromptKind   string `json:"prompt_kind"`
	RawDigest    string `json:"raw_digest"`
	SizeBytes    int    `json:"size_bytes"`
	Plaintext    string `json:"plaintext"` // base64 of the stored bytes
}

// subjectExport is an admin's read of every stored prompt of one subject. The vault decrypts them
// and audits the read; the caller (query-api) assembles the archive.
func (s *Server) subjectExport(w http.ResponseWriter, r *http.Request) {
	p, ok := s.person(w, r, auth.RoleAdmin)
	if !ok {
		return
	}
	var req subjectExportRequest
	if !decode(w, r, &req) {
		return
	}
	if req.SubjectRef == "" || len(req.SubjectRef) > 256 {
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), "subject_ref is required and at most 256 characters")
		return
	}
	res, err := s.vault.SubjectExport(r.Context(), vault.SubjectExportRequest{
		TenantID: p.TenantID, SubjectRef: req.SubjectRef, Principal: p.Actor, SessionID: p.SessionID,
	})
	if err != nil {
		s.writeVaultError(w, err)
		return
	}
	prompts := make([]subjectPrompt, 0, len(res.Prompts))
	for _, pr := range res.Prompts {
		prompts = append(prompts, subjectPrompt{
			EventID: pr.EventID, SubmissionID: pr.SubmissionID, PromptKind: pr.PromptKind,
			RawDigest: pr.RawDigest, SizeBytes: pr.SizeBytes, Plaintext: base64.StdEncoding.EncodeToString(pr.Plaintext),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": "available", "prompts": prompts, "skipped": res.Skipped,
	})
}

// decode reads a bounded JSON body strictly: an unknown field or trailing data is a bad request.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is over 64 KiB")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil || dec.More() {
		detail := "the body is not one JSON object of the documented fields"
		if err != nil {
			detail = err.Error()
		}
		writeError(w, http.StatusBadRequest, string(vault.ReasonInvalidRequest), detail)
		return false
	}
	return true
}

// writeVaultError renders a refusal with its status, or an internal failure.
func (s *Server) writeVaultError(w http.ResponseWriter, err error) {
	if d, ok := vault.AsDenial(err); ok {
		writeError(w, denialStatus(d.Reason), string(d.Reason), d.Detail)
		return
	}
	s.log.Error("content-vault: request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "the vault could not complete the request")
}

func denialStatus(r vault.Reason) int {
	switch r {
	case vault.ReasonInvalidRequest, vault.ReasonSearchCursorInvalid:
		return http.StatusBadRequest
	case vault.ReasonGrantConsumed, vault.ReasonAlreadyStored:
		return http.StatusConflict
	}
	return http.StatusForbidden
}

func unavailableBody(u *vault.Unavailable, withDetail bool) map[string]any {
	body := map[string]any{"state": "no_longer_available", "reason": string(u.Reason), "receipt_ref": u.ReceiptRef}
	if withDetail {
		body["detail"] = u.Detail
	}
	return body
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Detail: detail}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// canonicalUUID returns the lower-case hyphenated form of a UUID, which is the form every id is
// stored, compared and bound into ciphertext as.
func canonicalUUID(s string) (string, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
