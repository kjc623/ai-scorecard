package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Diagnostics is one ops.device_diagnostics row without its bundle. ReceivedAt is nil while the
// request waits for the device.
type Diagnostics struct {
	TenantID     string
	RequestID    string
	DeviceID     string
	RequestedBy  string
	RequestedAt  time.Time
	ReceivedAt   *time.Time
	AgentVersion string
	SizeBytes    int64
	SHA256       string
	ExpiresAt    time.Time
}

var (
	// ErrDeviceRevoked refuses a diagnostics request for a revoked device, which will never answer.
	ErrDeviceRevoked = errors.New("store: device revoked")
	// ErrDiagnosticsNotPending is an upload for a request that is not waiting for that device.
	ErrDiagnosticsNotPending = errors.New("store: diagnostics request not pending")
	// ErrDiagnosticsUnknown is a bundle the tenant does not hold for that device.
	ErrDiagnosticsUnknown = errors.New("store: diagnostics unknown")
)

// The diagnostics statements.
const (
	// SQLLockDiagnosticsDevice confirms the device and serialises its requests, so two admins asking
	// at once make one request.
	SQLLockDiagnosticsDevice = `
SELECT revoked_at IS NOT NULL
  FROM ops.device
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid
   FOR NO KEY UPDATE`

	diagnosticsColumns = `request_id::text, device_id::text, requested_by, requested_at, received_at,
       coalesce(agent_version, ''), coalesce(size_bytes, 0), coalesce(sha256, ''), expires_at`

	// SQLPendingDiagnostics is the device's oldest request still waiting at $3.
	SQLPendingDiagnostics = `
SELECT ` + diagnosticsColumns + `
  FROM ops.device_diagnostics
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid
   AND received_at IS NULL AND expires_at > $3::timestamptz
 ORDER BY requested_at, request_id
 LIMIT 1`

	SQLInsertDiagnostics = `
INSERT INTO ops.device_diagnostics (tenant_id, request_id, device_id, requested_by, requested_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, $4::text, $5::timestamptz, $6::timestamptz)`

	// SQLReceiveDiagnostics stores the bundle for a request still waiting for this device at $8.
	SQLReceiveDiagnostics = `
UPDATE ops.device_diagnostics
   SET received_at = $4::timestamptz, agent_version = nullif($5::text, ''), size_bytes = octet_length($6::bytea),
       sha256 = $7::text, bundle = $6::bytea, expires_at = $8::timestamptz
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND request_id = $3::uuid
   AND received_at IS NULL AND expires_at > $4::timestamptz
RETURNING request_id`

	// SQLDeviceDiagnostics is the device's requests not yet expired at $3, newest first.
	SQLDeviceDiagnostics = `
SELECT ` + diagnosticsColumns + `
  FROM ops.device_diagnostics
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND expires_at > $3::timestamptz
 ORDER BY requested_at DESC, request_id
 LIMIT $4::int`

	// SQLDiagnosticsBundle reads one received bundle not yet expired at $4.
	SQLDiagnosticsBundle = `
SELECT ` + diagnosticsColumns + `, bundle
  FROM ops.device_diagnostics
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND request_id = $3::uuid
   AND received_at IS NOT NULL AND expires_at > $4::timestamptz`
)

func scanDiagnostics(row interface{ Scan(...any) error }, d *Diagnostics, extra ...any) error {
	var received sql.NullTime
	dest := append([]any{&d.RequestID, &d.DeviceID, &d.RequestedBy, &d.RequestedAt, &received,
		&d.AgentVersion, &d.SizeBytes, &d.SHA256, &d.ExpiresAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	d.RequestedAt, d.ExpiresAt = d.RequestedAt.UTC(), d.ExpiresAt.UTC()
	if received.Valid {
		t := received.Time.UTC()
		d.ReceivedAt = &t
	}
	return nil
}

// RequestDiagnostics implements Store.
func (s *SQLStore) RequestDiagnostics(ctx context.Context, d Diagnostics, audit AuditEntry) (Diagnostics, bool, error) {
	created := false
	err := s.withTenant(ctx, d.TenantID, func(tx *sql.Tx) error {
		var revoked bool
		if err := tx.QueryRowContext(ctx, SQLLockDiagnosticsDevice, d.TenantID, d.DeviceID).Scan(&revoked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDeviceUnknown
			}
			return fmt.Errorf("store: lock device: %w", err)
		}
		if revoked {
			return ErrDeviceRevoked
		}
		pending := Diagnostics{TenantID: d.TenantID}
		err := scanDiagnostics(tx.QueryRowContext(ctx, SQLPendingDiagnostics, d.TenantID, d.DeviceID, d.RequestedAt.UTC()), &pending)
		if err == nil {
			d = pending
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: pending diagnostics: %w", err)
		}
		if _, err := tx.ExecContext(ctx, SQLInsertDiagnostics, d.TenantID, d.RequestID, d.DeviceID, d.RequestedBy,
			d.RequestedAt.UTC(), d.ExpiresAt.UTC()); err != nil {
			return fmt.Errorf("store: insert diagnostics: %w", err)
		}
		created = true
		return insertAudit(ctx, tx, audit)
	})
	return d, created, err
}

// PendingDiagnostics implements Store.
func (s *SQLStore) PendingDiagnostics(ctx context.Context, tenantID, deviceID string, now time.Time) (string, error) {
	var d Diagnostics
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		return scanDiagnostics(tx.QueryRowContext(ctx, SQLPendingDiagnostics, tenantID, deviceID, now.UTC()), &d)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: pending diagnostics: %w", err)
	}
	return d.RequestID, nil
}

// ReceiveDiagnostics implements Store.
func (s *SQLStore) ReceiveDiagnostics(ctx context.Context, d Diagnostics, bundle []byte, audit AuditEntry) error {
	return s.withTenant(ctx, d.TenantID, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, SQLReceiveDiagnostics, d.TenantID, d.DeviceID, d.RequestID, d.ReceivedAt.UTC(),
			d.AgentVersion, bundle, d.SHA256, d.ExpiresAt.UTC()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDiagnosticsNotPending
		}
		if err != nil {
			return fmt.Errorf("store: receive diagnostics: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
}

// DeviceDiagnostics implements Store.
func (s *SQLStore) DeviceDiagnostics(ctx context.Context, tenantID, deviceID string, now time.Time, limit int) ([]Diagnostics, error) {
	out := []Diagnostics{}
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, SQLDeviceDiagnostics, tenantID, deviceID, now.UTC(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d := Diagnostics{TenantID: tenantID}
			if err := scanDiagnostics(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: device diagnostics: %w", err)
	}
	return out, nil
}

// DiagnosticsBundle implements Store.
func (s *SQLStore) DiagnosticsBundle(ctx context.Context, tenantID, deviceID, requestID string, audit AuditEntry) (Diagnostics, []byte, error) {
	d := Diagnostics{TenantID: tenantID}
	var bundle []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := scanDiagnostics(tx.QueryRowContext(ctx, SQLDiagnosticsBundle, tenantID, deviceID, requestID, audit.OccurredAt.UTC()), &d, &bundle)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDiagnosticsUnknown
		}
		if err != nil {
			return fmt.Errorf("store: diagnostics bundle: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
	if err != nil {
		return Diagnostics{}, nil, err
	}
	return d, bundle, nil
}
