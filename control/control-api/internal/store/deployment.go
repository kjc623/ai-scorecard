package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The enterprise deployment seam (contract §5): the per-tenant deployment key a customer's package
// carries, the tenant's device-verification setting and the Intune binding it produces, the admin
// summary the Settings -> Deployment page reads, and the audit rows every one of those writes.
//
// It is a separate interface from Store on purpose. The single-use enrolment-token path reads none
// of these tables, so a deployment that has not yet migrated them keeps enrolling lab devices; and a
// caller that needs only the deployment half is not handed the credential half.

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

// Errors the deployment path distinguishes.
var (
	ErrDeploymentKeyUnknown = errors.New("store: deployment key unknown")
	ErrDeploymentKeyRevoked = errors.New("store: deployment key revoked")
	ErrDeploymentKeyExpired = errors.New("store: deployment key expired")
	// ErrNoEntraConnection refuses device_verification='intune' for a tenant with no active Entra
	// connection: there would be no customer tenant to ask Intune in.
	ErrNoEntraConnection = errors.New("store: tenant has no active entra connection")
	// ErrIntuneDeviceConflict is a second product device claiming an Intune id one already holds.
	ErrIntuneDeviceConflict = errors.New("store: intune device id already bound to another device")
	// ErrNoPolicyBundle means the tenant has no servable signed bundle.
	ErrNoPolicyBundle = errors.New("store: no policy bundle")
)

// DeploymentKey is ops.deployment_key. KeyHash is the stored sha256:<hex>; the plaintext is never
// held by the store.
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

// Usable reports whether the key may still enrol a device. Revocation is checked first, because a
// revoked key that has also expired was revoked by a person and that is the fact worth reporting.
func (k DeploymentKey) Usable(now time.Time) error {
	switch {
	case k.RevokedAt != nil:
		return ErrDeploymentKeyRevoked
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return ErrDeploymentKeyExpired
	}
	return nil
}

// DeviceVerification is the tenant's enrolment check and what it needs: the mode, and the Entra
// tenant of the tenant's active Entra connection (empty when there is none).
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

// AuditEntry is one ops.audit row. prev_hash and row_hash are the chain trigger's, never the
// caller's. Detail must not carry a key, a token or package content.
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

func (a AuditEntry) detailJSON() (string, error) {
	if len(a.Detail) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(a.Detail)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DeploymentSummary is everything GET /admin/v1/deployment reads from the database in one go.
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

// DeploymentStore is the persistence seam of deployment-key enrolment and of the deployment admin
// API. Every method is tenant-scoped, exactly as Store's are.
type DeploymentStore interface {
	// DeploymentKeyByHash resolves a presented key by (tenant, hash). ErrDeploymentKeyUnknown when
	// no row matches; the caller compares the hash back in constant time.
	DeploymentKeyByHash(ctx context.Context, tenantID, keyHash string) (DeploymentKey, error)
	// DeviceVerification reads the tenant's mode and its active Entra connection's tenant id.
	// ErrUnknownTenant when the tenant does not exist.
	DeviceVerification(ctx context.Context, tenantID string) (DeviceVerification, error)
	// FindDeviceByIntuneID is the Intune half of C11: ErrDeviceUnknown means the id is new.
	FindDeviceByIntuneID(ctx context.Context, tenantID, intuneDeviceID string) (Device, error)
	// SetDeviceIntuneID binds a verified Intune id to a device. ErrIntuneDeviceConflict when another
	// device of the tenant already holds it.
	SetDeviceIntuneID(ctx context.Context, tenantID, deviceID, intuneDeviceID string) error
	// RecordDeploymentEnrolment counts one enrolment against the key (enrolment_count,
	// last_used_at) and writes its audit row, in one transaction.
	RecordDeploymentEnrolment(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) error
	// Audit writes one audit row on its own, for an event that changes no other row.
	Audit(ctx context.Context, audit AuditEntry) error

	// CreateDeploymentKey inserts a key and its audit row in one transaction.
	CreateDeploymentKey(ctx context.Context, k DeploymentKey, audit AuditEntry) (DeploymentKey, error)
	// RevokeDeploymentKey revokes a key. Revoking a revoked key is not an error and writes no second
	// audit row; ErrDeploymentKeyUnknown when the tenant has no such key.
	RevokeDeploymentKey(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) (DeploymentKey, error)
	// SetDeviceVerification changes the tenant's mode and writes its audit row in one transaction.
	// 'intune' needs an active Entra connection, checked inside the same transaction:
	// ErrNoEntraConnection otherwise.
	SetDeviceVerification(ctx context.Context, tenantID, mode string, audit AuditEntry) error
	// DeploymentSummary reads the admin page's figures. ErrUnknownTenant when the tenant does not
	// exist.
	DeploymentSummary(ctx context.Context, tenantID string) (DeploymentSummary, error)
}

// PolicyTenant is the slice of ops.tenant a policy bundle is composed from.
type PolicyTenant struct {
	TenantID      string
	Status        string
	IngestEnabled bool
	CeilingMode   string
}

// Active mirrors Tenant.Active: a tenant that may not enrol may not be served policy either.
func (t PolicyTenant) Active() bool { return t.Status != "closed" && t.IngestEnabled }

// ClassifierRelease is the ref.classifier_release row a bundle names.
type ClassifierRelease struct {
	Version string
	State   string
}

// PolicyInputs is everything a bundle is composed from that lives in the database. Classifier is
// nil when no release in a servable state exists, which is the one input without which no bundle
// can be written (ops.policy_bundle.classifier_release is a NOT NULL foreign key).
type PolicyInputs struct {
	Tenant            PolicyTenant
	InterceptionHosts []string
	Classifier        *ClassifierRelease
}

// PolicyBundle is one ops.policy_bundle row as the policy path writes and serves it.
// SignedEnvelope is the exact bytes GET /v1/policy serves; a row without one (written before the
// column existed) is never served, but its version still bounds the next one.
type PolicyBundle struct {
	TenantID             string
	Version              int64
	ScopeMatrix          json.RawMessage
	DestinationAllowlist json.RawMessage
	FeatureState         json.RawMessage
	SpoolBounds          json.RawMessage
	ClassifierRelease    string
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

// PolicyStore is the persistence seam of GET /v1/policy.
type PolicyStore interface {
	// PolicyInputs reads the tenant row, the interception hosts of the tool catalogue and the
	// classifier release a new bundle would name. ErrUnknownTenant when the tenant does not exist.
	PolicyInputs(ctx context.Context, tenantID string) (PolicyInputs, error)
	// LatestPolicyBundle returns the highest-versioned bundle. ErrNoPolicyBundle when none exists.
	LatestPolicyBundle(ctx context.Context, tenantID string) (PolicyBundle, error)
	// MintPolicyBundle runs decide under a per-tenant lock with the latest bundle (nil when none),
	// inserts what it returns in the same transaction, and returns the latest bundle afterwards. The
	// lock is what makes two replicas that see the same changed inputs mint one version, not two.
	MintPolicyBundle(ctx context.Context, tenantID string, decide func(latest *PolicyBundle) (MintDecision, error)) (PolicyBundle, error)
}
