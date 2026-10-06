// Package httpapi is ingest-api's HTTP surface: POST /v1/events for devices, and /healthz and
// /readyz for the platform.
//
// It reads the body within the size caps, authenticates the device, hands the batch to the service,
// and renders failures as the common error envelope
//
//	{"error": {"code": "...", "detail": {...}, "server_time": "...", "message": "..."}}
//
// Every reason code comes from the service or the authenticator; the transport invents none.
package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// Authenticator turns a request into the device it is from.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (auth.Principal, error)
}

// Server is the HTTP surface.
type Server struct {
	Service *ingest.Service
	Auth    Authenticator
	// Ready answers the readiness probe; it must make a database round trip.
	Ready  func(ctx context.Context) error
	Logger *slog.Logger
	Now    func() time.Time
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Ready(ctx); err != nil {
			// Logged, not returned: a dependency error can name a host.
			s.Logger.Warn("not ready", "error", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not-ready"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, http.StatusMethodNotAllowed, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{Expected: "POST"}, "")
		return
	}
	receivedAt := s.now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, protocol.ReasonOversize, &protocol.BatchRejectionDetail{
				Expected: fmt.Sprintf("a request body of at most %d bytes", protocol.MaxRequestBodyBytes),
			}, "")
			return
		}
		s.Logger.Warn("reading the request body failed", "error", err)
		s.writeError(w, http.StatusBadRequest, protocol.ReasonSchemaViolation, nil, "the request body could not be read; send it again")
		return
	}

	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		if body, err = gunzip(body, protocol.MaxDecompressedBytes); err != nil {
			s.writeError(w, http.StatusRequestEntityTooLarge, protocol.ReasonOversize, &protocol.BatchRejectionDetail{
				Expected: fmt.Sprintf("a gzip body that decompresses to at most %d bytes", protocol.MaxDecompressedBytes),
			}, "")
			return
		}
	default:
		s.writeError(w, http.StatusBadRequest, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "Content-Encoding gzip, identity, or absent",
		}, "")
		return
	}

	var batch protocol.EventBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		// encoding/json describes only the device's own bytes, which is how a malformed batch is
		// diagnosed on the device.
		s.writeError(w, http.StatusBadRequest, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "a batch: schema_version, batch_id, device_sent_at, event_count, events",
		}, err.Error())
		return
	}

	principal, err := s.Auth.Authenticate(r.Context(), r)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}

	resp, err := s.Service.Submit(r.Context(), principal, batch, receivedAt)
	if err != nil {
		var apiErr *ingest.Error
		if !errors.As(err, &apiErr) {
			apiErr = &ingest.Error{Status: http.StatusServiceUnavailable, Code: protocol.ReasonSchemaViolation,
				Message: "the write failed; nothing was committed, so the batch is retryable", Cause: err}
		}
		if apiErr.Cause != nil {
			s.Logger.Error("batch failed", "error", apiErr.Cause, "code", apiErr.Code, "status", apiErr.Status,
				"batch_id", batch.BatchID, "tenant_id", principal.TenantID, "device_id", principal.DeviceID)
		}
		s.writeError(w, apiErr.Status, apiErr.Code, apiErr.Detail, apiErr.Message)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// writeAuthError names the failure for the device without echoing anything it did not send.
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusUnauthorized, protocol.ReasonRevokedDevice, ""
	switch {
	case errors.Is(err, auth.ErrNoCredential):
		message = "no client certificate was presented"
	case errors.Is(err, auth.ErrBadCredential), errors.Is(err, store.ErrCredentialUnknown):
		message = "the client certificate is not a device certificate this deployment issued"
	case errors.Is(err, store.ErrCredentialExpired):
		message = "the device certificate expired"
	case errors.Is(err, store.ErrCredentialRevoked):
		message = "the device certificate was revoked"
	case errors.Is(err, store.ErrDeviceRevoked):
		message = "the device was revoked"
	case errors.Is(err, store.ErrUnknownTenant):
		status, code, message = http.StatusForbidden, protocol.ReasonUnknownTenant, "the tenant is unknown to this deployment"
	case errors.Is(err, store.ErrTenantSuspended):
		status, code, message = http.StatusForbidden, protocol.ReasonUnknownTenant, "the tenant's ingest is disabled"
	case errors.Is(err, store.ErrRegionMismatch):
		status, code, message = http.StatusForbidden, protocol.ReasonRegionMismatch, "this deployment is not the tenant's region"
	default:
		// A status read that failed is the service's problem, not the device's.
		s.Logger.Error("authentication failed", "error", err)
		status, code, message = http.StatusServiceUnavailable, protocol.ReasonSchemaViolation, "the device could not be authenticated; retry"
	}
	s.writeError(w, status, code, nil, message)
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code        protocol.ReasonCode            `json:"code"`
	Detail      *protocol.BatchRejectionDetail `json:"detail,omitempty"`
	ServerTime  time.Time                      `json:"server_time"`
	RetryAfterS int                            `json:"retry_after_s,omitempty"`
	Message     string                         `json:"message,omitempty"`
}

// retryAfter is the delay a retryable failure asks for; the device adds jitter.
const retryAfter = 5

func (s *Server) writeError(w http.ResponseWriter, status int, code protocol.ReasonCode, detail *protocol.BatchRejectionDetail, message string) {
	body := errorEnvelope{Error: errorBody{Code: code, Detail: detail, ServerTime: s.now(), Message: message}}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		body.Error.RetryAfterS = retryAfter
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
	}
	s.writeJSON(w, status, body)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Logger.Warn("writing the response failed", "error", err)
	}
}

// gunzip decompresses at most limit bytes and fails when there are more.
func gunzip(body []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, errors.New("decompressed body is over the cap")
	}
	return out, nil
}
