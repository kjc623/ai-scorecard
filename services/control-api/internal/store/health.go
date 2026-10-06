package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The health channel's statements.
const (
	// SQLCollectorVocabulary reads the closed collector vocabulary. A report naming anything else is
	// refused rather than stored under a collector name nothing can interpret.
	SQLCollectorVocabulary = `SELECT collector_code FROM ref.collector`

	// SQLUpsertCollectorState keeps one row per device per collector. The guard in the DO UPDATE's
	// WHERE makes an out-of-order replay a zero-row update, so a stale report never overwrites a
	// newer one.
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

	// SQLSetDeviceHealth stamps device activity and the device-level identity fields. A stale report
	// updates nothing. The clear hostname is applied only for a 'clear' tenant and the hash only for
	// a 'hashed' one, so a device with stale configuration cannot make a hashed tenant store a clear
	// name. $9 is the tenant's setting.
	SQLSetDeviceHealth = `
UPDATE ops.device
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

	sqlTenantDeviceIdentity = `SELECT device_identity FROM ops.tenant WHERE tenant_id = $1::uuid`
)

// RecordHealth implements Store.
func (s *SQLStore) RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState, dev DeviceHealth) error {
	if len(reports) == 0 {
		return nil
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		known, err := knownCollectors(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range reports {
			if !known[r.Collector] {
				return fmt.Errorf("%w: %q", ErrUnknownCollector, r.Collector)
			}
		}
		for _, r := range reports {
			if _, err := tx.ExecContext(ctx, SQLUpsertCollectorState,
				tenantID, deviceID, r.Collector, r.State, r.Version, jsonText(r.Permissions, "{}"),
				r.LastSuccess, at.UTC(), r.SpoolDepth, r.SpoolCapacity, r.SpoolDroppedTotal,
				r.ErrorCode, jsonText(r.Detail, "{}")); err != nil {
				return fmt.Errorf("store: upsert collector state (%s): %w", r.Collector, err)
			}
		}
		// The tenant's identity setting is read in this transaction, so what is stored follows the
		// tenant's setting rather than anything the caller passed.
		var identity string
		err = tx.QueryRowContext(ctx, sqlTenantDeviceIdentity, tenantID).Scan(&identity)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: tenant identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, SQLSetDeviceHealth,
			tenantID, deviceID, at.UTC(), dev.Hostname, dev.HostnameHash,
			dev.AgentVersion, dev.CollectionMode, dev.ManagedState, identity); err != nil {
			return fmt.Errorf("store: set device health: %w", err)
		}
		return nil
	})
}

func knownCollectors(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, SQLCollectorVocabulary)
	if err != nil {
		return nil, fmt.Errorf("store: collector vocabulary: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("store: collector vocabulary: %w", err)
		}
		out[code] = true
	}
	return out, rows.Err()
}

func jsonText(raw json.RawMessage, empty string) string {
	if len(raw) == 0 {
		return empty
	}
	return string(raw)
}
