package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Every SQL statement the health channel issues, in one place, for the same reason sql.go holds the
// enrolment statements: the seam with database/schema.sql is reviewable in one screen and the live
// test executes this exact text against PostgreSQL.

const (
	// SQLCollectorVocabulary reads the closed collector vocabulary. A report naming anything else is
	// refused rather than stored under a coverage path ref.collector does not know.
	SQLCollectorVocabulary = `SELECT collector_code FROM ref.collector`

	// SQLUpsertCollectorState is the keyed upsert of docs/02 §5.4: one row per device per collector,
	// guarded so a stale report cannot overwrite a newer one. The guard is in the DO UPDATE's WHERE,
	// so an out-of-order replay is a zero-row update rather than a silent regression.
	SQLUpsertCollectorState = `
INSERT INTO ops.collector_state
  (tenant_id, device_id, collector, state, version, permissions, last_success_at,
   last_report_at, spool_depth, spool_capacity, spool_dropped_total, error_code, detail)
VALUES ($1::uuid, $2::uuid, $3::text, $4::text, nullif($5, ''), $6::jsonb, $7::timestamptz,
        $8::timestamptz, $9::bigint, $10::bigint, $11::bigint, nullif($12, ''), $13::jsonb)
ON CONFLICT (tenant_id, device_id, collector) DO UPDATE
   SET state               = EXCLUDED.state,
       version             = EXCLUDED.version,
       permissions         = EXCLUDED.permissions,
       last_success_at     = EXCLUDED.last_success_at,
       last_report_at      = EXCLUDED.last_report_at,
       spool_depth         = EXCLUDED.spool_depth,
       spool_capacity      = EXCLUDED.spool_capacity,
       spool_dropped_total = EXCLUDED.spool_dropped_total,
       error_code          = EXCLUDED.error_code,
       detail              = EXCLUDED.detail
 WHERE ops.collector_state.last_report_at < EXCLUDED.last_report_at`

	// SQLSetDeviceHealth stamps device activity and the device-level identity fields from the health
	// channel. It is monotonic: a stale report updates nothing. The clear hostname is applied only
	// for a 'clear' tenant and the hash only for a 'hashed' one, so a device with stale config
	// cannot make a hashed tenant store a clear name (ADR 0021). $9 is the tenant's setting.
	SQLSetDeviceHealth = `UPDATE ops.device
   SET last_seen_at    = $3::timestamptz,
       hostname        = CASE WHEN $9::text = 'clear'
                              THEN coalesce(nullif($4, ''), hostname) ELSE hostname END,
       hostname_hash   = CASE WHEN $9::text = 'hashed'
                              THEN coalesce(nullif($5, ''), hostname_hash) ELSE hostname_hash END,
       agent_version   = coalesce(nullif($6, ''), agent_version),
       collection_mode = coalesce(nullif($7, ''), collection_mode),
       managed_state   = coalesce(nullif($8, ''), managed_state)
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid
   AND (last_seen_at IS NULL OR last_seen_at <= $3::timestamptz)`
)

// Statements names the health statements, so the live test can execute them by name.
var HealthStatements = []Statement{
	{Name: "collector_vocabulary", Purpose: "closed collector vocabulary from ref.collector", SQL: SQLCollectorVocabulary},
	{Name: "upsert_collector_state", Purpose: "§5.4 keyed upsert, stale-guarded", SQL: SQLUpsertCollectorState},
	{Name: "touch_device_health", Purpose: "docs/04 §3.7 stamp device activity and identity (ADR 0021)", SQL: SQLSetDeviceHealth},
}

// RecordHealth implements Store: one transaction that validates the collector vocabulary, upserts
// each report and updates the device's activity and identity fields. A report naming an unknown
// collector rolls the whole transaction back, so a partly-invalid health report changes nothing.
func (s *SQLStore) RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState, dev DeviceHealth) error {
	if len(reports) == 0 {
		return nil
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		known, err := knownCollectorsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range reports {
			if !known[r.Collector] {
				return fmt.Errorf("%w: %q", ErrUnknownCollector, r.Collector)
			}
		}
		for _, r := range reports {
			permissions := r.Permissions
			if len(permissions) == 0 {
				permissions = json.RawMessage(`{}`)
			}
			detail := r.Detail
			if len(detail) == 0 {
				detail = json.RawMessage(`{}`)
			}
			if _, err := tx.ExecContext(ctx, SQLUpsertCollectorState,
				tenantID, deviceID, r.Collector, r.State, r.Version, string(permissions),
				r.LastSuccess, at, r.SpoolDepth, r.SpoolCapacity, r.SpoolDroppedTotal,
				r.ErrorCode, string(detail)); err != nil {
				return fmt.Errorf("store: upsert collector state (%s): %w", r.Collector, err)
			}
		}
		// The tenant's identity setting is read in this transaction so SQLSetDeviceHealth applies the
		// same authority the read path uses, not a value the caller could get wrong.
		identity, err := tenantDeviceIdentityTx(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLSetDeviceHealth,
			tenantID, deviceID, at, dev.Hostname, dev.HostnameHash,
			dev.AgentVersion, dev.CollectionMode, dev.ManagedState, string(identity)); err != nil {
			return fmt.Errorf("store: set device health: %w", err)
		}
		return nil
	})
}

// tenantDeviceIdentityTx reads ops.tenant.device_identity inside the transaction. Unknown tenant is
// reported as an error rather than defaulted: the health path authenticates a device, so its tenant
// must exist.
func tenantDeviceIdentityTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	var identity string
	if err := tx.QueryRowContext(ctx, `SELECT device_identity FROM ops.tenant WHERE tenant_id = $1::uuid`, tenantID).Scan(&identity); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrUnknownTenant
		}
		return "", fmt.Errorf("store: tenant identity: %w", err)
	}
	return identity, nil
}

func knownCollectorsTx(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, SQLCollectorVocabulary)
	if err != nil {
		return nil, fmt.Errorf("store: collector vocabulary: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("store: scan collector vocabulary: %w", err)
		}
		out[code] = true
	}
	return out, rows.Err()
}
