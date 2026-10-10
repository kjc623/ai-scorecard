package directoryadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every statement the admin store issues. Each runs in a transaction whose row-level-security
// tenant was set first, and every write commits with its audit row.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlTenantExists = `SELECT EXISTS (SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid)`

	sqlSyncState = `
SELECT enabled_by, enabled_at, last_started_at, last_completed_at, coalesce(last_status, ''),
       coalesce(last_error, ''), users_synced, groups_synced
  FROM ops.directory_sync WHERE tenant_id = $1::uuid`

	sqlEntraConnected = `
SELECT EXISTS (SELECT 1 FROM ops.identity_connection
                WHERE tenant_id = $1::uuid AND provider = 'entra' AND status = 'active')`

	sqlEntraTenant = `
SELECT coalesce((SELECT entra_tenant_id FROM ops.identity_connection
                  WHERE tenant_id = $1::uuid AND provider = 'entra' AND status = 'active'), '')`

	sqlPeopleCounts = `
SELECT (SELECT count(*) FROM ops.scim_user WHERE tenant_id = $1::uuid AND active),
       (SELECT count(*) FROM ops.scim_group WHERE tenant_id = $1::uuid)`

	sqlEnableSync = `
INSERT INTO ops.directory_sync (tenant_id, enabled_by, enabled_at)
VALUES ($1::uuid, $2::text, $3::timestamptz)
ON CONFLICT (tenant_id) DO NOTHING`

	sqlDisableSync = `DELETE FROM ops.directory_sync WHERE tenant_id = $1::uuid`

	// sqlTeams lists the tenant's teams with their current member count and, for a group team, the
	// group's name.
	sqlTeams = `
SELECT t.team_id::text, t.name, t.source, coalesce(t.group_id::text, ''), coalesce(g.display_name, ''),
       coalesce(g.external_id, ''), coalesce(t.match_value, ''), t.created_by, t.created_at,
       (SELECT count(*) FROM ops.v_team_member m WHERE m.tenant_id = t.tenant_id AND m.team_id = t.team_id)
  FROM ops.team t
  LEFT JOIN ops.scim_group g ON g.tenant_id = t.tenant_id AND g.scim_id = t.group_id
 WHERE t.tenant_id = $1::uuid
 ORDER BY lower(t.name), t.team_id`

	sqlTeamSource = `SELECT source FROM ops.team WHERE tenant_id = $1::uuid AND team_id = $2::uuid`

	sqlInsertTeam = `
INSERT INTO ops.team (tenant_id, team_id, name, source, group_id, match_value, created_by, created_at)
VALUES ($1::uuid, $2::uuid, $3::text, $4::text, NULLIF($5::text, '')::uuid, NULLIF($6::text, ''),
        $7::text, $8::timestamptz)`

	sqlRenameTeam = `UPDATE ops.team SET name = $3::text WHERE tenant_id = $1::uuid AND team_id = $2::uuid`

	sqlDeleteTeam = `DELETE FROM ops.team WHERE tenant_id = $1::uuid AND team_id = $2::uuid`

	sqlAddMember = `
INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by, added_at)
VALUES ($1::uuid, $2::uuid, $3::text, $4::text, $5::timestamptz)
ON CONFLICT DO NOTHING`

	sqlRemoveMember = `DELETE FROM ops.team_member WHERE tenant_id = $1::uuid AND team_id = $2::uuid AND user_ref = $3::text`

	// sqlTeamMembers names each member the way the people list does: the directory's display name,
	// else the account name their device last reported, else nothing (the ref stands in).
	sqlTeamMembers = `
SELECT m.user_ref,
       coalesce(ud.display_name,
                (SELECT d.last_subject_name FROM ops.device d
                  WHERE d.tenant_id = m.tenant_id AND d.last_user_ref = m.user_ref
                    AND d.last_subject_name IS NOT NULL
                  ORDER BY d.last_seen_at DESC NULLS LAST LIMIT 1), ''),
       coalesce(ud.department, '')
  FROM ops.v_team_member m
  LEFT JOIN ops.user_dim ud ON ud.tenant_id = m.tenant_id AND ud.user_ref = m.user_ref
 WHERE m.tenant_id = $1::uuid AND m.team_id = $2::uuid
 ORDER BY 2, 1
 LIMIT $3::int`

	sqlDepartments = `
SELECT department, count(*) FROM ops.user_dim
 WHERE tenant_id = $1::uuid AND department IS NOT NULL AND status <> 'inactive'
 GROUP BY department ORDER BY lower(department) LIMIT $2::int`

	sqlOrgUnits = `
SELECT org_unit, count(*) FROM ops.user_dim
 WHERE tenant_id = $1::uuid AND org_unit IS NOT NULL AND status <> 'inactive'
 GROUP BY org_unit ORDER BY lower(org_unit) LIMIT $2::int`

	sqlGroups = `
SELECT g.scim_id::text, g.display_name, coalesce(g.external_id, ''),
       (SELECT count(*) FROM ops.scim_group_member m WHERE m.tenant_id = g.tenant_id AND m.group_id = g.scim_id)
  FROM ops.scim_group g
 WHERE g.tenant_id = $1::uuid
 ORDER BY lower(g.display_name), g.scim_id
 LIMIT $2::int`

	sqlGroupByExternalID = `
SELECT scim_id::text FROM ops.scim_group
 WHERE tenant_id = $1::uuid AND lower(external_id) = lower($2::text)
 ORDER BY created_at LIMIT 1`

	sqlGroupExists = `SELECT EXISTS (SELECT 1 FROM ops.scim_group WHERE tenant_id = $1::uuid AND scim_id = $2::uuid)`

	sqlAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, 'user', $2::text, $3::text, $4::text, NULLIF($5::text, ''), $6::jsonb, $7::timestamptz)`
)

// Statements is every statement the store issues, for the live test.
var Statements = []string{
	sqlSetTenant, sqlTenantExists, sqlSyncState, sqlEntraConnected, sqlEntraTenant, sqlPeopleCounts,
	sqlEnableSync, sqlDisableSync, sqlTeams, sqlTeamSource, sqlInsertTeam, sqlRenameTeam, sqlDeleteTeam,
	sqlAddMember, sqlRemoveMember, sqlTeamMembers, sqlDepartments, sqlOrgUnits, sqlGroups,
	sqlGroupByExternalID, sqlGroupExists, sqlAudit,
}

// SQLStore is the PostgreSQL Store.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open pool; the caller owns it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Status implements Store.
func (s *SQLStore) Status(ctx context.Context, tenant string) (Status, error) {
	var st Status
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, sqlEntraConnected, tenant).Scan(&st.EntraConnected); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, sqlPeopleCounts, tenant).Scan(&st.People, &st.Groups); err != nil {
			return err
		}
		var sync SyncState
		var started, completed sql.NullTime
		var users, groups sql.NullInt64
		err := tx.QueryRowContext(ctx, sqlSyncState, tenant).Scan(&sync.EnabledBy, &sync.EnabledAt, &started,
			&completed, &sync.LastStatus, &sync.LastError, &users, &groups)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		sync.LastStartedAt, sync.LastCompletedAt = timePtr(started), timePtr(completed)
		sync.UsersSynced, sync.GroupsSynced = intPtr(users), intPtr(groups)
		st.Sync = &sync
		return nil
	})
	return st, err
}

// EntraTenant implements Store.
func (s *SQLStore) EntraTenant(ctx context.Context, tenant string) (string, error) {
	var tid string
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, sqlEntraTenant, tenant).Scan(&tid)
	})
	return tid, err
}

// SetSync implements Store.
func (s *SQLStore) SetSync(ctx context.Context, tenant string, on bool, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var n int64
		var err error
		if on {
			var ok bool
			if err := tx.QueryRowContext(ctx, sqlEntraConnected, tenant).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrNoEntra
			}
			n, err = exec(ctx, tx, sqlEnableSync, tenant, a.Actor, a.At)
		} else {
			n, err = exec(ctx, tx, sqlDisableSync, tenant)
		}
		if err != nil || n == 0 {
			// Already in the asked-for state: nothing to record.
			return err
		}
		return audit(ctx, tx, tenant, a)
	})
}

// Teams implements Store.
func (s *SQLStore) Teams(ctx context.Context, tenant string) ([]Team, error) {
	out := []Team{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlTeams, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Team
			if err := rows.Scan(&t.ID, &t.Name, &t.Source, &t.GroupID, &t.GroupName, &t.GroupExternalID,
				&t.MatchValue, &t.CreatedBy, &t.CreatedAt, &t.Members); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// CreateTeam implements Store.
func (s *SQLStore) CreateTeam(ctx context.Context, tenant string, t Team, members []string, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		if t.Source == SourceGroup {
			var ok bool
			if err := tx.QueryRowContext(ctx, sqlGroupExists, tenant, t.GroupID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrNotFound
			}
		}
		if _, err := exec(ctx, tx, sqlInsertTeam, tenant, t.ID, t.Name, t.Source, t.GroupID, t.MatchValue, t.CreatedBy, t.CreatedAt); err != nil {
			return err
		}
		for _, ref := range members {
			if _, err := exec(ctx, tx, sqlAddMember, tenant, t.ID, ref, a.Actor, a.At); err != nil {
				return err
			}
		}
		return audit(ctx, tx, tenant, a)
	})
}

// RenameTeam implements Store.
func (s *SQLStore) RenameTeam(ctx context.Context, tenant, id, name string, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		n, err := exec(ctx, tx, sqlRenameTeam, tenant, id, name)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, tenant, a)
	})
}

// DeleteTeam implements Store.
func (s *SQLStore) DeleteTeam(ctx context.Context, tenant, id string, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		n, err := exec(ctx, tx, sqlDeleteTeam, tenant, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, tenant, a)
	})
}

// ChangeMembers implements Store.
func (s *SQLStore) ChangeMembers(ctx context.Context, tenant, id string, add, remove []string, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var source string
		err := tx.QueryRowContext(ctx, sqlTeamSource, tenant, id).Scan(&source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if source != SourceConsole {
			return ErrNotConsole
		}
		for _, ref := range add {
			if _, err := exec(ctx, tx, sqlAddMember, tenant, id, ref, a.Actor, a.At); err != nil {
				return err
			}
		}
		for _, ref := range remove {
			if _, err := exec(ctx, tx, sqlRemoveMember, tenant, id, ref); err != nil {
				return err
			}
		}
		return audit(ctx, tx, tenant, a)
	})
}

// TeamMembers implements Store.
func (s *SQLStore) TeamMembers(ctx context.Context, tenant, id string, limit int) ([]Member, error) {
	out := []Member{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var source string
		err := tx.QueryRowContext(ctx, sqlTeamSource, tenant, id).Scan(&source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, sqlTeamMembers, tenant, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.UserRef, &m.Name, &m.Department); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// DirectoryValues implements Store.
func (s *SQLStore) DirectoryValues(ctx context.Context, tenant, kind string, limit int) ([]Value, error) {
	q := sqlDepartments
	if kind == SourceOrgUnit {
		q = sqlOrgUnits
	}
	out := []Value{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q, tenant, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Value
			if err := rows.Scan(&v.Value, &v.People); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// Groups implements Store.
func (s *SQLStore) Groups(ctx context.Context, tenant string, limit int) ([]ProvisionedGroup, error) {
	out := []ProvisionedGroup{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlGroups, tenant, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g ProvisionedGroup
			if err := rows.Scan(&g.ID, &g.Name, &g.ExternalID, &g.Members); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

// GroupByExternalID implements Store.
func (s *SQLStore) GroupByExternalID(ctx context.Context, tenant, externalID string) (string, bool, error) {
	var id string
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, sqlGroupByExternalID, tenant, externalID).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// Audit implements Store, for an action whose write another component made.
func (s *SQLStore) Audit(ctx context.Context, tenant string, a Audit) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error { return audit(ctx, tx, tenant, a) })
}

func audit(ctx context.Context, tx *sql.Tx, tenant string, a Audit) error {
	detail := a.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, sqlAudit, tenant, a.Actor, a.Action, a.ObjectType, a.ObjectID, string(raw), a.At)
	return err
}

func (s *SQLStore) inTenant(ctx context.Context, tenant string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("directoryadmin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenant); err != nil {
		return fmt.Errorf("directoryadmin: set tenant: %w", err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, sqlTenantExists, tenant).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrUnknownTenant
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if isUnique(err) {
			return ErrNameTaken
		}
		return fmt.Errorf("directoryadmin: commit: %w", err)
	}
	return nil
}

func exec(ctx context.Context, tx *sql.Tx, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		if isUnique(err) {
			return 0, ErrNameTaken
		}
		return 0, err
	}
	return res.RowsAffected()
}

// isUnique recognises SQLSTATE 23505; the only unique key a write here can break is a team's name.
func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func intPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
