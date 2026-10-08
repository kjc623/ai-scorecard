package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The enforcement rules' statements, and the two other inputs a rule is written against: the
// sanctioned tools and the data classes a label may name.
const (
	SQLEnforcementRules = `
SELECT rule_id, action, match, message, coalesce(link, '')
  FROM ops.enforcement_rule
 WHERE tenant_id = $1::uuid
 ORDER BY position`

	SQLRulesTenant = `SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid`

	// SQLLockEnforcementRules confirms the tenant exists and serialises replacements of its list,
	// so two concurrent saves apply one after the other rather than colliding on a position.
	SQLLockEnforcementRules = `SELECT 1 FROM ops.tenant WHERE tenant_id = $1::uuid FOR NO KEY UPDATE`

	SQLDeleteEnforcementRules = `DELETE FROM ops.enforcement_rule WHERE tenant_id = $1::uuid`

	SQLInsertEnforcementRule = `
INSERT INTO ops.enforcement_rule (tenant_id, position, rule_id, action, match, message, link, updated_by, updated_at)
VALUES ($1::uuid, $2::int, $3::text, $4::text, $5::jsonb, $6::text, nullif($7::text, ''), $8::text, $9::timestamptz)`

	SQLSanctionedTools = `
SELECT tool_fingerprint FROM ops.tool
 WHERE tenant_id = $1::uuid AND sanctioned_state = 'sanctioned'
 ORDER BY tool_fingerprint`

	SQLDataClasses = `SELECT class_code FROM ref.data_class ORDER BY class_code`
)

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

// RulesDetail is the audit's spelling of a rule list: the bundle's names, in order.
func RulesDetail(rules []EnforcementRule) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		d := map[string]any{"rule_id": r.RuleID, "action": r.Action, "message": r.Message,
			"match": map[string]any{"labels": nonNil(r.Match.Labels), "tools": nonNil(r.Match.Tools),
				"categories": nonNil(r.Match.Categories), "sanction": nonNil(r.Match.Sanction), "routes": nonNil(r.Match.Routes)}}
		if r.Link != "" {
			d["link"] = r.Link
		}
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
		var raw []byte
		if err := rows.Scan(&r.RuleID, &r.Action, &raw, &r.Message, &r.Link); err != nil {
			return nil, fmt.Errorf("store: enforcement rules: %w", err)
		}
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
			if _, err := tx.ExecContext(ctx, SQLInsertEnforcementRule, tenantID, i, r.RuleID, r.Action, match,
				r.Message, r.Link, audit.ActorID, at); err != nil {
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
