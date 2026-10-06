// Package store is control-api's PostgreSQL persistence for devices, their certificates, health,
// deployment keys, policy bundles and the audit trail.
//
// Every method takes the tenant explicitly and runs in a transaction that first sets the
// row-level-security session tenant: RLS is forced on every ops table, so a session without a
// tenant reads and writes nothing. Every statement is a constant in this package and is executed
// as written by the live test.
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

// Tenant is the slice of ops.tenant the device paths decide on. DeviceIdentity is returned to the
// device and decides which of hostname and hostname hash is stored.
type Tenant struct {
	TenantID        string
	Status          string
	IngestEnabled   bool
	ResidencyRegion string
	DeviceIdentity  protocol.DeviceIdentity
}

// Active reports whether the tenant may enrol devices and be served policy.
func (t Tenant) Active() bool { return t.Status != "closed" && t.IngestEnabled }

// Device is the slice of ops.device the control path writes and reads. HardwareIdentityHash is the
// per-tenant idempotency key: a re-enrolment that matches it returns the existing device.
type Device struct {
	TenantID             string
	DeviceID             string
	OS                   string
	OSVersion            string
	HardwareIdentityHash string
	ResidencyRegion      string
	// Hostname is stored for a 'clear' tenant and HostnameHash for a 'hashed' one.
	Hostname     string
	HostnameHash string
	AgentVersion string
	ManagedState string
	EnrolledAt   time.Time
	RevokedAt    *time.Time
	// IntuneDeviceID is the Intune managed-device id a deployment-key enrolment was verified
	// against. Only SetDeviceIntuneID writes it, and it is unique per tenant.
	IntuneDeviceID string
}

// Credential is one ops.device_credential row: a device certificate. CredentialID is derived from
// the certificate (protocol.CredentialID) and PublicKeyThumbprint is the SHA-256 of its
// SubjectPublicKeyInfo, base64url.
type Credential struct {
	TenantID            string
	CredentialID        string
	DeviceID            string
	PublicKeyThumbprint string
	IssuedAt            time.Time
	ExpiresAt           time.Time
	RevokedAt           *time.Time
}

// Active reports whether the credential may still authenticate the device.
func (c Credential) Active(now time.Time) error {
	switch {
	case c.RevokedAt != nil:
		return ErrCredentialRevoked
	case !c.ExpiresAt.After(now):
		return ErrCredentialExpired
	}
	return nil
}

// CollectorState is one ops.collector_state row as the health channel reports it. The spool
// figures are device-level in the report and repeated on every collector's row.
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

// DeviceHealth is the device-level part of a health report. Empty fields leave the stored value.
type DeviceHealth struct {
	Hostname       string
	HostnameHash   string
	AgentVersion   string
	CollectionMode string
	ManagedState   string
}

// Device-verification modes, the closed set ops.tenant.device_verification holds.
const (
	VerificationNone   = "none"
	VerificationIntune = "intune"
)

// Audit actor types, the closed set ops.audit.actor_type holds.
const (
	ActorUser    = "user"
	ActorDevice  = "device"
	ActorService = "service"
	ActorSystem  = "system"
)

// DeploymentKey is ops.deployment_key. KeyHash is the stored sha256:<hex>; the plaintext key is
// never stored.
type DeploymentKey struct {
	KeyID          string
	TenantID       string
	KeyHash        string
	Label          string
	CreatedBy      string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	RevokedAt      *time.Time
	EnrolmentCount int64
	LastUsedAt     *time.Time
}

// Usable reports whether the key may still enrol a device. A revoked key is reported as revoked
// even when it has also expired, because the revocation is the decision worth reporting.
func (k DeploymentKey) Usable(now time.Time) error {
	switch {
	case k.RevokedAt != nil:
		return ErrDeploymentKeyRevoked
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return ErrDeploymentKeyExpired
	}
	return nil
}

// DeviceVerification is the tenant's enrolment check and the Entra tenant of its active Entra
// connection (empty when there is none).
type DeviceVerification struct {
	Mode          string
	EntraTenantID string
}

// IdentityConnection is the slice of ops.identity_connection the admin page shows.
type IdentityConnection struct {
	Provider      string
	Status        string
	EntraTenantID string
	Issuer        string
}

// AuditEntry is one ops.audit row. The chain hashes are computed by the table's trigger. Detail
// never carries a key, a token or package content.
type AuditEntry struct {
	TenantID   string
	ActorType  string
	ActorID    string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
	OccurredAt time.Time
}

// DeploymentSummary is what the deployment admin page reads.
type DeploymentSummary struct {
	Connection         *IdentityConnection
	DeviceVerification string
	Keys               []DeploymentKey
	DevicesEnrolled    int64
	LastEnrolledAt     *time.Time
	ScimUsers          int64
	ScimGroups         int64
	LastProvisionedAt  *time.Time
}

// PolicyTenant is the slice of ops.tenant a policy bundle is composed from.
type PolicyTenant struct {
	TenantID      string
	Status        string
	IngestEnabled bool
	CeilingMode   string
}

// Active mirrors Tenant.Active.
func (t PolicyTenant) Active() bool { return t.Status != "closed" && t.IngestEnabled }

// PolicyInputs is everything in the database a bundle is composed from.
type PolicyInputs struct {
	Tenant            PolicyTenant
	InterceptionHosts []string
}

// PolicyBundle is one ops.policy_bundle row. SignedEnvelope is the exact bytes GET /v1/policy
// serves.
type PolicyBundle struct {
	TenantID             string
	Version              int64
	ScopeMatrix          json.RawMessage
	DestinationAllowlist json.RawMessage
	FeatureState         json.RawMessage
	SpoolBounds          json.RawMessage
	RetentionClass       string
	SignatureKID         string
	SignedDigest         string
	SignedEnvelope       []byte
	EffectiveFrom        time.Time
	CreatedBy            string
	CreatedAt            time.Time
}

// MintDecision is what a MintPolicyBundle callback returns: a bundle to insert and its audit row,
// or a nil Bundle to keep the latest in force.
type MintDecision struct {
	Bundle *PolicyBundle
	Audit  *AuditEntry
}

// Errors callers distinguish. Every other error is an infrastructure failure and is retryable.
var (
	ErrUnknownTenant     = errors.New("store: tenant unknown")
	ErrDeviceUnknown     = errors.New("store: device unknown")
	ErrCredentialUnknown = errors.New("store: device credential unknown")
	ErrCredentialRevoked = errors.New("store: device credential revoked")
	ErrCredentialExpired = errors.New("store: device credential expired")
	// ErrUnknownCollector is a health report naming a collector ref.collector does not hold.
	ErrUnknownCollector     = errors.New("store: collector unknown")
	ErrDeploymentKeyUnknown = errors.New("store: deployment key unknown")
	ErrDeploymentKeyRevoked = errors.New("store: deployment key revoked")
	ErrDeploymentKeyExpired = errors.New("store: deployment key expired")
	// ErrNoEntraConnection refuses device_verification 'intune' for a tenant with no active Entra
	// connection: there would be no customer tenant to look the device up in.
	ErrNoEntraConnection = errors.New("store: tenant has no active entra connection")
	// ErrIntuneDeviceConflict is a second device claiming an Intune id another device holds.
	ErrIntuneDeviceConflict = errors.New("store: intune device id already bound to another device")
	// ErrNoPolicyBundle means the tenant has no servable signed bundle.
	ErrNoPolicyBundle = errors.New("store: no policy bundle")
)

// Store is control-api's persistence. *SQLStore implements it; tests use storetest.Memory.
type Store interface {
	// Tenant resolves the tenant's lifecycle state, pinned region and identity setting.
	Tenant(ctx context.Context, tenantID string) (Tenant, error)
	// FindDeviceByHardwareIdentity is the idempotency lookup; ErrDeviceUnknown means the identity
	// is new.
	FindDeviceByHardwareIdentity(ctx context.Context, tenantID, hardwareIdentityHash string) (Device, error)
	// Device reads one device.
	Device(ctx context.Context, tenantID, deviceID string) (Device, error)
	// UpsertDevice inserts a device or refreshes the mutable fields of the one with the same id.
	UpsertDevice(ctx context.Context, d Device) (Device, error)
	// IssueCredential revokes the device's live credentials at c.IssuedAt and inserts c, in one
	// transaction, so a rotation cannot leave a device with two live credentials or none.
	IssueCredential(ctx context.Context, c Credential) error
	// DeviceCredential reads one credential.
	DeviceCredential(ctx context.Context, tenantID, credentialID string) (Credential, error)
	// RecordHealth upserts one ops.collector_state row per report (a stale report never overwrites
	// a newer one), applies the device-level fields and stamps last_seen_at, in one transaction. A
	// report naming an unknown collector returns ErrUnknownCollector and writes nothing.
	RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState, dev DeviceHealth) error

	// DeploymentKeyByHash resolves a presented deployment key; ErrDeploymentKeyUnknown when no row
	// matches.
	DeploymentKeyByHash(ctx context.Context, tenantID, keyHash string) (DeploymentKey, error)
	// DeviceVerification reads the tenant's mode and its active Entra connection's tenant id.
	DeviceVerification(ctx context.Context, tenantID string) (DeviceVerification, error)
	// FindDeviceByIntuneID is the Intune idempotency lookup; ErrDeviceUnknown means the id is new.
	FindDeviceByIntuneID(ctx context.Context, tenantID, intuneDeviceID string) (Device, error)
	// SetDeviceIntuneID binds a verified Intune id to a device; ErrIntuneDeviceConflict when another
	// device of the tenant holds it.
	SetDeviceIntuneID(ctx context.Context, tenantID, deviceID, intuneDeviceID string) error
	// RecordDeploymentEnrolment counts one enrolment against the key and writes its audit row.
	RecordDeploymentEnrolment(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) error
	// Audit writes one audit row.
	Audit(ctx context.Context, audit AuditEntry) error
	// CreateDeploymentKey inserts a key and its audit row.
	CreateDeploymentKey(ctx context.Context, k DeploymentKey, audit AuditEntry) (DeploymentKey, error)
	// RevokeDeploymentKey revokes a key. Revoking a revoked key returns it unchanged and writes no
	// second audit row; ErrDeploymentKeyUnknown when the tenant has no such key.
	RevokeDeploymentKey(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) (DeploymentKey, error)
	// SetDeviceVerification changes the tenant's mode and writes its audit row. 'intune' needs an
	// active Entra connection: ErrNoEntraConnection otherwise.
	SetDeviceVerification(ctx context.Context, tenantID, mode string, audit AuditEntry) error
	// DeploymentSummary reads the admin page's figures.
	DeploymentSummary(ctx context.Context, tenantID string) (DeploymentSummary, error)

	// PolicyInputs reads what a bundle is composed from.
	PolicyInputs(ctx context.Context, tenantID string) (PolicyInputs, error)
	// LatestPolicyBundle returns the highest-versioned bundle; ErrNoPolicyBundle when none exists.
	LatestPolicyBundle(ctx context.Context, tenantID string) (PolicyBundle, error)
	// MintPolicyBundle runs decide under a per-tenant lock with the latest bundle (nil when none),
	// inserts what it returns, and returns the bundle in force afterwards. The lock makes replicas
	// that see the same changed inputs mint one version, not two.
	MintPolicyBundle(ctx context.Context, tenantID string, decide func(latest *PolicyBundle) (MintDecision, error)) (PolicyBundle, error)

	// Ping checks the database is reachable, for readiness.
	Ping(ctx context.Context) error
}

// NewUUID returns a random version-4 UUID.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// IsUUID reports whether s is the canonical 8-4-4-4-12 hex form the database casts with ::uuid, so
// a malformed identifier is refused before it reaches a statement.
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
