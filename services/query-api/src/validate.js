// validate.js — the closed-document gate.
//
// Everything a client may utter passes through here, and only three verdicts exist: the
// document is normalised into a frozen canonical form, or it is rejected with a typed result
// state, or the process throws (a bug in this package, never in the request). Unknown keys,
// unknown identifiers and unknown operators are rejected rather than ignored, because ignoring
// an unrecognised filter answers a different question than the one asked.
//
// Two properties keep the browser from ever speaking SQL:
//
//   1. No value from the request is copied into any field the compiler reads as SQL. The
//      canonical form carries *names* (dimension/measure/operator names) and *values*, and the
//      compiler resolves each name through the frozen registry in registry.js.
//   2. The canonical form is rebuilt field by field into fresh objects. Nothing from the
//      request object is ever spread, merged or carried by reference, so `__proto__`,
//      `constructor` and prototype-laden JSON cannot reach the rest of the pipeline.

import {
  MAX_DIMENSIONS,
  MAX_FILTERS,
  MAX_IN_VALUES,
  MAX_JSON_DEPTH,
  MAX_VALUE_LENGTH,
  NATIVE_BUCKETS,
  OPERATORS,
  QUERY_CLASSES,
  QUERY_VERSION,
  REDUCTION_BUCKETS,
  SOURCES,
  SOURCE_IDS,
} from './registry.js';
import {
  PROHIBITED_FIELDS,
  REASON,
  QueryError,
  cursorExpired,
  tooBroad,
  unsupported,
} from './errors.js';

const ISO_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$/;
const DATE_ONLY = /^\d{4}-\d{2}-\d{2}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Top-level keys this DSL accepts. Everything else is `unknown_key`. */
const TOP_LEVEL_KEYS = Object.freeze([
  'query_version',
  'template',
  'params',
  'source',
  'bucket',
  'dimensions',
  'measures',
  'filters',
  'window',
  'order',
  'limit',
  'cursor',
  'rollup',
]);

/** Keys that must never appear anywhere in a request document. */
const FORBIDDEN_KEYS = Object.freeze(['__proto__', 'prototype', 'constructor']);

const MAX_WINDOW_DAYS = 366 * 4;

/**
 * @typedef {object} NormalisedQuery
 * @property {string} query_version
 * @property {string} source
 * @property {'aggregate'|'list'} kind
 * @property {string|null} bucket        hour|day|week|month, or null for a single window total
 * @property {ReadonlyArray<string>} dimensions
 * @property {ReadonlyArray<string>} measures
 * @property {ReadonlyArray<{field:string,op:string,value:unknown}>} filters
 * @property {{from:string,to:string}} window   half-open [from, to)
 * @property {ReadonlyArray<{by:string,dir:'asc'|'desc'}>} order  the caller's ordering, or []
 * @property {number|null} limit
 * @property {string|null} cursor
 * @property {boolean} rollup
 * @property {string} [template]
 */

function isPlainObject(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

function depthOf(value, depth = 0) {
  if (depth > MAX_JSON_DEPTH) return depth;
  if (value === null || typeof value !== 'object') return depth;
  let worst = depth;
  for (const key of Object.keys(value)) {
    const d = depthOf(value[key], depth + 1);
    if (d > worst) worst = d;
  }
  return worst;
}

function assertShape(doc) {
  if (!isPlainObject(doc)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'The query document must be a JSON object; arrays and scalars are not query documents.');
  }
  for (const forbidden of FORBIDDEN_KEYS) {
    if (Object.prototype.hasOwnProperty.call(doc, forbidden)) {
      throw unsupported(REASON.NOT_CLOSED, `The query document carries the reserved key "${forbidden}".`, { key: forbidden });
    }
  }
  if (depthOf(doc) > MAX_JSON_DEPTH) {
    throw unsupported(REASON.MAX_DEPTH_EXCEEDED, `The query document nests deeper than ${MAX_JSON_DEPTH} levels.`, { max_depth: MAX_JSON_DEPTH });
  }
  for (const key of Object.keys(doc)) {
    if (TOP_LEVEL_KEYS.includes(key)) continue;
    const prohibited = PROHIBITED_FIELDS[key];
    if (prohibited === REASON.TENANT_IN_REQUEST) {
      // A request carrying a tenant anywhere is rejected, not ignored. Silently dropping
      // it would turn an attempted cross-tenant read into an uneventful success.
      throw unsupported(
        REASON.TENANT_IN_REQUEST,
        'The tenant comes from the authenticated session and never from the request; a document carrying a tenant identifier is rejected.',
        { key },
      );
    }
    if (prohibited === REASON.TEXT_PREDICATE_IN_REQUEST) {
      // No content predicate exists on /v1/query. A text predicate is a different read,
      // against a table this component cannot see; it fails validation rather than being ignored.
      throw unsupported(
        REASON.TEXT_PREDICATE_IN_REQUEST,
        `"${key}" is a content-search field. /v1/query has no text predicate; use POST /v1/content-search.`,
        { key, endpoint: '/v1/content-search' },
      );
    }
    if (prohibited === REASON.PROHIBITED_FIELD) {
      throw unsupported(REASON.PROHIBITED_FIELD, `"${key}" is not part of the DSL; the DSL has no SQL, expression or ordering-text field.`, { key });
    }
    throw unsupported(REASON.UNKNOWN_KEY, `Unknown key "${key}" in the query document.`, {
      key,
      known_keys: TOP_LEVEL_KEYS,
    });
  }
}

function requireVersion(doc) {
  const version = doc.query_version;
  if (version === undefined) {
    throw unsupported(REASON.UNSUPPORTED_QUERY_VERSION, 'query_version is required; the DSL is a versioned contract.', { served: [QUERY_VERSION] });
  }
  if (version !== QUERY_VERSION) {
    throw unsupported(
      REASON.UNSUPPORTED_QUERY_VERSION,
      `Unsupported query_version "${String(version)}"; this service serves version ${QUERY_VERSION} and never silently downgrades.`,
      { requested: version, served: [QUERY_VERSION] },
    );
  }
}

function resolveSource(doc) {
  const source = doc.source;
  if (typeof source !== 'string') {
    throw unsupported(REASON.UNKNOWN_SOURCE, 'source is required and must be one of the registered source names.', { known_sources: SOURCE_IDS });
  }
  const entry = SOURCES[source];
  if (!entry) {
    throw unsupported(REASON.UNKNOWN_SOURCE, `Unknown source "${source}".`, { source, known_sources: SOURCE_IDS });
  }
  return entry;
}

function resolveBucket(doc, source) {
  const explicit = doc.bucket !== undefined && doc.bucket !== null;
  const bucket = doc.bucket ?? null;
  if (bucket === null) {
    if (source.kind === 'list') return { bucket: null, explicit: false };
    // No bucket asked for: the read is a single window total. It must still pin one
    // bucket_size, or summing hour and day rows would double-count the same events.
    return { bucket: 'day', explicit: false };
  }
  if (typeof bucket !== 'string' || ![...NATIVE_BUCKETS, ...REDUCTION_BUCKETS].includes(bucket)) {
    throw unsupported(REASON.UNKNOWN_BUCKET, `Unknown bucket "${String(bucket)}".`, {
      bucket,
      known_buckets: [...NATIVE_BUCKETS, ...REDUCTION_BUCKETS],
    });
  }
  if (source.kind === 'list') {
    throw unsupported(REASON.UNKNOWN_BUCKET, `Source "${source.id}" is a bounded list and does not bucket; remove "bucket".`, { source: source.id });
  }
  return { bucket, explicit };
}

function resolveDimensions(doc, source, bucket) {
  const raw = doc.dimensions ?? [];
  if (!Array.isArray(raw)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'dimensions must be an array of dimension names.');
  }
  const seen = new Set();
  const out = [];
  for (const name of raw) {
    if (typeof name !== 'string') {
      throw unsupported(REASON.UNKNOWN_DIMENSION, 'Every entry in dimensions must be a string.', { known_dimensions: Object.keys(source.dimensions) });
    }
    if (name === 'bucket') {
      throw unsupported(REASON.UNKNOWN_DIMENSION, 'Group by the bucket with the "bucket" key, not with a "bucket" dimension.', { fix: 'move it to "bucket"' });
    }
    if (seen.has(name)) {
      throw unsupported(REASON.UNKNOWN_DIMENSION, `Dimension "${name}" is repeated.`, { dimension: name });
    }
    seen.add(name);
    const dimension = source.dimensions[name];
    if (!dimension) {
      throw unsupported(REASON.UNKNOWN_DIMENSION, `Unknown dimension "${name}" for source "${source.id}".`, {
        dimension: name,
        known_dimensions: Object.keys(source.dimensions),
      });
    }
    out.push(name);
  }
  if (out.length > MAX_DIMENSIONS) {
    throw tooBroad(REASON.TOO_MANY_DIMENSIONS, `At most ${MAX_DIMENSIONS} dimensions plus one bucket may be grouped.`, {
      max_dimensions: MAX_DIMENSIONS,
      requested: out.length,
    });
  }
  if (out.length > 0 && source.kind === 'list') {
    // A bounded list is a list. Grouping it would collapse the rows the cursor pages over, and
    // any time-bucketed number must come from mart rather than from event rows.
    throw unsupported(REASON.NO_MEASURES, `Source "${source.id}" is a bounded list and does not group; filter on the dimension instead of grouping by it.`, {
      source: source.id,
      offered_as_filters: [...Object.keys(source.dimensions)],
    });
  }
  return out;
}

/**
 * A source whose row grain IS a person (`mart.agg_user_period`) may only be read about a named
 * subject. A person is a lookup, not a list: no read enumerates people sorted by volume, so a
 * volume leaderboard is never one document away.
 */
function assertSubjectScope(doc, source, dimensions) {
  if (!source.requiresSubjectScope) return;
  const filters = Array.isArray(doc.filters) ? doc.filters : [];
  const named = filters.some(
    (f) => isPlainObject(f) && f.field === 'subject' && (f.op === 'eq' || f.op === 'in'),
  );
  if (!named) {
    throw unsupported(
      REASON.SUBJECT_SCOPE_REQUIRED,
      `Source "${source.id}" is a per-subject series and must name the subject it is about: a read without a subject filter would enumerate people.`,
      {
        source: source.id,
        fix: { add_filter: { field: 'subject', op: 'eq', value: '<user_ref>' } },
        grouped_by: [...dimensions],
      },
    );
  }
}

function resolveMeasures(doc, source) {
  const raw = doc.measures ?? [];
  if (!Array.isArray(raw)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'measures must be an array of measure names.');
  }
  if (raw.length === 0 && source.kind === 'aggregate') {
    throw unsupported(REASON.NO_MEASURES, `Source "${source.id}" is an aggregate read and needs at least one measure.`, {
      known_measures: Object.keys(source.measures),
    });
  }
  const out = [];
  for (const name of raw) {
    if (typeof name !== 'string') {
      throw unsupported(REASON.UNKNOWN_MEASURE, 'Every entry in measures must be a string.', { known_measures: Object.keys(source.measures) });
    }
    if (!source.measures[name]) {
      throw unsupported(REASON.UNKNOWN_MEASURE, `Unknown measure "${name}" for source "${source.id}". A measure that is not carried by this source is not offered.`, {
        measure: name,
        known_measures: Object.keys(source.measures),
      });
    }
    if (!out.includes(name)) out.push(name);
  }
  return out;
}

/** Coerce and bounds-check one filter value against the dimension's declared type. */
function coerceValue(field, dimension, op, raw, path) {
  const fail = (reason, message, detail) => {
    throw unsupported(reason, message, { field, op, ...detail });
  };
  if (op === 'is_null') {
    if (raw !== undefined && raw !== null) {
      fail(REASON.TYPE_MISMATCH, 'is_null takes no value.', { received_type: Array.isArray(raw) ? 'array' : typeof raw });
    }
    return null;
  }
  if (op === 'between') {
    if (!Array.isArray(raw) || raw.length !== 2) {
      fail(REASON.TYPE_MISMATCH, 'between takes exactly two values, [low, high].', {});
    }
    return [coerceScalar(field, dimension, raw[0], fail), coerceScalar(field, dimension, raw[1], fail)];
  }
  if (op === 'in' || op === 'not_in') {
    if (!Array.isArray(raw)) {
      fail(REASON.TYPE_MISMATCH, `${op} takes an array of values.`, { received_type: typeof raw });
    }
    if (raw.length === 0) {
      fail(REASON.TYPE_MISMATCH, `${op} takes a non-empty array; an empty membership list is a question with no answer, not a filter.`, {});
    }
    if (raw.length > MAX_IN_VALUES) {
      throw tooBroad(REASON.LIST_TOO_LONG, `A membership list is capped at ${MAX_IN_VALUES} values.`, {
        field,
        max_values: MAX_IN_VALUES,
        requested: raw.length,
        path,
      });
    }
    const seen = new Set();
    const out = [];
    for (const item of raw) {
      const value = coerceScalar(field, dimension, item, fail);
      const key = String(value);
      if (!seen.has(key)) {
        seen.add(key);
        out.push(value);
      }
    }
    return out;
  }
  return coerceScalar(field, dimension, raw, fail);
}

function coerceScalar(field, dimension, raw, fail) {
  if (raw !== null && typeof raw === 'object') {
    fail(REASON.VALUE_NOT_SCALAR, 'A filter value must be a scalar; nested objects and arrays are not values.', {
      received_type: Array.isArray(raw) ? 'array' : 'object',
    });
  }
  switch (dimension.type) {
    case 'timestamp': {
      if (typeof raw !== 'string' || !ISO_UTC.test(raw)) {
        fail(REASON.TYPE_MISMATCH, 'A timestamp filter must be an ISO-8601 UTC instant, e.g. 2026-10-01T00:00:00Z.', { received_type: typeof raw });
      }
      const ms = Date.parse(raw);
      if (!Number.isFinite(ms)) fail(REASON.TYPE_MISMATCH, 'That timestamp is not a real instant.', {});
      return new Date(ms).toISOString();
    }
    case 'date': {
      if (typeof raw !== 'string' || !DATE_ONLY.test(raw)) {
        fail(REASON.TYPE_MISMATCH, 'A date filter must be YYYY-MM-DD.', { received_type: typeof raw });
      }
      const ms = Date.parse(`${raw}T00:00:00Z`);
      if (!Number.isFinite(ms) || new Date(ms).toISOString().slice(0, 10) !== raw) {
        fail(REASON.TYPE_MISMATCH, 'That date does not exist.', {});
      }
      return raw;
    }
    case 'number': {
      if (typeof raw !== 'number' || !Number.isFinite(raw)) {
        fail(REASON.TYPE_MISMATCH, 'A numeric filter must be a finite number.', { received_type: typeof raw });
      }
      return raw;
    }
    case 'boolean': {
      if (typeof raw !== 'boolean') {
        fail(REASON.TYPE_MISMATCH, 'A boolean filter must be true or false.', { received_type: typeof raw });
      }
      return raw;
    }
    case 'uuid': {
      if (typeof raw !== 'string' || !UUID_RE.test(raw)) {
        fail(REASON.TYPE_MISMATCH, 'A uuid filter must be a canonical uuid string.', { received_type: typeof raw });
      }
      return raw.toLowerCase();
    }
    case 'text':
    default: {
      if (typeof raw !== 'string') {
        fail(REASON.TYPE_MISMATCH, `A ${dimension.type} filter must be a string.`, { received_type: typeof raw });
      }
      if (raw.length === 0) {
        fail(REASON.TYPE_MISMATCH, 'An empty string is not a filter value.', {});
      }
      if (raw.length > MAX_VALUE_LENGTH) {
        throw tooBroad(REASON.VALUE_TOO_LONG, `A filter value is capped at ${MAX_VALUE_LENGTH} characters.`, {
          field,
          max_length: MAX_VALUE_LENGTH,
          requested: raw.length,
        });
      }
      return raw;
    }
  }
}

function resolveFilters(doc, source) {
  const raw = doc.filters ?? [];
  if (!Array.isArray(raw)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'filters must be an array of {field, op, value} objects.');
  }
  if (raw.length > MAX_FILTERS) {
    throw tooBroad(REASON.TOO_MANY_FILTERS, `At most ${MAX_FILTERS} filters may be combined.`, {
      max_filters: MAX_FILTERS,
      requested: raw.length,
    });
  }
  const out = [];
  for (let i = 0; i < raw.length; i += 1) {
    const item = raw[i];
    const path = `filters[${i}]`;
    if (!isPlainObject(item)) {
      throw unsupported(REASON.MALFORMED_DOCUMENT, `${path} must be an object with field, op and value.`, { path });
    }
    for (const key of Object.keys(item)) {
      if (!['field', 'op', 'value'].includes(key)) {
        throw unsupported(REASON.UNKNOWN_KEY, `${path} carries the unknown key "${key}".`, { path, key, known_keys: ['field', 'op', 'value'] });
      }
    }
    const field = item.field;
    const op = item.op;
    if (typeof field !== 'string') {
      throw unsupported(REASON.UNKNOWN_DIMENSION, `${path}.field must be a dimension or column name.`, { path });
    }
    if (typeof op !== 'string' || !OPERATORS.includes(op)) {
      throw unsupported(REASON.UNKNOWN_OPERATOR, `Unknown operator "${String(op)}"; recognised operators are a closed set.`, {
        path,
        operator: op,
        known_operators: OPERATORS,
      });
    }
    const dimension = source.dimensions[field] ?? source.columns?.[field] ?? source.predicates?.[field];
    if (!dimension) {
      throw unsupported(REASON.UNKNOWN_DIMENSION, `Unknown filter field "${field}" for source "${source.id}".`, {
        path,
        field,
        known_fields: [...Object.keys(source.dimensions), ...Object.keys(source.columns ?? {}), ...Object.keys(source.predicates ?? {})],
      });
    }
    if (!dimension.operators.includes(op)) {
      // `starts_with` is permitted only where a dimension grants it. Anything else is refused
      // with the reason, never quietly re-interpreted as equality.
      const detail = { path, field, operator: op, permitted: dimension.operators };
      if (op === 'starts_with' && !dimension.startsWith) {
        throw unsupported(REASON.OPERATOR_NOT_APPLICABLE, `starts_with is permitted only on tool fingerprints and a person's name_key; "${field}" does not support it.`, detail);
      }
      throw unsupported(REASON.OPERATOR_NOT_APPLICABLE, `Operator "${op}" does not apply to "${field}" (${dimension.type}).`, detail);
    }
    const value = coerceValue(field, dimension, op, item.value, path);
    out.push(Object.freeze({ field, op, value }));
  }
  return out;
}

/**
 * The window is mandatory everywhere except on a source that has no event clock at all: the
 * device-liveness list is current state, not an event stream, and its result set is bounded by
 * the cursor's page cap instead.
 */
function resolveWindow(doc, source) {
  const window = doc.window;
  if (window === null || window === undefined) {
    if (source.kind === 'list' && source.time === null) return null;
    throw unsupported(REASON.MISSING_WINDOW, 'A window is required: no read in this DSL is unbounded.', { example: { from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z' } });
  }
  for (const key of Object.keys(window)) {
    if (key !== 'from' && key !== 'to') {
      throw unsupported(REASON.UNKNOWN_KEY, `window carries the unknown key "${key}".`, { key, known_keys: ['from', 'to'] });
    }
  }
  const { from, to } = window;
  if (typeof from !== 'string' || !ISO_UTC.test(from) || typeof to !== 'string' || !ISO_UTC.test(to)) {
    throw unsupported(REASON.BAD_WINDOW, 'window.from and window.to must be ISO-8601 UTC instants.', { from, to });
  }
  const fromMs = Date.parse(from);
  const toMs = Date.parse(to);
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs)) {
    throw unsupported(REASON.BAD_WINDOW, 'window bounds must be real instants.', { from, to });
  }
  if (toMs <= fromMs) {
    throw unsupported(REASON.BAD_WINDOW, 'window.to must be after window.from; the window is half-open [from, to).', { from, to });
  }
  const days = (toMs - fromMs) / 86_400_000;
  if (days > MAX_WINDOW_DAYS) {
    throw tooBroad(REASON.WINDOW_TOO_WIDE, `A window is capped at ${MAX_WINDOW_DAYS} days.`, {
      max_days: MAX_WINDOW_DAYS,
      requested_days: days,
    });
  }
  return Object.freeze({ from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() });
}

function resolveOrder(doc, source, dimensions, measures, bucket) {
  /** Field name -> {dir} that the compiled statement can order by. */
  const orderable = new Set([...measures, ...dimensions]);
  if (bucket) orderable.add('bucket');
  const raw = doc.order ?? [];
  if (!Array.isArray(raw)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'order must be an array of {by, dir}.');
  }
  if (source.kind === 'list' && raw.length > 0) {
    // Each list read path has one ordering key, and it ends in columns unique for the
    // tenant. Letting a caller reorder a list would break the exactness of its keyset page.
    throw unsupported(REASON.CURSOR_REQUIRES_TOTAL_ORDER, `Source "${source.id}" has a fixed total ordering (${source.order.map((o) => `${o.dim} ${o.dir}`).join(', ')}); order is not a client choice on this read path.`, {
      source: source.id,
      ordering: source.order,
    });
  }
  if (raw.length > 3) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'order takes at most three terms; the rest is the deterministic tie-break and is added by the compiler.', { requested: raw.length });
  }
  const seen = new Set();
  const out = [];
  for (let i = 0; i < raw.length; i += 1) {
    const item = raw[i];
    if (!isPlainObject(item)) throw unsupported(REASON.MALFORMED_DOCUMENT, `order[${i}] must be an object with by and dir.`, {});
    for (const key of Object.keys(item)) {
      if (!['by', 'dir'].includes(key)) {
        throw unsupported(REASON.UNKNOWN_KEY, `order[${i}] carries the unknown key "${key}".`, { key, known_keys: ['by', 'dir'] });
      }
    }
    const by = item.by;
    const dir = item.dir ?? 'desc';
    if (typeof by !== 'string' || !orderable.has(by)) {
      throw unsupported(REASON.UNKNOWN_MEASURE, `Cannot order by "${String(by)}": order is only on a returned measure or a grouped dimension.`, {
        by,
        orderable: [...orderable],
      });
    }
    if (dir !== 'asc' && dir !== 'desc') {
      throw unsupported(REASON.MALFORMED_DOCUMENT, `order[${i}].dir must be "asc" or "desc".`, { dir });
    }
    if (source.rankable === false && measures.includes(by)) {
      // A per-person source ordered by a measure is a leaderboard.
      throw unsupported(REASON.UNKNOWN_MEASURE, `Source "${source.id}" lists people and is not ordered by a measure; order by a dimension instead.`, {
        by,
        orderable: [...dimensions],
      });
    }
    if (seen.has(by)) throw unsupported(REASON.MALFORMED_DOCUMENT, `Ordering on "${by}" twice is not meaningful.`, { by });
    seen.add(by);
    out.push(Object.freeze({ by, dir }));
  }
  return Object.freeze(out);
}

function resolveLimit(doc, source, klass) {
  const spec = klass;
  const raw = doc.limit ?? spec.defaultPageSize;
  if (raw === null || raw === undefined) return null;
  if (typeof raw !== 'number' || !Number.isInteger(raw) || raw < 1) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'limit must be a positive integer.', { limit: raw });
  }
  if (spec.maxPageSize !== null && raw > spec.maxPageSize) {
    throw tooBroad(REASON.COST_ESTIMATE_EXCEEDED, `limit ${raw} exceeds the ${spec.maxPageSize}-row cap for this query class.`, {
      max_limit: spec.maxPageSize,
      requested: raw,
      query_class: source.costClass,
    });
  }
  return raw;
}

function resolveCursor(doc, source) {
  const raw = doc.cursor ?? null;
  if (raw === null) return null;
  if (typeof raw !== 'string') {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'cursor must be an opaque string or null.', { received_type: typeof raw });
  }
  if (source.kind !== 'list') {
    throw unsupported(REASON.AGGREGATE_NOT_PAGED, `Source "${source.id}" is an aggregate and is not paged: one response is bounded by limit. Narrow the window or the filters instead.`, { source: source.id });
  }
  if (raw.length > 4096) {
    // Every cursor failure is `cursor_expired`: the client's action is the same in all of them,
    // restart from page one, told why.
    throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor is too long to be one of ours.', { max_length: 4096 });
  }
  return raw;
}

function resolveRollup(doc, source) {
  const rollup = doc.rollup ?? false;
  if (typeof rollup !== 'boolean') {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'rollup must be a boolean.', { received_type: typeof rollup });
  }
  if (rollup && source.kind !== 'aggregate') {
    throw unsupported(REASON.MALFORMED_DOCUMENT, `Source "${source.id}" is a bounded list; a published total over a list is a cost guard problem, not a grouping one.`, { source: source.id });
  }
  return rollup;
}

/**
 * The window column of a source, as a synthetic field the validator will accept in filters.
 * It exists so that "received_at between X and Y" is expressible *and* so that the mandatory
 * window has a field name to collide with.
 */
function timeField(source) {
  if (!source.time) return null;
  return Object.freeze({
    name: source.time.name,
    sql: source.time.sql,
    type: source.time.type,
    nullable: false,
    startsWith: false,
    cardinality: 0,
    operators: Object.freeze(['eq', 'ne', 'lt', 'lte', 'gt', 'gte', 'between']),
  });
}

/**
 * Validate and normalise a query document. Pure: same input, same output, no I/O.
 *
 * @param {unknown} doc
 * @returns {{query: NormalisedQuery, source: object, klass: object}}
 * @throws {QueryError} on any rejection, with its result_state already attached
 */
export function validate(doc) {
  assertShape(doc);
  requireVersion(doc);
  const source = resolveSource(doc);

  // The window column is filterable under its own name (`received_at`, `detected_at`, ...).
  const time = timeField(source);
  const withTime = time && !source.dimensions[time.name] && !source.columns?.[time.name]
    ? Object.freeze({ ...source, dimsPlus: null, columns: Object.freeze({ ...(source.columns ?? {}), [time.name]: time }) })
    : source;

  const klass = klassOf(withTime);
  const bucketChoice = resolveBucket(doc, withTime);
  const bucket = bucketChoice.bucket;
  const dimensions = resolveDimensions(doc, withTime, bucket);
  assertSubjectScope(doc, withTime, dimensions);
  const measures = resolveMeasures(doc, withTime);
  const filters = resolveFilters(doc, withTime);
  const window = resolveWindow(doc, withTime);
  const order = resolveOrder(doc, withTime, dimensions, measures, bucket);
  const limit = resolveLimit(doc, withTime, klass);
  const cursor = resolveCursor(doc, withTime);
  const rollup = resolveRollup(doc, withTime);

  /** @type {NormalisedQuery} */
  const query = Object.freeze({
    query_version: QUERY_VERSION,
    source: withTime.id,
    kind: withTime.kind,
    bucket,
    bucket_explicit: bucketChoice.explicit,
    dimensions: Object.freeze([...dimensions]),
    measures: Object.freeze([...measures]),
    filters: Object.freeze([...filters]),
    window,
    order,
    limit,
    cursor,
    rollup,
  });

  return Object.freeze({ query, source: withTime, klass });
}

function klassOf(source) {
  return QUERY_CLASSES[source.costClass];
}

/**
 * Stable canonical JSON: object keys sorted, no whitespace. Used for the cursor's `dsl_hash`, so
 * that two documents that mean the same question hash the same.
 * @param {unknown} value
 * @returns {string}
 */
export function canonicalJson(value) {
  if (value === null || value === undefined) return 'null';
  if (value instanceof Date) return JSON.stringify(value.toISOString());
  if (typeof value !== 'object') return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(',')}]`;
  const keys = Object.keys(value).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonicalJson(value[k])}`).join(',')}}`;
}
