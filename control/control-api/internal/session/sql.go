package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The statements over ops.auth_session (contract §1). The lookup is the definer function, because the
// dashboard presents only the opaque id and no tenant is known yet; it returns nothing for a session
// that is revoked or past expires_at, so a caller bug cannot resurrect one. Every write sets the
// session tenant first and names it, so row-level security is the final arbiter of whose session is
// changed.
//
// roles travels as a comma-joined string and is split in SQL: the values are a closed set of
// identifiers with no comma, and a string keeps the statements independent of how a driver encodes a
// text[] parameter.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlCreateSession = `
INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles, user_ref,
                              idp_refresh_token_enc, idp_refreshed_at, created_at, last_seen_at, expires_at)
VALUES ($1::bytea, $2::uuid, $3::uuid, $4::text, $5::text, string_to_array($6::text, ','),
        nullif($7::text, ''), $8::bytea, $9::timestamptz, $10::timestamptz, $10::timestamptz, $11::timestamptz)`

	sqlSessionByHash = `
SELECT session_hash, tenant_id::text, connection_id::text, subject, actor,
       array_to_string(roles, ','), coalesce(user_ref, ''), idp_refresh_token_enc, idp_refreshed_at,
       created_at, last_seen_at, expires_at, revoked_at
  FROM ops.auth_session_by_hash($1::bytea)`

	sqlTouchSession = `
UPDATE ops.auth_session SET last_seen_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND session_hash = $2::bytea AND revoked_at IS NULL`

	// A zero-row result is a session that was already revoked, which is how a repeated revoke audits
	// once rather than twice.
	sqlRevokeSession = `
UPDATE ops.auth_session SET revoked_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND session_hash = $2::bytea AND revoked_at IS NULL`

	sqlSetSessionRefresh = `
UPDATE ops.auth_session SET idp_refresh_token_enc = $3::bytea, idp_refreshed_at = $4::timestamptz
 WHERE tenant_id = $1::uuid AND session_hash = $2::bytea AND revoked_at IS NULL`
)

// Statement pairs a statement with what it is for, so the live test can prepare each by name.
type Statement struct {
	Name, Purpose, SQL string
}

// Statements is every statement the session store issues.
var Statements = []Statement{
	{"set_tenant", "RLS session tenant (transaction-local)", sqlSetTenant},
	{"create_session", "open a session", sqlCreateSession},
	{"session_by_hash", "pre-tenant lookup through the definer function", sqlSessionByHash},
	{"touch_session", "record activity for the idle bound", sqlTouchSession},
	{"revoke_session", "end a session once", sqlRevokeSession},
	{"set_session_refresh", "record a provider refresh", sqlSetSessionRefresh},
}

// SQLStore is the database/sql Store. The caller owns the driver and the handle.
type SQLStore struct{ db *sql.DB }

// NewSQL wraps an open handle.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("session: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("session: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// Create implements Store.
func (s *SQLStore) Create(ctx context.Context, rec Record) error {
	return s.withTenant(ctx, rec.TenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlCreateSession, rec.Hash, rec.TenantID, rec.ConnectionID,
			rec.Subject, rec.Actor, strings.Join(rec.Roles, ","), rec.UserRef, nullBytes(rec.RefreshEnc),
			nullTime(rec.RefreshedAt), rec.CreatedAt, rec.ExpiresAt)
		return err
	})
}

// ByHash implements Store.
func (s *SQLStore) ByHash(ctx context.Context, hash []byte) (Record, error) {
	var rec Record
	var roles string
	var refreshed, revoked sql.NullTime
	err := s.db.QueryRowContext(ctx, sqlSessionByHash, hash).Scan(&rec.Hash, &rec.TenantID, &rec.ConnectionID,
		&rec.Subject, &rec.Actor, &roles, &rec.UserRef, &rec.RefreshEnc, &refreshed, &rec.CreatedAt, &rec.LastSeenAt,
		&rec.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("session: lookup: %w", err)
	}
	if roles != "" {
		rec.Roles = strings.Split(roles, ",")
	}
	if len(rec.RefreshEnc) == 0 {
		rec.RefreshEnc = nil
	}
	if refreshed.Valid {
		t := refreshed.Time
		rec.RefreshedAt = &t
	}
	if revoked.Valid {
		t := revoked.Time
		rec.RevokedAt = &t
	}
	return rec, nil
}

// Touch implements Store.
func (s *SQLStore) Touch(ctx context.Context, tenantID string, hash []byte, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlTouchSession, tenantID, hash, at)
		return err
	})
}

// Revoke implements Store.
func (s *SQLStore) Revoke(ctx context.Context, tenantID string, hash []byte, at time.Time) (bool, error) {
	var n int64
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, sqlRevokeSession, tenantID, hash, at)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n == 1, err
}

// SetRefresh implements Store.
func (s *SQLStore) SetRefresh(ctx context.Context, tenantID string, hash []byte, enc []byte, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlSetSessionRefresh, tenantID, hash, nullBytes(enc), at)
		return err
	})
}
