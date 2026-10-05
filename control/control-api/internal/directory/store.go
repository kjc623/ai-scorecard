package directory

import (
	"context"
	"errors"
	"time"
)

// ErrUnknownTenant is returned by a Store when the tenant does not exist. It is distinct from an
// infrastructure failure so the CLI can report a named tenant rather than a connection error.
var ErrUnknownTenant = errors.New("directory: tenant unknown")

// Row is one ops.user_dim row as the synchroniser hands it to a Store. The nullable columns are
// pointers because the table distinguishes "no department" (NULL: unmapped) from a department with
// an empty name, and the read path relies on that distinction (docs/04 §3.3).
type Row struct {
	UserRef              string
	DirectoryObjectIDEnc []byte
	Department           *string
	Population           *string
	ManagerRef           *string
	DisplayName          *string
	Status               string
	SyncedAt             time.Time
}

// Store is the persistence seam. It is small and tenant-explicit, because row-level security is
// forced on ops.user_dim and a session that has not set a tenant reads and writes nothing.
type Store interface {
	// DeviceIdentity returns the tenant's device_identity setting ('clear' or 'hashed'), which
	// decides whether the directory display name is stored. See Row and the schema.
	DeviceIdentity(ctx context.Context, tenantID string) (string, error)
	// UpsertUser inserts or refreshes one person keyed by (tenant, user_ref). It never deletes.
	UpsertUser(ctx context.Context, tenantID string, row Row) error
	// RetireMissing marks every existing row whose user_ref is absent from present as inactive.
	// It returns how many rows changed. It never deletes and never touches the other columns.
	RetireMissing(ctx context.Context, tenantID string, present []string) (int64, error)
}

// Result is what one tenant's sync did, for the log and the report.
type Result struct {
	TenantID string
	Read     int // users the source returned
	Synced   int // users written (excluding those skipped for an empty user_ref)
	Skipped  int // users with no value for the mapping attribute
	Unmapped int // users written with no department
	Retired  int64
}

// nullable models an empty string as SQL NULL. It is used for every ops.user_dim column where the
// difference between NULL and ” is meaningful; department, above all, is the unmapped series.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
