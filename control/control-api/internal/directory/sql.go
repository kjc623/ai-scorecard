package directory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// =====================================================================================
// Every SQL statement the directory sync issues, in one file.
// =====================================================================================
//
// The same rule control-api's store holds applies here: one place for SQL, so the seam with
// database/schema.sql is reviewable in one screen and the live test can execute the text against a
// real PostgreSQL without going through a Go driver. Every statement is tenant-scoped, and the
// session tenant is set before any other statement; ops.user_dim is under forced row-level
// security, so a session with no tenant writes nothing.

const (
	// SQLDirectorySetTenant sets the row-level-security session tenant transaction-locally.
	SQLDirectorySetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// SQLDirectoryDeviceIdentity reads the tenant's device_identity setting, which decides whether
	// a clear directory display name may be stored (ADR 0021). $1 tenant.
	SQLDirectoryDeviceIdentity = `SELECT device_identity FROM ops.tenant WHERE tenant_id = $1::uuid`

	// SQLUpsertDirectoryUser inserts or refreshes one person. COALESCE keeps a previously sealed
	// directory identifier when this read had none; the department and the other attributes ARE
	// overwritten, because a NULL department is the meaningful "unmapped" value, not a missing one.
	SQLUpsertDirectoryUser = `
INSERT INTO ops.user_dim (tenant_id, user_ref, directory_object_id_enc, department, population,
                          manager_ref, display_name, status, synced_at)
VALUES ($1::uuid, $2::text, $3::bytea, $4::text, $5::text, $6::text, $7::text, $8::text,
        $9::timestamptz)
ON CONFLICT (tenant_id, user_ref) DO UPDATE
   SET directory_object_id_enc = COALESCE(EXCLUDED.directory_object_id_enc,
                                          ops.user_dim.directory_object_id_enc),
       department    = EXCLUDED.department,
       population    = EXCLUDED.population,
       manager_ref   = EXCLUDED.manager_ref,
       display_name  = EXCLUDED.display_name,
       status        = EXCLUDED.status,
       synced_at     = EXCLUDED.synced_at`

	// SQLRetireDirectoryUsers marks a row inactive when the directory no longer returns it. It never
	// deletes and never clears the attributes: history stays attributable to the person who left.
	// $1 tenant, $2 the user_refs the source did return.
	SQLRetireDirectoryUsers = `
UPDATE ops.user_dim
   SET status = 'inactive'
 WHERE tenant_id = $1::uuid
   AND status <> 'inactive'
   AND NOT (user_ref = ANY($2::text[]))`
)

// DirectoryStatement pairs a statement with what it is for, so the set can be listed and executed
// by name in the live test.
type DirectoryStatement struct {
	Name    string
	Purpose string
	SQL     string
}

// DirectoryStatements is every statement the sync issues.
var DirectoryStatements = []DirectoryStatement{
	{Name: "set_tenant", Purpose: "RLS session tenant (transaction-local)", SQL: SQLDirectorySetTenant},
	{Name: "device_identity", Purpose: "ADR 0021 display-name gate", SQL: SQLDirectoryDeviceIdentity},
	{Name: "upsert_user", Purpose: "insert/refresh one ops.user_dim row", SQL: SQLUpsertDirectoryUser},
	{Name: "retire_user", Purpose: "mark a departed user inactive (never delete)", SQL: SQLRetireDirectoryUsers},
}

// SQLStore is the database/sql Store. NewSQL takes an already-open *sql.DB; the driver is
// registered by the binary that embeds this package, because the default build carries no
// PostgreSQL driver.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open database handle. The caller owns the driver and closes the handle.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// DeviceIdentity implements Store.
func (s *SQLStore) DeviceIdentity(ctx context.Context, tenantID string) (string, error) {
	var identity string
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLDirectoryDeviceIdentity, tenantID).Scan(&identity)
		if err == sql.ErrNoRows {
			return ErrUnknownTenant
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("directory: device identity: %w", err)
	}
	return identity, nil
}

// UpsertUser implements Store.
func (s *SQLStore) UpsertUser(ctx context.Context, tenantID string, row Row) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var sealed any
		if len(row.DirectoryObjectIDEnc) > 0 {
			sealed = row.DirectoryObjectIDEnc
		}
		_, err := tx.ExecContext(ctx, SQLUpsertDirectoryUser,
			tenantID, row.UserRef, sealed, row.Department, row.Population, row.ManagerRef,
			row.DisplayName, row.Status, row.SyncedAt)
		return err
	})
}

// RetireMissing implements Store.
func (s *SQLStore) RetireMissing(ctx context.Context, tenantID string, present []string) (int64, error) {
	var changed int64
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, SQLRetireDirectoryUsers, tenantID, pgTextArray(present))
		if err != nil {
			return err
		}
		changed, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("directory: retire missing: %w", err)
	}
	return changed, nil
}

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, SQLDirectorySetTenant, tenantID); err != nil {
		return fmt.Errorf("directory: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// pgTextArray renders a []string as a PostgreSQL array literal. It is passed as one text parameter
// and cast to text[] in the statement. pgx sends a typed array for a []string on most paths, but a
// literal keeps the statement driver-independent and is what content-vault's store does for the
// same reason. Quotes and backslashes are escaped so a user_ref containing a comma or brace cannot
// change the array's meaning.
func pgTextArray(vals []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range v {
			if r == '"' || r == '\\' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
