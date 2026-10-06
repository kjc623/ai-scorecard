// Package apierr is control-api's error surface for devices and the admin API. One value carries
// the HTTP status, a machine-readable code, a sentence for the caller and an optional detail
// payload, rendered as the common envelope:
//
//	{ "error": { "code": "...", "detail": { ... }, "server_time": "...", "message": "..." } }
//
// An internal cause is logged and never sent.
package apierr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// The closed set of error codes.
const (
	CodeSchemaViolation          = "schema_violation"
	CodeUnsupportedSchemaVersion = "unsupported_schema_version"
	CodeInvalidRequest           = "invalid_request"
	CodeUnauthenticated          = "unauthenticated"
	CodeForbidden                = "forbidden"
	CodeNotFound                 = "not_found"
	CodeMethodNotAllowed         = "method_not_allowed"
	CodeRateLimited              = "rate_limited"
	CodeUnavailable              = "unavailable"

	// Device authentication and enrolment.
	CodeInvalidClientCert    = "invalid_client_certificate"
	CodeRevokedDevice        = "revoked_device"
	CodeUnknownTenant        = "unknown_tenant"
	CodeRegionMismatch       = "region_mismatch"
	CodeHardwareConflict     = "hardware_identity_conflict"
	CodeInvalidCSR           = "invalid_csr"
	CodeDeploymentKeyInvalid = "deployment_key_invalid"
	CodeDeploymentKeyRevoked = "deployment_key_revoked"
	CodeDeploymentKeyExpired = "deployment_key_expired"
	// CodeDeviceNotManaged is every refusal of a tenant's MDM check; detail.reason names which
	// check failed.
	CodeDeviceNotManaged = "device_not_managed"

	// CodeNoPolicyBundle is GET /v1/policy's 404: the device stays at M0.
	CodeNoPolicyBundle = "no_policy_bundle"

	// The admin API.
	CodeNoEntraConnection   = "no_entra_connection"
	CodeReleaseUnavailable  = "release_unavailable"
	CodeUnsupportedFormat   = "unsupported_format"
	CodeInvalidVerification = "invalid_device_verification"

	// The Settings API's refusals, each named so the page can say what the database refused.
	CodeCollectionExceedsCeiling = "collection_exceeds_ceiling"
	CodeScopeOverrideTooWide     = "scope_override_too_wide"
	CodeSearchTierRequiresMode   = "search_tier_requires_mode"
	CodeRetentionOutOfRange      = "retention_out_of_range"
)

// Error is a decided failure. Message is written for the caller; Cause, when set, is logged and
// never sent.
type Error struct {
	Status  int
	Code    string
	Message string
	Detail  any
	Cause   error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the internal cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// New builds an error with no detail and no internal cause.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Detailed builds an error carrying a detail payload.
func Detailed(status int, code, message string, detail any) *Error {
	return &Error{Status: status, Code: code, Message: message, Detail: detail}
}

// Internal builds a retryable 503 and records the cause for the log.
func Internal(cause error) *Error {
	return &Error{
		Status:  http.StatusServiceUnavailable,
		Code:    CodeUnavailable,
		Message: "the control plane could not complete the request; nothing was changed",
		Cause:   cause,
	}
}

// RetryAfterSeconds is what a 429 or 503 tells the caller to wait, in the header and the body.
const RetryAfterSeconds = 5

type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Code        string    `json:"code"`
	Detail      any       `json:"detail,omitempty"`
	ServerTime  time.Time `json:"server_time"`
	RetryAfterS int       `json:"retry_after_s,omitempty"`
	Message     string    `json:"message,omitempty"`
}

// Write renders err as the envelope. An error that is not an *Error is an unclassified failure and
// becomes a 503.
func Write(w http.ResponseWriter, err error, now time.Time, logger *slog.Logger) {
	var e *Error
	if !errors.As(err, &e) {
		logger.Error("control: unclassified failure", "error", err)
		e = Internal(err)
	}
	if e.Cause != nil {
		logger.Error("control: request failed with an internal cause", "error", e.Cause, "code", e.Code)
	}
	out := envelope{Error: body{Code: e.Code, Detail: e.Detail, ServerTime: now.UTC(), Message: e.Message}}
	if e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable {
		out.Error.RetryAfterS = RetryAfterSeconds
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds))
	}
	WriteJSON(w, e.Status, out, logger)
}

// WriteJSON writes v as an uncacheable JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("control: write response", "error", err)
	}
}
