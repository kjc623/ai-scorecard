package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The enforcement rules' statements, and the two other inputs a rule is written against: the
// sanctioned tools and the data classes a label may name beside the tenant's custom labels.
const (
	SQLEnforcementRules = `
SELECT rule_id, action, match, message, coalesce(link, ''), users, enabled
  FROM ops.enforcement_rule
 WHERE tenant_id = $1::uuid
 ORDER BY position`

	SQLRulesTenant = `SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid`

	// SQLLockEnforcementRules confirms the tenant exists and serialises replacements of its list,
	// so two concurrent saves apply one after the other rather than colliding on a position.
	SQLLockEnforcementRules = `SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid FOR NO KEY UPDATE`

	SQLDeleteEnforcementRules = `DELETE FROM ops.enforcement_rule WHERE tenant_id = $1::uuid`

	SQLInsertEnforcementRule = `
INSERT INTO ops.enforcement_rule (tenant_id, position, rule_id, action, match, message, link, updated_by, updated_at, users, enabled)
VALUES ($1::uuid, $2::int, $3::text, $4::text, $5::jsonb, $6::text, nullif($7::text, ''), $8::text, $9::timestamptz,
        $10::jsonb, $11::boolean)`

	// SQLRuleUserRefs is, by rule id, the refs an enabled rule's users reach: every member's
	// canonical ref and each alias of it.
	SQLRuleUserRefs = ruleUserMembers + `,
ref AS (
  SELECT rule_id, user_ref FROM member WHERE enabled
  UNION
  SELECT m.rule_id, a.alias_ref
    FROM member m
    JOIN ops.user_ref_alias a ON a.tenant_id = $1::uuid AND a.user_ref = m.user_ref
   WHERE m.enabled
)
SELECT rule_id, jsonb_agg(user_ref ORDER BY user_ref COLLATE "C")
  FROM ref
 GROUP BY rule_id
 ORDER BY rule_id`

	// SQLRuleUsers names each value the tenant's rules' users hold, the way the people list names a
	// person: the directory's display name, else the account name their device last reported.
	SQLRuleUsers = ruleUserMembers + `
SELECT p.value,
       CASE p.kind
         WHEN 'group' THEN (SELECT g.display_name FROM ops.scim_group g WHERE g.tenant_id = $1::uuid AND g.scim_id::text = p.ref)
         WHEN 'team' THEN (SELECT t.name FROM ops.team t WHERE t.tenant_id = $1::uuid AND t.team_id::text = p.ref)
         WHEN 'user' THEN coalesce(
           (SELECT ud.display_name FROM ops.user_dim ud WHERE ud.tenant_id = $1::uuid AND ud.user_ref = p.ref),
           (SELECT d.last_subject_name FROM ops.device d
             WHERE d.tenant_id = $1::uuid AND d.last_user_ref = p.ref AND d.last_subject_name IS NOT NULL
             ORDER BY d.last_seen_at DESC NULLS LAST LIMIT 1), '')
         ELSE p.ref
       END,
       (SELECT count(DISTINCT m.user_ref) FROM member m WHERE m.value = p.value)
  FROM (SELECT DISTINCT value, kind, ref FROM pick) p
 ORDER BY p.value COLLATE "C"`

	// SQLSanctionedTools is every catalogue fingerprint of a tool the tenant has sanctioned: what
	// the bundle names, since devices match fingerprints.
	SQLSanctionedTools = `
SELECT c.tool_fingerprint
  FROM ops.tool_sanction s
  JOIN ref.tool_catalogue c ON c.app_key = s.tool_key
 WHERE s.tenant_id = $1::uuid AND s.sanctioned_state = 'sanctioned'
 ORDER BY c.tool_fingerprint COLLATE "C"`

	// SQLToolFingerprints is the catalogue's fingerprints by tool key, which a rule's tools list
	// expands to.
	SQLToolFingerprints = `
SELECT app_key, jsonb_agg(tool_fingerprint ORDER BY tool_fingerprint COLLATE "C")
  FROM ref.tool_catalogue
 GROUP BY app_key
 ORDER BY app_key COLLATE "C"`

	SQLDataClasses = `SELECT class_code FROM ref.data_class ORDER BY class_code`
)

// ruleUserMembers expands every enforcement rule's users to the canonical refs of their members, as
// member (rule_id, enabled, value, user_ref): a person is their own member; a group, department,
// unit or team has the members ops.v_team_member would give it. A group or team since removed has
// none.
const ruleUserMembers = `
WITH pick AS (
  SELECT r.rule_id, r.enabled, u.value, split_part(u.value, ':', 1) AS kind, substr(u.value, strpos(u.value, ':') + 1) AS ref
    FROM ops.enforcement_rule r
   CROSS JOIN LATERAL jsonb_array_elements_text(r.users) AS u(value)
   WHERE r.tenant_id = $1::uuid
),
member AS (
  SELECT p.rule_id, p.enabled, p.value, p.ref AS user_ref FROM pick p WHERE p.kind = 'user'
  UNION
  SELECT p.rule_id, p.enabled, p.value, su.user_ref
    FROM pick p
    JOIN ops.scim_group_member gm ON gm.tenant_id = $1::uuid AND gm.group_id::text = p.ref
    JOIN ops.scim_user su ON su.tenant_id = gm.tenant_id AND su.scim_id = gm.user_id
   WHERE p.kind = 'group' AND su.active
  UNION
  SELECT p.rule_id, p.enabled, p.value, tm.user_ref
    FROM pick p
    JOIN ops.v_team_member tm ON tm.tenant_id = $1::uuid AND tm.team_id::text = p.ref
   WHERE p.kind = 'team'
  UNION
  SELECT p.rule_id, p.enabled, p.value, ud.user_ref
    FROM pick p
    JOIN ops.user_dim ud ON ud.tenant_id = $1::uuid
   WHERE (p.kind = 'department' AND lower(ud.department) = lower(p.ref))
      OR (p.kind = 'org_unit'
          AND (lower(ud.org_unit) = lower(p.ref)
               OR right(lower(ud.org_unit), length(p.ref) + 1) = ',' || lower(p.ref)))
)`

// matchJSON is the stored and audited spelling of a rule's match lists: the bundle's names, every
// list present.
type matchJSON struct {
	Labels     []string `json:"labels"`
	Tools      []string `json:"tools"`
	Categories []string `json:"categories"`
	Sanction   []string `json:"sanction"`
	Routes     []string `json:"routes"`
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// MatchJSON is the match lists as ops.enforcement_rule.match stores them.
func MatchJSON(m RuleMatch) ([]byte, error) {
	return json.Marshal(matchJSON{Labels: nonNil(m.Labels), Tools: nonNil(m.Tools),
		Categories: nonNil(m.Categories), Sanction: nonNil(m.Sanction), Routes: nonNil(m.Routes)})
}

// RulesDetail is the audit's spelling of a rule list: the admin API's names, in order.
func RulesDetail(rules []EnforcementRule) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		d := map[string]any{"rule_id": r.RuleID, "action": r.Action, "message": r.Message,
			"match": map[string]any{"labels": nonNil(r.Match.Labels), "tools": nonNil(r.Match.Tools),
				"categories": nonNil(r.Match.Categories), "sanction": nonNil(r.Match.Sanction), "routes": nonNil(r.Match.Routes)}}
		if r.Link != "" {
			d["link"] = r.Link
		}
		d["users"], d["enabled"] = nonNil(r.Users), !r.Disabled
		out = append(out, d)
	}
	return out
}

func enforcementRules(ctx context.Context, tx *sql.Tx, tenantID string) ([]EnforcementRule, error) {
	rows, err := tx.QueryContext(ctx, SQLEnforcementRules, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: enforcement rules: %w", err)
	}
	defer rows.Close()
	out := []EnforcementRule{}
	for rows.Next() {
		var r EnforcementRule
		var raw, users []byte
		var enabled bool
		if err := rows.Scan(&r.RuleID, &r.Action, &raw, &r.Message, &r.Link, &users, &enabled); err != nil {
			return nil, fmt.Errorf("store: enforcement rules: %w", err)
		}
		r.Disabled = !enabled
		if err := json.Unmarshal(users, &r.Users); err != nil {
			return nil, fmt.Errorf("store: enforcement rule %s users: %w", r.RuleID, err)
		}
		r.Users = nonNil(r.Users)
		var m matchJSON
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("store: enforcement rule %s match: %w", r.RuleID, err)
		}
		r.Match = RuleMatch{Labels: nonNil(m.Labels), Tools: nonNil(m.Tools), Categories: nonNil(m.Categories),
			Sanction: nonNil(m.Sanction), Routes: nonNil(m.Routes)}
		out = append(out, r)
	}
	return out, rows.Err()
}

func textColumn(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func sanctionedTools(ctx context.Context, tx *sql.Tx, tenantID string) ([]string, error) {
	out, err := textColumn(ctx, tx, SQLSanctionedTools, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: sanctioned tools: %w", err)
	}
	return out, nil
}

func toolFingerprints(ctx context.Context, tx *sql.Tx) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx, SQLToolFingerprints)
	if err != nil {
		return nil, fmt.Errorf("store: tool fingerprints: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var key string
		var fingerprints []byte
		if err := rows.Scan(&key, &fingerprints); err != nil {
			return nil, fmt.Errorf("store: tool fingerprints: %w", err)
		}
		var list []string
		if err := json.Unmarshal(fingerprints, &list); err != nil {
			return nil, fmt.Errorf("store: tool fingerprints: %w", err)
		}
		out[key] = list
	}
	return out, rows.Err()
}

func dataClasses(ctx context.Context, tx *sql.Tx) ([]string, error) {
	out, err := textColumn(ctx, tx, SQLDataClasses)
	if err != nil {
		return nil, fmt.Errorf("store: data classes: %w", err)
	}
	return out, nil
}

// tenantRow runs a query that returns one row for an existing tenant.
func tenantRow(ctx context.Context, tx *sql.Tx, query, tenantID string) error {
	var one int
	if err := tx.QueryRowContext(ctx, query, tenantID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		return fmt.Errorf("store: read tenant: %w", err)
	}
	return nil
}

// EnforcementRules implements Store.
func (s *SQLStore) EnforcementRules(ctx context.Context, tenantID string) ([]EnforcementRule, error) {
	var out []EnforcementRule
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := tenantRow(ctx, tx, SQLRulesTenant, tenantID); err != nil {
			return err
		}
		var err error
		out, err = enforcementRules(ctx, tx, tenantID)
		return err
	})
	return out, err
}

// ReplaceEnforcementRules implements Store.
func (s *SQLStore) ReplaceEnforcementRules(ctx context.Context, tenantID string, rules []EnforcementRule, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := tenantRow(ctx, tx, SQLLockEnforcementRules, tenantID); err != nil {
			return err
		}
		previous, err := enforcementRules(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLDeleteEnforcementRules, tenantID); err != nil {
			return fmt.Errorf("store: delete enforcement rules: %w", err)
		}
		at := audit.OccurredAt.UTC()
		for i, r := range rules {
			match, err := MatchJSON(r.Match)
			if err != nil {
				return fmt.Errorf("store: enforcement rule %s match: %w", r.RuleID, err)
			}
			users, err := json.Marshal(nonNil(r.Users))
			if err != nil {
				return fmt.Errorf("store: enforcement rule %s users: %w", r.RuleID, err)
			}
			if _, err := tx.ExecContext(ctx, SQLInsertEnforcementRule, tenantID, i, r.RuleID, r.Action, match,
				r.Message, r.Link, audit.ActorID, at, users, !r.Disabled); err != nil {
				if isCheckViolation(err, "enforcement_rule_labels_known") {
					return ErrUnknownRuleLabel
				}
				return fmt.Errorf("store: insert enforcement rule %s: %w", r.RuleID, err)
			}
		}
		audit = withChange(audit, RulesDetail(previous), RulesDetail(rules))
		return insertAudit(ctx, tx, audit)
	})
}

func ruleUserRefs(ctx context.Context, tx *sql.Tx, tenantID string) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx, SQLRuleUserRefs, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: rule user refs: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("store: rule user refs: %w", err)
		}
		var refs []string
		if err := json.Unmarshal(raw, &refs); err != nil {
			return nil, fmt.Errorf("store: rule user refs: %w", err)
		}
		out[id] = refs
	}
	return out, rows.Err()
}

// RuleUsers implements Store.
func (s *SQLStore) RuleUsers(ctx context.Context, tenantID string) ([]RuleUser, error) {
	out := []RuleUser{}
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := tenantRow(ctx, tx, SQLRulesTenant, tenantID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, SQLRuleUsers, tenantID)
		if err != nil {
			return fmt.Errorf("store: rule users: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var u RuleUser
			var name sql.NullString
			if err := rows.Scan(&u.Value, &name, &u.Members); err != nil {
				return fmt.Errorf("store: rule users: %w", err)
			}
			u.Name, u.Found = name.String, name.Valid
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}
