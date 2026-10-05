// Package apierr is the device-facing error surface of the control plane. One value carries the
// HTTP status, the machine-readable code, a hand-written sentence for the device, and an optional
// detail payload, so the transport can render docs/02 §5's common envelope without inventing an
// outcome and without ever echoing an internal cause.
package apierr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// The closed set of control-plane error codes. Docs/02 §7's codes are reused where they name the
// same fact (revoked_device, unknown_tenant, region_mismatch, schema_violation); the rest name
// enrolment- and token-specific failures the ingest vocabulary has no word for.
const (
	CodeSchemaViolation          = "schema_violation"
	CodeUnsupportedSchemaVersion = "unsupported_schema_version"
	CodeRevokedDevice            = "revoked_device"
	CodeUnknownTenant            = "unknown_tenant"
	CodeRegionMismatch           = "region_mismatch"
	CodeHardwareConflict         = "hardware_identity_conflict"
	CodeEnrolmentTokenExpired    = "enrolment_token_expired"
	CodeEnrolmentTokenInvalid    = "enrolment_token_invalid"
	CodeInvalidCSR               = "invalid_csr"
	CodeInvalidProof             = "invalid_dpop_proof"
	CodeInvalidAssertion         = "invalid_assertion"
	CodeInvalidRequest           = "invalid_request"
	CodeUnavailable              = "unavailable"

	// The deployment-key bootstrap (contract §5). Each lifecycle failure is its own code, as the
	// enrolment token's are, so an operator can tell a revoked key from an expired or a mistyped one.
	CodeDeploymentKeyInvalid = "deployment_key_invalid"
	CodeDeploymentKeyRevoked = "deployment_key_revoked"
	CodeDeploymentKeyExpired = "deployment_key_expired"
	// CodeRateLimited is §5's 429: the caller is told to come back, never refused for good.
	CodeRateLimited = "rate_limited"
	// CodeDeviceNotManaged is the one code for every refusal of a tenant's MDM check; detail.reason
	// names which check failed, so the device's log says why without a second vocabulary.
	CodeDeviceNotManaged = "device_not_managed"
	// CodeNoPolicyBundle is GET /v1/policy's 404: the device falls to M0 (docs/02 §5.2, C10).
	CodeNoPolicyBundle = "no_policy_bundle"

	// The admin surface (contract §5). It is not device-facing, but it renders the same envelope so
	// the dashboard reads one error shape from every control-plane route.
	CodeUnauthenticated     = "unauthenticated"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeNoEntraConnection   = "no_entra_connection"
	CodeReleaseUnavailable  = "release_unavailable"
	CodeMethodNotAllowed    = "method_not_allowed"
	CodeUnsupportedFormat   = "unsupported_format"
	CodeInvalidVerification = "invalid_device_verification"
)

// Error is the control-plane failure. Message is device-facing by construction; Cause, when set, is
// logged and never sent.
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

// Unwrap exposes the internal cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// New builds an error with no detail and no internal cause.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Detailed builds an error carrying a detail payload.
func Detailed(status int, code, message string, detail any) *Error {
	return &Error{Status: status, Code: code, Message: message, Detail: detail}
}

// Internal builds a 503 and records the cause for the log, never for the device.
func Internal(cause error) *Error {
	return &Error{
		Status:  http.StatusServiceUnavailable,
		Code:    CodeUnavailable,
		Message: "the control plane could not complete the request; nothing was changed",
		Cause:   cause,
	}
}

// RetryAfterSeconds is what a 429 or 503 tells the caller to wait. One number, so the header and
// the body cannot disagree.
const RetryAfterSeconds = 5

// envelope is docs/02 §5's common error body.
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

// Write renders err as §5's envelope: the same shape internal/httpapi writes, for the routes that
// live in their own packages (GET /v1/policy, the admin API). An error that is not an *Error is
// an unclassified failure and becomes a 503; a Cause is logged and never sent.
func Write(w http.ResponseWriter, err error, now time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		logger.Error("control: write error response", "error", err)
	}
}
