// dsl.js — the typed builder for template requests.
//
// The pages ask the ten questions as templates, and read a device's collectors as a list document.
// A request is checked here so a mistake is a red panel in the browser rather than a 400 round
// trip; query-api rejects anything outside its closed vocabulary either way, which is the security
// property. A caller supplies *values*; the template
// and parameter names come from vocab.js, which test/parity.test.mjs holds equal to query-api's.

import { QUERY_VERSION, SOURCES, TEMPLATES, WINDOWS } from './vocab.js';

/** A refusal this client produces itself, shaped exactly like the API's error envelope. */
export class DashboardQueryError extends Error {
  constructor(reason, message, detail = {}) {
    super(message);
    this.name = 'DashboardQueryError';
    this.reason = reason;
    this.detail = detail;
    /** Locally-produced refusals carry the same state the API would have used. */
    this.resultState = 'unsupported_query_shape';
    this.http = 400;
  }

  /** The same shape the API returns, so one renderer handles both. */
  toEnvelope() {
    return {
      api_version: '1',
      query_version: QUERY_VERSION,
      result_state: this.resultState,
      error: { code: this.reason, message: this.message, detail: this.detail },
    };
  }
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/**
 * An ISO-8601 instant, normalised to UTC.
 *
 * The wire format is `…Z` and only `…Z`; the API refuses anything else. An offset (`+02:00`) is
 * accepted here and converted, because a browser that hands back what a human typed should not
 * fail when the same instant was written a different way. A bare date or a time with no offset is
 * refused: both are ambiguous, and guessing a timezone for an analyst's window is exactly the kind
 * of silent reinterpretation this DSL exists to avoid.
 */
function isoUtc(value) {
  if (typeof value !== 'string') {
    throw new DashboardQueryError('bad_window', `"${String(value)}" is not an ISO-8601 instant.`);
  }
  const utc = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$/;
  const offset = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?[+-]\d{2}:\d{2}$/;
  if (!utc.test(value) && !offset.test(value)) {
    throw new DashboardQueryError(
      'bad_window',
      `"${value}" is not an ISO-8601 instant with an explicit offset. Use 2026-09-24T00:00:00Z.`,
    );
  }
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) {
    throw new DashboardQueryError('bad_window', `"${value}" is not a real instant.`);
  }
  return new Date(ms).toISOString();
}

/** A window from a named preset, anchored at `now` so the label and the request cannot disagree. */
export function windowFor(preset, now = new Date()) {
  const spec = WINDOWS[preset];
  if (!spec) {
    throw new DashboardQueryError('unknown_window', `Unknown window preset "${String(preset)}".`, { known: Object.keys(WINDOWS) });
  }
  const to = new Date(now.getTime());
  const from = new Date(to.getTime() - spec.hours * 3_600_000);
  return Object.freeze({ from: from.toISOString(), to: to.toISOString(), label: spec.label, preset, bucket: spec.bucket });
}

function checkWindow(window) {
  if (!isObject(window)) {
    throw new DashboardQueryError('missing_window', 'A window is required: no read in this DSL is unbounded.');
  }
  const from = isoUtc(window.from);
  const to = isoUtc(window.to);
  if (Date.parse(to) <= Date.parse(from)) {
    throw new DashboardQueryError('bad_window', 'The window ends before it begins; it is half-open [from, to).');
  }
  return Object.freeze({ from, to });
}

/**
 * Build a list document over a source that needs no window, for a read that is not one of the
 * questions: equality filters on the source's own dimensions and one page of rows.
 *
 * @param {string} source a list source of vocab.js with `noWindow`
 * @param {Record<string, string>} filters
 * @param {number} limit
 */
export function buildListDocument(source, filters, limit) {
  const spec = SOURCES[source];
  if (!spec || spec.kind !== 'list' || !spec.noWindow) {
    throw new DashboardQueryError('unknown_source', `"${String(source)}" is not a list read that needs no window.`);
  }
  const out = [];
  for (const [field, value] of Object.entries(filters)) {
    if (!spec.dimensions.includes(field)) {
      throw new DashboardQueryError('unknown_dimension', `${source} has no field "${field}".`, { known_fields: spec.dimensions });
    }
    if (typeof value !== 'string' || value === '') {
      throw new DashboardQueryError('type_mismatch', `The ${field} filter needs a value.`, { field });
    }
    out.push(Object.freeze({ field, op: 'eq', value }));
  }
  return Object.freeze({ query_version: QUERY_VERSION, source, filters: Object.freeze(out), limit });
}

/**
 * The people list: everyone, a case-insensitive name prefix, or one reference. The search is
 * lower-cased here because `name_key` is the lower-cased name.
 *
 * @param {{search?: string, subject?: string, cursor?: string|null, limit?: number}} input
 */
export function buildPeopleDocument({ search = '', subject = '', cursor = null, limit = 50 } = {}) {
  const filters = [];
  const term = String(search).trim().toLowerCase();
  if (term !== '') filters.push(Object.freeze({ field: 'name_key', op: 'starts_with', value: term.slice(0, 120) }));
  if (subject !== '') filters.push(Object.freeze({ field: 'subject', op: 'eq', value: String(subject) }));
  return Object.freeze({ query_version: QUERY_VERSION, source: 'mart.v_person', filters: Object.freeze(filters), limit, ...(cursor ? { cursor } : {}) });
}

/**
 * Build a template request. A template parameter that the template does not declare is refused,
 * because a typo in a parameter name would otherwise answer a broader question than the one asked.
 *
 * @param {string} name one of TEMPLATE_NAMES
 * @param {object} params
 */
export function buildTemplate(name, params = {}) {
  const template = TEMPLATES[name];
  if (!template) {
    throw new DashboardQueryError('unknown_template', `Unknown template "${String(name)}".`, { known_templates: Object.keys(TEMPLATES) });
  }
  for (const key of Object.keys(params)) {
    if (!template.params.includes(key)) {
      throw new DashboardQueryError('unknown_key', `Template ${name} has no parameter "${key}".`, {
        template: name,
        key,
        known_params: template.params,
      });
    }
  }
  for (const required of template.requires ?? []) {
    if (params[required] === undefined || params[required] === null || params[required] === '') {
      throw new DashboardQueryError('subject_scope_required', `Template ${name} requires "${required}".`, { template: name, required });
    }
  }
  const body = { query_version: QUERY_VERSION, template: name, params: { ...params } };
  if (isObject(params.window)) body.params.window = checkWindow(params.window);
  if (body.params.cursor === null) delete body.params.cursor;
  return Object.freeze(body);
}

