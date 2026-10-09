package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The app catalog's statements. The catalog is shared reference data, not tenant data.
const (
	// SQLAppCatalog is every app with its signals, one row per signal and one row with NULL signal
	// columns for an app without any, in the bundle's order.
	SQLAppCatalog = `
SELECT a.app_key, a.category, s.platform, s.kind, s.value
  FROM ref.app a
  LEFT JOIN ref.app_signal s ON s.app_key = a.app_key
 ORDER BY a.app_key COLLATE "C", s.platform COLLATE "C", s.kind COLLATE "C", s.value COLLATE "C"`

	// SQLAppCategories is the categories the catalog's apps fall in: what a rule may match.
	SQLAppCategories = `SELECT DISTINCT category FROM ref.app ORDER BY category`
)

// CatalogApp is one app of the catalog (ref.app) with its signals (ref.app_signal).
type CatalogApp struct {
	AppKey   string
	Category string
	Signals  []CatalogSignal
}

// CatalogSignal is one thing that identifies an app on a device.
type CatalogSignal struct {
	Platform string
	Kind     string
	Value    string
}

func appCatalog(ctx context.Context, tx *sql.Tx) ([]CatalogApp, error) {
	rows, err := tx.QueryContext(ctx, SQLAppCatalog)
	if err != nil {
		return nil, fmt.Errorf("store: app catalog: %w", err)
	}
	defer rows.Close()
	out := []CatalogApp{}
	for rows.Next() {
		var key, category string
		var platform, kind, value sql.NullString
		if err := rows.Scan(&key, &category, &platform, &kind, &value); err != nil {
			return nil, fmt.Errorf("store: app catalog: %w", err)
		}
		if n := len(out); n == 0 || out[n-1].AppKey != key {
			out = append(out, CatalogApp{AppKey: key, Category: category, Signals: []CatalogSignal{}})
		}
		if platform.Valid {
			last := &out[len(out)-1]
			last.Signals = append(last.Signals, CatalogSignal{Platform: platform.String, Kind: kind.String, Value: value.String})
		}
	}
	return out, rows.Err()
}

func appCategories(ctx context.Context, tx *sql.Tx) ([]string, error) {
	out, err := textColumn(ctx, tx, SQLAppCategories)
	if err != nil {
		return nil, fmt.Errorf("store: app categories: %w", err)
	}
	return out, nil
}
