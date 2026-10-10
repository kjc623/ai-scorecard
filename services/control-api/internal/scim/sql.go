package scim

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// =====================================================================================
// Every SQL statement the SCIM provider issues, in one file.
// =====================================================================================
//
// One place for SQL, so the seam with services/database/schema.sql is reviewable in one screen, and the live
// test prepares every statement against the real schema. Every statement after the token lookup runs in a transaction whose RLS session tenant was
// set first; the tables are under forced row-level security, so a session with no tenant reads and
// writes nothing. The one pre-tenant read is ops.tenant_for_scim_token, a definer function that
// returns only a tenant id.

const (
	SQLSCIMSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// SQLTenantForSCIMToken resolves an unrevoked token hash to its tenant; NULL when none. $1 hash.
	SQLTenantForSCIMToken = `SELECT ops.tenant_for_scim_token($1::text)::text`

	SQLSCIMDeviceIdentity = `SELECT device_identity FROM ops.tenant WHERE tenant_id = $1::uuid`

	sqlTokenColumns = `token_id::text, token_hash, COALESCE(label, ''), COALESCE(created_by, ''), created_at, revoked_at`

	SQLSCIMTokenByHash = `SELECT ` + sqlTokenColumns + ` FROM ops.scim_token
 WHERE tenant_id = $1::uuid AND token_hash = $2::text`

	SQLSCIMToken = `SELECT ` + sqlTokenColumns + ` FROM ops.scim_token
 WHERE tenant_id = $1::uuid AND token_id = $2::uuid`

	SQLInsertSCIMToken = `
INSERT INTO ops.scim_token (token_id, tenant_id, token_hash, label, created_by, created_at)
VALUES ($1::uuid, $2::uuid, $3::text, NULLIF($4::text, ''), $5::text, $6::timestamptz)`

	// SQLRevokeSCIMToken revokes a live token; zero rows means unknown or already revoked.
	SQLRevokeSCIMToken = `
UPDATE ops.scim_token SET revoked_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND token_id = $2::uuid AND revoked_at IS NULL`

	SQLListSCIMTokens = `SELECT ` + sqlTokenColumns + ` FROM ops.scim_token
 WHERE tenant_id = $1::uuid ORDER BY created_at DESC, token_id`

	sqlUserColumns = `scim_id::text, user_name_hash, external_id_hash, resource_enc, user_ref, active, created_at, updated_at`

	SQLSCIMUser = `SELECT ` + sqlUserColumns + ` FROM ops.scim_user
 WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`

	// SQLSCIMUserForUpdate locks the row so two concurrent PATCHes of one person apply in turn rather
	// than the second overwriting the first's sealed resource.
	SQLSCIMUserForUpdate = SQLSCIMUser + ` FOR UPDATE`

	SQLSCIMUsersByUserNameHash = `SELECT ` + sqlUserColumns + ` FROM ops.scim_user
 WHERE tenant_id = $1::uuid AND user_name_hash = $2::bytea ORDER BY created_at, scim_id`

	SQLSCIMUsersByExternalIDHash = `SELECT ` + sqlUserColumns + ` FROM ops.scim_user
 WHERE tenant_id = $1::uuid AND external_id_hash = $2::bytea ORDER BY created_at, scim_id`

	SQLSCIMListUsers = `SELECT ` + sqlUserColumns + ` FROM ops.scim_user
 WHERE tenant_id = $1::uuid ORDER BY created_at, scim_id OFFSET $2::int LIMIT $3::int`

	SQLSCIMCountUsers = `SELECT count(*) FROM ops.scim_user WHERE tenant_id = $1::uuid`

	SQLSCIMUserRefOwner = `SELECT scim_id::text FROM ops.scim_user
 WHERE tenant_id = $1::uuid AND user_ref = $2::text ORDER BY created_at LIMIT 1`

	SQLInsertSCIMUser = `
INSERT INTO ops.scim_user (tenant_id, scim_id, user_name_hash, external_id_hash, resource_enc, user_ref,
                           active, created_at, updated_at)
VALUES ($1::uuid, $2::uuid, $3::bytea, $4::bytea, $5::bytea, $6::text, $7::boolean,
        $8::timestamptz, $9::timestamptz)`

	// SQLUpdateSCIMUser never touches user_ref: the canonical ref is fixed at creation.
	SQLUpdateSCIMUser = `
UPDATE ops.scim_user
   SET user_name_hash = $3::bytea, external_id_hash = $4::bytea, resource_enc = $5::bytea,
       active = $6::boolean, updated_at = $7::timestamptz
 WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`

	// SQLPutUserRefAlias points a device-derived ref at a canonical one. An alias already pointing
	// elsewhere is moved: a upn-ref belongs to whoever holds that userName now. $1 tenant, $2 alias,
	// $3 canonical, $4 at.
	SQLPutUserRefAlias = `
INSERT INTO ops.user_ref_alias (tenant_id, alias_ref, user_ref, created_at)
VALUES ($1::uuid, $2::text, $3::text, $4::timestamptz)
ON CONFLICT (tenant_id, alias_ref) DO UPDATE
   SET user_ref = EXCLUDED.user_ref
 WHERE ops.user_ref_alias.user_ref IS DISTINCT FROM EXCLUDED.user_ref`

	// SQLUpsertUserDim writes the person's ops.user_dim row. COALESCE keeps a previously sealed
	// directory identifier when this write has none; the department and unit are overwritten,
	// because NULL is the meaningful "unmapped" value, not a missing one. $1 tenant, $2 user_ref,
	// $3 sealed directory id, $4 department, $5 display name, $6 status, $7 at, $8 org unit.
	SQLUpsertUserDim = `
INSERT INTO ops.user_dim (tenant_id, user_ref, directory_object_id_enc, department, display_name, status, synced_at, org_unit)
VALUES ($1::uuid, $2::text, $3::bytea, $4::text, $5::text, $6::text, $7::timestamptz, $8::text)
ON CONFLICT (tenant_id, user_ref) DO UPDATE
   SET directory_object_id_enc = COALESCE(EXCLUDED.directory_object_id_enc,
                                          ops.user_dim.directory_object_id_enc),
       department   = EXCLUDED.department,
       org_unit     = EXCLUDED.org_unit,
       display_name = EXCLUDED.display_name,
       status       = EXCLUDED.status,
       synced_at    = EXCLUDED.synced_at`

	sqlGroupColumns = `scim_id::text, display_name, COALESCE(external_id, ''), created_at, updated_at`

	SQLSCIMGroup = `SELECT ` + sqlGroupColumns + ` FROM ops.scim_group
 WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`

	SQLSCIMGroupForUpdate = SQLSCIMGroup + ` FOR UPDATE`

	SQLSCIMGroupsByDisplayName = `SELECT ` + sqlGroupColumns + ` FROM ops.scim_group
 WHERE tenant_id = $1::uuid AND lower(display_name) = lower($2::text) ORDER BY created_at, scim_id`

	SQLSCIMGroupsByExternalID = `SELECT ` + sqlGroupColumns + ` FROM ops.scim_group
 WHERE tenant_id = $1::uuid AND external_id = $2::text ORDER BY created_at, scim_id`

	SQLSCIMListGroups = `SELECT ` + sqlGroupColumns + ` FROM ops.scim_group
 WHERE tenant_id = $1::uuid ORDER BY created_at, scim_id OFFSET $2::int LIMIT $3::int`

	SQLSCIMCountGroups = `SELECT count(*) FROM ops.scim_group WHERE tenant_id = $1::uuid`

	SQLInsertSCIMGroup = `
INSERT INTO ops.scim_group (tenant_id, scim_id, display_name, external_id, created_at, updated_at)
VALUES ($1::uuid, $2::uuid, $3::text, NULLIF($4::text, ''), $5::timestamptz, $6::timestamptz)`

	SQLUpdateSCIMGroup = `
UPDATE ops.scim_group
   SET display_name = $3::text, external_id = NULLIF($4::text, ''), updated_at = $5::timestamptz
 WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`

	SQLDeleteSCIMGroupMembers = `DELETE FROM ops.scim_group_member WHERE tenant_id = $1::uuid AND group_id = $2::uuid`

	SQLDeleteSCIMGroup = `DELETE FROM ops.scim_group WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`

	SQLSCIMGroupMembers = `SELECT user_id::text FROM ops.scim_group_member
 WHERE tenant_id = $1::uuid AND group_id = $2::uuid ORDER BY user_id`

	// SQLAddSCIMGroupMember adds a membership only for a user this tenant has; zero rows means the
	// user is unknown or already a member.
	SQLAddSCIMGroupMember = `
INSERT INTO ops.scim_group_member (tenant_id, group_id, user_id)
SELECT $1::uuid, $2::uuid, $3::uuid
 WHERE EXISTS (SELECT 1 FROM ops.scim_user WHERE tenant_id = $1::uuid AND scim_id = $3::uuid)
ON CONFLICT DO NOTHING`

	SQLRemoveSCIMGroupMember = `DELETE FROM ops.scim_group_member
 WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND user_id = $3::uuid`

	// SQLSCIMInsertAudit writes one audit row; the ops.audit_chain() trigger computes the hashes.
	SQLSCIMInsertAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id,
                       subject_ref, detail, occurred_at)
VALUES ($1::uuid, $2::text, $3::text, $4::text, $5::text, NULLIF($6::text, ''),
        NULLIF($7::text, ''), $8::jsonb, $9::timestamptz)`
)

// Statement pairs a statement with its name, so the live test can prepare every one.
type Statement struct {
	Name string
	SQL  string
}

// Statements is every statement the SCIM store issues (plus the shared ops.user_dim upsert).
var Statements = []Statement{
	{"set_tenant", SQLSCIMSetTenant},
	{"tenant_for_token", SQLTenantForSCIMToken},
	{"device_identity", SQLSCIMDeviceIdentity},
	{"token_by_hash", SQLSCIMTokenByHash},
	{"token", SQLSCIMToken},
	{"insert_token", SQLInsertSCIMToken},
	{"revoke_token", SQLRevokeSCIMToken},
	{"list_tokens", SQLListSCIMTokens},
	{"user", SQLSCIMUser},
	{"user_for_update", SQLSCIMUserForUpdate},
	{"users_by_user_name", SQLSCIMUsersByUserNameHash},
	{"users_by_external_id", SQLSCIMUsersByExternalIDHash},
	{"list_users", SQLSCIMListUsers},
	{"count_users", SQLSCIMCountUsers},
	{"user_ref_owner", SQLSCIMUserRefOwner},
	{"insert_user", SQLInsertSCIMUser},
	{"update_user", SQLUpdateSCIMUser},
	{"put_alias", SQLPutUserRefAlias},
	{"upsert_user_dim", SQLUpsertUserDim},
	{"group", SQLSCIMGroup},
	{"group_for_update", SQLSCIMGroupForUpdate},
	{"groups_by_display_name", SQLSCIMGroupsByDisplayName},
	{"groups_by_external_id", SQLSCIMGroupsByExternalID},
	{"list_groups", SQLSCIMListGroups},
	{"count_groups", SQLSCIMCountGroups},
	{"insert_group", SQLInsertSCIMGroup},
	{"update_group", SQLUpdateSCIMGroup},
	{"delete_group_members", SQLDeleteSCIMGroupMembers},
	{"delete_group", SQLDeleteSCIMGroup},
	{"group_members", SQLSCIMGroupMembers},
	{"add_group_member", SQLAddSCIMGroupMember},
	{"remove_group_member", SQLRemoveSCIMGroupMember},
	{"insert_audit", SQLSCIMInsertAudit},
}

// SQLStore is the database/sql Store.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open handle; the caller owns it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// TenantForToken implements Store.
func (s *SQLStore) TenantForToken(ctx context.Context, tokenHash string) (string, error) {
	var tenant sql.NullString
	if err := s.db.QueryRowContext(ctx, SQLTenantForSCIMToken, tokenHash).Scan(&tenant); err != nil {
		return "", fmt.Errorf("scim: resolve token: %w", err)
	}
	return tenant.String, nil
}

// InTenant implements Store.
func (s *SQLStore) InTenant(ctx context.Context, tenantID string, fn func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, SQLSCIMSetTenant, tenantID); err != nil {
		return fmt.Errorf("scim: set tenant: %w", err)
	}
	if err := fn(&sqlTx{ctx: ctx, tx: tx, tenant: tenantID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("scim: commit: %w", err)
	}
	return nil
}

// isUniqueViolation recognises SQLSTATE 23505, unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type sqlTx struct {
	ctx    context.Context
	tx     *sql.Tx
	tenant string
}

func (x *sqlTx) exec(q string, args ...any) (int64, error) {
	res, err := x.tx.ExecContext(x.ctx, q, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.RowsAffected()
}

func (x *sqlTx) DeviceIdentity() (string, error) {
	var identity string
	err := x.tx.QueryRowContext(x.ctx, SQLSCIMDeviceIdentity, x.tenant).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownTenant
	}
	return identity, err
}

func scanToken(row interface{ Scan(...any) error }) (TokenRow, error) {
	var t TokenRow
	var revoked sql.NullTime
	if err := row.Scan(&t.ID, &t.Hash, &t.Label, &t.CreatedBy, &t.CreatedAt, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenRow{}, ErrNotFound
		}
		return TokenRow{}, err
	}
	if revoked.Valid {
		at := revoked.Time
		t.RevokedAt = &at
	}
	return t, nil
}

func (x *sqlTx) TokenByHash(hash string) (TokenRow, error) {
	return scanToken(x.tx.QueryRowContext(x.ctx, SQLSCIMTokenByHash, x.tenant, hash))
}

func (x *sqlTx) Token(id string) (TokenRow, error) {
	return scanToken(x.tx.QueryRowContext(x.ctx, SQLSCIMToken, x.tenant, id))
}

func (x *sqlTx) InsertToken(t TokenRow) error {
	_, err := x.exec(SQLInsertSCIMToken, t.ID, x.tenant, t.Hash, t.Label, t.CreatedBy, t.CreatedAt)
	return err
}

func (x *sqlTx) RevokeToken(id string, at time.Time) (bool, error) {
	n, err := x.exec(SQLRevokeSCIMToken, x.tenant, id, at)
	return n == 1, err
}

func (x *sqlTx) ListTokens() ([]TokenRow, error) {
	rows, err := x.tx.QueryContext(x.ctx, SQLListSCIMTokens, x.tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanUser(row interface{ Scan(...any) error }) (UserRow, error) {
	var u UserRow
	if err := row.Scan(&u.ID, &u.UserNameHash, &u.ExternalIDHash, &u.ResourceEnc, &u.UserRef, &u.Active, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRow{}, ErrNotFound
		}
		return UserRow{}, err
	}
	return u, nil
}

func (x *sqlTx) users(q string, args ...any) ([]UserRow, error) {
	rows, err := x.tx.QueryContext(x.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (x *sqlTx) User(id string, forUpdate bool) (UserRow, error) {
	q := SQLSCIMUser
	if forUpdate {
		q = SQLSCIMUserForUpdate
	}
	return scanUser(x.tx.QueryRowContext(x.ctx, q, x.tenant, id))
}

func (x *sqlTx) UsersByUserNameHash(hash []byte) ([]UserRow, error) {
	return x.users(SQLSCIMUsersByUserNameHash, x.tenant, hash)
}

func (x *sqlTx) UsersByExternalIDHash(hash []byte) ([]UserRow, error) {
	return x.users(SQLSCIMUsersByExternalIDHash, x.tenant, hash)
}

func (x *sqlTx) ListUsers(offset, limit int) ([]UserRow, error) {
	return x.users(SQLSCIMListUsers, x.tenant, offset, limit)
}

func (x *sqlTx) CountUsers() (int, error) {
	var n int
	err := x.tx.QueryRowContext(x.ctx, SQLSCIMCountUsers, x.tenant).Scan(&n)
	return n, err
}

func (x *sqlTx) UserRefOwner(ref string) (string, error) {
	var id string
	err := x.tx.QueryRowContext(x.ctx, SQLSCIMUserRefOwner, x.tenant, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// bytea passes a nil slice as NULL, which is what an absent externalId hash must be.
func bytea(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (x *sqlTx) InsertUser(u UserRow) error {
	_, err := x.exec(SQLInsertSCIMUser, x.tenant, u.ID, u.UserNameHash, bytea(u.ExternalIDHash), u.ResourceEnc,
		u.UserRef, u.Active, u.CreatedAt, u.UpdatedAt)
	return err
}

func (x *sqlTx) UpdateUser(u UserRow) error {
	n, err := x.exec(SQLUpdateSCIMUser, x.tenant, u.ID, u.UserNameHash, bytea(u.ExternalIDHash), u.ResourceEnc, u.Active, u.UpdatedAt)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (x *sqlTx) PutAlias(alias, canonical string, at time.Time) error {
	_, err := x.exec(SQLPutUserRefAlias, x.tenant, alias, canonical, at)
	return err
}

func (x *sqlTx) UpsertUserDim(row UserDimRow) error {
	_, err := x.exec(SQLUpsertUserDim, x.tenant, row.UserRef, bytea(row.DirectoryObjectIDEnc),
		row.Department, row.DisplayName, row.Status, row.SyncedAt, row.OrgUnit)
	return err
}

func scanGroup(row interface{ Scan(...any) error }) (GroupRow, error) {
	var g GroupRow
	if err := row.Scan(&g.ID, &g.DisplayName, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GroupRow{}, ErrNotFound
		}
		return GroupRow{}, err
	}
	return g, nil
}

func (x *sqlTx) groups(q string, args ...any) ([]GroupRow, error) {
	rows, err := x.tx.QueryContext(x.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupRow
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (x *sqlTx) Group(id string, forUpdate bool) (GroupRow, error) {
	q := SQLSCIMGroup
	if forUpdate {
		q = SQLSCIMGroupForUpdate
	}
	return scanGroup(x.tx.QueryRowContext(x.ctx, q, x.tenant, id))
}

func (x *sqlTx) GroupsByDisplayName(name string) ([]GroupRow, error) {
	return x.groups(SQLSCIMGroupsByDisplayName, x.tenant, name)
}

func (x *sqlTx) GroupsByExternalID(externalID string) ([]GroupRow, error) {
	return x.groups(SQLSCIMGroupsByExternalID, x.tenant, externalID)
}

func (x *sqlTx) ListGroups(offset, limit int) ([]GroupRow, error) {
	return x.groups(SQLSCIMListGroups, x.tenant, offset, limit)
}

func (x *sqlTx) CountGroups() (int, error) {
	var n int
	err := x.tx.QueryRowContext(x.ctx, SQLSCIMCountGroups, x.tenant).Scan(&n)
	return n, err
}

func (x *sqlTx) InsertGroup(g GroupRow) error {
	_, err := x.exec(SQLInsertSCIMGroup, x.tenant, g.ID, g.DisplayName, g.ExternalID, g.CreatedAt, g.UpdatedAt)
	return err
}

func (x *sqlTx) UpdateGroup(g GroupRow) error {
	n, err := x.exec(SQLUpdateSCIMGroup, x.tenant, g.ID, g.DisplayName, g.ExternalID, g.UpdatedAt)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (x *sqlTx) DeleteGroup(id string) error {
	if _, err := x.exec(SQLDeleteSCIMGroupMembers, x.tenant, id); err != nil {
		return err
	}
	_, err := x.exec(SQLDeleteSCIMGroup, x.tenant, id)
	return err
}

func (x *sqlTx) Members(groupID string) ([]string, error) {
	rows, err := x.tx.QueryContext(x.ctx, SQLSCIMGroupMembers, x.tenant, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (x *sqlTx) AddMember(groupID, userID string) (bool, error) {
	n, err := x.exec(SQLAddSCIMGroupMember, x.tenant, groupID, userID)
	return n == 1, err
}

func (x *sqlTx) RemoveMember(groupID, userID string) (bool, error) {
	n, err := x.exec(SQLRemoveSCIMGroupMember, x.tenant, groupID, userID)
	return n == 1, err
}

func (x *sqlTx) Audit(e AuditEntry) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("scim: encode audit detail: %w", err)
	}
	_, err = x.exec(SQLSCIMInsertAudit, x.tenant, e.ActorType, e.ActorID, e.Action, e.ObjectType, e.ObjectID,
		e.SubjectRef, string(detail), e.At)
	return err
}
