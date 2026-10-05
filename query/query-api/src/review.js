// review.js — the finding review write path.
//
// A finding is where a submission matched a rule (mart.finding). Whether an analyst judged that
// match real or a false positive is NOT stored on the finding: mart is derived and droppable, so
// workflow state there would be destroyed by a rebuild. It lives in ops.finding_review, keyed by
// the same natural key `(tenant_id, submission_id, rule_id)`, exactly as the schema's comment says.
//
// This module owns the request shape and the three statements. It is transport-free so it can be
// tested without a server, the same seam plan.js exposes. The one property to keep, as everywhere
// else in this service: the tenant comes from `ops.current_tenant()` on the session and never from
// the request, and the reviewer's identity comes from the authenticated principal, never the body.

import { REASON, unsupported } from './errors.js';
import { auditStatement } from './audit.js';

/** The two states an analyst may set. `open` is the absence of a review, not an action. */
export const REVIEW_STATES = Object.freeze(['confirmed', 'disputed']);

/** The audited action name written to ops.audit.action. */
export const FINDING_REVIEW_ACTION = 'finding.review';

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const MAX_RULE_ID = 128;
const MAX_NOTE = 2_000;
const ALLOWED_KEYS = Object.freeze(['submission_id', 'rule_id', 'review_state', 'note']);

/**
 * Validate a review request. Unknown keys and a body-supplied tenant are refused rather than
 * ignored, for the same reason the DSL refuses them: a request that quietly carries a tenant is a
 * cross-tenant write attempt that would otherwise look like a success.
 *
 * @param {unknown} body
 * @returns {Readonly<{submissionId:string, ruleId:string, reviewState:string, note:string|null}>}
 */
export function validateReviewRequest(body) {
  if (!body || typeof body !== 'object' || Array.isArray(body)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'The review body must be a JSON object.', {});
  }
  for (const key of Object.keys(body)) {
    if (ALLOWED_KEYS.includes(key)) continue;
    if (key === 'tenant_id' || key === 'tenant') {
      throw unsupported(REASON.TENANT_IN_REQUEST, 'The tenant comes from the authenticated session and never from the request.', { key });
    }
    throw unsupported(REASON.UNKNOWN_KEY, `Unknown key "${key}" in a review request.`, { key, known_keys: ALLOWED_KEYS });
  }

  const submissionId = body.submission_id;
  if (typeof submissionId !== 'string' || !UUID_RE.test(submissionId)) {
    throw unsupported(REASON.TYPE_MISMATCH, 'submission_id must be a canonical uuid.', { field: 'submission_id' });
  }
  const ruleId = body.rule_id;
  if (typeof ruleId !== 'string' || ruleId.length === 0 || ruleId.length > MAX_RULE_ID) {
    throw unsupported(REASON.TYPE_MISMATCH, `rule_id must be a non-empty string of at most ${MAX_RULE_ID} characters.`, { field: 'rule_id' });
  }
  const reviewState = body.review_state;
  if (!REVIEW_STATES.includes(reviewState)) {
    throw unsupported(REASON.TYPE_MISMATCH, 'review_state must be "confirmed" or "disputed".', { field: 'review_state', allowed: REVIEW_STATES });
  }
  const note = body.note;
  if (note !== undefined && note !== null && (typeof note !== 'string' || note.length > MAX_NOTE)) {
    throw unsupported(REASON.TYPE_MISMATCH, `note must be a string of at most ${MAX_NOTE} characters.`, { field: 'note' });
  }

  return Object.freeze({ submissionId, ruleId, reviewState, note: note ?? null });
}

/**
 * Does this finding exist for the current tenant? Returns the finding's person so the audit row
 * can name the subject. The tenant predicate is `ops.current_tenant()`, so a submission id from
 * another tenant is simply not found rather than found and refused.
 */
export const FINDING_FOR_REVIEW_SQL = `
SELECT s.user_ref
  FROM mart.finding f
  JOIN ingest.submission s
    ON s.tenant_id = f.tenant_id AND s.submission_id = f.submission_id
 WHERE f.tenant_id = ops.current_tenant()
   AND f.submission_id = $1::uuid
   AND f.rule_id = $2::text`;

/**
 * Upsert one review. `open` is never written: a row in this table is a human judgement, and the
 * check constraint requires an actor and a time for anything other than open. Re-reviewing
 * overwrites the previous judgement in place, which is the point of the natural key.
 */
export const UPSERT_REVIEW_SQL = `
INSERT INTO ops.finding_review
  (tenant_id, submission_id, rule_id, review_state, reviewed_by, reviewed_at, note)
VALUES (ops.current_tenant(), $1::uuid, $2::text, $3::text, $4::text, now(), $5::text)
ON CONFLICT (tenant_id, submission_id, rule_id) DO UPDATE
  SET review_state = EXCLUDED.review_state,
      reviewed_by  = EXCLUDED.reviewed_by,
      reviewed_at  = EXCLUDED.reviewed_at,
      note         = EXCLUDED.note
RETURNING review_state, reviewed_by, reviewed_at`;

/**
 * The audit row for a review, written in the same transaction as the review itself. A review is a
 * change to the record of what an analyst decided, so it is auditable on the same terms as a read
 * of the data it concerns.
 *
 * @param {{actorId:string, submissionId:string, ruleId:string, reviewState:string, note:string|null, subjectRef:string|null, caseReference:string|null, sessionId?:string|null}} input
 * @returns {{text:string, params:ReadonlyArray<unknown>, id:string}}
 */
export function findingReviewAuditStatement(input) {
  return auditStatement(
    { action: FINDING_REVIEW_ACTION, object_type: 'mart.v_finding' },
    {
      actorId: input.actorId,
      objectId: input.submissionId,
      subjectRef: input.subjectRef,
      caseReference: input.caseReference,
      sessionId: input.sessionId ?? null,
      detail: {
        rule_id: input.ruleId,
        review_state: input.reviewState,
        note: input.note,
      },
    },
  );
}
