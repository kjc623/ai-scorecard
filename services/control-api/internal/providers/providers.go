// Package providers reads AI providers' own records of the prompts a tenant's people sent, from the
// providers' admin APIs or from the logs they write in the customer's cloud, and records each one
// as a device's capture is recorded: classified under the tenant's collection mode, attributed to
// the person by user_ref, filed under the tool it was sent with, and, at M3, stored in
// content-vault.
//
// A tenant connects a provider (ops.provider_connection, one of ref.provider) on the console's
// Integrations page: with a key its admin issued, or, for Microsoft, with the consent the tenant
// already gave the vendor's Entra application, or, for a provider with no API to read, with an
// intake token the page issues, which the customer's cloud presents when it forwards the
// provider's log (intake.go). The runner pulls every other active connection on a schedule and on
// demand: a provider's adapter (Source) reads the records after the connection's cursor. The
// recorder writes each record through ingest.record_provider_event, whose event id derives from the
// provider's own id for the record, so a record read twice is recorded once. A record of a prompt a
// device also captured merges into the device's submission, so the prompt counts once.
//
// A provider's record is never enforced: it is read after the prompt was sent, so its policy
// decision is the default rule, logged, decided by the server.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol/canon"
)

// The providers this package has an adapter for, as ref.provider names them: those it pulls, and
// those whose log it receives.
const (
	Microsoft365Copilot  = "microsoft_365_copilot"
	ChatGPTEnterprise    = "chatgpt_enterprise"
	ClaudeEnterprise     = "claude_enterprise"
	GeminiCodeAssist     = "gemini_code_assist"
	AmazonBedrock        = "amazon_bedrock"
	PerplexityEnterprise = "perplexity_enterprise"
)

// How a provider's records reach the product (ref.provider.credential).
const (
	CredentialEntraApp = "entra_app"
	CredentialAPIKey   = "api_key"
	CredentialIntake   = "intake"
)

// The closed set of ops.provider_connection.last_error codes.
const (
	// ErrNotAvailable is a connection this deployment cannot read: Microsoft's without the vendor's
	// Entra application, or a provider with no adapter.
	ErrNotAvailable = "not_available"
	// ErrEntraNotConnected is a Microsoft connection of a tenant with no active Entra connection.
	ErrEntraNotConnected = "entra_not_connected"
	// ErrConsentMissing is Microsoft refusing the vendor's application the permission the read needs.
	ErrConsentMissing = "consent_missing"
	// ErrCredentialRefused is the provider refusing the tenant's key.
	ErrCredentialRefused = "credential_refused"
	ErrRateLimited       = "rate_limited"
	ErrProviderFailed    = "provider_unavailable"
	ErrInternal          = "internal"
)

// Record is one prompt as its provider recorded it.
type Record struct {
	// ID is the provider's id for the prompt, stable across reads. The event id derives from it.
	ID         string
	OccurredAt time.Time
	// App is the ref.app key of the tool the prompt was sent with.
	App string
	// UPN is the person's sign-in name or email address, ObjectID their Entra object id. Account is
	// a provider-side principal no directory row names (an AWS role), for a record with neither. A
	// record carries at least one.
	UPN      string
	ObjectID string
	Account  string
	// Text is the prompt as the provider received it.
	Text string
	// Attachments are the files sent with the prompt, as the provider describes them. A provider
	// does not hand over the bytes, so each ContentDigest is canon.Unreadable.
	Attachments []canon.Attachment
}

// Connection is one connection as a pass reads it.
type Connection struct {
	TenantID     string
	ConnectionID string
	Provider     string
	// Account is what the connection reads where the key does not name it: a ChatGPT workspace, a
	// Claude organization. Empty for Microsoft.
	Account string
	// Key is the opened key; empty for Microsoft.
	Key string
	// EntraTenantID is the tenant's Entra directory, for Microsoft; empty when the tenant has no
	// active Entra connection.
	EntraTenantID string
	// Cursor is where the pass resumes, as the adapter spells it; empty before the first pass.
	Cursor string
}

// Source reads one provider's records.
type Source interface {
	// Pull calls emit for the records after c.Cursor, oldest first where the provider allows, and
	// returns the cursor the next pass resumes from. more is true when the provider holds records
	// past it that this pass left for the next. On an error, next is the furthest point every
	// record before which was emitted, or "" when there is none, so a pass that fails partway does
	// not start again from the beginning. An error from emit ends the pass and is returned as is.
	Pull(ctx context.Context, c Connection, emit func(Record) error) (next string, more bool, err error)
}

// InitialLookback is how far back a connection's first pass reads.
const InitialLookback = 7 * 24 * time.Hour

// APIError is a provider answering other than success. Code is the provider's machine-readable
// error code, the only part of an error body safe to log.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("provider answered %d (%s)", e.Status, e.Code) }

// TokenError is the customer's Entra tenant refusing the vendor's application a token.
type TokenError struct{ Err error }

func (e *TokenError) Error() string { return "providers: token: " + e.Err.Error() }
func (e *TokenError) Unwrap() error { return e.Err }

// failureCode maps a failed pass to its closed code. A refusal of the vendor's application is
// missing consent; a refusal of the tenant's key is the key's.
func failureCode(provider string, err error) string {
	var a *APIError
	var t *TokenError
	var r recordError
	switch {
	case errors.As(err, &r):
		return ErrInternal
	case errors.As(err, &t):
		return ErrConsentMissing
	case errors.As(err, &a) && a.Status == http.StatusTooManyRequests:
		return ErrRateLimited
	case errors.As(err, &a) && (a.Status == http.StatusUnauthorized || a.Status == http.StatusForbidden):
		if provider == Microsoft365Copilot {
			return ErrConsentMissing
		}
		return ErrCredentialRefused
	}
	return ErrProviderFailed
}

// recordError is the product failing to record a record, which is the product's failure, not the
// provider's.
type recordError struct{ err error }

func (e recordError) Error() string { return "providers: record: " + e.err.Error() }
func (e recordError) Unwrap() error { return e.err }
