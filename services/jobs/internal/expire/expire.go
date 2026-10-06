// Package expire deletes the rows whose retention has passed and records what it removed.
//
// Every retained row carries expires_at, fixed when the row is written. For one tenant, the job
// deletes the expired rows of each table in bounded batches, all inside one transaction scoped to
// the tenant, and in that same transaction writes an ops.erasure_receipt when anything was
// removed. A receipt therefore exists exactly when a deletion committed, and states what it
// removed. A submission that outlives its stored content is marked shredded by retention in the
// statement that deletes the content.
package expire

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// BatchSize bounds every DELETE, so that no single statement approaches the server's statement
// timeout however large a tenant's backlog is.
const BatchSize = 5000

// table is one table whose rows expire, with the DELETE that removes up to $2 of the tenant's
// expired rows.
type table struct {
	name      string
	deleteSQL string
}

// deleteBatchSQL selects a batch by primary key (%[2]s, the key columns after tenant_id) and
// deletes exactly those rows. now() is the transaction's start time, so every table in a pass
// uses the same cutoff.
const deleteBatchSQL = `
DELETE FROM %[1]s
 WHERE tenant_id = $1
   AND (%[2]s) IN (SELECT %[2]s
                     FROM %[1]s
                    WHERE tenant_id = $1 AND expires_at < now()
                    LIMIT $2)`

func newTable(name, key string) table {
	return table{name: name, deleteSQL: fmt.Sprintf(deleteBatchSQL, name, key)}
}

// contentDeleteSQL deletes a batch of expired content and, in the same statement, marks each
// submission that still claims that content as shredded by retention, so a submission never
// reports content that is gone. The batch CTE is referenced twice, so PostgreSQL materialises it
// once and the UPDATE and the DELETE act on the same rows. The DELETE is the outer statement, so
// the statement's row count is the number of content rows removed, as for every other table.
const contentDeleteSQL = `
WITH batch AS (
  SELECT object_id, submission_id
    FROM ops.content
   WHERE tenant_id = $1 AND expires_at < now()
   LIMIT $2
),
shredded AS (
  UPDATE ingest.submission s
     SET content_state = 'shredded', shredded_reason = 'retention_expired'
    FROM batch
   WHERE s.tenant_id = $1
     AND s.submission_id = batch.submission_id
     AND s.content_state = 'uploaded'
)
DELETE FROM ops.content c
 USING batch
 WHERE c.tenant_id = $1 AND c.object_id = batch.object_id`

// tables, in deletion order. ingest.search_text also cascades from ingest.submission; deleting its
// expired entries first means the receipt counts them instead of leaving them to the cascade.
var tables = []table{
	newTable("ingest.search_text", "submission_id, unit_kind, unit_index"),
	{name: "ops.content", deleteSQL: contentDeleteSQL},
	newTable("ingest.observation", "event_id"),
	newTable("ingest.submission", "submission_id"),
	newTable("ingest.rejected", "rejected_id"),
}

// retentionPathSQL opens the retention path for the rest of the transaction. ingest.observation
// is append-only, and its trigger admits a DELETE only in a transaction that has set this flag, so
// whole-record expiry is distinguishable from any other removal.
const retentionPathSQL = `SET LOCAL sac.retention_delete = on`

// receiptSQL records one retention receipt. requested_at is the transaction's start, the cutoff
// every DELETE used; completed_at is the moment the receipt is written.
const receiptSQL = `
INSERT INTO ops.erasure_receipt
  (tenant_id, receipt_id, scope_kind, requested_by, requested_at, completed_at, mechanisms, removed_counts)
VALUES ($1, gen_random_uuid(), 'retention', 'expire-job', now(), clock_timestamp(), '{database}', $2::jsonb)`

// Execer is satisfied by *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Tenant deletes one tenant's expired rows and, when any were removed, writes the receipt. tx must
// already be scoped to the tenant; the caller commits. It returns the rows removed per table.
func Tenant(ctx context.Context, tx Execer, tenant string) (map[string]int64, error) {
	return expireTenant(ctx, tx, tenant, BatchSize)
}

func expireTenant(ctx context.Context, tx Execer, tenant string, batch int) (map[string]int64, error) {
	if _, err := tx.ExecContext(ctx, retentionPathSQL); err != nil {
		return nil, fmt.Errorf("expire: open retention path: %w", err)
	}
	removed := make(map[string]int64, len(tables))
	var total int64
	for _, t := range tables {
		n, err := deleteExpired(ctx, tx, t, tenant, batch)
		if err != nil {
			return nil, fmt.Errorf("expire: %s: %w", t.name, err)
		}
		removed[t.name] = n
		total += n
	}
	if total == 0 {
		return removed, nil
	}
	counts, err := json.Marshal(removed)
	if err != nil {
		return nil, fmt.Errorf("expire: encode receipt counts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, receiptSQL, tenant, string(counts)); err != nil {
		return nil, fmt.Errorf("expire: write receipt: %w", err)
	}
	return removed, nil
}

// deleteExpired deletes batches until one comes back short of the batch size, which means no
// expired row of the tenant is left in the table.
func deleteExpired(ctx context.Context, tx Execer, t table, tenant string, batch int) (int64, error) {
	var total int64
	for {
		res, err := tx.ExecContext(ctx, t.deleteSQL, tenant, batch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
