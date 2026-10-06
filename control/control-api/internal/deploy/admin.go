// Package deploy is the deployment admin API and the packages it builds: the Settings ->
// Deployment page's read, the tenant package download that mints a deployment key per download,
// key revocation, the device-verification setting, the SCIM token endpoints, and the browser
// extension's update manifest and CRX.
//
// Every admin route requires a product access token carrying the admin role, resolved by an
// injected Authenticator; the tenant is the token's, never the request's. Every write is audited
// with the real actor, in the same transaction as the write where the store owns both and
// immediately after it where another component does (SCIM tokens).
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Principal is who an admin request authenticates as: the product token's tenant, actor (UPN or
// email), subject and roles.
type Principal struct {
	Tenant  string
	Actor   string
	Subject string
	Roles   []string
}

// Authenticator resolves a request's Principal from its product access token. An error is a 401.
type Authenticator func(r *http.Request) (Principal, error)

// RoleAdmin is the role every route here requires.
const RoleAdmin = "admin"

// ScimToken is one SCIM bearer token as the admin page lists it; the token itself is shown once, at
// creation, and never listed.
type ScimToken struct {
	TokenID   string
	Label     string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// ScimTokens is the SCIM side's token store. Implementations do not audit: these routes do, with
// the admin as actor. Revoke returns an error wrapping ErrNotFound for a token the tenant does not
// have.
type ScimTokens interface {
	Create(ctx context.Context, tenantID, label, createdBy string) (tokenID, token string, err error)
	Revoke(ctx context.Context, tenantID, tokenID, revokedBy string) error
	List(ctx context.Context, tenantID string) ([]ScimToken, error)
}

// ErrNotFound is what a ScimTokens implementation wraps for an unknown token.
var ErrNotFound = errors.New("deploy: not found")

// Config is the admin surface's settings.
type Config struct {
	// ReleaseDir holds ShadowAICapture.msi, the browser extension and release.json.
	ReleaseDir string
	// DeviceEndpoint is the public device origin written into every tenant package.
	DeviceEndpoint string
	// ScimBaseURL is the SCIM base a customer's identity provider is pointed at.
	ScimBaseURL string
	// MaxReleaseBytes bounds the MSI a package is built from.
	MaxReleaseBytes int64
	Now             func() time.Time
	Logger          *slog.Logger
}

// Handler serves the deployment admin routes.
type Handler struct {
	store store.Store
	auth  Authenticator
	scim  ScimTokens
	cfg   Config
}

// NewHandler builds the admin surface. A nil ScimTokens leaves the SCIM token routes answering 503
// and the page's token list empty; the store and authenticator are required.
func NewHandler(st store.Store, auth Authenticator, scim ScimTokens, cfg Config) (*Handler, error) {
	if st == nil {
		return nil, errors.New("deploy: store is required")
	}
	if auth == nil {
		return nil, errors.New("deploy: an authenticator is required; the admin API has no anonymous mode")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxReleaseBytes <= 0 {
		cfg.MaxReleaseBytes = DefaultMaxReleaseBytes
	}
	return &Handler{store: st, auth: auth, scim: scim, cfg: cfg}, nil
}

// Register adds the routes to mux. They are method-qualified patterns, so they sit beside other
// packages' routes on the same mux without claiming the /admin/v1/ prefix.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/v1/deployment", h.handleSummary)
	mux.HandleFunc("POST /admin/v1/deployment/package", h.handlePackage)
	mux.HandleFunc("POST /admin/v1/deployment/keys/{key_id}/revoke", h.handleRevokeKey)
	mux.HandleFunc("PUT /admin/v1/deployment/verification", h.handleVerification)
	mux.HandleFunc("POST /admin/v1/scim/tokens", h.handleCreateScimToken)
	mux.HandleFunc("POST /admin/v1/scim/tokens/{id}/revoke", h.handleRevokeScimToken)
}

// maxAdminBody caps an admin request body: every one is a small JSON object.
const maxAdminBody = 16 << 10

// admin authenticates the request and requires the admin role. It answers the caller itself on
// failure.
func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := h.auth(r)
	if err != nil {
		var e *apierr.Error
		if !errors.As(err, &e) {
			err = apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a valid product access token is required")
		}
		h.fail(w, err)
		return Principal{}, false
	}
	if !store.IsUUID(p.Tenant) {
		h.fail(w, apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "the access token names no tenant"))
		return Principal{}, false
	}
	if !slices.Contains(p.Roles, RoleAdmin) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeForbidden, "the admin role is required"))
		return Principal{}, false
	}
	return p, true
}

func (p Principal) actorID() string {
	if strings.TrimSpace(p.Actor) != "" {
		return p.Actor
	}
	return p.Subject
}

func (h *Handler) userAudit(p Principal, action, objectType, objectID string, now time.Time, detail map[string]any) store.AuditEntry {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["subject"] = p.Subject
	return store.AuditEntry{
		TenantID: p.Tenant, ActorType: store.ActorUser, ActorID: p.actorID(),
		Action: action, ObjectType: objectType, ObjectID: objectID, OccurredAt: now, Detail: detail,
	}
}

// --- GET /admin/v1/deployment ---------------------------------------------------------------

type summaryJSON struct {
	Connection         *connectionJSON `json:"connection"`
	DeviceVerification string          `json:"device_verification"`
	DeploymentKeys     []keyJSON       `json:"deployment_keys"`
	Scim               scimJSON        `json:"scim"`
	Devices            devicesJSON     `json:"devices"`
	Release            *releaseJSON    `json:"release"`
}

type connectionJSON struct {
	Provider      string `json:"provider"`
	Status        string `json:"status"`
	EntraTenantID string `json:"entra_tenant_id,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
}

type keyJSON struct {
	KeyID          string     `json:"key_id"`
	Label          string     `json:"label"`
	CreatedAt      time.Time  `json:"created_at"`
	CreatedBy      string     `json:"created_by"`
	ExpiresAt      *time.Time `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	EnrolmentCount int64      `json:"enrolment_count"`
	LastUsedAt     *time.Time `json:"last_used_at"`
}

type scimTokenJSON struct {
	TokenID   string     `json:"token_id"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type scimJSON struct {
	Tokens            []scimTokenJSON `json:"tokens"`
	BaseURL           string          `json:"base_url"`
	Users             int64           `json:"users"`
	Groups            int64           `json:"groups"`
	LastProvisionedAt *time.Time      `json:"last_provisioned_at"`
}

type devicesJSON struct {
	Enrolled       int64      `json:"enrolled"`
	LastEnrolledAt *time.Time `json:"last_enrolled_at"`
}

type releaseJSON struct {
	Version     string `json:"version"`
	ProductCode string `json:"product_code"`
}

func (h *Handler) handleSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	sum, err := h.store.DeploymentSummary(r.Context(), p.Tenant)
	if errors.Is(err, store.ErrUnknownTenant) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("deployment summary: %w", err)))
		return
	}
	out := summaryJSON{
		DeviceVerification: sum.DeviceVerification,
		DeploymentKeys:     []keyJSON{},
		Scim:               scimJSON{Tokens: []scimTokenJSON{}, BaseURL: h.cfg.ScimBaseURL, Users: sum.ScimUsers, Groups: sum.ScimGroups, LastProvisionedAt: sum.LastProvisionedAt},
		Devices:            devicesJSON{Enrolled: sum.DevicesEnrolled, LastEnrolledAt: sum.LastEnrolledAt},
	}
	if c := sum.Connection; c != nil {
		out.Connection = &connectionJSON{Provider: c.Provider, Status: c.Status, EntraTenantID: c.EntraTenantID, Issuer: c.Issuer}
	}
	for _, k := range sum.Keys {
		out.DeploymentKeys = append(out.DeploymentKeys, keyJSON{
			KeyID: k.KeyID, Label: k.Label, CreatedAt: k.CreatedAt, CreatedBy: k.CreatedBy,
			ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, EnrolmentCount: k.EnrolmentCount, LastUsedAt: k.LastUsedAt,
		})
	}
	if h.scim != nil {
		tokens, err := h.scim.List(r.Context(), p.Tenant)
		if err != nil {
			h.fail(w, apierr.Internal(fmt.Errorf("list scim tokens: %w", err)))
			return
		}
		for _, t := range tokens {
			out.Scim.Tokens = append(out.Scim.Tokens, scimTokenJSON(t))
		}
	}
	// A missing or unreadable release is shown as no release, not as a failed page: the admin can
	// still read everything else and the package route says what is wrong.
	if rel, err := ReadRelease(h.cfg.ReleaseDir); err == nil {
		out.Release = &releaseJSON{Version: rel.Version, ProductCode: rel.ProductCode}
	}
	h.writeJSON(w, http.StatusOK, out)
}

// --- POST /admin/v1/deployment/package ------------------------------------------------------

// Package formats.
const (
	FormatIntuneWin = "intunewin"
	FormatZip       = "zip"
)

func (h *Handler) handlePackage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		Format string `json:"format"`
		Label  string `json:"label"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.Format != FormatIntuneWin && req.Format != FormatZip {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeUnsupportedFormat,
			"format must be intunewin or zip", map[string]any{"supported": []string{FormatIntuneWin, FormatZip}}))
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > 200 {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "label is longer than 200 characters"))
		return
	}
	now := h.cfg.Now().UTC()
	if label == "" {
		label = fmt.Sprintf("%s package, %s", req.Format, now.Format("2006-01-02 15:04 UTC"))
	}
	if strings.TrimSpace(h.cfg.DeviceEndpoint) == "" {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeReleaseUnavailable,
			"this deployment has no public device endpoint configured, so a package would not know where to enrol"))
		return
	}
	// The tenant is checked before anything is built, so an unknown tenant is a refusal, not a
	// package built for nobody.
	if _, err := h.store.DeviceVerification(r.Context(), p.Tenant); err != nil {
		if errors.Is(err, store.ErrUnknownTenant) {
			h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
			return
		}
		h.fail(w, apierr.Internal(fmt.Errorf("tenant: %w", err)))
		return
	}
	rel, msi, err := LoadRelease(h.cfg.ReleaseDir, h.cfg.MaxReleaseBytes)
	if err != nil {
		h.cfg.Logger.Error("control: agent release unavailable", "error", err)
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeReleaseUnavailable,
			"the agent release is not available on this deployment"))
		return
	}

	keyID, err := store.NewUUID()
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	plaintext, err := enrol.MintDeploymentKey(p.Tenant)
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	env := TenantEnv(strings.ToLower(p.Tenant), h.cfg.DeviceEndpoint, plaintext)
	var pkg []byte
	var filename, contentType string
	switch req.Format {
	case FormatIntuneWin:
		pkg, err = BuildIntuneWin(rel, msi, env, now)
		filename, contentType = "ShadowAICapture-"+safeVersion(rel.Version)+".intunewin", "application/octet-stream"
	default:
		pkg, err = BuildZip(rel, msi, env, now)
		filename, contentType = "ShadowAICapture-"+safeVersion(rel.Version)+".zip", "application/zip"
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("build %s package: %w", req.Format, err)))
		return
	}

	// The key is stored only once its package exists, so a failed build leaves no orphan key; and
	// the package is sent only once the key is stored, so no package carries a key nobody can use.
	// A key does not expire: an MDM installs the same package for as long as it is assigned, and
	// revocation is the control.
	key := store.DeploymentKey{
		KeyID: keyID, TenantID: p.Tenant, KeyHash: enrol.HashDeploymentKey(plaintext), Label: label,
		CreatedBy: p.actorID(), CreatedAt: now,
	}
	detail := map[string]any{"label": label, "format": req.Format, "release_version": rel.Version}
	if _, err := h.store.CreateDeploymentKey(r.Context(), key,
		h.userAudit(p, "deployment_key.create", "deployment_key", keyID, now, detail)); err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("store deployment key: %w", err)))
		return
	}
	h.cfg.Logger.Info("control: deployment package issued", "tenant", p.Tenant, "key_id", keyID,
		"format", req.Format, "release", rel.Version, "bytes", len(pkg))

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(pkg)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Sac-Deployment-Key-Id", keyID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pkg)
}

// safeVersion keeps a release version usable inside a quoted filename.
func safeVersion(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, v)
}

// --- POST /admin/v1/deployment/keys/{key_id}/revoke -----------------------------------------

func (h *Handler) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	keyID := strings.ToLower(r.PathValue("key_id"))
	if !store.IsUUID(keyID) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such deployment key"))
		return
	}
	now := h.cfg.Now().UTC()
	_, err := h.store.RevokeDeploymentKey(r.Context(), p.Tenant, keyID, now,
		h.userAudit(p, "deployment_key.revoke", "deployment_key", keyID, now, nil))
	if errors.Is(err, store.ErrDeploymentKeyUnknown) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such deployment key"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("revoke deployment key: %w", err)))
		return
	}
	h.cfg.Logger.Info("control: deployment key revoked", "tenant", p.Tenant, "key_id", keyID)
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/deployment/verification --------------------------------------------------

func (h *Handler) handleVerification(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		DeviceVerification string `json:"device_verification"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	mode := req.DeviceVerification
	if mode != store.VerificationNone && mode != store.VerificationIntune {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidVerification,
			"device_verification must be none or intune",
			map[string]any{"supported": []string{store.VerificationNone, store.VerificationIntune}}))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetDeviceVerification(r.Context(), p.Tenant, mode,
		h.userAudit(p, "tenant.device_verification.set", "tenant", p.Tenant, now, map[string]any{"device_verification": mode}))
	switch {
	case errors.Is(err, store.ErrNoEntraConnection):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeNoEntraConnection,
			"Intune verification needs an active Microsoft Entra connection for this tenant"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set device verification: %w", err)))
		return
	}
	h.cfg.Logger.Info("control: device verification set", "tenant", p.Tenant, "mode", mode)
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /admin/v1/scim/tokens, POST /admin/v1/scim/tokens/{id}/revoke ---------------------

func (h *Handler) handleCreateScimToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	if h.scim == nil {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "SCIM provisioning is not configured on this deployment"))
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > 200 {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "label is longer than 200 characters"))
		return
	}
	now := h.cfg.Now().UTC()
	tokenID, token, err := h.scim.Create(r.Context(), p.Tenant, label, p.actorID())
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("create scim token: %w", err)))
		return
	}
	if err := h.store.Audit(r.Context(), h.userAudit(p, "scim_token.create", "scim_token", tokenID, now,
		map[string]any{"label": label})); err != nil {
		// An unaudited credential must not exist: withdraw it and report the failure, so the admin
		// retries and gets a token whose creation is on the record.
		_ = h.scim.Revoke(r.Context(), p.Tenant, tokenID, "control-api:audit-failure")
		h.fail(w, apierr.Internal(fmt.Errorf("audit scim token: %w", err)))
		return
	}
	h.cfg.Logger.Info("control: scim token created", "tenant", p.Tenant, "token_id", tokenID)
	h.writeJSON(w, http.StatusCreated, map[string]string{"token_id": tokenID, "token": token, "base_url": h.cfg.ScimBaseURL})
}

func (h *Handler) handleRevokeScimToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	if h.scim == nil {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "SCIM provisioning is not configured on this deployment"))
		return
	}
	tokenID := strings.ToLower(r.PathValue("id"))
	if !store.IsUUID(tokenID) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such SCIM token"))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.scim.Revoke(r.Context(), p.Tenant, tokenID, p.actorID())
	if errors.Is(err, ErrNotFound) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such SCIM token"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("revoke scim token: %w", err)))
		return
	}
	if err := h.store.Audit(r.Context(), h.userAudit(p, "scim_token.revoke", "scim_token", tokenID, now, nil)); err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("audit scim token revoke: %w", err)))
		return
	}
	h.cfg.Logger.Info("control: scim token revoked", "tenant", p.Tenant, "token_id", tokenID)
	w.WriteHeader(http.StatusNoContent)
}

// --- plumbing -------------------------------------------------------------------------------

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err != nil {
		h.fail(w, apierr.New(http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest, "the request body is over the cap"))
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "the request body is not the expected JSON object"))
		return false
	}
	return true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	apierr.Write(w, err, h.cfg.Now(), h.cfg.Logger)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.cfg.Logger.Error("control: write response", "error", err)
	}
}
