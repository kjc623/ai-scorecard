package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// The Settings page's statements. Each writes the audit row in the same transaction as the write,
// with the actor, old value and new value, so a configuration change is never unattributed.
const (
	SQLSettingsTenant = `
SELECT ceiling_mode, coalesce(collection_mode, ''), scope_overrides, content_search, tls_inspection,
       (SELECT ttl_days FROM ops.retention_policy rp
         WHERE rp.tenant_id = t.tenant_id AND rp.applies_to = 'event' LIMIT 1),
       (SELECT ttl_days FROM ops.retention_policy rp
         WHERE rp.tenant_id = t.tenant_id AND rp.applies_to = 'content' LIMIT 1)
  FROM ops.tenant t
 WHERE t.tenant_id = $1::uuid`

	SQLRetentionDefaults = `
SELECT coalesce((SELECT default_ttl_days FROM ref.retention_class WHERE retention_class = 'standard'), 90),
       coalesce((SELECT default_ttl_days FROM ref.retention_class WHERE retention_class = 'content'), 30)`

	SQLSettingsTools = `
SELECT c.tool_fingerprint, coalesce(t.display_name, c.display_name) AS display_name,
       coalesce(t.sanctioned_state, 'unknown') AS sanctioned_state
  FROM ref.tool_catalogue c
  LEFT JOIN ops.tool t
    ON t.tenant_id = $1::uuid AND t.tool_fingerprint = c.tool_fingerprint
 ORDER BY display_name, c.tool_fingerprint`

	SQLSettingsDevices = `
SELECT device_id::text, coalesce(hostname, ''), coalesce(collection_mode, ''), last_seen_at
  FROM ops.device
 WHERE tenant_id = $1::uuid AND revoked_at IS NULL
 ORDER BY device_id`

	SQLCurrentCollectionMode = `SELECT coalesce(collection_mode, '') FROM ops.tenant WHERE tenant_id = $1::uuid`

	SQLSetCollectionMode = `
UPDATE ops.tenant SET collection_mode = nullif($2::text, ''), updated_at = now()
 WHERE tenant_id = $1::uuid`

	SQLCurrentScopeOverrides = `SELECT scope_overrides FROM ops.tenant WHERE tenant_id = $1::uuid`

	SQLSetScopeOverride = `
UPDATE ops.tenant SET scope_overrides = scope_overrides || jsonb_build_object($2::text, $3::text), updated_at = now()
 WHERE tenant_id = $1::uuid`

	SQLClearScopeOverride = `
UPDATE ops.tenant SET scope_overrides = scope_overrides - $2::text, updated_at = now()
 WHERE tenant_id = $1::uuid`

	SQLCurrentRetention = `
SELECT ttl_days FROM ops.retention_policy
 WHERE tenant_id = $1::uuid AND applies_to = $2::text
 ORDER BY ttl_days LIMIT 1`

	// SQLSetRetention writes the tenant's retention period for events (every mode) or content
	// (M3 only, where content is captured), one row per data class. ON CONFLICT replaces the
	// tenant's earlier rows, so the whole matrix always carries the one value.
	SQLSetRetention = `
INSERT INTO ops.retention_policy (tenant_id, applies_to, data_class, collection_mode, ttl_days, updated_by)
SELECT r.tenant_id, r.applies_to, c.class_code, m.collection_mode, $3::int, $4::text
  FROM (VALUES ($1::uuid, $2::text)) AS r(tenant_id, applies_to)
  JOIN ref.data_class c ON true
  JOIN (SELECT unnest(ARRAY['m0','m1','m2','m3']) AS collection_mode) m ON true
 WHERE r.applies_to = 'event' OR m.collection_mode = 'm3'
ON CONFLICT (tenant_id, applies_to, data_class, collection_mode)
DO UPDATE SET ttl_days = EXCLUDED.ttl_days, updated_by = EXCLUDED.updated_by, updated_at = now()`

	SQLCurrentContentSearch = `SELECT content_search FROM ops.tenant WHERE tenant_id = $1::uuid`

	SQLSetContentSearch = `
UPDATE ops.tenant SET content_search = $2::text, updated_at = now()
 WHERE tenant_id = $1::uuid`

	SQLCurrentTLSInspection = `SELECT tls_inspection FROM ops.tenant WHERE tenant_id = $1::uuid`

	SQLSetTLSInspection = `
UPDATE ops.tenant SET tls_inspection = $2::boolean, updated_at = now()
 WHERE tenant_id = $1::uuid`

	SQLCurrentToolState = `
SELECT coalesce((SELECT sanctioned_state FROM ops.tool
                  WHERE tenant_id = $1::uuid AND tool_fingerprint = $2::text), 'unknown')`

	SQLSetToolSanction = `
INSERT INTO ops.tool (tenant_id, tool_fingerprint, sanctioned_state, decided_by, decided_at)
VALUES ($1::uuid, $2::text, $3::text, nullif($4::text, ''), $5::timestamptz)
ON CONFLICT (tenant_id, tool_fingerprint)
DO UPDATE SET sanctioned_state = EXCLUDED.sanctioned_state,
              decided_by       = EXCLUDED.decided_by,
              decided_at       = EXCLUDED.decided_at`

	// SQLEndpointSettings is the tenant's endpoint collector settings as the policy bundle serves
	// them: the tenant's rows where they exist, else the defaults (every collector on; each tool on
	// for the collectors it has, so Cursor has no OTel and Copilot no hooks). The tools come back as
	// {tool_key: {"otel": bool, "hooks": bool}}.
	SQLEndpointSettings = `
SELECT coalesce(e.inventory, true), coalesce(e.processes, true), coalesce(e.flows, true),
       coalesce(e.otel, true), coalesce(e.hooks, true), coalesce(e.hooks_managed_only, false),
       (SELECT jsonb_object_agg(d.tool_key, jsonb_build_object(
                 'otel', coalesce(s.otel, d.otel), 'hooks', coalesce(s.hooks, d.hooks)))
          FROM (VALUES ('claude_code', true, true), ('codex', true, true),
                       ('copilot', true, false), ('cursor', false, true)) AS d(tool_key, otel, hooks)
          LEFT JOIN ops.endpoint_tool_setting s
            ON s.tenant_id = t.tenant_id AND s.tool_key = d.tool_key)
  FROM (VALUES ($1::uuid)) AS t(tenant_id)
  LEFT JOIN ops.endpoint_setting e ON e.tenant_id = t.tenant_id`

	SQLSetEndpointCollectors = `
INSERT INTO ops.endpoint_setting (tenant_id, inventory, processes, flows, otel, hooks, hooks_managed_only)
VALUES ($1::uuid, $2::boolean, $3::boolean, $4::boolean, $5::boolean, $6::boolean, $7::boolean)
ON CONFLICT (tenant_id)
DO UPDATE SET inventory          = EXCLUDED.inventory,
              processes          = EXCLUDED.processes,
              flows              = EXCLUDED.flows,
              otel               = EXCLUDED.otel,
              hooks              = EXCLUDED.hooks,
              hooks_managed_only = EXCLUDED.hooks_managed_only`

	SQLSetEndpointTool = `
INSERT INTO ops.endpoint_tool_setting (tenant_id, tool_key, otel, hooks)
VALUES ($1::uuid, $2::text, $3::boolean, $4::boolean)
ON CONFLICT (tenant_id, tool_key)
DO UPDATE SET otel = EXCLUDED.otel, hooks = EXCLUDED.hooks`
)

// Settings implements Store.
func (s *SQLStore) Settings(ctx context.Context, tenantID string) (Settings, error) {
	var out Settings
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var scope []byte
		var event, content sql.NullInt64
		err := tx.QueryRowContext(ctx, SQLSettingsTenant, tenantID).
			Scan(&out.CeilingMode, &out.CollectionMode, &scope, &out.ContentSearch, &out.TLSInspection, &event, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: settings tenant: %w", err)
		}
		out.ScopeOverrides = map[string]string{}
		if len(scope) > 0 {
			if err := json.Unmarshal(scope, &out.ScopeOverrides); err != nil {
				return fmt.Errorf("store: settings scope overrides: %w", err)
			}
		}
		if event.Valid {
			v := int(event.Int64)
			out.EventRetentionDays = &v
		}
		if content.Valid {
			v := int(content.Int64)
			out.ContentRetentionDays = &v
		}
		if err := tx.QueryRowContext(ctx, SQLRetentionDefaults).Scan(&out.RetentionDefaults.EventDays, &out.RetentionDefaults.ContentDays); err != nil {
			return fmt.Errorf("store: retention defaults: %w", err)
		}
		var err2 error
		if out.Tools, err2 = settingsTools(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		if out.Devices, err2 = settingsDevices(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		if out.Endpoint, err2 = endpointSettings(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		if out.DataClasses, err2 = dataClasses(ctx, tx); err2 != nil {
			return err2
		}
		if out.KillSwitches, err2 = killSwitches(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		out.AppCategories, err2 = appCategories(ctx, tx)
		return err2
	})
	return out, err
}

// endpointSettings reads the tenant's endpoint settings with the defaults applied.
func endpointSettings(ctx context.Context, tx *sql.Tx, tenantID string) (EndpointSettings, error) {
	var out EndpointSettings
	var tools []byte
	c := &out.Collectors
	if err := tx.QueryRowContext(ctx, SQLEndpointSettings, tenantID).
		Scan(&c.Inventory, &c.Processes, &c.Flows, &c.OTel, &c.Hooks, &c.HooksManagedOnly, &tools); err != nil {
		return EndpointSettings{}, fmt.Errorf("store: endpoint settings: %w", err)
	}
	var raw map[string]struct {
		OTel  bool `json:"otel"`
		Hooks bool `json:"hooks"`
	}
	if err := json.Unmarshal(tools, &raw); err != nil {
		return EndpointSettings{}, fmt.Errorf("store: endpoint tool settings: %w", err)
	}
	out.Tools = make(map[string]EndpointTool, len(raw))
	for k, v := range raw {
		out.Tools[k] = EndpointTool{OTel: v.OTel, Hooks: v.Hooks}
	}
	return out, nil
}

func settingsTools(ctx context.Context, tx *sql.Tx, tenantID string) ([]ToolDecision, error) {
	rows, err := tx.QueryContext(ctx, SQLSettingsTools, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: settings tools: %w", err)
	}
	defer rows.Close()
	var out []ToolDecision
	for rows.Next() {
		var d ToolDecision
		if err := rows.Scan(&d.ToolFingerprint, &d.DisplayName, &d.SanctionedState); err != nil {
			return nil, fmt.Errorf("store: settings tools: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func settingsDevices(ctx context.Context, tx *sql.Tx, tenantID string) ([]DeviceMode, error) {
	rows, err := tx.QueryContext(ctx, SQLSettingsDevices, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: settings devices: %w", err)
	}
	defer rows.Close()
	var out []DeviceMode
	for rows.Next() {
		var d DeviceMode
		var seen sql.NullTime
		if err := rows.Scan(&d.DeviceID, &d.Hostname, &d.CollectionMode, &seen); err != nil {
			return nil, fmt.Errorf("store: settings devices: %w", err)
		}
		d.LastSeenAt = nullTime(seen)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetCollectionMode implements Store.
func (s *SQLStore) SetCollectionMode(ctx context.Context, tenantID string, mode *string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := currentText(ctx, tx, SQLCurrentCollectionMode, tenantID)
		if err != nil {
			return err
		}
		var value any
		if mode != nil {
			value = *mode
		}
		if _, err := tx.ExecContext(ctx, SQLSetCollectionMode, tenantID, value); err != nil {
			if isCheckViolation(err, "tenant_collection_within_ceiling") {
				return ErrCollectionExceedsCeiling
			}
			return fmt.Errorf("store: set collection mode: %w", err)
		}
		newValue := ""
		if mode != nil {
			newValue = *mode
		}
		audit = withChange(audit, previous, newValue)
		return insertAudit(ctx, tx, audit)
	})
}

// SetScopeOverride implements Store.
func (s *SQLStore) SetScopeOverride(ctx context.Context, tenantID, fingerprint string, mode *string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := scopeOverrideValue(ctx, tx, tenantID, fingerprint)
		if err != nil {
			return err
		}
		if mode == nil {
			if _, err := tx.ExecContext(ctx, SQLClearScopeOverride, tenantID, fingerprint); err != nil {
				return fmt.Errorf("store: clear scope override: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, SQLSetScopeOverride, tenantID, fingerprint, *mode); err != nil {
			if isRaiseException(err) {
				return ErrScopeOverrideTooWide
			}
			return fmt.Errorf("store: set scope override: %w", err)
		}
		newValue := ""
		if mode != nil {
			newValue = *mode
		}
		detail := map[string]any{"tool_fingerprint": fingerprint}
		audit.Detail = mergeDetail(audit.Detail, detail)
		audit = withChange(audit, previous, newValue)
		return insertAudit(ctx, tx, audit)
	})
}

// SetRetention implements Store.
func (s *SQLStore) SetRetention(ctx context.Context, tenantID, appliesTo string, ttlDays int, audit AuditEntry) error {
	if appliesTo != "event" && appliesTo != "content" {
		return ErrRetentionOutOfRange
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := currentInt(ctx, tx, SQLCurrentRetention, tenantID, appliesTo)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLSetRetention, tenantID, appliesTo, ttlDays, audit.ActorID); err != nil {
			return fmt.Errorf("store: set retention: %w", err)
		}
		detail := map[string]any{"applies_to": appliesTo}
		audit.Detail = mergeDetail(audit.Detail, detail)
		audit = withChange(audit, previous, ttlDays)
		return insertAudit(ctx, tx, audit)
	})
}

// SetContentSearch implements Store.
func (s *SQLStore) SetContentSearch(ctx context.Context, tenantID, tier string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := currentText(ctx, tx, SQLCurrentContentSearch, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLSetContentSearch, tenantID, tier); err != nil {
			if isCheckViolation(err, "tenant_search_tier_requires_collection_mode") {
				return ErrSearchTierRequiresCeiling
			}
			return fmt.Errorf("store: set content search: %w", err)
		}
		audit = withChange(audit, previous, tier)
		return insertAudit(ctx, tx, audit)
	})
}

// SetTLSInspection implements Store.
func (s *SQLStore) SetTLSInspection(ctx context.Context, tenantID string, enabled bool, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var previous bool
		if err := tx.QueryRowContext(ctx, SQLCurrentTLSInspection, tenantID).Scan(&previous); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnknownTenant
			}
			return fmt.Errorf("store: read TLS inspection: %w", err)
		}
		if _, err := tx.ExecContext(ctx, SQLSetTLSInspection, tenantID, enabled); err != nil {
			return fmt.Errorf("store: set TLS inspection: %w", err)
		}
		audit = withChange(audit, previous, enabled)
		return insertAudit(ctx, tx, audit)
	})
}

// SetToolSanction implements Store.
func (s *SQLStore) SetToolSanction(ctx context.Context, tenantID, fingerprint, state string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := currentText(ctx, tx, SQLCurrentToolState, tenantID, fingerprint)
		if err != nil {
			return err
		}
		var decidedBy any
		var decidedAt any
		if state != "unknown" {
			decidedBy = audit.ActorID
			decidedAt = audit.OccurredAt.UTC()
		}
		if _, err := tx.ExecContext(ctx, SQLSetToolSanction, tenantID, fingerprint, state, decidedBy, decidedAt); err != nil {
			return fmt.Errorf("store: set tool sanction: %w", err)
		}
		detail := map[string]any{"tool_fingerprint": fingerprint}
		audit.Detail = mergeDetail(audit.Detail, detail)
		audit = withChange(audit, previous, state)
		return insertAudit(ctx, tx, audit)
	})
}

// SetEndpointCollectors implements Store.
func (s *SQLStore) SetEndpointCollectors(ctx context.Context, tenantID string, c EndpointCollectors, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := endpointSettings(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLSetEndpointCollectors, tenantID,
			c.Inventory, c.Processes, c.Flows, c.OTel, c.Hooks, c.HooksManagedOnly); err != nil {
			if isForeignKeyViolation(err) {
				return ErrUnknownTenant
			}
			return fmt.Errorf("store: set endpoint collectors: %w", err)
		}
		audit = withChange(audit, collectorsDetail(previous.Collectors), collectorsDetail(c))
		return insertAudit(ctx, tx, audit)
	})
}

// SetEndpointTool implements Store.
func (s *SQLStore) SetEndpointTool(ctx context.Context, tenantID, toolKey string, t EndpointTool, audit AuditEntry) error {
	if !slices.Contains(EndpointToolKeys, toolKey) {
		return ErrUnknownEndpointTool
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		previous, err := endpointSettings(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLSetEndpointTool, tenantID, toolKey, t.OTel, t.Hooks); err != nil {
			if isForeignKeyViolation(err) {
				return ErrUnknownTenant
			}
			return fmt.Errorf("store: set endpoint tool: %w", err)
		}
		audit.Detail = mergeDetail(audit.Detail, map[string]any{"tool_key": toolKey})
		audit = withChange(audit, toolDetail(previous.Tools[toolKey]), toolDetail(t))
		return insertAudit(ctx, tx, audit)
	})
}

// collectorsDetail and toolDetail are the audit's spelling of a setting: the bundle's JSON names.
func collectorsDetail(c EndpointCollectors) map[string]any {
	return map[string]any{"inventory": c.Inventory, "processes": c.Processes, "flows": c.Flows,
		"otel": c.OTel, "hooks": c.Hooks, "hooks_managed_only": c.HooksManagedOnly}
}

func toolDetail(t EndpointTool) map[string]any {
	return map[string]any{"otel": t.OTel, "hooks": t.Hooks}
}

// currentText runs a single-column text query and returns the value.
func currentText(ctx context.Context, tx *sql.Tx, query, tenantID string, args ...any) (string, error) {
	var v string
	if err := tx.QueryRowContext(ctx, query, append([]any{tenantID}, args...)...).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUnknownTenant
		}
		return "", fmt.Errorf("store: read current value: %w", err)
	}
	return v, nil
}

func currentInt(ctx context.Context, tx *sql.Tx, query, tenantID, appliesTo string) (any, error) {
	var v sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, tenantID, appliesTo).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No retention row yet is a tenant with no override, not an unknown tenant.
			return nil, nil
		}
		return nil, fmt.Errorf("store: read current value: %w", err)
	}
	if !v.Valid {
		return nil, nil
	}
	return int(v.Int64), nil
}

func scopeOverrideValue(ctx context.Context, tx *sql.Tx, tenantID, fingerprint string) (any, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, SQLCurrentScopeOverrides, tenantID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnknownTenant
		}
		return nil, fmt.Errorf("store: read scope overrides: %w", err)
	}
	m := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("store: read scope overrides: %w", err)
		}
	}
	if v, ok := m[fingerprint]; ok {
		return v, nil
	}
	return nil, nil
}

// withChange records the old and new values on an audit entry.
func withChange(a AuditEntry, previous, next any) AuditEntry {
	a.Detail = mergeDetail(a.Detail, map[string]any{"previous": previous, "new": next})
	return a
}

func mergeDetail(base map[string]any, extra map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range extra {
		if _, ok := base[k]; !ok {
			base[k] = v
		}
	}
	return base
}

func isCheckViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == name
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func isRaiseException(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "P0001"
}
