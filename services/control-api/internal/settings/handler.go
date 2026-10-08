// Package settings is the Settings admin API: the audited, admin-only reads and writes behind the
// dashboard's Settings page. It changes a tenant's collection mode (and its narrower per-tool
// overrides), event and content retention, tool sanction decisions, the content search tier, the
// endpoint collector switches, TLS inspection, the ordered enforcement rules and the interception
// routes' kill switches.
//
// Every route requires a product access token carrying the admin role (resolved by the injected
// Authenticator); the tenant is the token's, never the request's. Every write is audited with the
// real actor, the old value and the new value, in the same transaction as the write. A combination
// the database refuses (a mode above the ceiling, a search tier the ceiling cannot back) is
// returned as a named 409 so the page can say what was refused.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Config is the handler's settings.
type Config struct {
	Now    func() time.Time
	Logger *slog.Logger
}

// Handler serves the Settings admin routes.
type Handler struct {
	store store.Store
	auth  deploy.Authenticator
	cfg   Config
}

// NewHandler builds the Settings surface. The store and authenticator are required.
func NewHandler(st store.Store, auth deploy.Authenticator, cfg Config) (*Handler, error) {
	if st == nil {
		return nil, errors.New("settings: store is required")
	}
	if auth == nil {
		return nil, errors.New("settings: an authenticator is required; the Settings API has no anonymous mode")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Handler{store: st, auth: auth, cfg: cfg}, nil
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/v1/settings", h.handleGet)
	mux.HandleFunc("PUT /admin/v1/settings/collection-mode", h.handleCollectionMode)
	mux.HandleFunc("PUT /admin/v1/settings/scope-override", h.handleScopeOverride)
	mux.HandleFunc("PUT /admin/v1/settings/retention", h.handleRetention)
	mux.HandleFunc("PUT /admin/v1/settings/content-search", h.handleContentSearch)
	mux.HandleFunc("PUT /admin/v1/settings/tools/{fingerprint}/sanction", h.handleToolSanction)
	mux.HandleFunc("PUT /admin/v1/settings/endpoint", h.handleEndpoint)
	mux.HandleFunc("PUT /admin/v1/settings/endpoint/tools/{tool_key}", h.handleEndpointTool)
	mux.HandleFunc("PUT /admin/v1/settings/tls-inspection", h.handleTLSInspection)
	mux.HandleFunc("GET /admin/v1/settings/rules", h.handleGetRules)
	mux.HandleFunc("PUT /admin/v1/settings/rules", h.handlePutRules)
	mux.HandleFunc("PUT /admin/v1/settings/kill-switch/{route}", h.handleKillSwitch)
}

const maxSettingsBody = 16 << 10

// maxRulesBody holds a full list of rules, each with its longest message and its match lists.
const maxRulesBody = 1 << 20

func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (deploy.Principal, bool) {
	p, err := h.auth(r)
	if err != nil {
		var e *apierr.Error
		if !errors.As(err, &e) {
			err = apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a valid product access token is required")
		}
		h.fail(w, err)
		return deploy.Principal{}, false
	}
	if !store.IsUUID(p.Tenant) {
		h.fail(w, apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "the access token names no tenant"))
		return deploy.Principal{}, false
	}
	if !slices.Contains(p.Roles, deploy.RoleAdmin) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeForbidden, "the admin role is required"))
		return deploy.Principal{}, false
	}
	return p, true
}

func actorID(p deploy.Principal) string {
	if strings.TrimSpace(p.Actor) != "" {
		return p.Actor
	}
	return p.Subject
}

func (h *Handler) audit(p deploy.Principal, action, objectType, objectID string, now time.Time, detail map[string]any) store.AuditEntry {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["subject"] = p.Subject
	return store.AuditEntry{
		TenantID: p.Tenant, ActorType: store.ActorUser, ActorID: actorID(p),
		Action: action, ObjectType: objectType, ObjectID: objectID, OccurredAt: now, Detail: detail,
	}
}

// --- GET /admin/v1/settings ---------------------------------------------------------------

type toolJSON struct {
	ToolFingerprint string `json:"tool_fingerprint"`
	DisplayName     string `json:"display_name"`
	SanctionedState string `json:"sanctioned_state"`
}

type deviceJSON struct {
	DeviceID       string     `json:"device_id"`
	Hostname       string     `json:"hostname"`
	CollectionMode string     `json:"collection_mode"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
}

type settingsJSON struct {
	CeilingMode          string            `json:"ceiling_mode"`
	CollectionMode       string            `json:"collection_mode"`
	ScopeOverrides       map[string]string `json:"scope_overrides"`
	EventRetentionDays   *int              `json:"event_retention_days"`
	ContentRetentionDays *int              `json:"content_retention_days"`
	RetentionDefaults    retentionJSON     `json:"retention_defaults"`
	ContentSearch        string            `json:"content_search"`
	TLSInspection        bool              `json:"tls_inspection"`
	Tools                []toolJSON        `json:"tools"`
	Devices              []deviceJSON      `json:"devices"`
	Endpoint             endpointJSON      `json:"endpoint"`
	// DataClasses is the labels an enforcement rule may name.
	DataClasses []string `json:"data_classes"`
	// AppCategories is the app catalog's categories, which an enforcement rule may name.
	AppCategories []string `json:"app_categories"`
	// KillSwitches is the tripped kill switches, by route; a route without one is not listed.
	KillSwitches []killSwitchJSON `json:"kill_switches"`
}

// killSwitchJSON is one tripped kill switch: its route, why, since when and by whom.
type killSwitchJSON struct {
	Route       string    `json:"route"`
	ReasonCode  string    `json:"reason_code"`
	EffectiveAt time.Time `json:"effective_at"`
	SetBy       string    `json:"set_by"`
}

// endpointJSON is the tenant's endpoint collector switches, with the defaults where it set none.
type endpointJSON struct {
	Inventory        bool                        `json:"inventory"`
	Processes        bool                        `json:"processes"`
	Flows            bool                        `json:"flows"`
	OTel             bool                        `json:"otel"`
	Hooks            bool                        `json:"hooks"`
	HooksManagedOnly bool                        `json:"hooks_managed_only"`
	Tools            map[string]endpointToolJSON `json:"tools"`
}

type endpointToolJSON struct {
	OTel  bool `json:"otel"`
	Hooks bool `json:"hooks"`
}

type retentionJSON struct {
	EventDays   int `json:"event_days"`
	ContentDays int `json:"content_days"`
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	s, err := h.store.Settings(r.Context(), p.Tenant)
	if errors.Is(err, store.ErrUnknownTenant) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("read settings: %w", err)))
		return
	}
	out := settingsJSON{
		CeilingMode: s.CeilingMode, CollectionMode: s.CollectionMode,
		ScopeOverrides: s.ScopeOverrides, EventRetentionDays: s.EventRetentionDays,
		ContentRetentionDays: s.ContentRetentionDays,
		RetentionDefaults:    retentionJSON{EventDays: s.RetentionDefaults.EventDays, ContentDays: s.RetentionDefaults.ContentDays},
		ContentSearch:        s.ContentSearch,
		TLSInspection:        s.TLSInspection,
		Tools:                []toolJSON{}, Devices: []deviceJSON{},
		DataClasses:   append([]string{}, s.DataClasses...),
		AppCategories: append([]string{}, s.AppCategories...),
		KillSwitches:  []killSwitchJSON{},
	}
	for _, k := range s.KillSwitches {
		out.KillSwitches = append(out.KillSwitches, killSwitchJSON{Route: k.Route, ReasonCode: k.ReasonCode, EffectiveAt: k.EffectiveAt.UTC(), SetBy: k.SetBy})
	}
	if out.ScopeOverrides == nil {
		out.ScopeOverrides = map[string]string{}
	}
	for _, t := range s.Tools {
		out.Tools = append(out.Tools, toolJSON{ToolFingerprint: t.ToolFingerprint, DisplayName: t.DisplayName, SanctionedState: t.SanctionedState})
	}
	for _, d := range s.Devices {
		out.Devices = append(out.Devices, deviceJSON{DeviceID: d.DeviceID, Hostname: d.Hostname, CollectionMode: d.CollectionMode, LastSeenAt: d.LastSeenAt})
	}
	c := s.Endpoint.Collectors
	out.Endpoint = endpointJSON{Inventory: c.Inventory, Processes: c.Processes, Flows: c.Flows, OTel: c.OTel,
		Hooks: c.Hooks, HooksManagedOnly: c.HooksManagedOnly, Tools: map[string]endpointToolJSON{}}
	for k, t := range s.Endpoint.Tools {
		out.Endpoint.Tools[k] = endpointToolJSON{OTel: t.OTel, Hooks: t.Hooks}
	}
	h.writeJSON(w, http.StatusOK, out)
}

// --- PUT /admin/v1/settings/collection-mode -------------------------------------------------

func (h *Handler) handleCollectionMode(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		CollectionMode *string `json:"collection_mode"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.CollectionMode != nil && !validMode(*req.CollectionMode) {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"collection_mode must be m0, m1, m2 or m3", map[string]any{"supported": []string{"m0", "m1", "m2", "m3"}}))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetCollectionMode(r.Context(), p.Tenant, req.CollectionMode,
		h.audit(p, "tenant.collection_mode.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrCollectionExceedsCeiling):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeCollectionExceedsCeiling,
			"the requested mode is above the tenant's ceiling; raise the ceiling first"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set collection mode: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/scope-override --------------------------------------------------

func (h *Handler) handleScopeOverride(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		ToolFingerprint string  `json:"tool_fingerprint"`
		CollectionMode  *string `json:"collection_mode"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	req.ToolFingerprint = strings.TrimSpace(req.ToolFingerprint)
	if req.ToolFingerprint == "" {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "tool_fingerprint is required"))
		return
	}
	if req.CollectionMode != nil && !validMode(*req.CollectionMode) {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"collection_mode must be m0, m1, m2 or m3", map[string]any{"supported": []string{"m0", "m1", "m2", "m3"}}))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetScopeOverride(r.Context(), p.Tenant, req.ToolFingerprint, req.CollectionMode,
		h.audit(p, "tenant.scope_override.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrScopeOverrideTooWide):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeScopeOverrideTooWide,
			"the override is wider than the tenant's requested mode; an override can only narrow collection"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set scope override: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/retention -------------------------------------------------------

func (h *Handler) handleRetention(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		AppliesTo string `json:"applies_to"`
		TTLDays   int    `json:"ttl_days"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.AppliesTo != "event" && req.AppliesTo != "content" {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"applies_to must be event or content", map[string]any{"supported": []string{"event", "content"}}))
		return
	}
	if req.TTLDays <= 0 {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeRetentionOutOfRange, "ttl_days must be positive"))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetRetention(r.Context(), p.Tenant, req.AppliesTo, req.TTLDays,
		h.audit(p, "tenant.retention.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrRetentionOutOfRange):
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeRetentionOutOfRange, "the retention period is outside the retention classes"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set retention: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/content-search --------------------------------------------------

func (h *Handler) handleContentSearch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		ContentSearch string `json:"content_search"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if !validSearchTier(req.ContentSearch) {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"content_search must be disabled, attachment_names or full_text",
			map[string]any{"supported": []string{"disabled", "attachment_names", "full_text"}}))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetContentSearch(r.Context(), p.Tenant, req.ContentSearch,
		h.audit(p, "tenant.content_search.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrSearchTierRequiresCeiling):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeSearchTierRequiresMode,
			"this search tier needs a higher collection ceiling: full_text needs M3, attachment names need M1 or above"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set content search: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/tools/{fingerprint}/sanction ------------------------------------

func (h *Handler) handleToolSanction(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	fingerprint := r.PathValue("fingerprint")
	if strings.TrimSpace(fingerprint) == "" {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such tool"))
		return
	}
	var req struct {
		SanctionedState string `json:"sanctioned_state"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if !validSanction(req.SanctionedState) {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"sanctioned_state must be sanctioned, unsanctioned or unknown",
			map[string]any{"supported": []string{"sanctioned", "unsanctioned", "unknown"}}))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetToolSanction(r.Context(), p.Tenant, fingerprint, req.SanctionedState,
		h.audit(p, "tool.sanction", "tool", fingerprint, now, nil))
	switch {
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set tool sanction: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/endpoint --------------------------------------------------------

func (h *Handler) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	// Every switch is required, so a client that left one out does not turn a collector off.
	var req struct {
		Inventory        *bool `json:"inventory"`
		Processes        *bool `json:"processes"`
		Flows            *bool `json:"flows"`
		OTel             *bool `json:"otel"`
		Hooks            *bool `json:"hooks"`
		HooksManagedOnly *bool `json:"hooks_managed_only"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.Inventory == nil || req.Processes == nil || req.Flows == nil || req.OTel == nil || req.Hooks == nil || req.HooksManagedOnly == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"inventory, processes, flows, otel, hooks and hooks_managed_only are all required"))
		return
	}
	c := store.EndpointCollectors{Inventory: *req.Inventory, Processes: *req.Processes, Flows: *req.Flows,
		OTel: *req.OTel, Hooks: *req.Hooks, HooksManagedOnly: *req.HooksManagedOnly}
	now := h.cfg.Now().UTC()
	err := h.store.SetEndpointCollectors(r.Context(), p.Tenant, c,
		h.audit(p, "tenant.endpoint_collectors.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set endpoint collectors: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/endpoint/tools/{tool_key} ---------------------------------------

func (h *Handler) handleEndpointTool(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	toolKey := r.PathValue("tool_key")
	if !slices.Contains(store.EndpointToolKeys, toolKey) {
		h.fail(w, apierr.Detailed(http.StatusNotFound, apierr.CodeNotFound, "no such tool",
			map[string]any{"supported": store.EndpointToolKeys}))
		return
	}
	var req struct {
		OTel  *bool `json:"otel"`
		Hooks *bool `json:"hooks"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.OTel == nil || req.Hooks == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "otel and hooks are both required"))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetEndpointTool(r.Context(), p.Tenant, toolKey, store.EndpointTool{OTel: *req.OTel, Hooks: *req.Hooks},
		h.audit(p, "tenant.endpoint_tool.set", "endpoint_tool", toolKey, now, nil))
	switch {
	case errors.Is(err, store.ErrUnknownEndpointTool):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such tool"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set endpoint tool: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/tls-inspection -------------------------------------------------

func (h *Handler) handleTLSInspection(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	// Required, so a client that left it out does not turn inspection off.
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "enabled is required"))
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetTLSInspection(r.Context(), p.Tenant, *req.Enabled,
		h.audit(p, "tenant.tls_inspection.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set TLS inspection: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT /admin/v1/settings/kill-switch/{route} ---------------------------------------------

// reasonCodePattern is a kill switch's reason code (ops.kill_switch.reason_code): a short code an
// operator can attribute a coverage drop to, never free text.
var reasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// handleKillSwitch trips (on) or clears one interception route's kill switch. Tripping needs a
// reason code; a clear may carry one, which is audited.
func (h *Handler) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	route := r.PathValue("route")
	if !slices.Contains(store.KillSwitchRoutes, route) {
		h.fail(w, apierr.Detailed(http.StatusNotFound, apierr.CodeNotFound, "no kill switch for this route",
			map[string]any{"supported": store.KillSwitchRoutes}))
		return
	}
	// on is required, so a client that left it out neither trips nor clears the switch.
	var req struct {
		On         *bool   `json:"on"`
		ReasonCode *string `json:"reason_code"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.On == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "on is required"))
		return
	}
	reason := ""
	if req.ReasonCode != nil {
		reason = *req.ReasonCode
	}
	switch {
	case *req.On && reason == "":
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "reason_code is required to trip a kill switch"))
		return
	case reason != "" && !reasonCodePattern.MatchString(reason):
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
			"reason_code must start with a lower-case letter and use only lower-case letters, digits, \"_\", \".\" or \"-\" (at most 64)"))
		return
	}
	now := h.cfg.Now().UTC()
	var detail map[string]any
	if !*req.On && reason != "" {
		detail = map[string]any{"reason_code": reason}
	}
	err := h.store.SetKillSwitch(r.Context(), p.Tenant, route, *req.On, reason,
		h.audit(p, "tenant.kill_switch.set", "kill_switch", route, now, detail))
	switch {
	case errors.Is(err, store.ErrUnknownKillSwitchRoute):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no kill switch for this route"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("set kill switch: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET and PUT /admin/v1/settings/rules --------------------------------------------------

// ruleJSON is one enforcement rule in the policy bundle's spelling. Every match list is sent, empty
// when it matches anything.
type ruleJSON struct {
	RuleID  string        `json:"rule_id"`
	Action  string        `json:"action"`
	Match   ruleMatchJSON `json:"match"`
	Message string        `json:"message"`
	Link    string        `json:"link,omitempty"`
}

type ruleMatchJSON struct {
	Labels     []string `json:"labels"`
	Tools      []string `json:"tools"`
	Categories []string `json:"categories"`
	Sanction   []string `json:"sanction"`
	Routes     []string `json:"routes"`
}

type rulesJSON struct {
	Rules []ruleJSON `json:"rules"`
}

var ruleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// maxRuleMessage is the longest message a rule may carry, in characters.
const maxRuleMessage = 280

// ruleSanctions is the closed set of a rule's sanction values.
var ruleSanctions = []string{"sanctioned", "unsanctioned"}

func (h *Handler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	rules, err := h.store.EnforcementRules(r.Context(), p.Tenant)
	if errors.Is(err, store.ErrUnknownTenant) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("read enforcement rules: %w", err)))
		return
	}
	out := rulesJSON{Rules: []ruleJSON{}}
	for _, rule := range rules {
		m := rule.Match
		out.Rules = append(out.Rules, ruleJSON{RuleID: rule.RuleID, Action: rule.Action, Message: rule.Message, Link: rule.Link,
			Match: ruleMatchJSON{Labels: listJSON(m.Labels), Tools: listJSON(m.Tools), Categories: listJSON(m.Categories),
				Sanction: listJSON(m.Sanction), Routes: listJSON(m.Routes)}})
	}
	h.writeJSON(w, http.StatusOK, out)
}

func listJSON(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (h *Handler) handlePutRules(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	// The list is required, so a client that sent none does not clear the tenant's rules.
	var req struct {
		Rules *[]ruleJSON `json:"rules"`
	}
	if !h.decodeLimit(w, r, &req, maxRulesBody) {
		return
	}
	if req.Rules == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "rules is required; send [] to remove every rule"))
		return
	}
	rules, err := validRules(*req.Rules)
	if err != nil {
		h.fail(w, err)
		return
	}
	now := h.cfg.Now().UTC()
	err = h.store.ReplaceEnforcementRules(r.Context(), p.Tenant, rules,
		h.audit(p, "tenant.enforcement_rules.set", "tenant", p.Tenant, now, nil))
	switch {
	case errors.Is(err, store.ErrUnknownRuleLabel):
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "a rule names a label that is not a data class"))
		return
	case errors.Is(err, store.ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(fmt.Errorf("replace enforcement rules: %w", err)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validRules refuses every value the device's bundle validation would refuse, and the values the
// database refuses that can be checked here, naming the rule and the field. Labels are checked
// against ref.data_class by the database.
func validRules(in []ruleJSON) ([]store.EnforcementRule, error) {
	if len(in) > store.MaxEnforcementRules {
		return nil, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			fmt.Sprintf("at most %d rules are allowed", store.MaxEnforcementRules), map[string]any{"max": store.MaxEnforcementRules})
	}
	refuse := func(i int, field, message string) error {
		return apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest,
			fmt.Sprintf("rule %d: %s", i+1, message), map[string]any{"index": i, "field": field})
	}
	seen := map[string]bool{}
	out := make([]store.EnforcementRule, 0, len(in))
	for i, rule := range in {
		if !ruleIDPattern.MatchString(rule.RuleID) {
			return nil, refuse(i, "rule_id", "rule_id must start with a lower-case letter and hold at most 128 lower-case letters, digits, '_', '.' or '-'")
		}
		if seen[rule.RuleID] {
			return nil, refuse(i, "rule_id", fmt.Sprintf("rule_id %q is used by an earlier rule", rule.RuleID))
		}
		seen[rule.RuleID] = true
		if !slices.Contains(store.RuleActions, rule.Action) {
			return nil, refuse(i, "action", "action must be allow, warn or block")
		}
		if utf8.RuneCountInString(rule.Message) > maxRuleMessage {
			return nil, refuse(i, "message", fmt.Sprintf("message is longer than %d characters", maxRuleMessage))
		}
		if rule.Link != "" && !httpsURL(rule.Link) {
			return nil, refuse(i, "link", "link must be an https:// URL")
		}
		m := rule.Match
		for _, list := range []struct {
			field  string
			values []string
		}{{"labels", m.Labels}, {"tools", m.Tools}, {"categories", m.Categories}, {"sanction", m.Sanction}, {"routes", m.Routes}} {
			for _, v := range list.values {
				if strings.TrimSpace(v) == "" {
					return nil, refuse(i, "match."+list.field, "match."+list.field+" holds an empty value")
				}
			}
		}
		for _, v := range m.Sanction {
			if !slices.Contains(ruleSanctions, v) {
				return nil, refuse(i, "match.sanction", fmt.Sprintf("match.sanction %q must be sanctioned or unsanctioned", v))
			}
		}
		for _, v := range m.Routes {
			if !protocol.Route(v).Valid() {
				return nil, refuse(i, "match.routes", fmt.Sprintf("match.routes %q is not a collection route", v))
			}
		}
		out = append(out, store.EnforcementRule{RuleID: rule.RuleID, Action: rule.Action, Message: rule.Message, Link: rule.Link,
			Match: store.RuleMatch{Labels: listJSON(m.Labels), Tools: listJSON(m.Tools), Categories: listJSON(m.Categories),
				Sanction: listJSON(m.Sanction), Routes: listJSON(m.Routes)}})
	}
	return out, nil
}

// httpsURL reports whether s is an absolute https URL with a host, spelled with the lower-case
// scheme the database's CHECK requires.
func httpsURL(s string) bool {
	if !strings.HasPrefix(s, "https://") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// --- plumbing -------------------------------------------------------------------------------

func validMode(m string) bool {
	switch m {
	case "m0", "m1", "m2", "m3":
		return true
	}
	return false
}

func validSearchTier(t string) bool {
	switch t {
	case "disabled", "attachment_names", "full_text":
		return true
	}
	return false
}

func validSanction(s string) bool {
	switch s {
	case "sanctioned", "unsanctioned", "unknown":
		return true
	}
	return false
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return h.decodeLimit(w, r, v, maxSettingsBody)
}

func (h *Handler) decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
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
