// errors.js — the §13 result states, expressed as a typed rejection the caller cannot ignore.
//
// A query that is not servable is never answered with an empty page, a zero, or a silently
// dropped filter: it is answered with a result_state that says so, an HTTP status that
// distinguishes "the request failed" from "the answer is not a number", and — where a fix
// exists — the fix, named. §13's table is the only source for the codes below.

import { API_VERSION, QUERY_VERSION } from './registry.js';

/**
 * @typedef {'ok'|'empty'|'not_yet_covered'|'stale_aggregate'|'coverage_degraded'|'suppressed'|
 *   'no_longer_available'|'not_captured'|'not_retrievable'|'not_found'|'unsupported_query_shape'|
 *   'query_too_broad'|'cursor_expired'|'audit_unavailable'|'key_unavailable'|'busy'|
 *   'unauthorised_role'|'content_search_not_enabled'|'audit_chain_broken'} ResultState
 */

/** §13's table, verbatim in code. `data` is true only for states that carry an answer. */
export const RESULT_STATES = Object.freeze({
  ok: { http: 200, data: true },
  empty: { http: 200, data: true },
  not_yet_covered: { http: 200, data: true },
  stale_aggregate: { http: 200, data: true },
  coverage_degraded: { http: 200, data: true },
  suppressed: { http: 200, data: true },
  no_longer_available: { http: 410, data: false },
  not_captured: { http: 200, data: true },
  not_retrievable: { http: 200, data: true },
  not_found: { http: 404, data: false },
  unsupported_query_shape: { http: 400, data: false },
  query_too_broad: { http: 400, data: false },
  cursor_expired: { http: 400, data: false },
  audit_unavailable: { http: 503, data: false },
  key_unavailable: { http: 503, data: false },
  busy: { http: 429, data: false },
  unauthorised_role: { http: 403, data: false },
  content_search_not_enabled: { http: 403, data: false },
  audit_chain_broken: { http: 500, data: false },
});

export const RESULT_STATE_NAMES = Object.freeze(Object.keys(RESULT_STATES));

/** The states that assert the system cannot say, as opposed to "we looked and there was none". */
export const UNAVAILABLE_STATES = Object.freeze([
  'not_yet_covered',
  'coverage_degraded',
  'stale_aggregate',
  'no_longer_available',
  'not_captured',
  'not_retrievable',
  'audit_unavailable',
  'key_unavailable',
  'content_search_not_enabled',
  'audit_chain_broken',
]);

/** Stable machine codes for the *reason* inside a result_state. Not a second state space. */
export const REASON = Object.freeze({
  UNKNOWN_KEY: 'unknown_key',
  UNKNOWN_SOURCE: 'unknown_source',
  UNKNOWN_DIMENSION: 'unknown_dimension',
  UNKNOWN_MEASURE: 'unknown_measure',
  UNKNOWN_OPERATOR: 'unknown_operator',
  UNKNOWN_BUCKET: 'unknown_bucket',
  UNKNOWN_TEMPLATE: 'unknown_template',
  UNSUPPORTED_QUERY_VERSION: 'unsupported_query_version',
  OPERATOR_NOT_APPLICABLE: 'operator_not_applicable',
  TYPE_MISMATCH: 'type_mismatch',
  MISSING_WINDOW: 'missing_window',
  BAD_WINDOW: 'bad_window',
  WINDOW_TOO_WIDE: 'window_too_wide',
  TOO_MANY_DIMENSIONS: 'too_many_dimensions',
  TOO_MANY_FILTERS: 'too_many_filters',
  LIST_TOO_LONG: 'list_too_long',
  VALUE_TOO_LONG: 'value_too_long',
  VALUE_NOT_SCALAR: 'value_not_scalar',
  MALFORMED_DOCUMENT: 'malformed_document',
  MAX_DEPTH_EXCEEDED: 'max_depth_exceeded',
  NOT_CLOSED: 'not_closed',
  PROHIBITED_FIELD: 'prohibited_field',
  TENANT_IN_REQUEST: 'tenant_in_request',
  TEXT_PREDICATE_IN_REQUEST: 'text_predicate_in_request',
  NO_COVERING_INDEX: 'no_covering_index',
  ENUMERATES_PEOPLE: 'enumerates_people',
  SUBJECT_SCOPE_REQUIRED: 'subject_scope_required',
  CURSOR_REQUIRES_TOTAL_ORDER: 'cursor_requires_total_order',
  COST_ESTIMATE_EXCEEDED: 'cost_estimate_exceeded',
  SERIES_TOO_LONG: 'series_too_long',
  ROLLUP_WITH_CURSOR: 'rollup_with_cursor',
  NON_ADDITIVE_MEASURE: 'non_additive_measure',
  NO_MEASURES: 'no_measures',
  BUCKET_REQUIRED: 'bucket_required',
  DIRECTORY_NOT_SYNCED: 'directory_not_synced',
  BEFORE_ENROLMENT: 'before_enrolment',
  FEWER_THAN_K_SUBJECTS: 'fewer_than_k_subjects',
  COMPLEMENTARY_SUPPRESSION: 'complementary_suppression',
  CURSOR_UNKNOWN: 'cursor_unknown',
  CURSOR_EXPIRED: 'cursor_expired',
  CURSOR_MISMATCH: 'cursor_mismatch',
  CURSOR_TENANT_MISMATCH: 'cursor_tenant_mismatch',
  CURSOR_VERSION_MISMATCH: 'cursor_version_mismatch',
  AUDIT_WRITE_FAILED: 'audit_write_failed',
  CHAIN_MISMATCH: 'chain_mismatch',
  LINK_MISMATCH: 'link_mismatch',
  RECORD_PURGED: 'record_purged',
  ROLE: 'role',
});

const FIXABLE = new Set([
  REASON.WINDOW_TOO_WIDE,
  REASON.COST_ESTIMATE_EXCEEDED,
  REASON.SERIES_TOO_LONG,
  REASON.NO_COVERING_INDEX,
  REASON.SUBJECT_SCOPE_REQUIRED,
  REASON.TOO_MANY_DIMENSIONS,
  REASON.LIST_TOO_LONG,
  REASON.SUBJECT_SCOPE_REQUIRED,
]);

/**
 * A typed rejection. Every field of the wire error comes from here, and `result_state` is the
 * §13 state — never a bespoke one.
 */
export class QueryError extends Error {
  /**
   * @param {string} resultState
   * @param {string} reason
   * @param {string} message
   * @param {object} [detail] extra machine-readable fields, e.g. the fix
   */
  constructor(resultState, reason, message, detail = {}) {
    const spec = RESULT_STATES[resultState];
    if (!spec) throw new Error(`QueryError constructed with unknown result_state ${resultState}`);
    super(message);
    this.name = 'QueryError';
    this.resultState = resultState;
    this.reason = reason;
    this.detail = Object.freeze({ ...detail });
    this.http = spec.http;
  }

  /** The wire error body. `data` is never present on a rejection. */
  toEnvelope(extra = {}) {
    return Object.freeze({
      api_version: API_VERSION,
      query_version: QUERY_VERSION,
      result_state: this.resultState,
      error: Object.freeze({
        code: this.reason,
        message: this.message,
        ...(Object.keys(this.detail).length > 0 ? { detail: this.detail } : {}),
        ...(FIXABLE.has(this.reason) ? { fixable: true } : {}),
      }),
      ...extra,
    });
  }
}

/** @returns {QueryError} */
export function unsupported(reason, message, detail) {
  return new QueryError('unsupported_query_shape', reason, message, detail);
}
/** @returns {QueryError} */
export function tooBroad(reason, message, detail) {
  return new QueryError('query_too_broad', reason, message, detail);
}
/** @returns {QueryError} */
export function cursorExpired(reason, message, detail) {
  return new QueryError('cursor_expired', reason, message, detail);
}
/** @returns {QueryError} */
export function auditUnavailable(reason, message, detail) {
  return new QueryError('audit_unavailable', reason, message, detail);
}

/**
 * The fields §2.4 prohibits by name. Their presence is not "an unknown key": it is a request
 * that tried to speak SQL or to choose its own tenant, and §2.1/§2.4 say to reject it as a
 * validation error rather than ignore it.
 */
export const PROHIBITED_FIELDS = Object.freeze({
  sql: REASON.PROHIBITED_FIELD,
  raw: REASON.PROHIBITED_FIELD,
  where: REASON.PROHIBITED_FIELD,
  expression: REASON.PROHIBITED_FIELD,
  order_by: REASON.PROHIBITED_FIELD,
  filter_sql: REASON.PROHIBITED_FIELD,
  having: REASON.PROHIBITED_FIELD,
  join: REASON.PROHIBITED_FIELD,
  select: REASON.PROHIBITED_FIELD,
  from: REASON.PROHIBITED_FIELD,
  group_by: REASON.PROHIBITED_FIELD,
  tenant_id: REASON.TENANT_IN_REQUEST,
  tenant: REASON.TENANT_IN_REQUEST,
  content_match: REASON.TEXT_PREDICATE_IN_REQUEST,
  snippet: REASON.TEXT_PREDICATE_IN_REQUEST,
  search: REASON.TEXT_PREDICATE_IN_REQUEST,
  match: REASON.TEXT_PREDICATE_IN_REQUEST,
  tsv: REASON.TEXT_PREDICATE_IN_REQUEST,
  body: REASON.TEXT_PREDICATE_IN_REQUEST,
});
