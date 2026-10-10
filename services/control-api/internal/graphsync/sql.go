package graphsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The statements of the pull's store. Each runs in a transaction that sets the row-level-security
// tenant first, except the tenant listing (a definer function) and the advisory lock.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlTenantIDs = `SELECT t::text FROM ops.tenant_ids() AS t`

	// sqlTarget is the tenant's pull and the Entra directory it reads; no row when the pull is off.
	sqlTarget = `
SELECT ds.tenant_id::text, coalesce(c.entra_tenant_id, '')
  FROM ops.directory_sync ds
  LEFT JOIN ops.identity_connection c
    ON c.tenant_id = ds.tenant_id AND c.provider = 'entra' AND c.status = 'active'
 WHERE ds.tenant_id = $1::uuid`

	// sqlLock takes a session-level advisory lock on the tenant's pull, so two control-api replicas
	// never pull the same tenant at once.
	sqlLock   = `SELECT pg_try_advisory_lock(hashtextextended('graphsync:' || $1::text, 0))`
	sqlUnlock = `SELECT pg_advisory_unlock(hashtextextended('graphsync:' || $1::text, 0))`

	sqlStarted = `UPDATE ops.directory_sync SET last_started_at = $2::timestamptz WHERE tenant_id = $1::uuid`

	sqlFinished = `
UPDATE ops.directory_sync
   SET last_completed_at = $2::timestamptz,
       last_status       = CASE WHEN $3::text = '' THEN 'ok' ELSE 'failed' END,
       last_error        = NULLIF($3::text, ''),
       users_synced      = $4::int,
       groups_synced     = $5::int
 WHERE tenant_id = $1::uuid`

	// sqlImportedGroups lists the groups a team follows whose externalId is a Graph object id.
	sqlImportedGroups = `
SELECT DISTINCT g.scim_id::text, lower(g.external_id)
  FROM ops.team t
  JOIN ops.scim_group g ON g.tenant_id = t.tenant_id AND g.scim_id = t.group_id
 WHERE t.tenant_id = $1::uuid AND t.source = 'group'
   AND g.external_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
 ORDER BY 1`
)

// Statements is every statement the store issues, for the live test.
var Statements = []string{sqlSetTenant, sqlTenantIDs, sqlTarget, sqlLock, sqlUnlock, sqlStarted, sqlFinished, sqlImportedGroups}

// SQLStore is the PostgreSQL Store.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open pool; the caller owns it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Targets implements Store.
func (s *SQLStore) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, sqlTenantIDs)
	if err != nil {
		return nil, fmt.Errorf("graphsync: list tenants: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Target
	for _, id := range ids {
		t, ok, err := s.Target(ctx, id)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// Target implements Store.
func (s *SQLStore) Target(ctx context.Context, tenantID string) (Target, bool, error) {
	var t Target
	found := false
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, sqlTarget, tenantID).Scan(&t.TenantID, &t.EntraTenantID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return t, found, err
}

// Lock implements Store. The lock lives on one connection held for the pass.
func (s *SQLStore) Lock(ctx context.Context, tenantID string) (func(), bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("graphsync: lock connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, sqlLock, tenantID).Scan(&ok); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("graphsync: lock: %w", err)
	}
	if !ok {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		var released bool
		_ = conn.QueryRowContext(context.Background(), sqlUnlock, tenantID).Scan(&released)
		conn.Close()
	}, true, nil
}

// Started implements Store.
func (s *SQLStore) Started(ctx context.Context, tenantID string, at time.Time) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlStarted, tenantID, at)
		return err
	})
}

// Finished implements Store.
func (s *SQLStore) Finished(ctx context.Context, tenantID string, at time.Time, r Result) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlFinished, tenantID, at, r.Error, r.Users, r.Groups)
		return err
	})
}

// ImportedGroups implements Store.
func (s *SQLStore) ImportedGroups(ctx context.Context, tenantID string) ([]ImportedGroup, error) {
	var out []ImportedGroup
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlImportedGroups, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g ImportedGroup
			if err := rows.Scan(&g.ID, &g.ExternalID); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

func (s *SQLStore) inTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("graphsync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("graphsync: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
