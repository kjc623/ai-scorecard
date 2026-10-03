// Package apierr is the device-facing error surface of the control plane. One value carries the
// HTTP status, the machine-readable code, a hand-written sentence for the device, and an optional
// detail payload, so the transport can render docs/02 §5's common envelope without inventing an
// outcome and without ever echoing an internal cause.
package apierr

import "net/http"

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
