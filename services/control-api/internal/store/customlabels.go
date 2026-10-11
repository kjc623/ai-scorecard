package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
)

// The custom labels' statements.
const (
	SQLCustomLabels = `
SELECT label, display_name, terms
  FROM ops.custom_label
 WHERE tenant_id = $1::uuid
 ORDER BY display_name, label`

	SQLUpsertCustomLabel = `
INSERT INTO ops.custom_label (tenant_id, label, display_name, terms, updated_by, updated_at)
VALUES ($1::uuid, $2::text, $3::text, $4::jsonb, $5::text, $6::timestamptz)
ON CONFLICT (tenant_id, label) DO UPDATE
   SET display_name = EXCLUDED.display_name, terms = EXCLUDED.terms,
       updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at
 WHERE (ops.custom_label.display_name, ops.custom_label.terms) IS DISTINCT FROM (EXCLUDED.display_name, EXCLUDED.terms)`

	SQLDeleteCustomLabel = `DELETE FROM ops.custom_label WHERE tenant_id = $1::uuid AND label = $2::text`
)

// MaxCustomLabels is the most custom labels a tenant may hold.
const MaxCustomLabels = 50

// MaxCustomLabelTerms is the most terms one custom label may hold, across its four lists.
const MaxCustomLabelTerms = 200

// CustomLabel is one of the tenant's own labels (ops.custom_label). Label is the class code a rule
// names and an envelope carries; the device labels a prompt with it when the prompt's text holds a
// word or customer name, or an attached file has one of the extensions or a name matching one of
// the file name patterns. The store reads every list as non-nil.
type CustomLabel struct {
	Label          string
	DisplayName    string
	Words          []string
	CustomerNames  []string
	FileExtensions []string
	FileNames      []string
}

// termsJSON is the stored and audited spelling of a custom label's lists, every list present.
type termsJSON struct {
	Words          []string `json:"words"`
	CustomerNames  []string `json:"customer_names"`
	FileExtensions []string `json:"file_extensions"`
	FileNames      []string `json:"file_names"`
}

func termsOf(l CustomLabel) termsJSON {
	return termsJSON{Words: nonNil(l.Words), CustomerNames: nonNil(l.CustomerNames),
		FileExtensions: nonNil(l.FileExtensions), FileNames: nonNil(l.FileNames)}
}

// CustomLabelsDetail is the audit's spelling of a custom label list.
func CustomLabelsDetail(labels []CustomLabel) []any {
	out := make([]any, 0, len(labels))
	for _, l := range labels {
		t := termsOf(l)
		out = append(out, map[string]any{"label": l.Label, "display_name": l.DisplayName, "words": t.Words,
			"customer_names": t.CustomerNames, "file_extensions": t.FileExtensions, "file_names": t.FileNames})
	}
	return out
}

func customLabels(ctx context.Context, tx *sql.Tx, tenantID string) ([]CustomLabel, error) {
	rows, err := tx.QueryContext(ctx, SQLCustomLabels, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: custom labels: %w", err)
	}
	defer rows.Close()
	out := []CustomLabel{}
	for rows.Next() {
		var l CustomLabel
		var raw []byte
		if err := rows.Scan(&l.Label, &l.DisplayName, &raw); err != nil {
			return nil, fmt.Errorf("store: custom labels: %w", err)
		}
		var t termsJSON
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("store: custom label %s terms: %w", l.Label, err)
		}
		l.Words, l.CustomerNames = nonNil(t.Words), nonNil(t.CustomerNames)
		l.FileExtensions, l.FileNames = nonNil(t.FileExtensions), nonNil(t.FileNames)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ReplaceCustomLabels implements Store.
func (s *SQLStore) ReplaceCustomLabels(ctx context.Context, tenantID string, labels []CustomLabel, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		// The enforcement rules' lock also serialises label lists: a rule save and a label save
		// for one tenant then see each other's result.
		if err := tenantRow(ctx, tx, SQLLockEnforcementRules, tenantID); err != nil {
			return err
		}
		previous, err := customLabels(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		kept := make([]string, 0, len(labels))
		for _, l := range labels {
			kept = append(kept, l.Label)
		}
		for _, p := range previous {
			if slices.Contains(kept, p.Label) {
				continue
			}
			if _, err := tx.ExecContext(ctx, SQLDeleteCustomLabel, tenantID, p.Label); err != nil {
				if isCheckViolation(err, "custom_label_in_use") {
					return ErrCustomLabelInUse
				}
				return fmt.Errorf("store: delete custom label %s: %w", p.Label, err)
			}
		}
		at := audit.OccurredAt.UTC()
		for _, l := range labels {
			terms, err := json.Marshal(termsOf(l))
			if err != nil {
				return fmt.Errorf("store: custom label %s terms: %w", l.Label, err)
			}
			if _, err := tx.ExecContext(ctx, SQLUpsertCustomLabel, tenantID, l.Label, l.DisplayName, terms, audit.ActorID, at); err != nil {
				return fmt.Errorf("store: write custom label %s: %w", l.Label, err)
			}
		}
		audit = withChange(audit, CustomLabelsDetail(previous), CustomLabelsDetail(labels))
		return insertAudit(ctx, tx, audit)
	})
}
