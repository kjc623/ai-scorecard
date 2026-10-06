package scim

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

// Page sizes. A provider asks for what it wants (Okta imports 100 at a time); MaxPageSize bounds one
// response, and ServiceProviderConfig advertises it as filter.maxResults.
const (
	DefaultPageSize = 100
	MaxPageSize     = 200
)

// KeySource yields a tenant's user-reference key. *directory.UserRefKeys is the implementation; it
// mints the key on first need, so the first SCIM write for a tenant may be the call that mints it.
type KeySource interface {
	Key(ctx context.Context, tenantID string) ([]byte, error)
}

// Config is the deployment's SCIM settings.
type Config struct {
	// BaseURL is the public SCIM base the identity provider was given, such as
	// `https://app.example.com/scim/v2`. It spells meta.location; empty omits it rather than trusting a
	// Host header to spell it.
	BaseURL string
}

// Error is a SCIM error (RFC 7644 §3.12). Detail is written for the identity provider's admin, never
// with a personal value in it; Cause is logged, never sent.
type Error struct {
	Status   int
	ScimType string
	Detail   string
	Cause    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("scim %d %s: %s", e.Status, e.ScimType, e.Detail)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func badRequest(scimType, detail string) *Error {
	return &Error{Status: http.StatusBadRequest, ScimType: scimType, Detail: detail}
}

func notFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Detail: what + " not found"}
}

func unauthorized() *Error {
	return &Error{Status: http.StatusUnauthorized, Detail: "a valid SCIM bearer token for this tenant is required"}
}

func internal(cause error) *Error {
	return &Error{Status: http.StatusInternalServerError, Detail: "the request could not be completed; nothing was changed", Cause: cause}
}

// Principal is an authenticated SCIM client: a tenant and the token it presented.
type Principal struct {
	TenantID string
	TokenID  string
}

// actorID is how ops.audit names the client: the token, not the identity provider's claim about
// itself, so a revoked token's history is still traceable to the token.
func (p Principal) actorID() string { return "scim:" + p.TokenID }

// Service is the SCIM provider's logic, independent of HTTP.
type Service struct {
	store  Store
	keys   KeySource
	cipher *directory.Cipher
	cfg    Config
	// Now defaults to time.Now; a test pins it.
	Now    func() time.Time
	Logger *slog.Logger
}

// NewService wires the provider. All three collaborators are required: without the Cipher a
// resource could not be sealed, and without the key no lookup hash or user_ref could be computed.
func NewService(st Store, keys KeySource, cipher *directory.Cipher, cfg Config) (*Service, error) {
	if st == nil || keys == nil || cipher == nil {
		return nil, fmt.Errorf("scim: the service needs a store, a user_ref key source and a cipher")
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return &Service{store: st, keys: keys, cipher: cipher, cfg: cfg, Now: time.Now, Logger: slog.Default()}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Authenticate resolves `Authorization: Bearer sacscim_…` to a tenant and a token. Every failure is
// the same 401, so a caller learns nothing about which part was wrong.
func (s *Service) Authenticate(ctx context.Context, authorization string) (Principal, *Error) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Principal{}, unauthorized()
	}
	tok = strings.TrimSpace(tok)
	claimed, err := ParseToken(tok)
	if err != nil {
		return Principal{}, unauthorized()
	}
	hash := HashToken(tok)
	owner, err := s.store.TenantForToken(ctx, hash)
	if err != nil {
		return Principal{}, internal(err)
	}
	// The clear tenant only chooses the RLS session; the database's answer is what grants it. A token
	// whose clear tenant was edited to another tenant's id fails here.
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(owner)), []byte(claimed)) != 1 {
		return Principal{}, unauthorized()
	}
	var p Principal
	err = s.store.InTenant(ctx, claimed, func(tx Tx) error {
		row, err := tx.TokenByHash(hash)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(row.Hash), []byte(hash)) != 1 || row.RevokedAt != nil {
			return ErrNotFound
		}
		p = Principal{TenantID: claimed, TokenID: row.ID}
		return nil
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnknownTenant) {
		return Principal{}, unauthorized()
	}
	if err != nil {
		return Principal{}, internal(err)
	}
	return p, nil
}

// LookupHash is how ops.scim_user stores a userName or externalId for lookup: HMAC-SHA256 under the
// tenant's user-reference key over the trimmed, lower-cased value. The identity
// service may compute the same to find a signed-in person's row.
func LookupHash(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(value))))
	return mac.Sum(nil)
}

func optionalHash(key []byte, value string) []byte {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return LookupHash(key, value)
}

func (s *Service) sealResource(tenantID string, res map[string]any) ([]byte, error) {
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("scim: encode resource: %w", err)
	}
	return s.cipher.SealBytes(tenantID, raw)
}

func (s *Service) openResource(tenantID string, sealed []byte) (map[string]any, error) {
	raw, err := s.cipher.OpenBytes(tenantID, sealed)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

// decodeObject decodes a JSON object keeping numbers as written, so a resource round-trips without
// a provider's integer turning into a float.
func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("scim: not a JSON object")
	}
	return m, nil
}

func deepCopy(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	out, err := decodeObject(raw)
	if err != nil {
		return map[string]any{}
	}
	return out
}

func (s *Service) auditEntry(p Principal, action, objectType, objectID, subjectRef string, detail map[string]any, at time.Time) AuditEntry {
	if detail == nil {
		detail = map[string]any{}
	}
	return AuditEntry{
		ActorType:  "service",
		ActorID:    p.actorID(),
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		SubjectRef: subjectRef,
		Detail:     detail,
		At:         at,
	}
}

func (s *Service) location(resourceType, id string) string {
	if s.cfg.BaseURL == "" {
		return ""
	}
	return s.cfg.BaseURL + "/" + resourceType + "/" + id
}

func meta(resourceType string, created, modified time.Time, location string) map[string]any {
	m := map[string]any{
		"resourceType": resourceType,
		"created":      created.UTC().Format(time.RFC3339Nano),
		"lastModified": modified.UTC().Format(time.RFC3339Nano),
		"version":      fmt.Sprintf("W/\"%d\"", modified.UnixNano()),
	}
	if location != "" {
		m["location"] = location
	}
	return m
}

// ListQuery is a GET on a collection.
type ListQuery struct {
	Filter string
	// StartIndex is 1-based; anything below 1 is 1 (RFC 7644 §3.4.2.4).
	StartIndex int
	// Count is the page size; negative means the default, and 0 asks only for totalResults.
	Count int
	// ExcludeMembers drops a group's members (Entra's excludedAttributes=members).
	ExcludeMembers bool
}

func (q ListQuery) page() (start, count int) {
	start = q.StartIndex
	if start < 1 {
		start = 1
	}
	count = q.Count
	if count < 0 {
		count = DefaultPageSize
	}
	if count > MaxPageSize {
		count = MaxPageSize
	}
	return start, count
}

// ListResponse is RFC 7644 §3.4.2's envelope. Resources is never null: Entra reads an absent array
// as a malformed response.
type ListResponse struct {
	Schemas      []string         `json:"schemas"`
	TotalResults int              `json:"totalResults"`
	StartIndex   int              `json:"startIndex"`
	ItemsPerPage int              `json:"itemsPerPage"`
	Resources    []map[string]any `json:"Resources"`
}

func listResponse(total, start int, resources []map[string]any) *ListResponse {
	if resources == nil {
		resources = []map[string]any{}
	}
	return &ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   start,
		ItemsPerPage: len(resources),
		Resources:    resources,
	}
}

// pageOf slices an already-filtered result to the requested page.
func pageOf(all []map[string]any, start, count int) []map[string]any {
	if count == 0 || start > len(all) {
		return nil
	}
	end := start - 1 + count
	if end > len(all) {
		end = len(all)
	}
	return all[start-1 : end]
}

func isUUID(s string) bool { return session.IsUUID(s) }

// errAbort carries a SCIM error out of a store transaction so the transaction rolls back.
type errAbort struct{ e *Error }

func (a errAbort) Error() string { return a.e.Error() }

// txError turns what a store transaction returned into the SCIM error to send.
func txError(err error, conflictDetail string) *Error {
	var abort errAbort
	switch {
	case err == nil:
		return nil
	case errors.As(err, &abort):
		return abort.e
	case errors.Is(err, ErrConflict):
		return &Error{Status: http.StatusConflict, ScimType: "uniqueness", Detail: conflictDetail}
	case errors.Is(err, ErrUnknownTenant):
		return &Error{Status: http.StatusNotFound, Detail: "tenant not found"}
	default:
		return internal(err)
	}
}
