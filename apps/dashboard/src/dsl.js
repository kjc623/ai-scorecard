// dsl.js — the typed builder for the closed query DSL.
//
// The dashboard can only construct a query the API admits. This is enforced twice, on purpose:
//
//   1. here, locally, so a mistake is a red panel in the browser rather than a 400 round trip;
//   2. at the API, which rejects anything outside the enumerated vocabulary
//      (`unsupported_query_shape`, HTTP 400) rather than escaping it.
//
// The second is the security property and the first is the usability one. Neither replaces the
// other: a client that "validates" and then sends whatever it likes would be worse than no client
// validation at all, so `buildDocument` returns the request body and nothing else — there is no
// path in this app that hands a hand-written body to the transport.
//
// What is deliberately absent: no string concatenation of anything, no free-text predicate, no
// tenant, no ordering text. A caller supplies *values*; the names they are attached to come from
// vocab.js, which is asserted equal to the server's registry by test/parity.test.mjs.

import {
  BUCKETS,
  K,
  OPERATORS,
  QUERY_VERSION,
  SOURCES,
  TEMPLATES,
  WINDOWS,
} from './vocab.js';

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

const TOP_LEVEL_KEYS = Object.freeze([
  'query_version', 'source', 'bucket', 'dimensions', 'measures', 'filters',
  'window', 'order', 'limit', 'cursor', 'rollup',
]);

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/** An ISO-8601 UTC instant. Local times are refused rather than silently reinterpreted. */
export function isoUtc(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$/.test(value)) {
    throw new DashboardQueryError('bad_window', `"${String(value)}" is not an ISO-8601 UTC instant.`);
  }
  return new Date(value).toISOString();
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

function checkFilter(filter, source) {
  if (!isObject(filter)) {
    throw new DashboardQueryError('malformed_document', 'A filter is an object with field, op and value.');
  }
  for (const key of Object.keys(filter)) {
    if (!['field', 'op', 'value'].includes(key)) {
      throw new DashboardQueryError('unknown_key', `A filter carries the unknown key "${key}".`, { key });
    }
  }
  const spec = SOURCES[source];
  const known = spec ? new Set([...spec.dimensions, ...(spec.filterOnly ?? [])]) : new Set();
  // `class` is a predicate over the labels column on the event list, not a dimension.
  const isPredicate = source === 'ingest.submission' && filter.field === 'class';
  if (!known.has(filter.field) && !isPredicate) {
    throw new DashboardQueryError('unknown_dimension', `"${String(filter.field)}" is not a filter field of ${source}.`, {
      field: filter.field,
      known_fields: [...known],
    });
  }
  if (!OPERATORS.includes(filter.op)) {
    throw new DashboardQueryError('unknown_operator', `Unknown operator "${String(filter.op)}".`, { known_operators: OPERATORS });
  }
  if (isPredicate && filter.op !== 'eq') {
    throw new DashboardQueryError('operator_not_applicable', 'The class predicate on an event list takes eq only.', { operator: filter.op });
  }
  if (filter.op === 'starts_with' && filter.field !== 'tool') {
    throw new DashboardQueryError('operator_not_applicable', 'starts_with is permitted only on tool fingerprints.', { field: filter.field });
  }
  return Object.freeze({ field: filter.field, op: filter.op, value: filter.value });
}

/**
 * Build a closed query document.
 *
 * @param {object} input
 * @param {string} input.source       one of SOURCES
 * @param {object} input.window       {from,to} in ISO-8601 UTC (omit for a source that needs none)
 * @param {string} [input.bucket]     hour|day|week|month
 * @param {string[]} [input.dimensions]
 * @param {string[]} [input.measures]
 * @param {object[]} [input.filters]
 * @param {object[]} [input.order]
 * @param {number} [input.limit]
 * @param {string|null} [input.cursor]
 * @param {boolean} [input.rollup]
 * @returns {object} the request body, and nothing else
 */
export function buildDocument(input) {
  if (!isObject(input)) {
    throw new DashboardQueryError('malformed_document', 'A query is an object.');
  }
  for (const key of Object.keys(input)) {
    if (!TOP_LEVEL_KEYS.includes(key)) {
      throw new DashboardQueryError('unknown_key', `Unknown key "${key}" in a query document.`, { known_keys: TOP_LEVEL_KEYS });
    }
  }
  const spec = SOURCES[input.source];
  if (!spec) {
    throw new DashboardQueryError('unknown_source', `Unknown source "${String(input.source)}".`, { known_sources: Object.keys(SOURCES) });
  }

  const dimensions = [...(input.dimensions ?? [])];
  if (dimensions.length > 3) {
    throw new DashboardQueryError('too_many_dimensions', 'At most three dimensions plus one bucket may be grouped.');
  }
  for (const name of dimensions) {
    if (!spec.dimensions.includes(name) || name === 'bucket') {
      throw new DashboardQueryError('unknown_dimension', `"${name}" is not a grouping dimension of ${input.source}.`, {
        dimension: name,
        known_dimensions: spec.dimensions.filter((d) => d !== 'bucket'),
      });
    }
  }
  if (spec.kind === 'list' && dimensions.length > 0) {
    throw new DashboardQueryError('no_measures', `${input.source} is a bounded list and does not group.`);
  }

  const measures = [...(input.measures ?? [])];
  for (const name of measures) {
    if (!spec.measures.includes(name)) {
      throw new DashboardQueryError('unknown_measure', `"${name}" is not a measure of ${input.source}.`, {
        measure: name,
        known_measures: spec.measures,
      });
    }
  }
  if (spec.kind === 'aggregate' && measures.length === 0) {
    throw new DashboardQueryError('no_measures', `${input.source} is an aggregate read and needs at least one measure.`, {
      known_measures: spec.measures,
    });
  }

  const bucket = input.bucket ?? null;
  if (bucket !== null && !BUCKETS.includes(bucket)) {
    throw new DashboardQueryError('unknown_bucket', `Unknown bucket "${String(bucket)}".`, { known_buckets: BUCKETS });
  }
  if (bucket !== null && spec.kind === 'list') {
    throw new DashboardQueryError('unknown_bucket', `${input.source} is a bounded list and does not bucket.`);
  }

  const filters = (input.filters ?? []).map((f) => checkFilter(f, input.source));
  // A per-subject source must name its subject. Without this the client could ask for every
  // person's series at once, which is the ranking the product forbids.
  if (spec.requiresSubjectFilter) {
    const named = filters.some((f) => f.field === 'subject' && (f.op === 'eq' || f.op === 'in'));
    if (!named) {
      throw new DashboardQueryError('subject_scope_required', `${input.source} is a per-person series and must name the person it is about (docs/04 §11.2).`, {
        fix: { add_filter: { field: 'subject', op: 'eq', value: '<user_ref>' } },
      });
    }
  }

  const document = {
    query_version: QUERY_VERSION,
    source: input.source,
    ...(bucket === null ? {} : { bucket }),
    ...(spec.kind === 'aggregate' ? { dimensions, measures } : {}),
    filters,
    ...(spec.noWindow ? {} : { window: checkWindow(input.window) }),
    ...(input.order && input.order.length > 0 ? { order: input.order.map((o) => Object.freeze({ by: o.by, dir: o.dir ?? 'desc' })) } : {}),
    ...(input.limit === undefined || input.limit === null ? {} : { limit: input.limit }),
    ...(input.cursor ? { cursor: input.cursor } : {}),
    ...(input.rollup ? { rollup: true } : {}),
  };
  return Object.freeze(document);
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

/** The page-size cap for a source, so no screen can ask for more than the API allows. */
export function maxPageSize(source) {
  return SOURCES[source]?.kind === 'list' ? 500 : 2000;
}

/** The applied bucket for a window and a requested bucket, mirroring the API's default. */
export function effectiveBucket(source, requested) {
  if (SOURCES[source]?.kind === 'list') return null;
  return requested ?? WINDOWS.d7.bucket;
}

/** How many points a window produces at a bucket: the hint the cost guard will judge on. */
export function estimatedPoints(window, bucket) {
  if (!window || !bucket) return 1;
  const ms = Date.parse(window.to) - Date.parse(window.from);
  const unit = { hour: 3_600_000, day: 86_400_000, week: 604_800_000, month: 2_629_800_000 }[bucket];
  return unit ? Math.max(1, Math.ceil(ms / unit)) : 1;
}

export { K };
