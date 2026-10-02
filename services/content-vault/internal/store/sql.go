package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func protocolMode(s string) protocol.CollectionMode { return protocol.CollectionMode(s) }

// =====================================================================================
// Every SQL statement this service issues, in one file.
// =====================================================================================
//
// The rule this file exists to hold: the vault has exactly one place where SQL appears, so the
// integration seam with db/schema.sql is reviewable in one screen and the statement *text* can be
// executed against the real schema by a test without going through a Go driver. Every operation is
// a single statement (or a short fixed sequence inside one transaction), which is what makes the
// audit-before-serve ordering and the single-use claim checkable by reading rather than by
// trusting.
//
// The retrieval-grant statements target ops.retrieval_grant, which the database owner landed with
// the two CHECKs this service asked for and a trigger that refuses an unguarded UPDATE of a
// redeemed grant. SQLRetrievalGrantDDL below is the shape as requested, kept so a future
// migration can be diffed against what this service actually needs.
// add the table.

const (
	// SQLSetTenant sets the row-level-security session tenant. ops.current_tenant() reads this
	// setting; a session that has not set it reads zero rows, which is C32's fail-closed default.
	// `true` makes it transaction-local, so a pooled connection cannot leak one tenant's session
	// into the next request. The cast on $1 is required, not cosmetic: set_config's parameters are
	// not inferable from a bare placeholder, and PostgreSQL refuses to prepare the statement
	// without it — a defect the live-schema harness found rather than a style preference.
	SQLSetTenant = `SELECT set_config('app.tenant_id', $1::text, true)`

	// SQLTenant reads the tenant row the vault branches on: custody decides which key store the
	// KEK lives in (§6.2), and content_search decides which search tiers are available (§6.1).
	SQLTenant = `
SELECT tenant_id::text, name, status, key_custody, COALESCE(kek_id, ''), ceiling_mode,
       content_search, ingest_enabled, read_enabled
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	// SQLPutContentObject writes the row that holds the wrapped DEK. The conditional update
	// refuses to resurrect a shredded object: a late finaliser for content that was erased must
	// not be able to put it back, and with ON CONFLICT ... WHERE the statement simply affects no
	// row, which the caller turns into an error rather than a silent success.
	SQLPutContentObject = `
INSERT INTO ops.content_object (
    tenant_id, object_id, submission_id, event_id, blob_path, ciphertext_sha256,
    plaintext_size_bytes, wrapped_dek, kek_id, kek_version, retention_class, state,
    created_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::text, $6::text,
        $7::bigint, $8::bytea, $9::text, $10::text, $11::text, 'uploaded',
        $12::timestamptz, $13::timestamptz)
ON CONFLICT (tenant_id, object_id) DO UPDATE
   SET submission_id        = EXCLUDED.submission_id,
       event_id             = EXCLUDED.event_id,
       blob_path            = EXCLUDED.blob_path,
       ciphertext_sha256    = EXCLUDED.ciphertext_sha256,
       plaintext_size_bytes = EXCLUDED.plaintext_size_bytes,
       wrapped_dek          = EXCLUDED.wrapped_dek,
       kek_id               = EXCLUDED.kek_id,
       kek_version          = EXCLUDED.kek_version,
       retention_class      = EXCLUDED.retention_class,
       state                = 'uploaded',
       expires_at           = EXCLUDED.expires_at
 WHERE ops.content_object.state <> 'shredded'
RETURNING object_id::text`

	// SQLContentObject reads one object.
	SQLContentObject = `
SELECT tenant_id::text, object_id::text, COALESCE(submission_id::text, ''), COALESCE(event_id::text, ''),
       blob_path, ciphertext_sha256, plaintext_size_bytes, wrapped_dek, kek_id, kek_version,
       retention_class, state, COALESCE(shredded_reason, ''), created_at, expires_at, shredded_at
  FROM ops.content_object
 WHERE tenant_id = $1::uuid AND object_id = $2::uuid`

	// SQLObjectForEvent resolves an event's content object: the retrieval path starts from an
	// event id, not an object id (docs/02 §11).
	SQLObjectForEvent = `
SELECT tenant_id::text, object_id::text, COALESCE(submission_id::text, ''), COALESCE(event_id::text, ''),
       blob_path, ciphertext_sha256, plaintext_size_bytes, wrapped_dek, kek_id, kek_version,
       retention_class, state, COALESCE(shredded_reason, ''), created_at, expires_at, shredded_at
  FROM ops.content_object
 WHERE tenant_id = $1::uuid AND event_id = $2::uuid
 ORDER BY created_at DESC
 LIMIT 1`

	// SQLObjectsForTenant lists what a rotation must re-wrap. Only uploaded rows: a shredded
	// object has no key to re-wrap.
	SQLObjectsForTenant = `
SELECT tenant_id::text, object_id::text, COALESCE(submission_id::text, ''), COALESCE(event_id::text, ''),
       blob_path, ciphertext_sha256, plaintext_size_bytes, wrapped_dek, kek_id, kek_version,
       retention_class, state, COALESCE(shredded_reason, ''), created_at, expires_at, shredded_at
  FROM ops.content_object
 WHERE tenant_id = $1::uuid AND state = 'uploaded'
 ORDER BY object_id`

	// SQLRewrapObject records a DEK sealed under a new KEK version. $3 is the version the row is
	// *expected* to be on: the guard is what stops two concurrent rotations from both winning, and
	// what makes "re-wrap, never re-encrypt" observable as a single UPDATE with no blob access.
	SQLRewrapObject = `
UPDATE ops.content_object
   SET wrapped_dek = $4::bytea,
       kek_version = $5::text
 WHERE tenant_id = $1::uuid AND object_id = $2::uuid
   AND kek_version = $3::text
   AND state = 'uploaded'`

	// SQLShredObject destroys the wrapped key and records the shred **in one statement**, so
	// "shredded" is never recorded while a usable key survives. $5 is a single zero byte: the
	// column is NOT NULL, and a value that cannot open anything is the honest representation of
	// "the key is gone". The blob lifecycle rule removes the ciphertext independently (C34's two
	// mechanisms).
	SQLShredObject = `
UPDATE ops.content_object
   SET state = 'shredded',
       shredded_reason = $3::text,
       shredded_at = $4::timestamptz,
       wrapped_dek = '\x00'::bytea
 WHERE tenant_id = $1::uuid AND object_id = $2::uuid AND state = 'uploaded'`

	// SQLPutSearchUnit writes one ingest.search_text row. The tsv column is generated, so it is
	// never written here; that is the schema's job and it keeps the vector consistent with the
	// two-argument to_tsvector the comment in schema.sql requires.
	SQLPutSearchUnit = `
INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
VALUES ($1::uuid, $2::uuid, $3::text, $4::int, $5::text, $6::timestamptz)
ON CONFLICT (tenant_id, submission_id, unit_kind, unit_index) DO UPDATE
   SET body = EXCLUDED.body,
       expires_at = EXCLUDED.expires_at`

	// SQLDeleteSearchTextForSubmission removes the index rows of one submission.
	SQLDeleteSearchTextForSubmission = `
DELETE FROM ingest.search_text WHERE tenant_id = $1::uuid AND submission_id = $2::uuid`

	// SQLDeleteSearchTextForTenant removes every index row of a tenant (offboarding).
	SQLDeleteSearchTextForTenant = `
DELETE FROM ingest.search_text WHERE tenant_id = $1::uuid`

	// SQLSearchTerms is the term/phrase form of docs/04 §15.3, served by search_text_tsv_gin.
	// $2 is a tsquery *text this service constructed* from parsed terms, never the analyst's raw
	// input: to_tsquery() raises on malformed syntax, and a search must not be a way to make the
	// database raise. $3 restricts the unit kinds the caller's tier permits.
	SQLSearchTerms = `
SELECT submission_id::text,
       unit_kind,
       unit_index,
       ts_headline('simple', body, to_tsquery('simple', $2::text),
                   'StartSel=<em>, StopSel=</em>, MaxFragments=1, MaxWords=24, MinWords=6, FragmentDelimiter= … '),
       ts_rank(tsv, to_tsquery('simple', $2::text))
  FROM ingest.search_text
 WHERE tenant_id = $1::uuid
   AND tsv @@ to_tsquery('simple', $2::text)
   AND ($3::text = '' OR unit_kind = $3::text)
 ORDER BY 5 DESC, 1
 LIMIT $4::int`

	// SQLSearchFilenameSubstring is the substring form, served by the partial trigram index and
	// restricted to filenames by that index's predicate. The stored text and the parameter are both
	// lower-cased, so this statement and the in-memory double answer the same question rather than
	// differing on case.
	SQLSearchFilenameSubstring = `
SELECT submission_id::text,
       unit_kind,
       unit_index,
       body,
       similarity(lower(body), lower($2::text))
  FROM ingest.search_text
 WHERE tenant_id = $1::uuid
   AND unit_kind = 'attachment_name'
   AND lower(body) LIKE '%' || lower($2::text) || '%'
 ORDER BY 5 DESC, 1
 LIMIT $3::int`

	// SQLSearchFilenameFuzzy is the fuzzy form: pg_trgm similarity over filenames only, with both
	// sides lower-cased for the same reason.
	SQLSearchFilenameFuzzy = `
SELECT submission_id::text,
       unit_kind,
       unit_index,
       body,
       similarity(lower(body), lower($2::text))
  FROM ingest.search_text
 WHERE tenant_id = $1::uuid
   AND unit_kind = 'attachment_name'
   AND lower(body) % lower($2::text)
   AND similarity(lower(body), lower($2::text)) >= $3::float8
 ORDER BY 5 DESC, 1
 LIMIT $4::int`

	// SQLInsertAudit writes one audit row. prev_hash and row_hash are deliberately absent: the
	// ops.audit_chain() trigger computes both, so a caller cannot forge a chain link, and
	// docs/02 §11 requires this row to be committed *before* any content is served.
	SQLInsertAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id,
                       subject_ref, case_reference, detail, occurred_at)
VALUES ($1::uuid, $2::text, $3::text, $4::text, $5::text, $6::text,
        $7::text, $8::text, $9::jsonb, $10::timestamptz)`

	// SQLPutRetrievalGrant records a single-use retrieval grant. Requires ops.retrieval_grant
	// (SQLRetrievalGrantDDL); see the package comment.
	SQLPutRetrievalGrant = `
INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                 principal, case_reference, second_approver, issued_at, expires_at, raw_digest)
VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6::text, $7::text, $8::text,
        $9::timestamptz, $10::timestamptz, $11::text)`

	// SQLRetrievalGrant reads one grant.
	SQLRetrievalGrant = `
SELECT tenant_id::text, grant_id::text, event_id::text, object_id::text, submission_id::text,
       principal, case_reference, second_approver, issued_at, expires_at, used_at, COALESCE(used_by, ''), raw_digest
  FROM ops.retrieval_grant
 WHERE tenant_id = $1::uuid AND grant_id = $2::uuid`

	// SQLClaimRetrievalGrant consumes a grant in one statement. `used_at IS NULL` in the WHERE
	// clause is the single-use mechanism: two concurrent redemptions cannot both update the row,
	// so the second gets no row back and is refused. An UPDATE that returns nothing is the
	// strongest form this record can take without a second table.
	SQLClaimRetrievalGrant = `
UPDATE ops.retrieval_grant
   SET used_at = $3::timestamptz,
       used_by = $4::text
 WHERE tenant_id = $1::uuid AND grant_id = $2::uuid AND used_at IS NULL
RETURNING tenant_id::text, grant_id::text, event_id::text, object_id::text, submission_id::text,
          principal, case_reference, second_approver, issued_at, expires_at, used_at, COALESCE(used_by, ''), raw_digest`

	// SQLPutErasureReceipt records what erasure removed and what deliberately survived (C34).
	SQLPutErasureReceipt = `
INSERT INTO ops.erasure_receipt (tenant_id, receipt_id, scope_kind, subject_ref, requested_by,
                                 requested_at, completed_at, mechanisms, removed_counts, remaining_counts)
VALUES ($1::uuid, $2::uuid, $3::text, $4::text, $5::text, $6::timestamptz, $7::timestamptz,
        $8::text[], $9::jsonb, $10::jsonb)`

	// SQLLastReceipt answers "which receipt explains this unavailability": §11 requires a
	// no_longer_available result to link the erasure or retention receipt, and this is that link.
	SQLLastReceipt = `
SELECT tenant_id::text, receipt_id::text, scope_kind, subject_ref, requested_by, requested_at,
       completed_at, array_to_string(mechanisms, ','), removed_counts, remaining_counts
  FROM ops.erasure_receipt
 WHERE tenant_id = $1::uuid
 ORDER BY completed_at DESC NULLS LAST, requested_at DESC
 LIMIT 1`

// SQLRetrievalGrantDDL is the table the retrieval path needs, as this service asked for it. It is
// live in db/schema.sql with these columns (plus the single-use trigger); this constant is kept
// as the requested shape so a future migration can be diffed against it.
	SQLRetrievalGrantDDL = `
CREATE TABLE ops.retrieval_grant (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  grant_id        uuid NOT NULL,
  event_id        uuid NOT NULL,
  object_id       uuid NOT NULL,
  submission_id   uuid NOT NULL,
  principal       text NOT NULL,
  case_reference  text NOT NULL,
  second_approver text NOT NULL,
  issued_at       timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,
  used_at         timestamptz,
  used_by         text,
  raw_digest      text NOT NULL,
  PRIMARY KEY (tenant_id, grant_id),
  CONSTRAINT retrieval_grant_second_approver_distinct CHECK (second_approver <> principal),
  CONSTRAINT retrieval_grant_window_bounded CHECK (expires_at > issued_at)
);`
)

// Statement pairs a statement with what it is for, so the set can be listed by name and executed
// against the real schema by the integration test.
type Statement struct {
	Name    string
	Purpose string
	SQL     string
	// Verified is false for statements whose target table does not exist in db/schema.sql yet.
	// The integration test reports those as a gap instead of executing them.
	Verified bool
}

// Statements is every statement this service issues.
var Statements = []Statement{
	{Name: "set_tenant", Purpose: "RLS session tenant (transaction-local)", SQL: SQLSetTenant, Verified: true},
	{Name: "tenant", Purpose: "custody + search tier + gates for the tenant", SQL: SQLTenant, Verified: true},
	{Name: "put_content_object", Purpose: "store the wrapped DEK beside the blob reference", SQL: SQLPutContentObject, Verified: true},
	{Name: "content_object", Purpose: "read one object by id", SQL: SQLContentObject, Verified: true},
	{Name: "object_for_event", Purpose: "resolve an event's object (the retrieval path)", SQL: SQLObjectForEvent, Verified: true},
	{Name: "objects_for_tenant", Purpose: "list what a rotation must re-wrap", SQL: SQLObjectsForTenant, Verified: true},
	{Name: "rewrap_object", Purpose: "rotation: re-wrap, never re-encrypt", SQL: SQLRewrapObject, Verified: true},
	{Name: "shred_object", Purpose: "erasure: destroy the wrapped key and record the shred", SQL: SQLShredObject, Verified: true},
	{Name: "put_search_unit", Purpose: "write one ingest.search_text row", SQL: SQLPutSearchUnit, Verified: true},
	{Name: "delete_search_text_submission", Purpose: "erasure reaches the index by row deletion", SQL: SQLDeleteSearchTextForSubmission, Verified: true},
	{Name: "delete_search_text_tenant", Purpose: "offboarding: every index row of a tenant", SQL: SQLDeleteSearchTextForTenant, Verified: true},
	{Name: "search_terms", Purpose: "§15.3 term/phrase form over tsv", SQL: SQLSearchTerms, Verified: true},
	{Name: "search_substring", Purpose: "§15.3 substring form over filenames", SQL: SQLSearchFilenameSubstring, Verified: true},
	{Name: "search_fuzzy", Purpose: "§15.3 fuzzy form over filenames", SQL: SQLSearchFilenameFuzzy, Verified: true},
	{Name: "insert_audit", Purpose: "audit-before-serve; the chain trigger fills the hashes", SQL: SQLInsertAudit, Verified: true},
	{Name: "put_erasure_receipt", Purpose: "the receipt C34 requires", SQL: SQLPutErasureReceipt, Verified: true},
	{Name: "last_receipt", Purpose: "link a no_longer_available result to its receipt", SQL: SQLLastReceipt, Verified: true},
	{Name: "put_retrieval_grant", Purpose: "record a single-use retrieval grant", SQL: SQLPutRetrievalGrant, Verified: true},
	{Name: "retrieval_grant", Purpose: "read a retrieval grant", SQL: SQLRetrievalGrant, Verified: true},
	{Name: "claim_retrieval_grant", Purpose: "consume it atomically (UPDATE ... WHERE used_at IS NULL)", SQL: SQLClaimRetrievalGrant, Verified: true},
}

// SQLStore is the database/sql implementation.
//
// No PostgreSQL wire driver is available offline (ADR 0016), so NewSQL takes an already-opened
// *sql.DB and the binary that embeds this service registers the driver. What is verified without a
// driver is the statement text, executed against a live PostgreSQL 17 by sql_integration_test.go.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open database handle. The caller owns the driver.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Close implements Store.
func (s *SQLStore) Close() error { return s.db.Close() }

// withTenant runs fn in a transaction whose session tenant is set first. Every statement in this
// file runs inside one, because row-level security reads the setting per transaction and a
// connection pool would otherwise carry it across requests.
func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, SQLSetTenant, tenantID); err != nil {
		return fmt.Errorf("store: setting the session tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Tenant implements Store.
func (s *SQLStore) Tenant(ctx context.Context, tenantID string) (Tenant, error) {
	var t Tenant
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var mode, tier string
		err := tx.QueryRowContext(ctx, SQLTenant, tenantID).Scan(
			&t.TenantID, &t.Name, &t.Status, (*string)(&t.KeyCustody), &t.KEKID, &mode, &tier,
			&t.IngestEnabled, &t.ReadEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
		}
		if err != nil {
			return fmt.Errorf("store: tenant: %w", err)
		}
		t.CeilingMode = protocolMode(mode)
		t.ContentSearch = SearchTier(tier)
		return nil
	})
	return t, err
}

// PutContentObject implements Store.
func (s *SQLStore) PutContentObject(ctx context.Context, obj ContentObject) error {
	return s.withTenant(ctx, obj.TenantID, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, SQLPutContentObject,
			obj.TenantID, obj.ObjectID, nullable(obj.SubmissionID), nullable(obj.EventID),
			obj.BlobPath, obj.CiphertextSHA256, obj.PlaintextSizeBytes, obj.WrappedDEK,
			obj.KEKID, obj.KEKVersion, obj.RetentionClass, obj.CreatedAt, nullTime(obj.ExpiresAt)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			// The conditional update matched no row: the object exists and is shredded.
			return fmt.Errorf("store: object %s is shredded and cannot be rewritten", obj.ObjectID)
		}
		if err != nil {
			return fmt.Errorf("store: put content object: %w", err)
		}
		return nil
	})
}

// ContentObject implements Store.
func (s *SQLStore) ContentObject(ctx context.Context, tenantID, objectID string) (ContentObject, error) {
	var obj ContentObject
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		obj, err = scanObject(tx.QueryRowContext(ctx, SQLContentObject, tenantID, objectID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s", ErrObjectNotFound, tenantID, objectID)
		}
		return err
	})
	return obj, err
}

// ObjectForEvent implements Store.
func (s *SQLStore) ObjectForEvent(ctx context.Context, tenantID, eventID string) (ContentObject, error) {
	var obj ContentObject
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		obj, err = scanObject(tx.QueryRowContext(ctx, SQLObjectForEvent, tenantID, eventID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: event %s", ErrObjectNotFound, eventID)
		}
		return err
	})
	return obj, err
}

// ObjectsForTenant implements Store.
func (s *SQLStore) ObjectsForTenant(ctx context.Context, tenantID string) ([]ContentObject, error) {
	var out []ContentObject
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, SQLObjectsForTenant, tenantID)
		if err != nil {
			return fmt.Errorf("store: objects for tenant: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			obj, err := scanObject(rows)
			if err != nil {
				return err
			}
			out = append(out, obj)
		}
		return rows.Err()
	})
	return out, err
}

// RewrapObject implements Store.
func (s *SQLStore) RewrapObject(ctx context.Context, tenantID, objectID string, wrapped []byte, kekVersion, expectVersion string, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, SQLRewrapObject, tenantID, objectID, expectVersion, wrapped, kekVersion)
		if err != nil {
			return fmt.Errorf("store: rewrap object: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("store: object %s is not on key version %s or is shredded; nothing re-wrapped", objectID, expectVersion)
		}
		return nil
	})
}

// ShredObject implements Store.
func (s *SQLStore) ShredObject(ctx context.Context, tenantID, objectID, reason string, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, SQLShredObject, tenantID, objectID, reason, at)
		if err != nil {
			return fmt.Errorf("store: shred object: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s/%s (already shredded or absent)", ErrObjectNotFound, tenantID, objectID)
		}
		return nil
	})
}

// PutSearchUnit implements Store.
func (s *SQLStore) PutSearchUnit(ctx context.Context, unit SearchUnit) error {
	return s.withTenant(ctx, unit.TenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, SQLPutSearchUnit,
			unit.TenantID, unit.SubmissionID, unit.UnitKind, unit.UnitIndex, unit.Body, unit.ExpiresAt)
		if err != nil {
			return fmt.Errorf("store: put search unit: %w", err)
		}
		return nil
	})
}

// DeleteSearchText implements Store.
func (s *SQLStore) DeleteSearchText(ctx context.Context, tenantID, submissionID string) (int64, error) {
	var n int64
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if submissionID == "" {
			res, err = tx.ExecContext(ctx, SQLDeleteSearchTextForTenant, tenantID)
		} else {
			res, err = tx.ExecContext(ctx, SQLDeleteSearchTextForSubmission, tenantID, submissionID)
		}
		if err != nil {
			return fmt.Errorf("store: delete search text: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// SearchAudited implements Store: one transaction, audit row first, then the query. If the audit
// insert fails the query never runs and the caller serves nothing, which is §6.3's "fails closed".
func (s *SQLStore) SearchAudited(ctx context.Context, q SearchQuery, e AuditEntry) ([]SearchHit, error) {
	if e.TenantID != q.TenantID {
		return nil, fmt.Errorf("store: search audit tenant %s does not match the query tenant %s", e.TenantID, q.TenantID)
	}
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	blob, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("store: audit detail: %w", err)
	}
	var out []SearchHit
	err = s.withTenant(ctx, q.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLInsertAudit, e.TenantID, e.ActorType, e.ActorID, e.Action,
			e.ObjectType, nullable(e.ObjectID), nullable(e.SubjectRef), nullable(e.CaseReference), blob, e.OccurredAt); err != nil {
			return fmt.Errorf("store: search audit (failing closed): %w", err)
		}
		var rows *sql.Rows
		var err error
		switch q.Form {
		case FormTerms:
			rows, err = tx.QueryContext(ctx, SQLSearchTerms, q.TenantID, q.Text, q.UnitKind, q.Limit)
		case FormSubstring:
			rows, err = tx.QueryContext(ctx, SQLSearchFilenameSubstring, q.TenantID, q.Text, q.Limit)
		case FormFuzzy:
			rows, err = tx.QueryContext(ctx, SQLSearchFilenameFuzzy, q.TenantID, q.Text, q.MinSimilar, q.Limit)
		default:
			return fmt.Errorf("store: search form %q is outside the closed set", q.Form)
		}
		if err != nil {
			return fmt.Errorf("store: search: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var h SearchHit
			var rank sql.NullFloat64
			if err := rows.Scan(&h.SubmissionID, &h.UnitKind, &h.UnitIndex, &h.Snippet, &rank); err != nil {
				return fmt.Errorf("store: scan search hit: %w", err)
			}
			h.Rank = rank.Float64
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// LastReceipt implements Store.
func (s *SQLStore) LastReceipt(ctx context.Context, tenantID string) (ErasureReceipt, error) {
	var r ErasureReceipt
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var subject sql.NullString
		var completed sql.NullTime
		var mechanisms string
		var removed, remaining []byte
		err := tx.QueryRowContext(ctx, SQLLastReceipt, tenantID).Scan(&r.TenantID, &r.ReceiptID, &r.ScopeKind,
			&subject, &r.RequestedBy, &r.RequestedAt, &completed, &mechanisms, &removed, &remaining)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNoReceipt, tenantID)
		}
		if err != nil {
			return fmt.Errorf("store: last receipt: %w", err)
		}
		r.SubjectRef = subject.String
		if completed.Valid {
			r.CompletedAt = completed.Time
		}
		r.Mechanisms = parseTextArray(mechanisms)
		_ = json.Unmarshal(removed, &r.RemovedCounts)
		_ = json.Unmarshal(remaining, &r.RemainingCounts)
		return nil
	})
	return r, err
}

// AppendAudit implements Store.
func (s *SQLStore) AppendAudit(ctx context.Context, e AuditEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	blob, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: audit detail: %w", err)
	}
	return s.withTenant(ctx, e.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLInsertAudit, e.TenantID, e.ActorType, e.ActorID, e.Action,
			e.ObjectType, nullable(e.ObjectID), nullable(e.SubjectRef), nullable(e.CaseReference), blob, e.OccurredAt); err != nil {
			return fmt.Errorf("store: append audit: %w", err)
		}
		return nil
	})
}

// PutRetrievalGrant implements Store.
func (s *SQLStore) PutRetrievalGrant(ctx context.Context, g RetrievalGrant) error {
	return s.withTenant(ctx, g.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLPutRetrievalGrant, g.TenantID, g.GrantID, g.EventID, g.ObjectID,
			g.SubmissionID, g.Principal, g.CaseReference, g.SecondApprover, g.IssuedAt, g.ExpiresAt, g.RawDigest); err != nil {
			return fmt.Errorf("store: put retrieval grant: %w", err)
		}
		return nil
	})
}

// RetrievalGrant implements Store.
func (s *SQLStore) RetrievalGrant(ctx context.Context, tenantID, grantID string) (RetrievalGrant, error) {
	var g RetrievalGrant
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		g, err = scanGrant(tx.QueryRowContext(ctx, SQLRetrievalGrant, tenantID, grantID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrGrantNotFound, grantID)
		}
		return err
	})
	return g, err
}

// ClaimRetrievalGrant implements Store: one conditional UPDATE, so single use survives concurrency.
func (s *SQLStore) ClaimRetrievalGrant(ctx context.Context, tenantID, grantID, principal string, now time.Time) (RetrievalGrant, error) {
	var g RetrievalGrant
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		g, err = scanGrant(tx.QueryRowContext(ctx, SQLClaimRetrievalGrant, tenantID, grantID, now, principal))
		if errors.Is(err, sql.ErrNoRows) {
			// Either the grant does not exist or it was already consumed. The caller has already
			// read it, so the honest answer is the one it read: used.
			return fmt.Errorf("%w: %s", ErrGrantAlreadyUsed, grantID)
		}
		return err
	})
	return g, err
}

// PutErasureReceipt implements Store.
func (s *SQLStore) PutErasureReceipt(ctx context.Context, r ErasureReceipt) error {
	return s.withTenant(ctx, r.TenantID, func(tx *sql.Tx) error {
		removed, err := json.Marshal(orEmpty(r.RemovedCounts))
		if err != nil {
			return err
		}
		remaining, err := json.Marshal(orEmpty(r.RemainingCounts))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, SQLPutErasureReceipt, r.TenantID, r.ReceiptID, r.ScopeKind,
			nullable(r.SubjectRef), r.RequestedBy, r.RequestedAt, r.CompletedAt, pgTextArray(r.Mechanisms), removed, remaining); err != nil {
			return fmt.Errorf("store: put erasure receipt: %w", err)
		}
		return nil
	})
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanObject(row rowScanner) (ContentObject, error) {
	var obj ContentObject
	var expires, shredded sql.NullTime
	if err := row.Scan(&obj.TenantID, &obj.ObjectID, &obj.SubmissionID, &obj.EventID, &obj.BlobPath,
		&obj.CiphertextSHA256, &obj.PlaintextSizeBytes, &obj.WrappedDEK, &obj.KEKID, &obj.KEKVersion,
		&obj.RetentionClass, &obj.State, &obj.ShreddedReason, &obj.CreatedAt, &expires, &shredded); err != nil {
		return ContentObject{}, err
	}
	if expires.Valid {
		obj.ExpiresAt = expires.Time
	}
	if shredded.Valid {
		obj.ShreddedAt = shredded.Time
	}
	return obj, nil
}

func scanGrant(row rowScanner) (RetrievalGrant, error) {
	var g RetrievalGrant
	var used sql.NullTime
	if err := row.Scan(&g.TenantID, &g.GrantID, &g.EventID, &g.ObjectID, &g.SubmissionID, &g.Principal,
		&g.CaseReference, &g.SecondApprover, &g.IssuedAt, &g.ExpiresAt, &used, &g.UsedBy, &g.RawDigest); err != nil {
		return RetrievalGrant{}, err
	}
	if used.Valid {
		g.UsedAt = used.Time
	}
	return g, nil
}

// nullable turns the empty string into SQL NULL. Several columns are nullable uuids and text
// fields where ” would be a value, not an absence: blob_path is required but event_id is not, and
// writing ” to a uuid column would be a syntax error rather than a NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func orEmpty(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

// pgTextArray renders a []string as a PostgreSQL array literal parameter. The values are audit
// mechanism names from a closed set this service defines, and pgx/lib-pq would send a proper array;
// quoting here keeps it correct even for a value that ever contains a comma.
func pgTextArray(vals []string) string {
	out := "{"
	for i, v := range vals {
		if i > 0 {
			out += ","
		}
		out += `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return out + "}"
}

// parseTextArray splits the comma-joined form SQLLastReceipt returns. The mechanism names come from
// a closed set with no commas, which is why array_to_string is enough here.
func parseTextArray(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
