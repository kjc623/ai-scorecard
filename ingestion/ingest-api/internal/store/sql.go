package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// =====================================================================================
// Every SQL statement this service issues, in one file.
// =====================================================================================
//
// The rule this file exists to hold: the ingest write path has exactly one place where SQL
// appears, so the integration seam with db/schema.sql is reviewable in one screen and the
// statements can be executed against the real schema by a test without going through a Go driver.
// The integration test in sql_integration_test.go PREPAREs and EXECUTEs these exact strings
// (placeholders and all) against the live PostgreSQL 17 server, so what is verified is the text
// below, not a paraphrase of it.
//
// The write itself is NOT re-implemented here. ingest.record_event(p_envelope jsonb,
// p_received_at timestamptz) owns idempotency, the dedup tie-break and retention; §6 is explicit
// that a unique constraint decides receipt rather than a prior SELECT. The statements around it
// only read back what the function decided, and quarantine the events that never reached it.

const (
	// SQLSetTenant sets the row-level-security session tenant. ops.current_tenant() reads this
	// setting; a session that has not set it reads zero rows, which is the fail-closed behaviour
	// C32 asks for. `true` makes it transaction-local, so a pooled connection cannot leak one
	// tenant's session into the next request.
	SQLSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// SQLPrincipalStatus resolves the authenticated principal. $1 tenant, $2 device, $3 credential.
	// It is issued twice per batch: at admission, and again inside the write transaction before
	// commit, which is what §2.3 requires.
	SQLPrincipalStatus = `
SELECT t.status,
       t.ingest_enabled,
       t.residency_region,
       d.device_id IS NOT NULL AS device_known,
       d.revoked_at           AS device_revoked_at,
       c.credential_id IS NOT NULL AS credential_known,
       c.expires_at,
       c.revoked_at           AS credential_revoked_at,
       c.credential_type,
       c.public_key_thumbprint
  FROM ops.tenant t
  LEFT JOIN ops.device d
         ON d.tenant_id = t.tenant_id AND d.device_id = $2::uuid
  LEFT JOIN ops.device_credential c
         ON c.tenant_id = d.tenant_id AND c.device_id = d.device_id AND c.credential_id = $3::uuid
 WHERE t.tenant_id = $1::uuid`

	// SQLDPoPReplayInsert records one RFC 9449 proof jti. ON CONFLICT DO NOTHING makes a replay a
	// no-op the caller detects from rows-affected == 0, which is the mechanism the table's own COMMENT
	// names. expires_at is floored at now()+1s so a caller clock that is behind the database cannot
	// insert a row that violates dpop_replay_expiry_after_seen; the window stays bounded in the
	// future by construction rather than by trust in the caller.
	SQLDPoPReplayInsert = `INSERT INTO ops.dpop_replay (tenant_id, jti, seen_at, expires_at)
VALUES ($1::uuid, $2::text, now(), GREATEST($3::timestamptz, now() + interval '1 second'))
ON CONFLICT (tenant_id, jti) DO NOTHING`

	// SQLDPoPReplaySweep deletes the tenant's expired rows on the way past, so the table stays a
	// bounded window without a separate job. A scheduled sweeper is welcome to do it too; this is
	// cheap because the table is tiny and the index leads with tenant_id.
	SQLDPoPReplaySweep = `DELETE FROM ops.dpop_replay
 WHERE tenant_id = $1::uuid AND expires_at <= now()`

	// SQLRouteFidelity reads the stored route ranking. §4.4: ranks live in ref.route_fidelity,
	// "not compiled into services".
	SQLRouteFidelity = `SELECT source, fidelity_rank, yields_content FROM ref.route_fidelity`

	// SQLRecordEvent is THE integration seam of the ingest write path. $1 is the envelope exactly
	// as the device sent it (the function is kept in step with
	// contracts/event-envelope.schema.json by taking jsonb rather than a parameter list), $2 is the
	// batch's single receive time. It returns event_outcome ('inserted' | 'merged' | 'duplicate')
	// and the logical submission's id.
	SQLRecordEvent = `SELECT event_outcome, event_submission_id
  FROM ingest.record_event($1::jsonb, $2::timestamptz)`

	// SQLFirstReceivedAt reads the first receipt of an event that has just reported as a duplicate.
	// §5.3: received_at is stamped once per request; a retry is not a second receipt, so the stored
	// row keeps its first value and that is what the device is told.
	SQLFirstReceivedAt = `SELECT received_at
  FROM ingest.observation
 WHERE tenant_id = $1::uuid AND event_id = $2::uuid`

	// SQLSubmissionWinner reads back the store's tie-break decision, so the response can report
	// `won_fields` from what the store did rather than from a second opinion computed here. §4.4
	// makes the winning route and its fidelity a property of the row.
	SQLSubmissionWinner = `SELECT winning_source, winning_fidelity
  FROM ingest.submission
 WHERE tenant_id = $1::uuid AND submission_id = $2::uuid`

	// SQLInsertRejected quarantines one validation failure (§7). The TTL is read from
	// ref.retention_class rather than hard-coded, because the quarantine window is a policy
	// setting; the COALESCE is the fallback when the class row is missing. envelope_redacted is
	// already content-stripped by the caller: the CHECK constraints on this table refuse
	// content_digest, content_excerpt, content_bytes, prompt_text and attachments outright, which
	// is stricter than §7's wording and is the rule that actually has to be satisfied.
	SQLInsertRejected = `INSERT INTO ingest.rejected (
    tenant_id, device_id, received_at, reason_code, detail, field_presence, envelope_redacted, expires_at)
VALUES ($1::uuid, $2::uuid, $3::timestamptz, $4::text, $5::jsonb, $6::jsonb, $7::jsonb,
        $3::timestamptz + make_interval(days => COALESCE(
            (SELECT rc.default_ttl_days FROM ref.retention_class rc
              WHERE rc.retention_class = 'quarantine'), 30)))`
)

// Statement pairs a statement with what it is for, so the set can be listed and executed by name.
type Statement struct {
	Name    string
	Purpose string
	SQL     string
}

// Statements is every statement the service issues, in execution order within one batch.
var Statements = []Statement{
	{Name: "set_tenant", Purpose: "RLS session tenant (transaction-local)", SQL: SQLSetTenant},
	{Name: "principal_status", Purpose: "§2.3 admission and in-transaction credential check", SQL: SQLPrincipalStatus},
	{Name: "dpop_replay_insert", Purpose: "RFC 9449 jti one-shot memory (ADR 0020 §4)", SQL: SQLDPoPReplayInsert},
	{Name: "dpop_replay_sweep", Purpose: "forget expired RFC 9449 jti rows", SQL: SQLDPoPReplaySweep},
	{Name: "route_fidelity", Purpose: "§4.4 stored route ranking", SQL: SQLRouteFidelity},
	{Name: "record_event", Purpose: "§6 THE write: idempotency + dedup tie-break + retention", SQL: SQLRecordEvent},
	{Name: "first_received_at", Purpose: "§5.3 duplicate reports the first receipt", SQL: SQLFirstReceivedAt},
	{Name: "submission_winner", Purpose: "§4.4 read back the winning route for won_fields", SQL: SQLSubmissionWinner},
	{Name: "insert_rejected", Purpose: "§7 quarantine a validation failure in the same transaction", SQL: SQLInsertRejected},
}

// SQLStore is the database/sql implementation.
//
// It is written against the real schema and every statement it issues is executed against a live
// PostgreSQL 17 by sql_integration_test.go. What that test does not exercise is the database/sql
// plumbing itself: this repository has no PostgreSQL wire driver available offline (GOPROXY=off,
// Go standard library only), so NewSQL takes an already-opened *sql.DB and the driver must be
// registered by the binary that embeds this service.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open database handle. The caller owns the driver.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Close implements Store.
func (s *SQLStore) Close() error { return s.db.Close() }

// RouteFidelity implements Store. ref is not tenant-scoped, so no session tenant is needed.
func (s *SQLStore) RouteFidelity(ctx context.Context) (RouteTable, error) {
	rows, err := s.db.QueryContext(ctx, SQLRouteFidelity)
	if err != nil {
		return nil, fmt.Errorf("store: route fidelity: %w", err)
	}
	defer rows.Close()
	out := RouteTable{}
	for rows.Next() {
		var rf RouteFidelity
		if err := rows.Scan(&rf.Source, &rf.Rank, &rf.YieldsContent); err != nil {
			return nil, fmt.Errorf("store: scan route fidelity: %w", err)
		}
		out[rf.Source] = rf
	}
	return out, rows.Err()
}

// PrincipalStatus implements Store: a read-only transaction that sets the session tenant first.
func (s *SQLStore) PrincipalStatus(ctx context.Context, tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	var st PrincipalStatus
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		st, err = principalStatusTx(ctx, tx, tenantID, deviceID, credentialID)
		return err
	})
	return st, err
}

func principalStatusTx(ctx context.Context, tx *sql.Tx, tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	var (
		st          PrincipalStatus
		deviceKnown bool
		credKnown   bool
		deviceRev   sql.NullTime
		credRev     sql.NullTime
		expires     sql.NullTime
		credType    sql.NullString
		thumbprint  sql.NullString
	)
	err := tx.QueryRowContext(ctx, SQLPrincipalStatus, tenantID, deviceID, credentialID).
		Scan(&st.TenantStatus, &st.IngestEnabled, &st.TenantRegion,
			&deviceKnown, &deviceRev, &credKnown, &expires, &credRev, &credType, &thumbprint)
	if err == sql.ErrNoRows {
		return PrincipalStatus{}, nil
	}
	if err != nil {
		return PrincipalStatus{}, fmt.Errorf("store: principal status: %w", err)
	}
	st.TenantKnown = true
	st.DeviceKnown = deviceKnown
	st.CredentialKnown = credKnown
	if deviceRev.Valid {
		t := deviceRev.Time
		st.DeviceRevokedAt = &t
	}
	if credRev.Valid {
		t := credRev.Time
		st.CredentialRevoked = &t
	}
	if expires.Valid {
		st.CredentialExpiry = expires.Time
	}
	st.CredentialType = credType.String
	st.PublicKeyThumbprint = thumbprint.String
	return st, nil
}

// DPoPReplaySeen implements Store. Eligibility is decided by the database's own PRIMARY KEY
// (tenant_id, jti): the insert conflicts if and only if the jti is already inside its window, so
// the outcome does not depend on application-side state or on a prior SELECT. Expired rows are
// swept opportunistically in the same transaction.
func (s *SQLStore) DPoPReplaySeen(ctx context.Context, tenantID, jti string, expiresAt time.Time) (bool, error) {
	var replay bool
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLDPoPReplaySweep, tenantID); err != nil {
			return fmt.Errorf("store: sweep dpop replay: %w", err)
		}
		res, err := tx.ExecContext(ctx, SQLDPoPReplayInsert, tenantID, jti, expiresAt)
		if err != nil {
			return fmt.Errorf("store: record dpop replay: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: dpop replay rows affected: %w", err)
		}
		replay = n == 0
		return nil
	})
	return replay, err
}

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, SQLSetTenant, tenantID); err != nil {
		return fmt.Errorf("store: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// WriteBatch implements Store: one transaction for the whole batch (§6). A validation error is a
// per-event outcome and never reaches here; a write error aborts everything and is retryable, so
// the device never observes a partial commit.
func (s *SQLStore) WriteBatch(ctx context.Context, w BatchWrite) (BatchResult, error) {
	var res BatchResult
	err := s.withTenant(ctx, w.TenantID, func(tx *sql.Tx) error {
		// 1. Credential status re-checked inside the transaction, before commit (§2.3). A batch
		//    whose device is revoked while it is being validated fails with nothing written.
		st, err := principalStatusTx(ctx, tx, w.TenantID, w.DeviceID, w.CredentialID)
		if err != nil {
			return err
		}
		if err := st.CheckWritable(w.ReceivedAt); err != nil {
			return err
		}

		// 2. One ingest.record_event() call per accepted event, in request order.
		outcomes := make([]EventOutcome, 0, len(w.Accepted))
		for _, ev := range w.Accepted {
			out := EventOutcome{Index: ev.Index, EventID: ev.EventID}
			var subID sql.NullString
			if err := tx.QueryRowContext(ctx, SQLRecordEvent, string(ev.Envelope), w.ReceivedAt).
				Scan(&out.Outcome, &subID); err != nil {
				return fmt.Errorf("store: ingest.record_event for event %s: %w", ev.EventID, err)
			}
			out.SubmissionID = subID.String

			switch out.Outcome {
			case OutcomeDuplicate:
				// §5.3: the stored row keeps its first receipt, and that is what we report.
				var first time.Time
				if err := tx.QueryRowContext(ctx, SQLFirstReceivedAt, w.TenantID, ev.EventID).Scan(&first); err == nil {
					out.FirstReceivedAt = &first
				} else if err != sql.ErrNoRows {
					return fmt.Errorf("store: first received_at for event %s: %w", ev.EventID, err)
				}
			case OutcomeMerged:
				// Read back the store's own tie-break decision rather than recomputing it.
				var winner string
				var fidelity int
				if out.SubmissionID != "" {
					err := tx.QueryRowContext(ctx, SQLSubmissionWinner, w.TenantID, out.SubmissionID).
						Scan(&winner, &fidelity)
					if err != nil && err != sql.ErrNoRows {
						return fmt.Errorf("store: submission winner for %s: %w", out.SubmissionID, err)
					}
					out.WonFields = winner == ev.Route
				}
			case OutcomeInserted:
				out.WonFields = true
			}
			outcomes = append(outcomes, out)
		}

		// 3. The rejections, in the same transaction (§6 step 5).
		for _, r := range w.Rejected {
			if !r.Quarantine {
				continue
			}
			code, ok := QuarantineReason(r.Reason)
			if !ok {
				continue
			}
			detail, err := json.Marshal(r.Detail)
			if err != nil {
				return fmt.Errorf("store: marshal rejection detail: %w", err)
			}
			presence, err := json.Marshal(r.Presence)
			if err != nil {
				return fmt.Errorf("store: marshal field presence: %w", err)
			}
			redacted, err := json.Marshal(r.Redacted)
			if err != nil {
				return fmt.Errorf("store: marshal redacted envelope: %w", err)
			}
			var device any
			if w.DeviceID != "" {
				device = w.DeviceID
			}
			if _, err := tx.ExecContext(ctx, SQLInsertRejected,
				w.TenantID, device, w.ReceivedAt, code, string(detail), string(presence), string(redacted)); err != nil {
				return fmt.Errorf("store: insert rejected (%s): %w", code, err)
			}
		}

		res = BatchResult{Outcomes: outcomes}
		return nil
	})
	if err != nil {
		return BatchResult{}, err
	}
	return res, nil
}

// Bind renders a statement's placeholders as psql-executable PREPARE/EXECUTE, so a test can run
// the exact text above against a live server without a Go driver. It exists for verification; it
// is not part of the request path.
func Bind(stmt string, args ...any) (string, error) {
	if strings.Count(stmt, "$") != len(args) {
		return "", fmt.Errorf("store: statement expects %d arguments, got %d", strings.Count(stmt, "$"), len(args))
	}
	literals := make([]string, 0, len(args))
	for _, a := range args {
		literals = append(literals, literal(a))
	}
	return fmt.Sprintf("%s(%s)", stmt, strings.Join(literals, ", ")), nil
}

func literal(a any) string {
	switch v := a.(type) {
	case string:
		// Dollar-quoting avoids every escaping question a JSON envelope would otherwise raise.
		tag := "$sac$"
		for strings.Contains(v, tag) {
			tag = "$" + tag + "$"
		}
		return tag + v + tag
	case time.Time:
		return "'" + v.UTC().Format(time.RFC3339Nano) + "'::timestamptz"
	case nil:
		return "NULL"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ReasonCodeForQuarantine is a documented convenience for tests asserting the mapping.
func ReasonCodeForQuarantine(r protocol.ReasonCode) (string, bool) { return QuarantineReason(r) }
