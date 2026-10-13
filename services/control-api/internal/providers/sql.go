package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// The statements. Each runs in a transaction that sets the row-level-security tenant first, except
// the tenant listing (a definer function) and the advisory lock.
const (
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlTenantIDs = `SELECT t::text FROM ops.tenant_ids() AS t`

	// sqlTargets is the tenant's active connections, or the one $2 names, while its ingest is open,
	// with the Entra directory a Microsoft connection reads.
	sqlTargets = `
SELECT c.connection_id::text, c.provider, coalesce(c.account, ''), c.credential_enc,
       coalesce(c.cursor, ''), coalesce(e.entra_tenant_id, '')
  FROM ops.provider_connection c
  JOIN ops.tenant t ON t.tenant_id = c.tenant_id
  LEFT JOIN ops.identity_connection e
    ON e.tenant_id = c.tenant_id AND e.provider = 'entra' AND e.status = 'active'
 WHERE c.tenant_id = $1::uuid AND c.status = 'active'
   AND t.ingest_enabled AND t.status <> 'closed'
   AND ($2::text = '' OR c.connection_id::text = $2::text)
 ORDER BY c.provider`

	// sqlLock takes a session-level advisory lock on the connection's pull, so two control-api
	// replicas never pull one connection at once.
	sqlLock   = `SELECT pg_try_advisory_lock(hashtextextended('providers:' || $1::text, 0))`
	sqlUnlock = `SELECT pg_advisory_unlock(hashtextextended('providers:' || $1::text, 0))`

	sqlStarted = `
UPDATE ops.provider_connection SET last_started_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`

	sqlFinished = `
UPDATE ops.provider_connection
   SET last_completed_at = $3::timestamptz,
       last_status       = CASE WHEN $4::text = '' THEN 'ok' ELSE 'failed' END,
       last_error        = nullif($4::text, ''),
       records_read      = records_read + $5::bigint,
       records_new       = records_new + $6::bigint,
       cursor            = coalesce(nullif($7::text, ''), cursor)
 WHERE tenant_id = $1::uuid AND connection_id = $2::uuid`

	sqlPolicy = `
SELECT ceiling_mode, coalesce(collection_mode, ceiling_mode), scope_overrides::text, content_search
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	sqlCustomLabels = `
SELECT label, terms::text
  FROM ops.custom_label
 WHERE tenant_id = $1::uuid
 ORDER BY label`

	sqlRecord = `
SELECT event_outcome, coalesce(event_submission_id::text, '')
  FROM ingest.record_provider_event($1::jsonb)`

	sqlContentPending = `
SELECT EXISTS (SELECT 1 FROM ingest.submission
                WHERE tenant_id = $1::uuid AND submission_id = $2::uuid
                  AND connection_id = $3::uuid AND content_state = 'local_only')`

	sqlInsertGrant = `
INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, connection_id, decided_at, decision,
                       expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, nullif($4::text, '')::uuid, $5::uuid, now(), 'granted',
        $6::timestamptz)`

	sqlAddContentUsage = `
INSERT INTO ops.usage_daily (tenant_id, usage_day, content_bytes_added)
VALUES ($1::uuid, (now() AT TIME ZONE 'utc')::date, $2)
ON CONFLICT (tenant_id, usage_day) DO UPDATE
   SET content_bytes_added = ops.usage_daily.content_bytes_added + EXCLUDED.content_bytes_added`

	sqlTenantExists = `SELECT EXISTS (SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid)`

	// sqlProviders is every provider, the tools it files records under, and the tenant's
	// connection to it when there is one.
	sqlProviders = `
SELECT p.provider, p.display_name, p.vendor, p.credential, p.source_url,
       coalesce(p.credential_label, ''), coalesce(p.account_label, ''),
       (SELECT coalesce(jsonb_agg(jsonb_build_object('app', a.app_key, 'name', a.display_name) ORDER BY u.o), '[]')
          FROM unnest(p.apps) WITH ORDINALITY AS u(k, o)
          JOIN ref.app a ON a.app_key = u.k)::text,
       c.connection_id::text, c.account, c.status, c.created_by, c.created_at, c.updated_by, c.updated_at,
       c.last_started_at, c.last_completed_at, c.last_status, c.last_error, c.records_read, c.records_new
  FROM ref.provider p
  LEFT JOIN ops.provider_connection c ON c.tenant_id = $1::uuid AND c.provider = p.provider
 ORDER BY p.display_name`

	sqlEntraConnected = `
SELECT EXISTS (SELECT 1 FROM ops.identity_connection
                WHERE tenant_id = $1::uuid AND provider = 'entra' AND status = 'active')`

	sqlProviderCredential = `SELECT credential FROM ref.provider WHERE provider = $1::text`

	// sqlConnect connects a provider, or reconnects it with a new key. A connection to another
	// account starts over; one to the same account resumes where it stopped.
	sqlConnect = `
INSERT INTO ops.provider_connection (tenant_id, provider, account, credential_enc, created_by, updated_by)
VALUES ($1::uuid, $2::text, nullif($3::text, ''), $4::bytea, $5::text, $5::text)
ON CONFLICT (tenant_id, provider) DO UPDATE
   SET account        = EXCLUDED.account,
       credential_enc = EXCLUDED.credential_enc,
       status         = 'active',
       cursor         = CASE WHEN ops.provider_connection.account IS NOT DISTINCT FROM EXCLUDED.account
                             THEN ops.provider_connection.cursor END,
       updated_by     = EXCLUDED.updated_by,
       updated_at     = now()
RETURNING connection_id::text`

	// sqlDisconnect stops a connection and forgets its key. The row stays: the observations it made
	// name it.
	sqlDisconnect = `
UPDATE ops.provider_connection
   SET status = 'disabled', credential_enc = NULL, updated_by = $3::text, updated_at = now()
 WHERE tenant_id = $1::uuid AND provider = $2::text AND status = 'active'
RETURNING connection_id::text`

	sqlConnection = `
SELECT connection_id::text, status = 'active'
  FROM ops.provider_connection
 WHERE tenant_id = $1::uuid AND provider = $2::text`

	sqlAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, 'user', $2::text, $3::text, $4::text, NULLIF($5::text, ''), $6::jsonb, $7::timestamptz)`
)

// Statements is every statement the store issues, for the live test.
var Statements = []string{
	sqlSetTenant, sqlTenantIDs, sqlTargets, sqlLock, sqlUnlock, sqlStarted, sqlFinished, sqlPolicy,
	sqlCustomLabels, sqlRecord, sqlContentPending, sqlInsertGrant, sqlAddContentUsage, sqlTenantExists,
	sqlProviders, sqlEntraConnected, sqlProviderCredential, sqlConnect, sqlDisconnect, sqlConnection,
	sqlAudit,
}

// SQLStore is the PostgreSQL Store and AdminStore.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open pool; the caller owns it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Targets implements Store.
func (s *SQLStore) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, sqlTenantIDs)
	if err != nil {
		return nil, fmt.Errorf("providers: list tenants: %w", err)
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
		ts, err := s.targets(ctx, id, "")
		if err != nil {
			return out, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// Target implements Store.
func (s *SQLStore) Target(ctx context.Context, tenantID, connectionID string) (Target, bool, error) {
	if !store.IsUUID(connectionID) {
		return Target{}, false, nil
	}
	ts, err := s.targets(ctx, tenantID, connectionID)
	if err != nil || len(ts) == 0 {
		return Target{}, false, err
	}
	return ts[0], true, nil
}

func (s *SQLStore) targets(ctx context.Context, tenantID, connectionID string) ([]Target, error) {
	var out []Target
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlTargets, tenantID, connectionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := Target{Connection: Connection{TenantID: tenantID}}
			if err := rows.Scan(&t.ConnectionID, &t.Provider, &t.Account, &t.SealedKey, &t.Cursor, &t.EntraTenantID); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Lock implements Store. The lock lives on one connection held for the pass.
func (s *SQLStore) Lock(ctx context.Context, connectionID string) (func(), bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("providers: lock connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, sqlLock, connectionID).Scan(&ok); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("providers: lock: %w", err)
	}
	if !ok {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		var released bool
		_ = conn.QueryRowContext(context.Background(), sqlUnlock, connectionID).Scan(&released)
		conn.Close()
	}, true, nil
}

// Started implements Store.
func (s *SQLStore) Started(ctx context.Context, tenantID, connectionID string, at time.Time) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlStarted, tenantID, connectionID, at)
		return err
	})
}

// Finished implements Store.
func (s *SQLStore) Finished(ctx context.Context, tenantID, connectionID string, at time.Time, r Result) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlFinished, tenantID, connectionID, at, r.Error, int64(r.Read), int64(r.New), r.Cursor)
		return err
	})
}

// Policy implements Store.
func (s *SQLStore) Policy(ctx context.Context, tenantID string) (Policy, error) {
	var p Policy
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var overrides string
		if err := tx.QueryRowContext(ctx, sqlPolicy, tenantID).Scan(&p.CeilingMode, &p.Mode, &overrides, &p.ContentSearch); err != nil {
			return fmt.Errorf("providers: read tenant: %w", err)
		}
		if err := json.Unmarshal([]byte(overrides), &p.ScopeOverrides); err != nil {
			return fmt.Errorf("providers: scope overrides: %w", err)
		}
		rows, err := tx.QueryContext(ctx, sqlCustomLabels, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l store.CustomLabel
			var raw string
			if err := rows.Scan(&l.Label, &raw); err != nil {
				return err
			}
			var terms struct {
				Words          []string `json:"words"`
				CustomerNames  []string `json:"customer_names"`
				FileExtensions []string `json:"file_extensions"`
				FileNames      []string `json:"file_names"`
			}
			if err := json.Unmarshal([]byte(raw), &terms); err != nil {
				return fmt.Errorf("providers: custom label %s: %w", l.Label, err)
			}
			l.Words, l.CustomerNames, l.FileExtensions, l.FileNames = terms.Words, terms.CustomerNames, terms.FileExtensions, terms.FileNames
			p.CustomLabels = append(p.CustomLabels, l)
		}
		return rows.Err()
	})
	return p, err
}

// RecordEvent implements RecordStore.
func (s *SQLStore) RecordEvent(ctx context.Context, tenantID string, envelope map[string]any) (string, string, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", "", fmt.Errorf("providers: envelope: %w", err)
	}
	var outcome, submission string
	err = s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, sqlRecord, string(raw)).Scan(&outcome, &submission)
	})
	return outcome, submission, err
}

// ContentPending implements RecordStore.
func (s *SQLStore) ContentPending(ctx context.Context, tenantID, submissionID, connectionID string) (bool, error) {
	if !store.IsUUID(submissionID) {
		return false, nil
	}
	var pending bool
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, sqlContentPending, tenantID, submissionID, connectionID).Scan(&pending)
	})
	return pending, err
}

// GrantContent implements RecordStore.
func (s *SQLStore) GrantContent(ctx context.Context, tenantID string, g Grant) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlInsertGrant, tenantID, g.GrantID, g.EventID, g.SubmissionID, g.ConnectionID,
			g.ExpiresAt.UTC().Format(time.RFC3339Nano))
		return err
	})
}

// AddContentUsage implements RecordStore.
func (s *SQLStore) AddContentUsage(ctx context.Context, tenantID string, bytes int64) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, sqlAddContentUsage, tenantID, bytes)
		return err
	})
}

// Providers implements AdminStore.
func (s *SQLStore) Providers(ctx context.Context, tenantID string) ([]Provider, error) {
	var out []Provider
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := s.tenantExists(ctx, tx, tenantID); err != nil {
			return err
		}
		var err error
		out, err = scanProviders(ctx, tx, tenantID)
		return err
	})
	return out, err
}

func scanProviders(ctx context.Context, tx *sql.Tx, tenantID string) ([]Provider, error) {
	rows, err := tx.QueryContext(ctx, sqlProviders, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		var tools string
		var id, account, status, createdBy, updatedBy, lastStatus, lastError sql.NullString
		var createdAt, updatedAt, lastStarted, lastCompleted sql.NullTime
		var read, added sql.NullInt64
		if err := rows.Scan(&p.Provider, &p.DisplayName, &p.Vendor, &p.Credential, &p.SourceURL,
			&p.CredentialLabel, &p.AccountLabel, &tools,
			&id, &account, &status, &createdBy, &createdAt, &updatedBy, &updatedAt,
			&lastStarted, &lastCompleted, &lastStatus, &lastError, &read, &added); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tools), &p.Tools); err != nil {
			return nil, fmt.Errorf("providers: tools of %s: %w", p.Provider, err)
		}
		if id.Valid {
			p.Connection = &ConnectionState{
				ConnectionID: id.String, Account: account.String, Status: status.String,
				CreatedBy: createdBy.String, CreatedAt: createdAt.Time, UpdatedBy: updatedBy.String, UpdatedAt: updatedAt.Time,
				LastStartedAt: timePtr(lastStarted), LastCompletedAt: timePtr(lastCompleted),
				LastStatus: lastStatus.String, LastError: lastError.String, RecordsRead: read.Int64, RecordsNew: added.Int64,
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// EntraConnected implements AdminStore.
func (s *SQLStore) EntraConnected(ctx context.Context, tenantID string) (bool, error) {
	var ok bool
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, sqlEntraConnected, tenantID).Scan(&ok)
	})
	return ok, err
}

// Connect implements AdminStore.
func (s *SQLStore) Connect(ctx context.Context, tenantID, provider, account string, sealedKey []byte, a Audit) (string, error) {
	var id string
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := s.tenantExists(ctx, tx, tenantID); err != nil {
			return err
		}
		var credential string
		err := tx.QueryRowContext(ctx, sqlProviderCredential, provider).Scan(&credential)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if credential == "entra_app" {
			var ok bool
			if err := tx.QueryRowContext(ctx, sqlEntraConnected, tenantID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrNoEntra
			}
		}
		if err := tx.QueryRowContext(ctx, sqlConnect, tenantID, provider, account, sealedKey, a.Actor).Scan(&id); err != nil {
			return err
		}
		a.ObjectID = id
		return audit(ctx, tx, tenantID, a)
	})
	return id, err
}

// Disconnect implements AdminStore.
func (s *SQLStore) Disconnect(ctx context.Context, tenantID, provider string, a Audit) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := s.tenantExists(ctx, tx, tenantID); err != nil {
			return err
		}
		var id string
		err := tx.QueryRowContext(ctx, sqlDisconnect, tenantID, provider, a.Actor).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		a.ObjectID = id
		return audit(ctx, tx, tenantID, a)
	})
}

// Connection implements AdminStore.
func (s *SQLStore) Connection(ctx context.Context, tenantID, provider string) (string, bool, error) {
	var id string
	var active bool
	err := s.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := s.tenantExists(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, sqlConnection, tenantID, provider).Scan(&id, &active)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, active, err
}

// Audit implements AdminStore.
func (s *SQLStore) Audit(ctx context.Context, tenantID string, a Audit) error {
	return s.inTenant(ctx, tenantID, func(tx *sql.Tx) error { return audit(ctx, tx, tenantID, a) })
}

func audit(ctx context.Context, tx *sql.Tx, tenantID string, a Audit) error {
	detail, err := json.Marshal(a.Detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, sqlAudit, tenantID, a.Actor, a.Action, a.ObjectType, a.ObjectID, string(detail), a.At)
	return err
}

func (s *SQLStore) tenantExists(ctx context.Context, tx *sql.Tx, tenantID string) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, sqlTenantExists, tenantID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnknownTenant
	}
	return nil
}

func (s *SQLStore) inTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("providers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("providers: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
