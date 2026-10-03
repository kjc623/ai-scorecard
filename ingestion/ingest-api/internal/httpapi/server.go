// Package httpapi is the transport for the device-facing ingest endpoint.
//
// It owns exactly three things: reading the bytes (size caps, optional gzip), turning an
// authenticated request into a call to the service, and rendering errors as §5's common envelope:
//
//	{ "error": { "code": "...", "detail": { ... }, "server_time": "..." } }
//
// All validation decisions and reason codes come from the service, so the transport cannot invent
// an outcome the write path did not make.
package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/batchguard"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/ingest"
)

// Server is the HTTP surface.
type Server struct {
	Service *ingest.Service
	Auth    auth.Authenticator
	Guard   *batchguard.Guard
	Logger  *slog.Logger
	Now     func() time.Time

	// MaxCompressedBody and MaxDecompressedBody are §5.3's caps: 8 MiB compressed, 32 MiB
	// decompressed (the decompression-bomb guard).
	MaxCompressedBody   int64
	MaxDecompressedBody int64
}

// New builds a server with the protocol package's caps.
func New(svc *ingest.Service, a auth.Authenticator, guard *batchguard.Guard, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		Service:             svc,
		Auth:                a,
		Guard:               guard,
		Logger:              logger,
		Now:                 time.Now,
		MaxCompressedBody:   protocol.MaxRequestBodyBytes,
		MaxDecompressedBody: protocol.MaxDecompressedBytes,
	}
}

// Handler returns the routes. POST /v1/events is the only device-facing endpoint this service
// serves; /healthz is deployment infrastructure, not a sixth device API (§5).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, 405, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "POST",
		}, "")
		return
	}

	now := s.Now
	if now == nil {
		now = time.Now
	}
	receivedAt := now().UTC()

	// 1. Body, with the compressed cap enforced by the reader rather than after the fact.
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxCompressedBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, 413, protocol.ReasonOversize, &protocol.BatchRejectionDetail{
				Expected: "request body at most " + itoa(s.MaxCompressedBody) + " bytes compressed",
			}, "")
			return
		}
		// A read error is the transport's business, not the device's: logged here, described to the
		// caller only as "unreadable". The device already knows what it sent.
		s.Logger.Warn("ingest: reading the request body failed", "error", err, "remote", r.RemoteAddr)
		s.writeError(w, 400, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "a readable request body",
		}, "the request body could not be read; send it again in full")
		return
	}

	// 2. Optional gzip (§5.3), bounded so a small body cannot expand without limit.
	if enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc == "gzip" {
		body, err = gunzipBounded(body, s.MaxDecompressedBody)
		if err != nil {
			s.Logger.Warn("ingest: decompressing the request body failed", "error", err, "remote", r.RemoteAddr)
			s.writeError(w, 413, protocol.ReasonOversize, &protocol.BatchRejectionDetail{
				Expected: "decompressed body at most " + itoa(s.MaxDecompressedBody) + " bytes",
			}, "the body could not be decompressed or is over the cap")
			return
		}
	} else if enc != "" && enc != "identity" {
		s.writeError(w, 400, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "Content-Encoding gzip, identity, or absent",
		}, "")
		return
	}

	// 3. The batch envelope itself (§5.3: 400 when it is unparseable).
	var batch protocol.EventBatch
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&batch); err != nil {
		// Kept verbatim, unlike the two above: this text is derived entirely from the device's own
		// bytes (encoding/json names the offending character or field), it is how a poison batch is
		// diagnosed from the device side, and it carries nothing the caller does not already have.
		// See the note in internal/ingest on Error.Message for the rule this satisfies.
		s.writeError(w, 400, protocol.ReasonSchemaViolation, &protocol.BatchRejectionDetail{
			Expected: "the batch envelope of §5.3: schema_version, batch_id, device_sent_at, event_count, events",
		}, err.Error())
		return
	}

	// 4. The device credential (ADR 0005). Tenant, device and region come from here and never from
	//    the body; the authoritative revocation check happens again inside the write transaction.
	principal, err := s.Auth.Authenticate(r.Context(), r)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}

	// 5. duplicate_batch: §5.3's batch-level idempotency. The check happens before the write and the
	//    mark after it commits, so a batch refused for its shape can be re-sent unchanged and a
	//    genuine replay is answered with the batch-level code. §6 prices the race in between: both
	//    racers fall through to the event-key constraint and both events report as duplicates, which
	//    is the correct answer anyway.
	if s.Guard != nil && s.Guard.Check(principal.TenantID, principal.DeviceID, batch.BatchID) {
		s.writeError(w, 409, protocol.ReasonDuplicateBatch, &protocol.BatchRejectionDetail{
			Expected: "a batch_id not seen from this device inside the replay window; re-send with a fresh batch_id (§5.3)",
		}, batch.BatchID)
		return
	}

	resp, err := s.Service.Submit(r.Context(), principal, batch, receivedAt)
	if err != nil {
		var apiErr *ingest.Error
		if errors.As(err, &apiErr) {
			// The service's own message is device-facing by construction; its internal cause, if it
			// has one, is logged here and never sent.
			if apiErr.Cause != nil {
				s.Logger.Error("ingest: batch failed with an internal cause",
					"error", apiErr.Cause, "code", apiErr.Code, "status", apiErr.Status,
					"batch_id", batch.BatchID, "device_id", principal.DeviceID)
			}
			s.writeError(w, apiErr.Status, apiErr.Code, apiErr.Detail, apiErr.Message)
			return
		}
		// A failure the service could not classify is still a machine-readable surface (I3): the
		// closed code and a sentence the device can act on. The failure itself is logged, not sent —
		// it can be a driver's text, and a device is an untrusted reader.
		s.Logger.Error("ingest: unclassified write failure", "error", err, "batch_id", batch.BatchID)
		s.writeError(w, 503, protocol.ReasonSchemaViolation, nil,
			"the write failed; nothing was committed, so the batch is retryable")
		return
	}
	if s.Guard != nil {
		s.Guard.Mark(principal.TenantID, principal.DeviceID, batch.BatchID)
	}

	// §5.3: a batch that parses always returns 200, even when every event inside it is rejected.
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	// Each message names the stage, so an operator reading one line can tell an admission refusal
	// from the in-transaction re-check of §2.3. A failure path that cannot be told apart from
	// another is a failure path that reports the wrong cause (I3).
	switch {
	case errors.Is(err, ErrBadDevIdentity):
		// A development-only path, so the message can name the headers; the code stays in §7's
		// closed set, because a malformed credential is a 401 and nothing else.
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil,
			"admission: the development principal headers must be uuid strings (tenant id and device id)")
	case errors.Is(err, auth.ErrUnknownTenant):
		s.writeError(w, 403, protocol.ReasonUnknownTenant, nil, "admission: the authenticated principal's tenant is unknown to this deployment")
	case errors.Is(err, auth.ErrTenantSuspended):
		s.writeError(w, 403, protocol.ReasonUnknownTenant, nil, "admission: the tenant's ingest gate is shut")
	case errors.Is(err, auth.ErrRegionMismatch):
		s.writeError(w, 403, protocol.ReasonRegionMismatch, nil, "admission: this deployment is not the tenant's pinned region")
	case errors.Is(err, auth.ErrNoCredential):
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: no client credential presented")
	case errors.Is(err, auth.ErrCredentialExpired):
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: the device credential expired")
	case errors.Is(err, auth.ErrCredentialRevoked):
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: the device credential was revoked")
	case errors.Is(err, auth.ErrDeviceRevoked):
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: the device was revoked")
	case errors.Is(err, auth.ErrBadCredential), errors.Is(err, auth.ErrCredentialUnknown):
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: the client credential is not a device credential this deployment issued")
	default:
		s.Logger.Error("ingest: unclassified authentication failure", "error", err)
		s.writeError(w, 401, protocol.ReasonRevokedDevice, nil, "admission: the client credential could not be verified")
	}
}

// errorEnvelope is §5's common error body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code       protocol.ReasonCode            `json:"code"`
	Detail     *protocol.BatchRejectionDetail `json:"detail,omitempty"`
	ServerTime time.Time                      `json:"server_time"`
	// RetryAfterS is present for the statuses §5 marks retryable. The value is an ASSUMPTION: the
	// document requires the field, not a number, and the device applies full jitter on top of it
	// (§8), so a small value costs nothing.
	RetryAfterS int    `json:"retry_after_s,omitempty"`
	Message     string `json:"message,omitempty"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, code protocol.ReasonCode, detail *protocol.BatchRejectionDetail, message string) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	body := errorEnvelope{Error: errorBody{Code: code, Detail: detail, ServerTime: now().UTC(), Message: message}}
	if status == 429 || status == 503 {
		body.Error.RetryAfterS = 5
		w.Header().Set("Retry-After", "5")
	}
	s.writeJSON(w, status, body)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Logger.Error("ingest: write response", "error", err)
	}
}

// gunzipBounded decompresses at most limit bytes and reports an error when there are more.
func gunzipBounded(body []byte, limit int64) ([]byte, error) {
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

func itoa(n int64) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ErrBadDevIdentity is returned when the development headers do not carry uuids.
//
// The check exists because the identity a request claims is validated *where it is established*, and
// this authenticator is an identity source like any other. Without it a typo in a header reached the
// store, where the device id is bound as a uuid parameter and PostgreSQL answered with a driver
// error surfaced as a 503 — a database error for what is really a malformed credential. A malformed
// identifier is an authentication failure: 401, before anything is read or written.
var ErrBadDevIdentity = errors.New("httpapi: the development principal headers must be uuids")

// DevHeader is a development and verification authenticator. It trusts two headers and is used
// ONLY when the binary is started with -dev-trust-principal, which also refuses to run without an
// explicit acknowledgement. It exists so the endpoint can be exercised end to end without
// provisioning a certificate authority, and it is not part of any deployment.
//
// It still goes through the store's authoritative credential check: trusting a header is not the
// same as skipping the revocation lookup, which is why the credential id has to name a row that
// exists.
type DevHeader struct {
	TenantHeader string
	DeviceHeader string
	// CredentialID names the row the store's status check resolves. Default "dev".
	CredentialID string
}

// Authenticate implements auth.Authenticator.
func (d DevHeader) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	tenant := r.Header.Get(d.TenantHeader)
	device := r.Header.Get(d.DeviceHeader)
	if tenant == "" || device == "" {
		return auth.Principal{}, auth.ErrNoCredential
	}
	// The certificate path validates the same shapes in auth.identityFromCertificate; a header is no
	// less an identity claim for being a development one.
	if !contract.IsUUID(tenant) || !contract.IsUUID(device) {
		return auth.Principal{}, ErrBadDevIdentity
	}
	credential := d.CredentialID
	if credential == "" {
		credential = "dev"
	}
	return auth.Principal{TenantID: tenant, DeviceID: device, CredentialID: credential}, nil
}
