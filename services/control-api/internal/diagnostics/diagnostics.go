// Package diagnostics is a device's diagnostics on request: an admin asks for them on the device's
// page, the health response tells the device a request is waiting, the device uploads a zip of its
// health snapshot and its own logs to POST /v1/diagnostics, and the admin downloads it.
//
// The admin routes require a product access token carrying the admin role, and every request,
// upload and download is audited in the transaction that writes or reads it. The upload is
// authenticated by the device certificate and is accepted only for a request waiting for that
// device. A request lapses after RequestLifetime unanswered, and a bundle is kept for
// BundleLifetime after it arrives; the expire job removes both.
package diagnostics

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

const (
	// RequestLifetime is how long a request waits for a device that does not check in.
	RequestLifetime = 7 * 24 * time.Hour
	// BundleLifetime is how long a received bundle is kept.
	BundleLifetime = 14 * 24 * time.Hour
	// listLimit bounds the requests a device's page lists.
	listLimit = 20
)

// Audit actions.
const (
	ActionRequest  = "device.diagnostics.request"
	ActionReceive  = "device.diagnostics.receive"
	ActionDownload = "device.diagnostics.download"
	objectType     = "device_diagnostics"
)

// Store is the subset of store.Store diagnostics use.
type Store interface {
	Device(ctx context.Context, tenantID, deviceID string) (store.Device, error)
	RequestDiagnostics(ctx context.Context, d store.Diagnostics, audit store.AuditEntry) (store.Diagnostics, bool, error)
	ReceiveDiagnostics(ctx context.Context, d store.Diagnostics, bundle []byte, audit store.AuditEntry) error
	DeviceDiagnostics(ctx context.Context, tenantID, deviceID string, now time.Time, limit int) ([]store.Diagnostics, error)
	DiagnosticsBundle(ctx context.Context, tenantID, deviceID, requestID string, audit store.AuditEntry) (store.Diagnostics, []byte, error)
}

// Handler serves the device upload and the admin routes.
type Handler struct {
	store  Store
	auth   deploy.Authenticator
	logger *slog.Logger
	now    func() time.Time
}

// New builds the handler. The store and authenticator are required.
func New(st Store, auth deploy.Authenticator, logger *slog.Logger) (*Handler, error) {
	if st == nil {
		return nil, errors.New("diagnostics: a store is required")
	}
	if auth == nil {
		return nil, errors.New("diagnostics: an authenticator is required; the admin routes have no anonymous mode")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: st, auth: auth, logger: logger, now: time.Now}, nil
}

// Register adds the routes to mux: the upload authenticated by device, the admin routes by token.
func (h *Handler) Register(mux *http.ServeMux, device deploy.DeviceAuthenticator) {
	mux.HandleFunc("POST "+protocol.DiagnosticsUploadPath, func(w http.ResponseWriter, r *http.Request) {
		tenantID, deviceID, err := device(r)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.handleUpload(w, r, tenantID, deviceID)
	})
	mux.HandleFunc("GET /admin/v1/devices/{device_id}/diagnostics", h.handleList)
	mux.HandleFunc("POST /admin/v1/devices/{device_id}/diagnostics", h.handleRequest)
	mux.HandleFunc("GET /admin/v1/devices/{device_id}/diagnostics/{request_id}/bundle", h.handleBundle)
}

// --- the device's upload ------------------------------------------------------------------------

func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request, tenantID, deviceID string) {
	requestID := r.Header.Get(protocol.HeaderDiagnosticsRequest)
	if !store.IsUUID(requestID) {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the upload names no diagnostics request"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxDiagnosticsBundleBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, apierr.New(http.StatusRequestEntityTooLarge, string(protocol.ReasonOversize), "the diagnostics bundle is over its cap"))
			return
		}
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the request body could not be read"))
		return
	}
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the diagnostics bundle is not a zip"))
		return
	}
	dev, err := h.store.Device(r.Context(), tenantID, deviceID)
	if err != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("device: %w", err)))
		return
	}
	now := h.now().UTC()
	sum := sha256.Sum256(body)
	d := store.Diagnostics{TenantID: tenantID, DeviceID: deviceID, RequestID: requestID, ReceivedAt: &now,
		AgentVersion: dev.AgentVersion, SHA256: hex.EncodeToString(sum[:]), ExpiresAt: now.Add(BundleLifetime)}
	audit := store.AuditEntry{TenantID: tenantID, ActorType: store.ActorDevice, ActorID: deviceID, Action: ActionReceive,
		ObjectType: objectType, ObjectID: requestID, OccurredAt: now,
		Detail: map[string]any{"device_id": deviceID, "size_bytes": len(body), "sha256": d.SHA256}}
	if err := h.store.ReceiveDiagnostics(r.Context(), d, body, audit); err != nil {
		if errors.Is(err, store.ErrDiagnosticsNotPending) {
			h.fail(w, apierr.New(http.StatusConflict, string(protocol.ReasonDiagnosticsNotPending),
				"the diagnostics request is not waiting for this device"))
			return
		}
		h.fail(w, apierr.Internal(err))
		return
	}
	h.logger.Info("control: diagnostics received", "tenant", tenantID, "device", deviceID, "request", requestID, "bytes", len(body))
	w.WriteHeader(http.StatusNoContent)
}

// --- the admin routes ---------------------------------------------------------------------------

// diagnosticsJSON is one request as the device's page lists it.
type diagnosticsJSON struct {
	RequestID    string     `json:"request_id"`
	State        string     `json:"state"` // waiting | received
	RequestedAt  time.Time  `json:"requested_at"`
	RequestedBy  string     `json:"requested_by"`
	ReceivedAt   *time.Time `json:"received_at"`
	AgentVersion *string    `json:"agent_version"`
	SizeBytes    *int64     `json:"size_bytes"`
	ExpiresAt    time.Time  `json:"expires_at"`
}

func toJSON(d store.Diagnostics) diagnosticsJSON {
	out := diagnosticsJSON{RequestID: d.RequestID, State: "waiting", RequestedAt: d.RequestedAt, RequestedBy: d.RequestedBy,
		ExpiresAt: d.ExpiresAt}
	if d.ReceivedAt != nil {
		size := d.SizeBytes
		out.State, out.ReceivedAt, out.SizeBytes = "received", d.ReceivedAt, &size
		if d.AgentVersion != "" {
			v := d.AgentVersion
			out.AgentVersion = &v
		}
	}
	return out
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, deviceID, ok := h.admin(w, r)
	if !ok {
		return
	}
	list, err := h.store.DeviceDiagnostics(r.Context(), p.Tenant, deviceID, h.now().UTC(), listLimit)
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	out := make([]diagnosticsJSON, 0, len(list))
	for _, d := range list {
		out = append(out, toJSON(d))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"diagnostics": out})
}

func (h *Handler) handleRequest(w http.ResponseWriter, r *http.Request) {
	p, deviceID, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, err := store.NewUUID()
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	now := h.now().UTC()
	actor := actorOf(p)
	d, created, err := h.store.RequestDiagnostics(r.Context(), store.Diagnostics{TenantID: p.Tenant, RequestID: id, DeviceID: deviceID,
		RequestedBy: actor, RequestedAt: now, ExpiresAt: now.Add(RequestLifetime)},
		store.AuditEntry{TenantID: p.Tenant, ActorType: store.ActorUser, ActorID: actor, Action: ActionRequest,
			ObjectType: objectType, ObjectID: id, OccurredAt: now,
			Detail: map[string]any{"device_id": deviceID, "subject": p.Subject}})
	switch {
	case errors.Is(err, store.ErrDeviceUnknown):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "the tenant has no such device"))
		return
	case errors.Is(err, store.ErrDeviceRevoked):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeRevokedDevice, "the device is revoked and will not check in again"))
		return
	case err != nil:
		h.fail(w, apierr.Internal(err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.writeJSON(w, status, toJSON(d))
}

func (h *Handler) handleBundle(w http.ResponseWriter, r *http.Request) {
	p, deviceID, ok := h.admin(w, r)
	if !ok {
		return
	}
	requestID := r.PathValue("request_id")
	if !store.IsUUID(requestID) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such diagnostics"))
		return
	}
	now := h.now().UTC()
	d, bundle, err := h.store.DiagnosticsBundle(r.Context(), p.Tenant, deviceID, requestID,
		store.AuditEntry{TenantID: p.Tenant, ActorType: store.ActorUser, ActorID: actorOf(p), Action: ActionDownload,
			ObjectType: objectType, ObjectID: requestID, OccurredAt: now,
			Detail: map[string]any{"device_id": deviceID, "subject": p.Subject}})
	if errors.Is(err, store.ErrDiagnosticsUnknown) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such diagnostics, or they have expired"))
		return
	}
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	name := h.filename(r, p.Tenant, deviceID, d)
	w.Header().Set("Content-Type", protocol.DiagnosticsContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(bundle)))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bundle)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// filename names a download after the device's hostname where the tenant stores it, else its id,
// and the time the device collected it.
func (h *Handler) filename(r *http.Request, tenantID, deviceID string, d store.Diagnostics) string {
	label := deviceID[:8]
	if dev, err := h.store.Device(r.Context(), tenantID, deviceID); err == nil {
		if host := strings.Trim(unsafeName.ReplaceAllString(dev.Hostname, "-"), "-."); host != "" {
			label = host
		}
	}
	return "diagnostics-" + label + "-" + d.ReceivedAt.UTC().Format("20060102-1504") + ".zip"
}

// admin authenticates the request, requires the admin role and a well-formed device id, and
// answers the caller itself on failure.
func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (deploy.Principal, string, bool) {
	p, err := h.auth(r)
	if err != nil {
		var e *apierr.Error
		if !errors.As(err, &e) {
			err = apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a valid product access token is required")
		}
		h.fail(w, err)
		return deploy.Principal{}, "", false
	}
	if !store.IsUUID(p.Tenant) {
		h.fail(w, apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "the access token names no tenant"))
		return deploy.Principal{}, "", false
	}
	if !slices.Contains(p.Roles, deploy.RoleAdmin) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeForbidden, "the admin role is required"))
		return deploy.Principal{}, "", false
	}
	deviceID := r.PathValue("device_id")
	if !store.IsUUID(deviceID) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "the tenant has no such device"))
		return deploy.Principal{}, "", false
	}
	return p, strings.ToLower(deviceID), true
}

func actorOf(p deploy.Principal) string {
	if strings.TrimSpace(p.Actor) != "" {
		return p.Actor
	}
	return p.Subject
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
