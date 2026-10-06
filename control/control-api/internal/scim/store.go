package scim

import (
	"context"
	"errors"
	"time"
)

// Store errors. A Tx returns them unwrapped so the service can map each to its SCIM status.
var (
	ErrNotFound      = errors.New("scim: not found")
	ErrConflict      = errors.New("scim: uniqueness conflict")
	ErrUnknownTenant = errors.New("scim: tenant unknown")
)

// UserRow is one ops.scim_user row. ResourceEnc is the sealed resource as last provisioned; the
// hashes are HMACs under the tenant's user-reference key (LookupHash), never clear values.
type UserRow struct {
	ID             string
	UserNameHash   []byte
	ExternalIDHash []byte
	ResourceEnc    []byte
	UserRef        string
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UserDimRow is the person's ops.user_dim row as SCIM writes it: the sealed externalId (else
// userName), the department from the enterprise extension, the display name only while the tenant's
// device_identity is 'clear', and the status from `active`. A nil pointer is SQL NULL.
type UserDimRow struct {
	UserRef              string
	DirectoryObjectIDEnc []byte
	Department           *string
	DisplayName          *string
	Status               string
	SyncedAt             time.Time
}

// The two ops.user_dim status values SCIM writes.
const (
	DimActive   = "active"
	DimInactive = "inactive"
)

// GroupRow is one ops.scim_group row. A group's name is not a person's data, so it is stored clear.
type GroupRow struct {
	ID          string
	DisplayName string
	ExternalID  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TokenRow is one ops.scim_token row.
type TokenRow struct {
	ID        string
	Hash      string
	Label     string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// AuditEntry is one ops.audit row. Detail must never carry personal data: attribute names and
// counts, not values.
type AuditEntry struct {
	ActorType  string
	ActorID    string
	Action     string
	ObjectType string
	ObjectID   string
	SubjectRef string
	Detail     map[string]any
	At         time.Time
}

// Store is the persistence seam. TenantForToken is the one pre-tenant read (a definer function);
// everything else runs inside InTenant, one transaction under that tenant's RLS session, so a SCIM
// write and its aliases, its ops.user_dim row and its audit row commit together or not at all.
type Store interface {
	// TenantForToken returns the tenant an unrevoked token hash belongs to, or "" when none does.
	TenantForToken(ctx context.Context, tokenHash string) (string, error)
	InTenant(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// Tx is the tenant-scoped unit of work. Every method acts on the tenant InTenant was opened for.
type Tx interface {
	// DeviceIdentity is the tenant's device_identity ('clear' or 'hashed'); ErrUnknownTenant if the
	// tenant does not exist.
	DeviceIdentity() (string, error)

	TokenByHash(hash string) (TokenRow, error)
	Token(id string) (TokenRow, error)
	InsertToken(TokenRow) error
	// RevokeToken reports whether the token was live and is now revoked.
	RevokeToken(id string, at time.Time) (bool, error)
	ListTokens() ([]TokenRow, error)

	// User reads one user; forUpdate locks the row for the rest of the transaction.
	User(id string, forUpdate bool) (UserRow, error)
	UsersByUserNameHash(hash []byte) ([]UserRow, error)
	UsersByExternalIDHash(hash []byte) ([]UserRow, error)
	ListUsers(offset, limit int) ([]UserRow, error)
	CountUsers() (int, error)
	// UserRefOwner returns the id of the user whose canonical ref is ref, or "".
	UserRefOwner(ref string) (string, error)
	// InsertUser and UpdateUser return ErrConflict when another user holds the userName hash.
	InsertUser(UserRow) error
	UpdateUser(UserRow) error
	// PutAlias points alias at canonical, moving it if it pointed elsewhere.
	PutAlias(alias, canonical string, at time.Time) error
	UpsertUserDim(UserDimRow) error

	Group(id string, forUpdate bool) (GroupRow, error)
	GroupsByDisplayName(name string) ([]GroupRow, error)
	GroupsByExternalID(externalID string) ([]GroupRow, error)
	ListGroups(offset, limit int) ([]GroupRow, error)
	CountGroups() (int, error)
	InsertGroup(GroupRow) error
	UpdateGroup(GroupRow) error
	// DeleteGroup removes the group and its memberships.
	DeleteGroup(id string) error
	Members(groupID string) ([]string, error)
	// AddMember reports whether a membership was added: false when the user is not this tenant's or
	// is already a member.
	AddMember(groupID, userID string) (bool, error)
	RemoveMember(groupID, userID string) (bool, error)

	Audit(AuditEntry) error
}
