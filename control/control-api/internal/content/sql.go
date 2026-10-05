package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The statements of the grant path. Like the rest of control-api's SQL they are constants, they are
// tenant-scoped, and they run in a transaction whose session tenant is set first.
//
// As deployed, three of them need database grants the schema does not yet give `sac_control`: a read
// of ingest.observation (the §5.5 "an observation of that device" check), an update of
// ingest.submission.content_state, and a write of ops.usage_daily. The lab connects as the database
// owner, so they run there; the grants are the remaining work for a least-privilege deployment.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// sqlEventContext reads the event as this device's observation, the submission it belongs to,
	// and the tenant's ceiling and budget, in one row. No row means the tenant is unknown; a NULL
	// kind means the device has no such event.
	sqlEventContext = `
SELECT t.ceiling_mode, t.content_budget_bytes_per_day,
       coalesce((SELECT u.content_bytes_added FROM ops.usage_daily u
                  WHERE u.tenant_id = t.tenant_id AND u.usage_day = (now() AT TIME ZONE 'utc')::date), 0),
       o.kind, o.prompt_kind, o.collection_mode, o.expires_at, s.submission_id, s.content_state
  FROM ops.tenant t
  LEFT JOIN ingest.observation o
         ON o.tenant_id = t.tenant_id AND o.event_id = $2::uuid AND o.device_id = $3::uuid
  LEFT JOIN ingest.submission s
         ON s.tenant_id = o.tenant_id AND s.dedup_key = o.dedup_key
 WHERE t.tenant_id = $1::uuid`

	// sqlLatestGrant is the most recent decision for the event.
	sqlLatestGrant = `
SELECT grant_id, event_id, coalesce(submission_id::text, ''), device_id, decision,
       coalesce(denial_reason, ''), coalesce(object_id::text, ''), upload_expires_at
  FROM ops.grant
 WHERE tenant_id = $1::uuid AND event_id = $2::uuid
 ORDER BY requested_at DESC
 LIMIT 1`

	sqlGrant = `
SELECT grant_id, event_id, coalesce(submission_id::text, ''), device_id, decision,
       coalesce(denial_reason, ''), coalesce(object_id::text, ''), upload_expires_at
  FROM ops.grant
 WHERE tenant_id = $1::uuid AND grant_id = $2::uuid`

	sqlInsertGrant = `
INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id, decided_at, decision,
                       denial_reason, object_id, upload_expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, nullif($4, '')::uuid, $5::uuid, now(), $6,
        nullif($7, ''), nullif($8, '')::uuid, $9::timestamptz)`

	// sqlMarkLocalOnly records the one thing a grant request proves about content: the device
	// holds it. Until a verified upload, that is where it is (docs/02 §3, ADR 0017).
	sqlMarkLocalOnly = `
UPDATE ingest.submission SET content_state = 'local_only'
 WHERE tenant_id = $1::uuid AND submission_id = nullif($2, '')::uuid AND content_state = 'not_captured'`

	sqlVoidGrant = `
UPDATE ops.grant SET decision = 'voided', decided_at = now()
 WHERE tenant_id = $1::uuid AND grant_id = $2::uuid AND decision = 'granted'`

	sqlMarkUploaded = `
UPDATE ingest.submission SET content_state = 'uploaded'
 WHERE tenant_id = $1::uuid AND submission_id = $2::uuid AND content_state IN ('not_captured', 'local_only')`

	sqlAddContentUsage = `
INSERT INTO ops.usage_daily (tenant_id, usage_day, content_bytes_added)
VALUES ($1::uuid, (now() AT TIME ZONE 'utc')::date, $2)
ON CONFLICT (tenant_id, usage_day) DO UPDATE
   SET content_bytes_added = ops.usage_daily.content_bytes_added + EXCLUDED.content_bytes_added`
)

// SQLStore is the database/sql implementation of Store.
type SQLStore struct{ db *sql.DB }

// NewSQL wraps an open handle. The caller owns the driver, as with the enrolment store.
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
		var kind, promptKind, mode, submission, state sql.NullString
		var expires sql.NullTime
		err := tx.QueryRowContext(ctx, sqlEventContext, tenantID, eventID, deviceID).Scan(
			&ec.CeilingMode, &ec.BudgetBytesPerDay, &ec.BytesAddedToday,
			&kind, &promptKind, &mode, &expires, &submission, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // unknown tenant: the event is not found either
		}
		if err != nil {
			return fmt.Errorf("content: event context: %w", err)
		}
		if !kind.Valid {
			return nil
		}
		ec.Found = true
		ec.Kind, ec.CollectionMode, ec.ExpiresAt = kind.String, mode.String, expires.Time
		ec.PromptKind, ec.SubmissionID, ec.ContentState = promptKind.String, submission.String, state.String

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
	var expires sql.NullTime
	err := row.Scan(&g.GrantID, &g.EventID, &g.SubmissionID, &g.DeviceID, &g.Decision, &g.DenialReason, &g.ObjectID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGrantUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("content: reading grant: %w", err)
	}
	g.UploadExpiresAt = expires.Time
	return &g, nil
}

// InsertGrant implements Store.
func (s *SQLStore) InsertGrant(ctx context.Context, tenantID string, g Grant) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var expires any
		if !g.UploadExpiresAt.IsZero() {
			expires = g.UploadExpiresAt.Format(time.RFC3339Nano)
		}
		_, err := tx.ExecContext(ctx, sqlInsertGrant, tenantID, g.GrantID, g.EventID, g.SubmissionID, g.DeviceID,
			g.Decision, g.DenialReason, g.ObjectID, expires)
		if err != nil {
			return fmt.Errorf("content: recording grant: %w", err)
		}
		if _, err := tx.ExecContext(ctx, sqlMarkLocalOnly, tenantID, g.SubmissionID); err != nil {
			return fmt.Errorf("content: marking submission local_only: %w", err)
		}
		return nil
	})
}

// VoidGrant implements Store.
func (s *SQLStore) VoidGrant(ctx context.Context, tenantID, grantID string) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlVoidGrant, tenantID, grantID)
		return err
	})
}

// RecordUpload implements Store: the content-state transition and the usage counter move together.
func (s *SQLStore) RecordUpload(ctx context.Context, tenantID, submissionID string, bytes int64) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if submissionID != "" {
			if _, err := tx.ExecContext(ctx, sqlMarkUploaded, tenantID, submissionID); err != nil {
				return fmt.Errorf("content: marking submission uploaded: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, sqlAddContentUsage, tenantID, bytes); err != nil {
			return fmt.Errorf("content: recording content usage: %w", err)
		}
		return nil
	})
}
