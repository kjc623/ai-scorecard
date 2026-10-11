package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The grant path's statements. Each runs in a transaction whose row-level-security tenant is set
// first.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// sqlEventContext reads the event as this device's observation, the submission it belongs to,
	// and the tenant's ceiling and content search setting, in one row. No row means the tenant is
	// unknown; a NULL kind means the device has no such event.
	sqlEventContext = `
SELECT t.ceiling_mode, t.content_search,
       o.kind, o.collection_mode, o.expires_at, s.submission_id::text, s.content_state
  FROM ops.tenant t
  LEFT JOIN ingest.observation o
         ON o.tenant_id = t.tenant_id AND o.event_id = $2::uuid AND o.device_id = $3::uuid
  LEFT JOIN ingest.submission s
         ON s.tenant_id = o.tenant_id AND s.dedup_key = o.dedup_key
 WHERE t.tenant_id = $1::uuid`

	sqlLatestGrant = `
SELECT ` + grantColumns + `
  FROM ops.grant
 WHERE tenant_id = $1::uuid AND event_id = $2::uuid
 ORDER BY requested_at DESC
 LIMIT 1`

	sqlGrant = `
SELECT ` + grantColumns + `
  FROM ops.grant
 WHERE tenant_id = $1::uuid AND grant_id = $2::uuid`

	sqlInsertGrant = `
INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id, decided_at, decision,
                       denial_reason, expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, nullif($4, '')::uuid, $5::uuid, now(), $6,
        nullif($7, ''), $8::timestamptz)`

	sqlAddContentUsage = `
INSERT INTO ops.usage_daily (tenant_id, usage_day, content_bytes_added)
VALUES ($1::uuid, (now() AT TIME ZONE 'utc')::date, $2)
ON CONFLICT (tenant_id, usage_day) DO UPDATE
   SET content_bytes_added = ops.usage_daily.content_bytes_added + EXCLUDED.content_bytes_added`
)

const grantColumns = `grant_id::text, event_id::text, coalesce(submission_id::text, ''), device_id::text, decision,
       coalesce(denial_reason, ''), expires_at, used_at`

// Statements is every statement the grant path issues, for the live test.
var Statements = map[string]string{
	"set_tenant":        sqlSetTenant,
	"event_context":     sqlEventContext,
	"latest_grant":      sqlLatestGrant,
	"grant":             sqlGrant,
	"insert_grant":      sqlInsertGrant,
	"add_content_usage": sqlAddContentUsage,
}

// SQLStore is the PostgreSQL implementation of Store.
type SQLStore struct{ db *sql.DB }

// NewSQL wraps an open pool. The caller owns and closes it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("content: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("content: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// EventContext implements Store.
func (s *SQLStore) EventContext(ctx context.Context, tenantID, deviceID, eventID string) (*EventContext, error) {
	ec := &EventContext{}
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var kind, mode, submission, state sql.NullString
		var expires sql.NullTime
		err := tx.QueryRowContext(ctx, sqlEventContext, tenantID, eventID, deviceID).Scan(
			&ec.CeilingMode, &ec.ContentSearch,
			&kind, &mode, &expires, &submission, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("content: event context: %w", err)
		}
		if !kind.Valid {
			return nil
		}
		ec.Found = true
		ec.Kind, ec.CollectionMode, ec.ExpiresAt = kind.String, mode.String, expires.Time
		ec.SubmissionID, ec.ContentState = submission.String, state.String
		g, err := scanGrant(tx.QueryRowContext(ctx, sqlLatestGrant, tenantID, eventID))
		if err != nil && !errors.Is(err, ErrGrantUnknown) {
			return err
		}
		ec.Grant = g
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ec, nil
}

// Grant implements Store.
func (s *SQLStore) Grant(ctx context.Context, tenantID, grantID string) (*Grant, error) {
	var g *Grant
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		g, err = scanGrant(tx.QueryRowContext(ctx, sqlGrant, tenantID, grantID))
		return err
	})
	return g, err
}

func scanGrant(row *sql.Row) (*Grant, error) {
	var g Grant
	var expires, used sql.NullTime
	err := row.Scan(&g.GrantID, &g.EventID, &g.SubmissionID, &g.DeviceID, &g.Decision, &g.DenialReason, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGrantUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("content: read grant: %w", err)
	}
	g.ExpiresAt = expires.Time
	if used.Valid {
		t := used.Time
		g.UsedAt = &t
	}
	return &g, nil
}

// InsertGrant implements Store.
func (s *SQLStore) InsertGrant(ctx context.Context, tenantID string, g Grant) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var expires any
		if !g.ExpiresAt.IsZero() {
			expires = g.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, sqlInsertGrant, tenantID, g.GrantID, g.EventID, g.SubmissionID, g.DeviceID,
			g.Decision, g.DenialReason, expires); err != nil {
			return fmt.Errorf("content: record grant: %w", err)
		}
		return nil
	})
}

// AddContentUsage implements Store.
func (s *SQLStore) AddContentUsage(ctx context.Context, tenantID string, bytes int64) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, sqlAddContentUsage, tenantID, bytes); err != nil {
			return fmt.Errorf("content: record content usage: %w", err)
		}
		return nil
	})
}
