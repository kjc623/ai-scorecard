package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// The kill switch statements.
const (
	SQLKillSwitches = `
SELECT route, reason_code, effective_at, set_by
  FROM ops.kill_switch
 WHERE tenant_id = $1::uuid
 ORDER BY route`

	// SQLTripKillSwitch trips a switch, or gives a tripped one a new reason and actor while keeping
	// the time it came into effect.
	SQLTripKillSwitch = `
INSERT INTO ops.kill_switch (tenant_id, route, reason_code, effective_at, set_by)
VALUES ($1::uuid, $2::text, $3::text, $4::timestamptz, $5::text)
ON CONFLICT (tenant_id, route)
DO UPDATE SET reason_code = EXCLUDED.reason_code, set_by = EXCLUDED.set_by`

	SQLClearKillSwitch = `DELETE FROM ops.kill_switch WHERE tenant_id = $1::uuid AND route = $2::text`
)

func killSwitches(ctx context.Context, tx *sql.Tx, tenantID string) ([]KillSwitch, error) {
	rows, err := tx.QueryContext(ctx, SQLKillSwitches, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: kill switches: %w", err)
	}
	defer rows.Close()
	out := []KillSwitch{}
	for rows.Next() {
		var k KillSwitch
		if err := rows.Scan(&k.Route, &k.ReasonCode, &k.EffectiveAt, &k.SetBy); err != nil {
			return nil, fmt.Errorf("store: kill switches: %w", err)
		}
		k.EffectiveAt = k.EffectiveAt.UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

// KillSwitchDetail is the audit's spelling of one route's switch: off, or on with its reason.
func KillSwitchDetail(switches []KillSwitch, route string) map[string]any {
	for _, k := range switches {
		if k.Route == route {
			return map[string]any{"on": true, "reason_code": k.ReasonCode}
		}
	}
	return map[string]any{"on": false}
}

// SetKillSwitch implements Store.
func (s *SQLStore) SetKillSwitch(ctx context.Context, tenantID, route string, on bool, reasonCode string, audit AuditEntry) error {
	if !slices.Contains(KillSwitchRoutes, route) {
		return ErrUnknownKillSwitchRoute
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := tenantRow(ctx, tx, SQLRulesTenant, tenantID); err != nil {
			return err
		}
		previous, err := killSwitches(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		next := map[string]any{"on": false}
		if on {
			at := audit.OccurredAt
			if at.IsZero() {
				at = time.Now()
			}
			if _, err := tx.ExecContext(ctx, SQLTripKillSwitch, tenantID, route, reasonCode, at.UTC(), audit.ActorID); err != nil {
				return fmt.Errorf("store: trip kill switch: %w", err)
			}
			next = map[string]any{"on": true, "reason_code": reasonCode}
		} else if _, err := tx.ExecContext(ctx, SQLClearKillSwitch, tenantID, route); err != nil {
			return fmt.Errorf("store: clear kill switch: %w", err)
		}
		audit.Detail = mergeDetail(audit.Detail, map[string]any{"route": route})
		audit = withChange(audit, KillSwitchDetail(previous, route), next)
		return insertAudit(ctx, tx, audit)
	})
}
