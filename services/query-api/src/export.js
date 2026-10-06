// export.js — the list export: a bounded CSV of the current filtered events or findings list.
//
// An export is the easiest way to turn this product into a ranking of people, which the product
// rules forbid, so the export keeps exactly the list's own shape and none of the aggregate path:
//
//   * only the two per-person list sources (ingest.submission, mart.v_finding) can be exported —
//     there is no aggregate export, so a k-suppressed cell can never be exported around the k rule;
//   * rows come back in the server's fixed total ordering (newest first), never sorted by any
//     volume, so no CSV here is a leaderboard;
//   * the whole list is bounded by EXPORT_MAX_ROWS, and an export that would exceed it is refused
//     rather than truncated silently;
//   * the CSV is metadata only: the list read never selects prompt content, and neither does this.
//
// The export is generated server-side and stored in ops.export, returned to the caller as a
// single-use, short-lived download link; the audit row records the filters used.

import { expandTemplate } from './templates.js';
import { validate } from './validate.js';
import { guard } from './guard.js';
import { compile } from './compile.js';
import { AUDIT_ACTIONS, auditStatement, subjectRefOf } from './audit.js';
import { REASON, tooBroad, unsupported } from './errors.js';

/** The row cap on one list export. A longer list is refused, naming the narrowing that would fit. */
export const EXPORT_MAX_ROWS = 10_000;
/** How long a generated export stays downloadable. */
export const EXPORT_TTL_MS = 15 * 60 * 1000;

/** The only sources a list export may hold: the two bounded per-person lists. */
const EXPORTABLE = Object.freeze(['ingest.submission', 'mart.v_finding']);

/** The CSV columns per source, the same field names the list read returns. */
const CSV_COLUMNS = Object.freeze({
  'ingest.submission': Object.freeze([
    'submission_id', 'received_at', 'first_occurred_at', 'last_occurred_at', 'subject', 'tool',
    'tool_name', 'device', 'mode', 'action', 'content_state', 'route', 'detection_basis',
    'prompt_kind', 'merge_confidence', 'confidence', 'observation_count', 'size_bytes', 'labels',
    'observed_routes',
  ]),
  'mart.v_finding': Object.freeze([
    'submission_id', 'detected_at', 'rule', 'rule_title', 'class', 'severity', 'subject', 'tool',
    'tool_name', 'mode', 'decided_locally', 'review_state', 'reviewed_by', 'reviewed_at',
    'policy_action',
  ]),
});

/**
 * Plan one list export from a template request (q8_activity or q5_findings). The request's cursor,
 * if any, is dropped: an export is the whole filtered list, not one page of it.
 *
 * @param {object} request  a template request: {query_version, template, params}
 * @param {object} ctx      {actorId, caseReference, sessionId}
 * @returns {{source: object, query: object, compiled: object, audit: object}}
 */
export function planListExport(request, ctx) {
  const expansion = expandTemplate(request);
  if (expansion.document === null) {
    throw unsupported(REASON.UNKNOWN_SOURCE, 'Only an events or findings list can be exported; a single-record read is not a list.', {});
  }
  const document = expansion.document;
  if (!EXPORTABLE.includes(document.source)) {
    throw unsupported(REASON.UNKNOWN_SOURCE, `Only an events or findings list can be exported; "${document.source}" is neither.`, {
      source: document.source,
      exportable: EXPORTABLE,
    });
  }

  const validated = validate({ ...document, cursor: null, limit: null });
  const query = Object.freeze({ ...validated.query, cursor: null, limit: EXPORT_MAX_ROWS });
  const guarded = guard({ query, source: validated.source, klass: validated.klass });
  const compiled = compile({ query: guarded.query, source: validated.source, klass: validated.klass }, {});

  const decision = Object.freeze({
    required: true,
    phase: 'pre_read',
    action: AUDIT_ACTIONS.list_export,
    object_type: validated.source.id,
    reasons: Object.freeze(['list_export']),
    selfAudited: true,
  });
  const audit = auditStatement(decision, {
    actorId: ctx.actorId ?? 'unknown',
    caseReference: ctx.caseReference ?? null,
    sessionId: ctx.sessionId ?? null,
    subjectRef: subjectRefOf(query),
    detail: {
      source: validated.source.id,
      filters: query.filters.map((f) => ({ field: f.field, op: f.op, ...(f.field === 'subject' ? {} : { value: f.value }) })),
      window: query.window,
      limit: EXPORT_MAX_ROWS,
    },
  });

  return Object.freeze({ source: validated.source, query, compiled, audit });
}

/** Refuse a list whose export would exceed the row bound. */
export function exportTooLarge(rows) {
  return tooBroad(
    REASON.COST_ESTIMATE_EXCEEDED,
    `The filtered list has more than ${EXPORT_MAX_ROWS} rows; narrow the window or the filters and export again.`,
    { max_rows: EXPORT_MAX_ROWS, fix: { narrow_window: true, add_filter: true } },
  );
}

function csvValue(value) {
  if (value === null || value === undefined) return '';
  if (value instanceof Date) return value.toISOString();
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

function csvCell(value) {
  const text = csvValue(value);
  return /[",\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

/** Serialise rows under the given column names, with a header row. */
export function rowsToCsv(columns, rows) {
  const lines = [columns.join(',')];
  for (const row of rows) lines.push(columns.map((c) => csvCell(row[c])).join(','));
  return `${lines.join('\r\n')}\r\n`;
}

/** One CSV row per list row, one column per declared field. */
export function listExportCsv(sourceId, rows) {
  const columns = CSV_COLUMNS[sourceId];
  if (!columns) throw new Error(`no CSV columns for source ${sourceId}`);
  return rowsToCsv(columns, rows);
}
