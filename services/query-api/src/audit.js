// audit.js — read auditing.
//
// Every read of subject-level data writes an audit entry as it is served: not afterwards, not in a
// batch, not best-effort.
//
// Three mechanics matter here and each is visible in the code:
//
//   1. The audit row is inserted with the tenant taken from `ops.current_tenant()`, never from
//      a parameter. There is no tenant argument to get wrong.
//   2. Fail closed. If the insert fails the transaction aborts and the read returns
//      `audit_unavailable` (503) with zero rows: "a read that cannot be proved to have happened
//      does not happen."
//   3. The decision is made before suppression. A query answered with `suppressed` still writes
//      an audit entry, because the attempt to resolve a small group is the fact worth recording.
//
// Most audit rows are written before the read (`phase: 'pre_read'`). The small-cell trigger needs
// the cells' distinct-subject counts, which exist only once the read has run, so that row is
// written after the read inside the same transaction (`phase: 'post_read'`); nothing is emitted
// before the transaction commits either way.

import { REASON } from './errors.js';

/** The closed action vocabulary written into ops.audit.action. */
export const AUDIT_ACTIONS = Object.freeze({
  aggregate_read: 'query.aggregate',
  event_list: 'query.events',
  finding_list: 'query.findings',
  device_list: 'query.devices',
  coverage_read: 'query.coverage',
  audit_read: 'audit.read',
  single_record: 'query.record',
  content_search: 'content.search',
  content_reveal: 'content.reveal',
  finding_review: 'finding.review',
  tool_sanction: 'tool.sanction',
  list_export: 'export.list',
  subject_export: 'export.subject',
  subject_erasure: 'subject.erasure',
});

const ACTION_BY_SOURCE = Object.freeze({
  'mart.v_tool_usage': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_tool_period': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_tool_user_period': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_org_period': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_class_period': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_user_period': AUDIT_ACTIONS.aggregate_read,
  'mart.agg_device_period': AUDIT_ACTIONS.device_list,
  'mart.v_device_liveness': AUDIT_ACTIONS.device_list,
  'ops.collector_state': AUDIT_ACTIONS.device_list,
  'ops.coverage_snapshot': AUDIT_ACTIONS.coverage_read,
  'ingest.submission': AUDIT_ACTIONS.event_list,
  'mart.v_finding': AUDIT_ACTIONS.finding_list,
  'ops.audit': AUDIT_ACTIONS.audit_read,
});

/**
 * @typedef {object} AuditDecision
 * @property {boolean} required
 * @property {'pre_read'|'post_read'|'none'} phase
 * @property {string} action
 * @property {string} object_type
 * @property {ReadonlyArray<string>} reasons
 * @property {boolean} selfAudited   false for a read of ops.audit: one row per query, no recursion
 */

/**
 * Decide whether a read must be audited, and when. Pure: it looks at the shape, not at any data.
 *
 * @param {{query: object, source: object}} validated
 * @param {object} [context]
 * @param {boolean} [context.singleRecord] a single-submission / single-finding detail read
 * @param {string} [context.subjectRef]    the subject the read is about, when one is named
 * @returns {AuditDecision}
 */
export function auditDecision(validated, context = {}) {
  const { query, source } = validated;
  const reasons = [];
  let phase = 'none';

  // Any query that FILTERS ON user_ref. Asking about a person is the act being
  // recorded, whatever comes back.
  const subjectFilters = query.filters.filter((f) => f.field === 'subject');
  if (subjectFilters.length > 0) {
    reasons.push('filters_on_subject');
    phase = 'pre_read';
  }

  // Any query that RETURNS user_ref. The response identifies a subject.
  if (source.subjectBearing) {
    reasons.push('returns_subject_reference');
    if (phase === 'none') phase = 'pre_read';
  }

  // A single-submission or single-finding detail read.
  if (context.singleRecord) {
    reasons.push('single_record_detail');
    phase = 'pre_read';
  }

  // Any read of ops.audit: one row per query, not itself re-audited, so it terminates.
  if (source.id === 'ops.audit') {
    reasons.push('reads_the_audit_trail');
    phase = 'pre_read';
  }

  const required = phase !== 'none';
  return Object.freeze({
    required,
    phase: required ? phase : 'none',
    action: ACTION_BY_SOURCE[source.id] ?? AUDIT_ACTIONS.aggregate_read,
    object_type: source.id,
    reasons: Object.freeze(reasons),
    selfAudited: source.id !== 'ops.audit',
  });
}

/**
 * The subject the audit row should name, taken from the query's own filters. `subject_ref` is a
 * single column, so an `in` list is joined and capped; the full shape travels in `detail`.
 */
export function subjectRefOf(query) {
  const values = [];
  for (const filter of query.filters) {
    if (filter.field !== 'subject') continue;
    if (filter.op === 'eq') values.push(String(filter.value));
    else if (filter.op === 'in') for (const v of filter.value) values.push(String(v));
  }
  if (values.length === 0) return null;
  const joined = values.join(',');
  return joined.length <= 256 ? joined : `${joined.slice(0, 253)}...`;
}

/**
 * The `detail` jsonb. It describes the question, not the answer: the filter VALUES of
 * non-subject dimensions (a tool name, a class, a severity) are exactly what Q10 exists to show,
 * and the subject values travel in `subject_ref` instead.
 */
export function auditDetail(validated, context = {}) {
  const { query, source } = validated;
  return Object.freeze({
    source: source.id,
    query_version: query.query_version,
    bucket: query.bucket,
    dimensions: query.dimensions,
    measures: query.measures,
    order: query.order,
    window: query.window,
    rollup: query.rollup,
    filters: query.filters.map((f) => ({
      field: f.field,
      op: f.op,
      ...(f.field === 'subject' ? {} : { value: f.value }),
    })),
    ...(context.rows !== undefined ? { rows: context.rows } : {}),
    ...(context.coarsened ? { coarsened: context.coarsened } : {}),
  });
}

/**
 * The parameterised audit insert. Every column but the tenant is a parameter; the tenant is
 * `ops.current_tenant()` and cannot be anything else.
 *
 * `sessionId` is the product token's `sid`. ops.audit has no column for it, so it rides in
 * `detail` as `sid`: enough to tie a row to the sign-in that wrote it (control-api audits the
 * session under the same id).
 *
 * @param {AuditDecision} decision
 * @param {{actorId:string, actorType?:string, subjectRef?:string|null, caseReference?:string|null, detail:object, objectId?:string|null, sessionId?:string|null}} input
 * @returns {{text:string, params:ReadonlyArray<unknown>, id:string}}
 */
export function auditStatement(decision, input) {
  const detail = input.sessionId ? { ...(input.detail ?? {}), sid: input.sessionId } : (input.detail ?? {});
  const params = [
    input.actorType ?? 'user',
    input.actorId,
    decision.action,
    decision.object_type,
    input.objectId ?? null,
    input.subjectRef ?? null,
    input.caseReference ?? null,
    JSON.stringify(detail),
  ];
  const text = [
    'INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id,',
    '                       subject_ref, case_reference, detail)',
    'VALUES (ops.current_tenant(), $1::text, $2::text, $3::text, $4::text, $5::text,',
    '        $6::text, $7::text, $8::jsonb)',
    'RETURNING audit_seq, occurred_at, row_hash',
  ].join('\n');
  return Object.freeze({ id: 'audit_insert', text, params: Object.freeze(params) });
}

/**
 * The read path verifies the hash links within each returned page; a mismatch returns
 * `audit_chain_broken` rather than a list that looks fine.
 *
 * The two checks are:
 *   * recomputation — the stored `row_hash` equals the sha256 the `ops.audit_chain()` trigger
 *     would compute for that row, recomputed by the database itself in the same statement (the
 *     `__recomputed_hash` column). Recomputing in JS would duplicate `detail::text`'s jsonb
 *     rendering and would cry wolf.
 *   * linkage — within the page, the next-newer row's `prev_hash` equals this row's `row_hash`.
 *
 * @param {ReadonlyArray<object>} rows
 * @returns {{ok:boolean, broken:ReadonlyArray<object>}}
 */
export function verifyAuditPage(rows) {
  const broken = [];
  for (const row of rows) {
    if (row.__recomputed_hash !== undefined && row.row_hash !== undefined && row.__recomputed_hash !== row.row_hash) {
      broken.push(Object.freeze({ audit_seq: row.audit_seq, reason: REASON.CHAIN_MISMATCH }));
      continue;
    }
    if (row.__newer_prev_hash !== undefined && row.__newer_prev_hash !== null && row.__newer_prev_hash !== row.row_hash) {
      broken.push(Object.freeze({ audit_seq: row.audit_seq, reason: REASON.LINK_MISMATCH }));
    }
  }
  return Object.freeze({ ok: broken.length === 0, broken: Object.freeze(broken) });
}

/**
 * The fail-closed transaction shape, as data. The executor in plan.js runs it; keeping it here
 * means the ordering ("audit first, then the read, then COMMIT, and only then a response body")
 * is stated once, next to the reasoning for it.
 *
 * `phase`:
 *   * `pre_read` — insert the audit row, then run the read, then commit, then serve. Nothing is
 *     served before the commit.
 *   * `none`     — the shape is not subject-level; the read is served without an audit row.
 *
 * @param {AuditDecision} decision
 */
export function auditPlan(decision) {
  if (!decision.required) return Object.freeze({ phase: 'none', auditRequired: false, statements: Object.freeze([]) });
  return Object.freeze({ phase: 'pre_read', auditRequired: true, statements: Object.freeze(['audit_insert', 'read']) });
}
