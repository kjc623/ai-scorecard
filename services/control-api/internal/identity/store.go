package identity

import (
	"context"
	"errors"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// The connection providers and statuses, as ops.identity_connection spells them.
const (
	ProviderEntra = "entra"
	ProviderOIDC  = "oidc"

	StatusPending  = "pending"
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Errors a Store returns. Each is a fact about the data, distinct from an infrastructure failure.
var (
	ErrNotFound = errors.New("identity: not found")
	// ErrInviteUsed is an invite that was redeemed (or expired) before this activation could.
	ErrInviteUsed = errors.New("identity: invite already used or expired")
	// ErrLinkedElsewhere is an Entra tenant id or an issuer already linked to another product tenant.
	ErrLinkedElsewhere = errors.New("identity: already linked to another tenant")
	// ErrConnectionDisabled is a connection a vendor or admin disabled.
	ErrConnectionDisabled = errors.New("identity: connection disabled")
)

// Connection is one ops.identity_connection row.
type Connection struct {
	ID              string
	TenantID        string
	Provider        string
	EntraTenantID   string // lower-case uuid; entra only
	Issuer          string // exact iss; oidc only
	ClientID        string // oidc only (Entra uses the vendor's app)
	ClientSecretEnc []byte // oidc only, sealed under the tenant's key
	Scopes          string
	RolesClaim      string
	// RoleMap maps an IdP role value to a product role. Empty means the identity map over the
	// product role names.
	RoleMap     map[string]string
	Status      string
	CreatedAt   time.Time
	ActivatedAt *time.Time
	ActivatedBy string
}

// Invite is one ops.onboarding_invite row.
type Invite struct {
	ID        string
	TenantID  string
	TokenHash string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    string
}

// Live reports whether the invite can still be redeemed at now.
func (i Invite) Live(now time.Time) bool { return i.UsedAt == nil && now.Before(i.ExpiresAt) }

// Attempt is one ops.auth_signin row: a sign-in in flight, or an onboarding consent in flight. Only
// the hash of the handle the caller holds is stored, and the PKCE verifier is sealed, so a read of the
// table yields nothing that completes a sign-in.
type Attempt struct {
	Hash         []byte
	ConnectionID string // empty for "Sign in with Microsoft", where the tid decides at completion
	State        string
	Nonce        string
	VerifierEnc  []byte
	RedirectURI  string
	InviteID     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// AuditEntry is one ops.audit row's caller-supplied part; the store adds the tenant and the time.
type AuditEntry struct {
	ActorType  string // user | service | system
	ActorID    string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
}

// Activation is the onboarding sign-in's commit: the invite is spent, the connection becomes active,
// and the person who signed in through it is granted admin, all or nothing.
type Activation struct {
	TenantID     string
	ConnectionID string
	InviteID     string
	Subject      string
	Actor        string
	At           time.Time
}

// NewConnection is a pending connection an onboarding step creates.
type NewConnection struct {
	TenantID        string
	Provider        string
	EntraTenantID   string
	Issuer          string
	ClientID        string
	ClientSecretEnc []byte
	Scopes          string
}

// TenantAccess is the slice of ops.tenant that decides whether its people may be given product
// tokens.
type TenantAccess struct {
	Status      string
	ReadEnabled bool
}

// TenantSummary is what the onboarding page shows about the tenant it is connecting.
type TenantSummary struct {
	Name    string
	Domains []string
}

// Store is the persistence seam. Lookups that run before a tenant is known go through SECURITY
// DEFINER functions; everything else sets the row-level-security tenant first.
type Store interface {
	// ConnectionForEntraTenant is ops.identity_connection_for_entra: active connections only.
	ConnectionForEntraTenant(ctx context.Context, entraTenantID string) (Connection, error)
	// ConnectionByID is ops.identity_connection_by_id: any status.
	ConnectionByID(ctx context.Context, connectionID string) (Connection, error)
	// TenantForEmailDomain is ops.tenant_for_email_domain.
	TenantForEmailDomain(ctx context.Context, domain string) (string, error)

	// PutAttempt and TakeAttempt hold sign-in and consent state in ops.auth_signin. TakeAttempt
	// deletes as it reads, so an attempt is redeemable exactly once.
	PutAttempt(ctx context.Context, a Attempt) error
	TakeAttempt(ctx context.Context, hash []byte) (Attempt, error)
	SweepAttempts(ctx context.Context, before time.Time) error

	TenantConnections(ctx context.Context, tenantID string) ([]Connection, error)
	TenantSummary(ctx context.Context, tenantID string) (TenantSummary, error)
	// TenantAccess reads the tenant's status and read gate; ErrNotFound when it does not exist.
	TenantAccess(ctx context.Context, tenantID string) (TenantAccess, error)
	Invite(ctx context.Context, tenantID, tokenHash string) (Invite, error)
	InviteByID(ctx context.Context, tenantID, inviteID string) (Invite, error)
	RoleGrants(ctx context.Context, tenantID, connectionID, subject string) ([]string, error)
	// UserRefKey is the tenant's sealed user-reference key, or nil when none has been minted.
	UserRefKey(ctx context.Context, tenantID string) ([]byte, error)
	// ScimUser finds the SCIM-provisioned person whose canonical ref, or an alias of it, is one of
	// refs. ErrNotFound when SCIM does not know them.
	ScimUser(ctx context.Context, tenantID string, refs []string) (userRef string, active bool, err error)

	// CreatePendingConnection reuses this tenant's pending connection for the same Entra tenant or
	// issuer, or inserts one. ErrLinkedElsewhere when another product tenant holds it,
	// ErrConnectionDisabled when this tenant's own connection for it is disabled. The audit row
	// commits with it.
	CreatePendingConnection(ctx context.Context, c NewConnection, audit AuditEntry) (Connection, error)
	// Activate commits an Activation. ErrInviteUsed when the invite is spent or expired;
	// ErrConnectionDisabled when the connection is.
	Activate(ctx context.Context, a Activation) error
	Audit(ctx context.Context, tenantID string, e AuditEntry) error
}

// ActivationAudit is the three rows an activation commits, shared by every Store so they cannot
// drift: the invite spent, the connection activated, the first admin granted — each attributed to
// the person whose sign-in did it.
func ActivationAudit(a Activation) []AuditEntry {
	return []AuditEntry{
		{ActorType: "user", ActorID: a.Actor, Action: "onboarding_invite.use", ObjectType: "onboarding_invite", ObjectID: a.InviteID},
		{ActorType: "user", ActorID: a.Actor, Action: "identity_connection.activate", ObjectType: "identity_connection",
			ObjectID: a.ConnectionID, Detail: map[string]any{"invite_id": a.InviteID}},
		{ActorType: "user", ActorID: a.Actor, Action: "role.grant", ObjectType: "role_grant", ObjectID: a.ConnectionID + ":" + a.Subject,
			Detail: map[string]any{"role": session.RoleAdmin, "granted_by": "onboarding-invite:" + a.InviteID}},
	}
}
