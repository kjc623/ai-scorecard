package directory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// =====================================================================================
// Every SQL statement this package issues (the lab sync and the user_ref key), in one file.
// =====================================================================================
//
// The same rule control-api's store holds applies here: one place for SQL, so the seam with
// database/schema.sql is reviewable in one screen and the live test can execute the text against a
// real PostgreSQL without going through a Go driver. Every statement is tenant-scoped, and the
// session tenant is set before any other statement; ops.user_dim and ops.tenant are under forced
// row-level security, so a session with no tenant reads and writes nothing.

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
	// A row whose user_ref is a SCIM user's canonical ref is not the file's to retire: SCIM owns
	// that person's status (internal/scim). $1 tenant, $2 the user_refs the source did return.
	SQLRetireDirectoryUsers = `
UPDATE ops.user_dim ud
   SET status = 'inactive'
 WHERE ud.tenant_id = $1::uuid
   AND ud.status <> 'inactive'
   AND NOT (ud.user_ref = ANY($2::text[]))
   AND NOT EXISTS (SELECT 1 FROM ops.scim_user su
                    WHERE su.tenant_id = ud.tenant_id AND su.user_ref = ud.user_ref)`

	// SQLSealedUserRefKey reads the tenant's sealed user-reference key; NULL until first minted.
	// $1 tenant.
	SQLSealedUserRefKey = `SELECT user_ref_key_enc FROM ops.tenant WHERE tenant_id = $1::uuid`

	// SQLInitUserRefKey stores a proposed key only where none is stored, and returns the stored one.
	// Under READ COMMITTED a second concurrent writer blocks on the first's row lock, then re-reads
	// the committed row, so COALESCE keeps the first key and both callers get it back: the race is
	// decided by the database, not by the callers. $1 tenant, $2 the sealed proposal.
	SQLInitUserRefKey = `
UPDATE ops.tenant
   SET user_ref_key_enc = COALESCE(user_ref_key_enc, $2::bytea)
 WHERE tenant_id = $1::uuid
RETURNING user_ref_key_enc`
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
	{Name: "sealed_user_ref_key", Purpose: "read the tenant's sealed user_ref key", SQL: SQLSealedUserRefKey},
	{Name: "init_user_ref_key", Purpose: "mint the tenant's user_ref key once, race-safe", SQL: SQLInitUserRefKey},
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

// SealedUserRefKey implements UserRefKeyStore.
func (s *SQLStore) SealedUserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	var sealed []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLSealedUserRefKey, tenantID).Scan(&sealed)
		if err == sql.ErrNoRows {
			return ErrUnknownTenant
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// InitUserRefKey implements UserRefKeyStore.
func (s *SQLStore) InitUserRefKey(ctx context.Context, tenantID string, sealed []byte) ([]byte, error) {
	var stored []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLInitUserRefKey, tenantID, sealed).Scan(&stored)
		if err == sql.ErrNoRows {
			return ErrUnknownTenant
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
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
