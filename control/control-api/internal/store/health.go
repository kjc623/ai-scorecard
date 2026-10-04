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

	// SQLTouchDeviceHealth stamps device activity from the health channel. Monotonic, like the
	// batch path's stamp: a report cannot move a live device's last_seen_at backwards.
	SQLTouchDeviceHealth = `UPDATE ops.device
   SET last_seen_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid
   AND (last_seen_at IS NULL OR last_seen_at < $3::timestamptz)`
)

// Statements names the health statements, so the live test can execute them by name.
var HealthStatements = []Statement{
	{Name: "collector_vocabulary", Purpose: "closed collector vocabulary from ref.collector", SQL: SQLCollectorVocabulary},
	{Name: "upsert_collector_state", Purpose: "§5.4 keyed upsert, stale-guarded", SQL: SQLUpsertCollectorState},
	{Name: "touch_device_health", Purpose: "docs/04 §3.7 stamp device activity", SQL: SQLTouchDeviceHealth},
}

// RecordHealth implements Store: one transaction that validates the collector vocabulary, upserts
// each report and stamps the device's last_seen_at. A report naming an unknown collector rolls the
// whole transaction back, so a partly-invalid health report changes nothing.
func (s *SQLStore) RecordHealth(ctx context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState) error {
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
		if _, err := tx.ExecContext(ctx, SQLTouchDeviceHealth, tenantID, deviceID, at); err != nil {
			return fmt.Errorf("store: touch device last_seen_at: %w", err)
		}
		return nil
	})
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
