// Package store is ingest-api's PostgreSQL access: the device's credential status, the route
// table, and the batch write.
//
// Merge semantics live in the database. ingest.record_event() decides whether an event is new, a
// merge into an existing submission, or a duplicate, enforced by unique constraints rather than by
// a read in application code; this package calls it and reads back what it decided.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Why a principal may not write. Anything else a method returns is an infrastructure failure, and
// the batch is retryable.
var (
	ErrUnknownTenant     = errors.New("store: tenant unknown")
	ErrTenantSuspended   = errors.New("store: tenant ingest disabled")
	ErrRegionMismatch    = errors.New("store: tenant is pinned to another region")
	ErrCredentialUnknown = errors.New("store: device credential unknown")
	ErrDeviceRevoked     = errors.New("store: device revoked")
	ErrCredentialRevoked = errors.New("store: device credential revoked")
	ErrCredentialExpired = errors.New("store: device credential expired")
)

// PrincipalStatus is ops.tenant, ops.device and ops.device_credential as the write path needs
// them. It is read at admission and again inside the write transaction.
type PrincipalStatus struct {
	TenantKnown       bool
	TenantStatus      string
	IngestEnabled     bool
	TenantRegion      string
	DeviceKnown       bool
	DeviceRevokedAt   *time.Time
	CredentialKnown   bool
	CredentialExpiry  time.Time
	CredentialRevoked *time.Time
}

// Check reports why the principal may not write at now, or nil. region is the deployment's
// region; an empty region skips that comparison, which the in-transaction re-check uses because
// residency cannot change between admission and commit.
func (s PrincipalStatus) Check(now time.Time, region string) error {
	switch {
	case !s.TenantKnown:
		return ErrUnknownTenant
	case !s.IngestEnabled || s.TenantStatus == "closed":
		return ErrTenantSuspended
	case region != "" && s.TenantRegion != region:
		return ErrRegionMismatch
	case !s.DeviceKnown:
		return ErrCredentialUnknown
	case s.DeviceRevokedAt != nil:
		return ErrDeviceRevoked
	case !s.CredentialKnown:
		return ErrCredentialUnknown
	case s.CredentialRevoked != nil:
		return ErrCredentialRevoked
	case !s.CredentialExpiry.After(now):
		return ErrCredentialExpired
	}
	return nil
}

// Outcome is what ingest.record_event() returned for one event.
type Outcome string

const (
	OutcomeInserted  Outcome = "inserted"  // this observation created the logical submission
	OutcomeMerged    Outcome = "merged"    // it folded into an existing submission
	OutcomeDuplicate Outcome = "duplicate" // the event_id was already recorded
)

// AcceptedEvent is one validated event to record.
type AcceptedEvent struct {
	Index    int             // position in the batch
	EventID  string          // validated uuid
	Route    string          // the envelope's source, present in ref.route_fidelity
	Envelope json.RawMessage // exactly the bytes the device sent
}

// Rejection is one event that failed validation, quarantined in ingest.rejected in the same
// transaction as the accepted events.
type Rejection struct {
	Reason   protocol.ReasonCode
	Detail   *protocol.BatchRejectionDetail
	Present  []string       // the field names the envelope carried
	Redacted map[string]any // the envelope with content-derived and undeclared fields removed
}

// BatchWrite is one transaction: the principal to re-check, the events to record and the
// rejections to quarantine.
type BatchWrite struct {
	TenantID     string
	DeviceID     string
	CredentialID string
	ReceivedAt   time.Time
	Accepted     []AcceptedEvent
	Rejected     []Rejection
}

// EventOutcome is the result of recording one accepted event.
type EventOutcome struct {
	Index           int
	EventID         string
	Outcome         Outcome
	SubmissionID    string
	FirstReceivedAt *time.Time // the original receipt of a duplicate
	// WonFields reports whether the submission's contested fields come from this event's route,
	// read back from the submission's winning_source rather than recomputed.
	WonFields bool
}

// The statements this service issues.
const (
	// sqlSetTenant scopes row-level security to one tenant for the rest of the transaction.
	sqlSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlPrincipalStatus = `
SELECT t.status,
       t.ingest_enabled,
       t.residency_region,
       d.device_id IS NOT NULL,
       d.revoked_at,
       c.credential_id IS NOT NULL,
       c.expires_at,
       c.revoked_at
  FROM ops.tenant t
  LEFT JOIN ops.device d
         ON d.tenant_id = t.tenant_id AND d.device_id = $2::uuid
  LEFT JOIN ops.device_credential c
         ON c.tenant_id = d.tenant_id AND c.device_id = d.device_id AND c.credential_id = $3::uuid
 WHERE t.tenant_id = $1::uuid`

	sqlRoutes = `SELECT source FROM ref.route_fidelity`

	// sqlRecordEvent is the write: $1 is the envelope as the device sent it, $2 the batch's single
	// receive time.
	sqlRecordEvent = `SELECT event_outcome, event_submission_id FROM ingest.record_event($1::jsonb, $2::timestamptz)`

	sqlFirstReceivedAt = `SELECT received_at FROM ingest.observation WHERE tenant_id = $1::uuid AND event_id = $2::uuid`

	sqlSubmissionWinner = `SELECT winning_source FROM ingest.submission WHERE tenant_id = $1::uuid AND submission_id = $2::uuid`

	// sqlInsertRejected takes the quarantine window from ref.retention_class.
	sqlInsertRejected = `
INSERT INTO ingest.rejected (tenant_id, device_id, received_at, reason_code, detail, field_presence, envelope_redacted, expires_at)
VALUES ($1::uuid, $2::uuid, $3::timestamptz, $4::text, $5::jsonb, $6::jsonb, $7::jsonb,
        $3::timestamptz + make_interval(days => COALESCE(
            (SELECT default_ttl_days FROM ref.retention_class WHERE retention_class = 'quarantine'), 30)))`

	// sqlTouchDevice only moves last_seen_at forward, so a retried or out-of-order batch cannot
	// make a live device look quiet.
	sqlTouchDevice = `
UPDATE ops.device SET last_seen_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid
   AND (last_seen_at IS NULL OR last_seen_at < $3::timestamptz)`
)

// Postgres implements the store on a database/sql pool.
type Postgres struct {
	db *sql.DB
}

// New wraps an open pool.
func New(db *sql.DB) *Postgres { return &Postgres{db: db} }

// Ping is a database round trip, for readiness.
func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

// Routes returns the sources in ref.route_fidelity. ingest.record_event() refuses a route the
// table does not hold, so the service checks it per event before the transaction opens.
func (p *Postgres) Routes(ctx context.Context) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, sqlRoutes)
	if err != nil {
		return nil, fmt.Errorf("store: routes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			return nil, fmt.Errorf("store: routes: %w", err)
		}
		out = append(out, source)
	}
	return out, rows.Err()
}

// PrincipalStatus reads the tenant, device and credential state for an authenticated
// certificate.
func (p *Postgres) PrincipalStatus(ctx context.Context, tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	var st PrincipalStatus
	err := p.inTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		st, err = principalStatus(ctx, tx, tenantID, deviceID, credentialID)
		return err
	})
	return st, err
}

func principalStatus(ctx context.Context, tx *sql.Tx, tenantID, deviceID, credentialID string) (PrincipalStatus, error) {
	var (
		st                                        PrincipalStatus
		deviceRevoked, credentialRevoked, expires sql.NullTime
	)
	err := tx.QueryRowContext(ctx, sqlPrincipalStatus, tenantID, deviceID, credentialID).Scan(
		&st.TenantStatus, &st.IngestEnabled, &st.TenantRegion,
		&st.DeviceKnown, &deviceRevoked, &st.CredentialKnown, &expires, &credentialRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return PrincipalStatus{}, nil
	}
	if err != nil {
		return PrincipalStatus{}, fmt.Errorf("store: principal status: %w", err)
	}
	st.TenantKnown = true
	if deviceRevoked.Valid {
		st.DeviceRevokedAt = &deviceRevoked.Time
	}
	if credentialRevoked.Valid {
		st.CredentialRevoked = &credentialRevoked.Time
	}
	st.CredentialExpiry = expires.Time
	return st, nil
}

// WriteBatch records a batch in one transaction: the credential is re-checked first, so a device
// revoked while its batch was validated writes nothing; then one ingest.record_event() call per
// accepted event in request order; then the rejections; then the device's last_seen_at. Either all
// of it commits or none of it does.
func (p *Postgres) WriteBatch(ctx context.Context, w BatchWrite) ([]EventOutcome, error) {
	var outcomes []EventOutcome
	err := p.inTenant(ctx, w.TenantID, func(tx *sql.Tx) error {
		st, err := principalStatus(ctx, tx, w.TenantID, w.DeviceID, w.CredentialID)
		if err != nil {
			return err
		}
		if err := st.Check(w.ReceivedAt, ""); err != nil {
			return err
		}

		outcomes = make([]EventOutcome, 0, len(w.Accepted))
		for _, ev := range w.Accepted {
			out, err := recordEvent(ctx, tx, w, ev)
			if err != nil {
				return err
			}
			outcomes = append(outcomes, out)
		}

		for _, r := range w.Rejected {
			if err := insertRejected(ctx, tx, w, r); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, sqlTouchDevice, w.TenantID, w.DeviceID, w.ReceivedAt); err != nil {
			return fmt.Errorf("store: touch device: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcomes, nil
}

func recordEvent(ctx context.Context, tx *sql.Tx, w BatchWrite, ev AcceptedEvent) (EventOutcome, error) {
	out := EventOutcome{Index: ev.Index, EventID: ev.EventID}
	var submissionID sql.NullString
	if err := tx.QueryRowContext(ctx, sqlRecordEvent, string(ev.Envelope), w.ReceivedAt).
		Scan(&out.Outcome, &submissionID); err != nil {
		return out, fmt.Errorf("store: record event %s: %w", ev.EventID, err)
	}
	out.SubmissionID = submissionID.String

	switch out.Outcome {
	case OutcomeDuplicate:
		var first time.Time
		err := tx.QueryRowContext(ctx, sqlFirstReceivedAt, w.TenantID, ev.EventID).Scan(&first)
		switch {
		case err == nil:
			first = first.UTC()
			out.FirstReceivedAt = &first
		case !errors.Is(err, sql.ErrNoRows):
			return out, fmt.Errorf("store: first receipt of %s: %w", ev.EventID, err)
		}
	case OutcomeMerged:
		var winner sql.NullString
		err := tx.QueryRowContext(ctx, sqlSubmissionWinner, w.TenantID, out.SubmissionID).Scan(&winner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("store: winner of submission %s: %w", out.SubmissionID, err)
		}
		out.WonFields = winner.String == ev.Route
	case OutcomeInserted:
		out.WonFields = true
	default:
		return out, fmt.Errorf("store: record event %s returned outcome %q", ev.EventID, out.Outcome)
	}
	return out, nil
}

func insertRejected(ctx context.Context, tx *sql.Tx, w BatchWrite, r Rejection) error {
	detail, err := json.Marshal(r.Detail)
	if err != nil {
		return fmt.Errorf("store: rejection detail: %w", err)
	}
	presence, err := json.Marshal(map[string][]string{"present": r.Present})
	if err != nil {
		return fmt.Errorf("store: rejection presence: %w", err)
	}
	redacted, err := json.Marshal(r.Redacted)
	if err != nil {
		return fmt.Errorf("store: redacted envelope: %w", err)
	}
	if _, err := tx.ExecContext(ctx, sqlInsertRejected, w.TenantID, w.DeviceID, w.ReceivedAt,
		string(r.Reason), string(detail), string(presence), string(redacted)); err != nil {
		return fmt.Errorf("store: quarantine %s: %w", r.Reason, err)
	}
	return nil
}

// inTenant runs fn in a transaction scoped to tenantID by row-level security.
func (p *Postgres) inTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlSetTenant, tenantID); err != nil {
		return fmt.Errorf("store: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
