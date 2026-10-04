// Package store is the persistence seam of control-api's enrolment and token paths.
//
// The interface is deliberately small and tenant-explicit: every method takes the tenant that the
// authenticated credential resolved to, because row-level security is forced on every ops table and
// a session that has not set a tenant reads zero rows (brief C32; docs/02-ingest-and-transport.md
// §12). Two implementations:
//
//   - Memory: the test double and the local run. It mirrors the statements in sql.go so the service
//     can be tested without a database, and the live test checks the SQL text against a real
//     PostgreSQL rather than trusting the mirror.
//   - SQL: database/sql against the real schema. Every statement it issues is a constant in sql.go,
//     and the live test executes that text against a reachable PostgreSQL 17 when one is present.
//
// The credential type and its single transport binding are ADR 0020 decision 4: `x509` or `dpop`,
// with public_key_thumbprint as the one value the authenticator compares for both modes.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Tenant is the slice of ops.tenant the control path decides on.
type Tenant struct {
	TenantID        string
	Status          string
	IngestEnabled   bool
	ResidencyRegion string
}

// Active reports whether the tenant may enrol or obtain a token. A suspended or closed tenant is
// refused; nothing here silently treats an unknown gate as open.
func (t Tenant) Active() bool { return t.Status != "closed" && t.IngestEnabled }

// EnrolmentToken is ops.enrolment_token as the enrolment path needs it.
type EnrolmentToken struct {
	TenantID             string
	TokenHash            string
	HardwareIdentityHash string
	IssuedAt             time.Time
	ExpiresAt            time.Time
	UsedAt               *time.Time
	RevokedAt            *time.Time
}

// Usable reports whether the token may still redeem. A used, revoked or expired token is refused
// with a distinct error at the caller so the operator can tell the causes apart.
func (t EnrolmentToken) Usable(now time.Time) error {
	switch {
	case t.RevokedAt != nil:
		return ErrTokenRevoked
	case t.UsedAt != nil:
		return ErrTokenUsed
	case !t.ExpiresAt.After(now):
		return ErrTokenExpired
	}
	return nil
}

// Device is the slice of ops.device the control path writes and reads. hardware_identity_hash is
// the C11 idempotency key: it is set-once, and a re-enrolment that matches it returns the existing
// device rather than minting a second identity.
type Device struct {
	TenantID             string
	DeviceID             string
	OS                   string
	OSVersion            string
	MDMID                string
	HardwareIdentityHash string
	ResidencyRegion      string
	EnrolledAt           time.Time
	RevokedAt            *time.Time
}

// Credential is the slice of ops.device_credential the control path writes and reads. Type is the
// ADR 0020 mode; PublicKeyThumbprint is the single binding for both modes; PublicKeyJWK is the
// registered DPoP key and is empty for x509.
type Credential struct {
	TenantID            string
	CredentialID        string
	DeviceID            string
	Type                protocol.AuthMode
	PublicKeyThumbprint string
	PublicKeyJWK        json.RawMessage
	IssuedAt            time.Time
	ExpiresAt           time.Time
	RevokedAt           *time.Time
}

// Active reports whether the credential may still be used or rotated.
func (c Credential) Active(now time.Time) error {
	switch {
	case c.RevokedAt != nil:
		return ErrCredentialRevoked
	case !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now):
		return ErrCredentialExpired
	}
	return nil
}

// IssueCredential is one transaction: revoke the device's live credentials and insert the new one.
// The previous credential is never deleted, so revocation granularity and history survive.
type IssueCredential struct {
	TenantID            string
	DeviceID            string
	CredentialID        string
	Type                protocol.AuthMode
	PublicKeyThumbprint string
	PublicKeyJWK        json.RawMessage
	IssuedAt            time.Time
	ExpiresAt           time.Time
	RevokedAt           time.Time
}

// Errors the control path distinguishes. Everything else is an infrastructure failure and is
// retryable.
var (
	ErrUnknownTenant     = errors.New("store: tenant unknown")
	ErrTenantInactive    = errors.New("store: tenant is not active")
	ErrTokenUnknown      = errors.New("store: enrolment token unknown")
	ErrTokenUsed         = errors.New("store: enrolment token already used")
	ErrTokenExpired      = errors.New("store: enrolment token expired")
	ErrTokenRevoked      = errors.New("store: enrolment token revoked")
	ErrTokenBinding      = errors.New("store: enrolment token is bound to another hardware identity")
	ErrDeviceRevoked     = errors.New("store: device revoked")
	ErrDeviceUnknown     = errors.New("store: device unknown")
	ErrCredentialUnknown = errors.New("store: device credential unknown")
	ErrCredentialRevoked = errors.New("store: device credential revoked")
	ErrCredentialExpired = errors.New("store: device credential expired")
	// ErrUnknownCollector is a report naming a collector ref.collector does not hold. It is a
	// validation failure, not an infrastructure one: the report is refused rather than stored under
	// a coverage path nobody can interpret (docs/01 §4.3).
	ErrUnknownCollector = errors.New("store: collector unknown")
)

// CollectorState is one collector's health row as the health channel reports it (docs/02 §5.4).
// State is the closed healthy|degraded|absent|tampered; Detail is the closed error-code vocabulary
// carried to ops.collector_state.error_code; the rest is the row's own shape. SpoolDepth,
// SpoolCapacity and SpoolDroppedTotal are device-level in the report but per-collector rows in the
// schema, so the caller repeats them.
type CollectorState struct {
	Collector         string
	State             string
	Version           string
	Permissions       json.RawMessage
	LastSuccess       *time.Time
	SpoolDepth        *int64
	SpoolCapacity     *int64
	SpoolDroppedTotal int64
	ErrorCode         string
	Detail            json.RawMessage
}

// Store is the persistence seam.
type Store interface {
	// Tenant resolves the tenant's lifecycle state and pinned region. The tenant comes from the
	// authenticated credential or the token, never from the request body.
	Tenant(ctx context.Context, tenantID string) (Tenant, error)
	// ResolveEnrolmentToken looks up a token by (tenant, hash). The caller has hashed the presented
	// token and checks the returned hash back in constant time, so the stored value is the authority.
	ResolveEnrolmentToken(ctx context.Context, tenantID, tokenHash string) (EnrolmentToken, error)
	// FindDeviceByHardwareIdentity is the C11 idempotency lookup. ErrDeviceUnknown means the
	// identity is new.
	FindDeviceByHardwareIdentity(ctx context.Context, tenantID, hardwareIdentityHash string) (Device, error)
	// Device reads one device by id, for re-enrolment and token issue.
	Device(ctx context.Context, tenantID, deviceID string) (Device, error)
	// UpsertDevice inserts a device or updates the mutable fields of the one with the same id.
	UpsertDevice(ctx context.Context, d Device) (Device, error)
	// IssueCredential revokes the device's live credentials and inserts the new one in one
	// transaction, so a rotation cannot leave a device with two live credentials or none.
	IssueCredential(ctx context.Context, in IssueCredential) (Credential, error)
	// DeviceCredential reads one credential by id.
	DeviceCredential(ctx context.Context, tenantID, credentialID string) (Credential, error)
	// DeviceCredentialByDevice reads the device's live credential, for re-enrolment and token issue.
	DeviceCredentialByDevice(ctx context.Context, tenantID, deviceID string) (Credential, error)
	// MarkEnrolmentTokenUsed settles a token after a successful enrolment (§5.1, single-use).
	MarkEnrolmentTokenUsed(ctx context.Context, tenantID, tokenHash string, at time.Time) error
	// RecordHealth upserts one ops.collector_state row per report and stamps the device's
	// last_seen_at, in one transaction (§5.4 step 4, docs/04 §3.7). A report naming a collector not
	// in ref.collector returns ErrUnknownCollector and writes nothing. The per-collector guard means
	// a stale report never overwrites a newer one, so a retry or an out-of-order replay is harmless.
	RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState) error
	// Close releases resources.
	Close() error
}

// NewUUID returns a random RFC 4122 version-4 UUID. It is here rather than in a service so both
// stores and the services that mint device and credential ids agree on one generator.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf), nil
}

// IsUUID reports whether s is the canonical 8-4-4-4-12 hex form. It is the same shape the database
// casts with ::uuid, so a malformed identifier is refused before it reaches a driver.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}
