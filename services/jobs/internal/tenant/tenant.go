// Package tenant lists the tenants and runs one tenant's work in a transaction that row-level
// security confines to that tenant.
package tenant

import (
	"context"
	"database/sql"
	"fmt"
)

// listSQL returns every tenant id. ops.tenant is under forced row-level security, so a session
// with no tenant set sees none of its rows; ops.tenant_ids() is the SECURITY DEFINER function that
// lists them for the jobs' role.
const listSQL = `SELECT id::text FROM ops.tenant_ids() AS t(id) ORDER BY 1`

// scopeSQL puts the tenant on the transaction. The third argument makes the setting local to the
// transaction, so a pooled connection never carries one tenant's scope into the next tenant's work.
const scopeSQL = `SELECT set_config('app.tenant_id', $1, true)`

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// IDs returns every tenant id, in a stable order.
func IDs(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenants: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return ids, nil
}

// Scope confines every later statement in tx to tenant.
func Scope(ctx context.Context, tx *sql.Tx, tenant string) error {
	if _, err := tx.ExecContext(ctx, scopeSQL, tenant); err != nil {
		return fmt.Errorf("scope transaction to tenant: %w", err)
	}
	return nil
}

// Run executes fn in a transaction scoped to tenant. It commits only when fn succeeds; otherwise
// the transaction rolls back and nothing fn wrote is kept.
func Run(ctx context.Context, db *sql.DB, tenant string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := Scope(ctx, tx, tenant); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
