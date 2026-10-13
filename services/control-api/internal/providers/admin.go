package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Admin store errors.
var (
	ErrUnknownTenant = errors.New("providers: tenant unknown")
	ErrNotFound      = errors.New("providers: no such provider or connection")
	ErrNoEntra       = errors.New("providers: no active Entra connection")
)

// Provider is one provider as the Integrations page lists it, with the tenant's connection to it.
type Provider struct {
	Provider        string `json:"provider"`
	DisplayName     string `json:"display_name"`
	Vendor          string `json:"vendor"`
	Credential      string `json:"credential"`
	CredentialLabel string `json:"credential_label"`
	AccountLabel    string `json:"account_label"`
	SourceURL       string `json:"source_url"`
	Tools           []Tool `json:"tools"`
	Available       bool   `json:"available"`
	// Intake is what the customer sets up to send an intake provider's log (Transport.Kind); empty
	// for a provider control-api pulls.
	Intake     string           `json:"intake"`
	Connection *ConnectionState `json:"connection"`
}

// Tool is a catalogued tool a provider's records are filed under.
type Tool struct {
	App  string `json:"app"`
	Name string `json:"name"`
}

// ConnectionState is a tenant's connection as the page shows it. The key is never read back.
type ConnectionState struct {
	ConnectionID    string     `json:"connection_id"`
	Account         string     `json:"account"`
	Status          string     `json:"status"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedBy       string     `json:"updated_by"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastStartedAt   *time.Time `json:"last_started_at"`
	LastCompletedAt *time.Time `json:"last_completed_at"`
	LastStatus      string     `json:"last_status"`
	LastError       string     `json:"last_error"`
	RecordsRead     int64      `json:"records_read"`
	RecordsNew      int64      `json:"records_new"`
	// IntakeURL is where an active intake connection's deliveries are sent; empty otherwise.
	IntakeURL string `json:"intake_url"`
}

// Audit is one write's audit row.
type Audit struct {
	Actor      string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
	At         time.Time
}

// AdminStore is the persistence the page needs. Every method is tenant-scoped and returns
// ErrUnknownTenant for a tenant that does not exist.
type AdminStore interface {
	Providers(ctx context.Context, tenantID string) ([]Provider, error)
	EntraConnected(ctx context.Context, tenantID string) (bool, error)
	// Connect connects a provider, or reconnects it, with a sealed key or an intake token's hash,
	// and returns the connection's id: ErrNotFound for an unknown provider, ErrNoEntra for
	// Microsoft's without an active Entra connection.
	Connect(ctx context.Context, tenantID, provider, account string, sealedKey []byte, tokenHash string, a Audit) (string, error)
	// Disconnect stops an active connection and forgets its key or intake token; ErrNotFound when
	// there is none.
	Disconnect(ctx context.Context, tenantID, provider string, a Audit) error
	// Connection returns the connection to a provider and whether it is active; ErrNotFound when
	// the tenant has none.
	Connection(ctx context.Context, tenantID, provider string) (id string, active bool, err error)
	Audit(ctx context.Context, tenantID string, a Audit) error
}

// Sealer seals a key under the tenant's directory key. *directory.Cipher implements it.
type Sealer interface {
	Seal(tenantID, plaintext string) ([]byte, error)
}

// Pull is the runner as the page drives it. *Runner implements it.
type Pull interface {
	Available(provider string) bool
	Trigger(tenantID, connectionID string) bool
}

// Intakes is the intake as the page shows it. *Intake implements it.
type Intakes interface {
	Available(provider string) bool
	Kind(provider string) string
	URL(provider, connectionID string) string
}

// Handler serves the Integrations page's API: every provider, and the tenant's connection to each.
// Every route requires a product access token carrying the admin role; the tenant is the token's.
// Every write is audited with the admin as actor, in the write's own transaction, and no response
// or audit row carries a key. An intake token is returned once, by the write that issues it.
type Handler struct {
	store  AdminStore
	auth   deploy.Authenticator
	sealer Sealer
	pull   Pull
	intake Intakes
	now    func() time.Time
	logger *slog.Logger
}

// NewHandler builds the handler. Every argument but the logger is required.
func NewHandler(st AdminStore, auth deploy.Authenticator, sealer Sealer, pull Pull, intake Intakes, logger *slog.Logger) (*Handler, error) {
	if st == nil || auth == nil || sealer == nil || pull == nil || intake == nil {
		return nil, errors.New("providers: a store, an authenticator, a sealer, the pull and the intake are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: st, auth: auth, sealer: sealer, pull: pull, intake: intake, now: time.Now, logger: logger}, nil
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/v1/providers", h.handleList)
	mux.HandleFunc("PUT /admin/v1/providers/{provider}", h.handleConnect)
	mux.HandleFunc("DELETE /admin/v1/providers/{provider}", h.handleDisconnect)
	mux.HandleFunc("POST /admin/v1/providers/{provider}/run", h.handleRun)
}

// Bounds on what a request carries.
const (
	maxBody    = 16 << 10
	maxKeyLen  = 4096
	maxAccount = 200
)

// accountPattern is what an account may be: a ChatGPT workspace id, a Claude organization id. It
// goes into a provider's URL path and query.
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

type listJSON struct {
	EntraConnected bool       `json:"entra_connected"`
	Providers      []Provider `json:"providers"`
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	list, err := h.store.Providers(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "list providers") {
		return
	}
	entra, err := h.store.EntraConnected(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "entra connection") {
		return
	}
	for i := range list {
		p := &list[i]
		p.Available = h.available(*p)
		if p.Credential != CredentialIntake {
			continue
		}
		p.Intake = h.intake.Kind(p.Provider)
		if c := p.Connection; c != nil && c.Status == "active" && p.Available {
			c.IntakeURL = h.intake.URL(p.Provider, c.ConnectionID)
		}
	}
	h.writeJSON(w, http.StatusOK, listJSON{EntraConnected: entra, Providers: list})
}

type connectJSON struct {
	Account string `json:"account"`
	Key     string `json:"key"`
}

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	prov, ok := h.provider(w, r, p)
	if !ok {
		return
	}
	var req connectJSON
	if !h.decode(w, r, &req) {
		return
	}
	account, key := strings.TrimSpace(req.Account), strings.TrimSpace(req.Key)
	switch prov.Credential {
	case CredentialEntraApp:
		if account != "" || key != "" {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
				prov.DisplayName+" is read with the consent the tenant gave in Microsoft Entra; it takes no key or account"))
			return
		}
	case CredentialIntake:
		if account != "" || key != "" {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
				prov.DisplayName+" sends its log with a token this page issues; it takes no key or account"))
			return
		}
		h.connectIntake(w, r, p, prov)
		return
	default:
		if key == "" || len(key) > maxKeyLen || strings.ContainsAny(key, " \t\r\n") {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
				fmt.Sprintf("the %s is required, without spaces", strings.ToLower(prov.CredentialLabel))))
			return
		}
		if (prov.AccountLabel != "") != (account != "") || account != "" && !accountPattern.MatchString(account) {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest,
				fmt.Sprintf("the %s is required: up to %d letters, digits and . _ : -", strings.ToLower(prov.AccountLabel), maxAccount)))
			return
		}
	}
	var sealed []byte
	if key != "" {
		var err error
		if sealed, err = h.sealer.Seal(p.Tenant, key); err != nil {
			h.fail(w, apierr.Internal(fmt.Errorf("seal key: %w", err)))
			return
		}
	}
	a := h.audit(p, "provider.connect", map[string]any{"provider": prov.Provider, "account": account})
	id, err := h.store.Connect(r.Context(), p.Tenant, prov.Provider, account, sealed, "", a)
	if errors.Is(err, ErrNoEntra) {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeNoEntraConnection,
			"reading "+prov.DisplayName+" needs an active Microsoft Entra connection for this tenant"))
		return
	}
	if h.storeFailed(w, err, "connect provider") {
		return
	}
	queued := h.pull.Trigger(p.Tenant, id)
	h.logger.Info("control: provider connected", "tenant", p.Tenant, "provider", prov.Provider, "connection", id)
	h.writeJSON(w, http.StatusOK, map[string]any{"connection_id": id, "queued": queued})
}

// connectIntake connects an intake provider with a new token, or replaces the token of its
// connection, which the old one stops opening. The token is in this answer and nowhere else.
func (h *Handler) connectIntake(w http.ResponseWriter, r *http.Request, p deploy.Principal, prov Provider) {
	token, hash, err := NewIntakeToken()
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	a := h.audit(p, "provider.connect", map[string]any{"provider": prov.Provider, "intake_token": "issued"})
	id, err := h.store.Connect(r.Context(), p.Tenant, prov.Provider, "", nil, hash, a)
	if h.storeFailed(w, err, "connect provider") {
		return
	}
	h.logger.Info("control: provider intake token issued", "tenant", p.Tenant, "provider", prov.Provider, "connection", id)
	h.writeJSON(w, http.StatusOK, map[string]any{"connection_id": id, "queued": false,
		"intake_url": h.intake.URL(prov.Provider, id), "intake_token": token})
}

func (h *Handler) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	prov, ok := h.provider(w, r, p)
	if !ok {
		return
	}
	err := h.store.Disconnect(r.Context(), p.Tenant, prov.Provider, h.audit(p, "provider.disconnect", map[string]any{"provider": prov.Provider}))
	if errors.Is(err, ErrNotFound) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, prov.DisplayName+" is not connected"))
		return
	}
	if h.storeFailed(w, err, "disconnect provider") {
		return
	}
	h.logger.Info("control: provider disconnected", "tenant", p.Tenant, "provider", prov.Provider)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleRun(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	prov, ok := h.provider(w, r, p)
	if !ok {
		return
	}
	if prov.Credential == CredentialIntake {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeInvalidRequest,
			prov.DisplayName+" sends its log as it is written; there is nothing to pull"))
		return
	}
	id, active, err := h.store.Connection(r.Context(), p.Tenant, prov.Provider)
	if errors.Is(err, ErrNotFound) || err == nil && !active {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeInvalidRequest, "connect "+prov.DisplayName+" first"))
		return
	}
	if h.storeFailed(w, err, "provider connection") {
		return
	}
	a := h.audit(p, "provider.run", map[string]any{"provider": prov.Provider})
	a.ObjectID = id
	if err := h.store.Audit(r.Context(), p.Tenant, a); h.storeFailed(w, err, "audit provider run") {
		return
	}
	h.writeJSON(w, http.StatusAccepted, map[string]bool{"queued": h.pull.Trigger(p.Tenant, id)})
}

// provider resolves the path's provider: one ref.provider holds and this deployment reads.
func (h *Handler) provider(w http.ResponseWriter, r *http.Request, p deploy.Principal) (Provider, bool) {
	name := r.PathValue("provider")
	list, err := h.store.Providers(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "list providers") {
		return Provider{}, false
	}
	i := slices.IndexFunc(list, func(x Provider) bool { return x.Provider == name })
	if i < 0 {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such provider"))
		return Provider{}, false
	}
	if !h.available(list[i]) && r.Method != http.MethodDelete {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable,
			"this deployment cannot read "+list[i].DisplayName))
		return Provider{}, false
	}
	return list[i], true
}

// available reports whether this deployment reads p: pulls it, or receives its intake.
func (h *Handler) available(p Provider) bool {
	if p.Credential == CredentialIntake {
		return h.intake.Available(p.Provider)
	}
	return h.pull.Available(p.Provider)
}

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

func (h *Handler) audit(p deploy.Principal, action string, detail map[string]any) Audit {
	detail["subject"] = p.Subject
	actor := p.Actor
	if strings.TrimSpace(actor) == "" {
		actor = p.Subject
	}
	return Audit{Actor: actor, Action: action, ObjectType: "provider_connection", Detail: detail, At: h.now().UTC()}
}

func (h *Handler) storeFailed(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
	case errors.Is(err, ErrNotFound):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such provider"))
	default:
		h.fail(w, apierr.Internal(fmt.Errorf("%s: %w", what, err)))
	}
	return true
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
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
	apierr.Write(w, err, h.now(), h.logger)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("control: write response", "error", err)
	}
}
