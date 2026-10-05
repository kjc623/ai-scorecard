// sanction.js — the per-tenant tool sanction decision write path.
//
// A tool's sanctioned state is configuration, not a property of any event or aggregate: the same
// fingerprint is approved by one tenant and prohibited by another, and `unknown` must never be
// conflated with `unsanctioned` (brief C8). It therefore lives in `ops.tool`, is joined at read
// time by Q1/Q2, and is set here rather than denormalised into a bucket — a decision must show on
// the next read without rewriting history.
//
// This module owns the request shape and the two statements, and is transport-free so it can be
// tested without a server (the same seam review.js exposes). The two invariants that hold
// everywhere in this service hold here: the tenant comes from `ops.current_tenant()` on the
// session and never from the request, and the actor comes from the authenticated principal, never
// from the body.

import { REASON, unsupported } from './errors.js';
import { auditStatement } from './audit.js';

/** The three states a decision may name. `unknown` is an answer, not the absence of one. */
export const SANCTION_STATES = Object.freeze(['sanctioned', 'unsanctioned', 'unknown']);

/** The audited action name written to ops.audit.action. */
export const TOOL_SANCTION_ACTION = 'tool.sanction';

const MAX_FINGERPRINT = 256;
const MAX_DISPLAY_NAME = 200;
const MAX_NOTE = 2_000;
const ALLOWED_KEYS = Object.freeze(['tool_fingerprint', 'sanctioned_state', 'display_name', 'note']);

/**
 * Validate a sanction request. Unknown keys and a body-supplied tenant are refused rather than
 * ignored, for the same reason the DSL refuses them: a request that quietly carries a tenant is a
 * cross-tenant write attempt that would otherwise look like a success.
 *
 * @param {unknown} body
 * @returns {Readonly<{toolFingerprint:string, sanctionedState:string, displayName:string|null, note:string|null}>}
 */
export function validateSanctionRequest(body) {
  if (!body || typeof body !== 'object' || Array.isArray(body)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'The sanction body must be a JSON object.', {});
  }
  for (const key of Object.keys(body)) {
    if (ALLOWED_KEYS.includes(key)) continue;
    if (key === 'tenant_id' || key === 'tenant') {
      throw unsupported(REASON.TENANT_IN_REQUEST, 'The tenant comes from the authenticated session and never from the request.', { key });
    }
    throw unsupported(REASON.UNKNOWN_KEY, `Unknown key "${key}" in a sanction request.`, { key, known_keys: ALLOWED_KEYS });
  }

  const toolFingerprint = body.tool_fingerprint;
  if (typeof toolFingerprint !== 'string' || toolFingerprint.length === 0 || toolFingerprint.length > MAX_FINGERPRINT) {
    throw unsupported(REASON.TYPE_MISMATCH, `tool_fingerprint must be a non-empty string of at most ${MAX_FINGERPRINT} characters.`, { field: 'tool_fingerprint' });
  }
  const sanctionedState = body.sanctioned_state;
  if (!SANCTION_STATES.includes(sanctionedState)) {
    throw unsupported(REASON.TYPE_MISMATCH, `sanctioned_state must be one of: ${SANCTION_STATES.join(', ')}.`, { field: 'sanctioned_state', allowed: SANCTION_STATES });
  }
  const displayName = body.display_name;
  if (displayName !== undefined && displayName !== null && (typeof displayName !== 'string' || displayName.length === 0 || displayName.length > MAX_DISPLAY_NAME)) {
    throw unsupported(REASON.TYPE_MISMATCH, `display_name must be a non-empty string of at most ${MAX_DISPLAY_NAME} characters.`, { field: 'display_name' });
  }
  const note = body.note;
  if (note !== undefined && note !== null && (typeof note !== 'string' || note.length > MAX_NOTE)) {
    throw unsupported(REASON.TYPE_MISMATCH, `note must be a string of at most ${MAX_NOTE} characters.`, { field: 'note' });
  }

  return Object.freeze({
    toolFingerprint,
    sanctionedState,
    displayName: displayName ?? null,
    note: note ?? null,
  });
}

/**
 * The current decision for this tenant and tool, if one exists. Read before the upsert so the audit
 * row can state what changed; a fingerprint with no row is `unknown`, which is a real answer.
 */
export const TOOL_SANCTION_FOR_REVIEW_SQL = `
SELECT sanctioned_state, display_name
  FROM ops.tool
 WHERE tenant_id = ops.current_tenant()
   AND tool_fingerprint = $1::text`;

/**
 * Record the decision. One row per (tenant, fingerprint); a re-decision overwrites in place, which
 * is the point of the natural key. The schema's own check enforces that anything other than
 * `unknown` is attributed with an actor and a time, so `unknown` clears both.
 *
 * `last_seen_at` is deliberately untouched: it records when the tool was observed, and a policy
 * decision is not an observation.
 */
export const UPSERT_TOOL_SANCTION_SQL = `
INSERT INTO ops.tool
  (tenant_id, tool_fingerprint, display_name, sanctioned_state, decided_by, decided_at)
VALUES (ops.current_tenant(), $1::text, $2::text, $3::text,
        CASE WHEN $3::text = 'unknown' THEN NULL ELSE $4::text END,
        CASE WHEN $3::text = 'unknown' THEN NULL ELSE now() END)
ON CONFLICT (tenant_id, tool_fingerprint) DO UPDATE
  SET sanctioned_state = EXCLUDED.sanctioned_state,
      display_name     = coalesce(EXCLUDED.display_name, ops.tool.display_name),
      decided_by       = EXCLUDED.decided_by,
      decided_at       = EXCLUDED.decided_at
RETURNING tool_fingerprint, display_name, sanctioned_state, decided_by, decided_at`;

/**
 * The audit row for a decision, written in the same transaction as the decision itself. A sanction
 * changes how every later read of the tool is judged, so it is auditable on the same terms as the
 * read it governs.
 *
 * @param {{actorId:string, toolFingerprint:string, sanctionedState:string, previousState:string, displayName:string|null, note:string|null, caseReference:string|null}} input
 * @returns {{text:string, params:ReadonlyArray<unknown>, id:string}}
 */
export function toolSanctionAuditStatement(input) {
  return auditStatement(
    { action: TOOL_SANCTION_ACTION, object_type: 'ops.tool' },
    {
      actorId: input.actorId,
      objectId: input.toolFingerprint,
      subjectRef: null,
      caseReference: input.caseReference,
      detail: {
        sanctioned_state: input.sanctionedState,
        previous_state: input.previousState,
        display_name: input.displayName,
        note: input.note,
      },
    },
  );
}
