// Package erase performs subject erasure: the removal of one person's events, findings and stored
// prompts, requested by an admin and recorded in ops.erasure_request.
//
// For one tenant, the job reads the pending requests and, for each, deletes the subject's rows in
// bounded batches inside the tenant's transaction, then writes an ops.erasure_receipt (scope_kind
// 'subject') stating what was removed and marks the request complete. The receipt therefore exists
// exactly when a deletion committed.
//
// It mirrors package expire: observations are immutable, so their deletion needs the
// sac.retention_delete session flag; a submission deletion cascades to mart.finding,
// ops.finding_review and ingest.search_text; and stored content is deleted as rows without being
// read, so content-vault remains the only component that can decrypt it.
package erase

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// BatchSize bounds every DELETE, so no single statement approaches the server's statement timeout
// however much a subject has sent.
const BatchSize = 5000

// retentionPathSQL opens the retention path for the rest of the transaction, exactly as expire does,
// so a subject's observations are distinguishable from any other removal.
const retentionPathSQL = `SET LOCAL sac.retention_delete = on`

// pendingSQL lists the requests the job has not completed.
const pendingSQL = `
SELECT request_id::text, subject_ref, requested_by
  FROM ops.erasure_request
 WHERE tenant_id = $1::uuid AND completed_at IS NULL
 ORDER BY requested_at, request_id`

// The subject's stored content, deleted by submission or by event so an object whose submission was
// not yet known at upload time is still removed.
const contentDeleteSQL = `
DELETE FROM ops.content c
 WHERE c.tenant_id = $1::uuid
   AND (c.submission_id IN (SELECT submission_id FROM ingest.submission
                             WHERE tenant_id = $1::uuid AND user_ref = $2::text)
        OR c.event_id IN (SELECT event_id FROM ingest.observation
                           WHERE tenant_id = $1::uuid AND user_ref = $2::text))`

// The search index rows die with the submission, but deleting them first means the receipt counts
// them instead of leaving them to the cascade.
const searchTextDeleteSQL = `
DELETE FROM ingest.search_text
 WHERE tenant_id = $1::uuid
   AND submission_id IN (SELECT submission_id FROM ingest.submission
                          WHERE tenant_id = $1::uuid AND user_ref = $2::text)`

// Findings are derived from the submission and are deleted with it; counted first so the receipt
// states them.
const findingCountSQL = `
SELECT count(*) FROM mart.finding
 WHERE tenant_id = $1::uuid
   AND submission_id IN (SELECT submission_id FROM ingest.submission
                          WHERE tenant_id = $1::uuid AND user_ref = $2::text)`

// deleteBatchSQL removes up to $3 of a subject's rows, keyed by the primary key after tenant_id.
const deleteBatchSQL = `
DELETE FROM %[1]s
 WHERE tenant_id = $1::uuid
   AND (%[2]s) IN (SELECT %[2]s
                     FROM %[1]s
                    WHERE tenant_id = $1::uuid AND user_ref = $2::text
                    LIMIT $3::int)`

// receiptSQL records one subject receipt. requested_at is now(), the moment the removal ran;
// completed_at is clock_timestamp(), the moment the receipt was written.
const receiptSQL = `
INSERT INTO ops.erasure_receipt
  (tenant_id, receipt_id, scope_kind, subject_ref, requested_by, requested_at, completed_at, mechanisms, removed_counts)
VALUES ($1::uuid, gen_random_uuid(), 'subject', $2::text, $3::text, now(), clock_timestamp(), '{database}', $4::jsonb)`

// completeSQL marks a request done, with the receipt that proves it.
const completeSQL = `
UPDATE ops.erasure_request
   SET completed_at = now()
 WHERE tenant_id = $1::uuid AND request_id = $2::uuid`

// request is one pending erasure.
type request struct {
	ID          string
	SubjectRef  string
	RequestedBy string
}

// tx is the part of *sql.Tx the job uses, so a test can record its statements.
type tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// execer is the write half; a test can fake it without rows.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// querier is the read half.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Tenant performs every pending erasure for one tenant. tx must already be scoped to the tenant;
// the caller commits. It returns the rows removed per table across all requests.
func Tenant(ctx context.Context, tx *sql.Tx, tenant string) (map[string]int64, error) {
	return eraseTenant(ctx, tx, tenant, BatchSize)
}

func eraseTenant(ctx context.Context, tx tx, tenant string, batch int) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, pendingSQL, tenant)
	if err != nil {
		return nil, fmt.Errorf("erase: list pending: %w", err)
	}
	var requests []request
	for rows.Next() {
		var r request
		if err := rows.Scan(&r.ID, &r.SubjectRef, &r.RequestedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("erase: read pending: %w", err)
		}
		requests = append(requests, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("erase: list pending: %w", err)
	}

	removed := make(map[string]int64)
	for _, r := range requests {
		counts, err := eraseSubject(ctx, tx, tenant, r, batch)
		if err != nil {
			return nil, err
		}
		for name, n := range counts {
			removed[name] += n
		}
	}
	return removed, nil
}

func eraseSubject(ctx context.Context, tx tx, tenant string, r request, batch int) (map[string]int64, error) {
	if _, err := tx.ExecContext(ctx, retentionPathSQL); err != nil {
		return nil, fmt.Errorf("erase: open retention path: %w", err)
	}

	counts := make(map[string]int64)

	findings, err := count(ctx, tx, findingCountSQL, tenant, r.SubjectRef)
	if err != nil {
		return nil, fmt.Errorf("erase: count findings: %w", err)
	}
	counts["mart.finding"] = findings

	for _, name := range []string{"ingest.search_text", "ops.content"} {
		stmt := searchTextDeleteSQL
		if name == "ops.content" {
			stmt = contentDeleteSQL
		}
		res, err := tx.ExecContext(ctx, stmt, tenant, r.SubjectRef)
		if err != nil {
			return nil, fmt.Errorf("erase: %s: %w", name, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("erase: %s: %w", name, err)
		}
		counts[name] = n
	}

	for _, spec := range []struct{ table, key string }{
		{"ingest.observation", "event_id"},
		{"ingest.submission", "submission_id"},
	} {
		n, err := deleteAll(ctx, tx, spec.table, spec.key, tenant, r.SubjectRef, batch)
		if err != nil {
			return nil, err
		}
		counts[spec.table] = n
	}

	raw, err := json.Marshal(counts)
	if err != nil {
		return nil, fmt.Errorf("erase: encode receipt counts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, receiptSQL, tenant, r.SubjectRef, r.RequestedBy, string(raw)); err != nil {
		return nil, fmt.Errorf("erase: write receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, completeSQL, tenant, r.ID); err != nil {
		return nil, fmt.Errorf("erase: mark complete: %w", err)
	}
	return counts, nil
}

// count runs a single-row count statement.
func count(ctx context.Context, q querier, query, tenant, subject string) (int64, error) {
	rows, err := q.QueryContext(ctx, query, tenant, subject)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, nil
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}

// deleteAll deletes a subject's rows in batches until one comes back short of the batch size.
func deleteAll(ctx context.Context, e execer, table, key, tenant, subject string, batch int) (int64, error) {
	stmt := fmt.Sprintf(deleteBatchSQL, table, key)
	var total int64
	for {
		res, err := e.ExecContext(ctx, stmt, tenant, subject, batch)
		if err != nil {
			return total, fmt.Errorf("erase: %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("erase: %s: %w", table, err)
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
