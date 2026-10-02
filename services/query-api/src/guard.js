// guard.js — §12.2's cost guard: rejection, not degradation.
//
// "The DSL is closed, so cost is knowable before execution. Each query shape declares a cost
// class; the guard multiplies requested cells by grouping cardinalities and refuses over-budget
// requests before the statement runs."
//
// So this module never runs EXPLAIN and never touches the database. It multiplies the declared
// cardinality of each grouped dimension by the number of buckets in the window, and compares
// the product to the class's cap. Where a filter pins a dimension, the estimate is tightened, so
// that a well-narrowed question is not refused for the shape of a broad one.
//
// Every refusal names the fix: the coarser bucket, the narrower window, or the dimension to drop.

import {
  COARSENING_ORDER,
  K,
  MAX_LIST_WINDOW_DAYS,
  MAX_RESPONSE_BYTES,
  MAX_SERIES_POINTS,
  MAX_UNNARROWED_SUBJECT_WINDOW_DAYS,
  NATIVE_BUCKETS,
} from './registry.js';
import { REASON, tooBroad, unsupported } from './errors.js';

const MS_PER_DAY = 86_400_000;
const ESTIMATED_BYTES_PER_CELL = 320;
const ESTIMATED_BYTES_OVERHEAD = 4096;

/** Bucket -> its length in days, for the bucket count. */
const BUCKET_DAYS = Object.freeze({ hour: 1 / 24, day: 1, week: 7, month: 30.4375 });

/**
 * @typedef {object} GuardVerdict
 * @property {object} query          the query, possibly with a coarsened bucket applied
 * @property {number} buckets        buckets in the window under the applied bucket
 * @property {number} estimatedCells cells the response may contain
 * @property {number} estimatedBytes lower-bound estimate of the response body
 * @property {object|null} coarsened {from, to, reason} when §7.5's auto-coarsening fired
 * @property {ReadonlyArray<string>} notes
 * @property {object|null} severability
 */

/**
 * @param {{query: object, source: object, klass: object}} validated
 * @param {object} [opts]
 * @param {number} [opts.maxSeriesPoints]
 * @param {number} [opts.maxCells]
 * @returns {GuardVerdict}
 * @throws {QueryError} query_too_broad / unsupported_query_shape
 */
export function guard(validated, opts = {}) {
  const { source, klass } = validated;
  let query = validated.query;
  const notes = [];

  assertListWindow(query, source);
  assertSubjectScope(query, source);
  const severability = assertSeverability(query, source);

  const windowDays = windowDaysOf(query);

  // -------------------------------------------------------------------------------------------
  // §7.5 time series: at most 400 points, auto-coarsened, and the applied bucket is reported in
  // `freshness`. Auto-coarsening is the one place the document allows the shape to change under
  // the caller; it never changes silently, and `coarsened` is returned for that reason.
  // -------------------------------------------------------------------------------------------
  let coarsened = null;
  const maxSeriesPoints = opts.maxSeriesPoints ?? MAX_SERIES_POINTS;
  if (query.bucket) {
    let applied = query.bucket;
    let buckets = countBuckets(windowDays, applied);
    while (buckets > maxSeriesPoints) {
      const next = COARSENING_ORDER[COARSENING_ORDER.indexOf(applied) + 1];
      if (!next) break;
      applied = next;
      buckets = countBuckets(windowDays, applied);
    }
    if (applied !== query.bucket) {
      coarsened = {
        from: query.bucket,
        to: applied,
        reason: `series_exceeds_${maxSeriesPoints}_points`,
      };
      notes.push(`A ${query.bucket}-bucketed window of ${round(windowDays)} days exceeds ${maxSeriesPoints} points; it was auto-coarsened to ${applied} (§7.5) and the applied bucket is reported in freshness.`);
      query = Object.freeze({ ...query, bucket: applied });
    }
    if (countBuckets(windowDays, query.bucket) > maxSeriesPoints) {
      throw tooBroad(REASON.SERIES_TOO_LONG, `This series cannot be reduced below ${maxSeriesPoints} points even at month granularity; narrow the window or drop a grouping dimension.`, {
        max_points: maxSeriesPoints,
        window_days: round(windowDays),
        coarsest_bucket: 'month',
      });
    }
  }

  const buckets = query.bucket ? countBuckets(windowDays, query.bucket) : 1;
  const cardinalities = groupedCardinalities(query, source);
  let estimatedCells = buckets;
  for (const c of cardinalities) estimatedCells *= c;
  if (query.kind === 'list') estimatedCells = query.limit ?? 0;
  if (query.rollup) estimatedCells += 1;

  const maxCells = opts.maxCells ?? klass.maxCells;
  if (maxCells !== null && query.kind === 'aggregate' && estimatedCells > maxCells) {
    const suggestion = suggestCoarserBucket(query, source, cardinalities, maxCells);
    throw tooBroad(REASON.COST_ESTIMATE_EXCEEDED, `This shape may return about ${estimatedCells} cells, above the ${maxCells}-cell cap for a ${source.costClass} read.`, {
      estimated_cells: estimatedCells,
      max_cells: maxCells,
      window_days: round(windowDays),
      grouped_cardinalities: cardinalities,
      fix: suggestion,
    });
  }

  const estimatedBytes = query.kind === 'list'
    ? (query.limit ?? 0) * ESTIMATED_BYTES_PER_CELL + ESTIMATED_BYTES_OVERHEAD
    : estimatedCells * ESTIMATED_BYTES_PER_CELL + ESTIMATED_BYTES_OVERHEAD;
  if (estimatedBytes > MAX_RESPONSE_BYTES) {
    throw tooBroad(REASON.COST_ESTIMATE_EXCEEDED, `This response could reach about ${Math.round(estimatedBytes / 1024 / 1024)} MB, above the 8 MB bound.`, {
      estimated_bytes: estimatedBytes,
      max_bytes: MAX_RESPONSE_BYTES,
      fix: { drop_dimensions: true, narrow_window: true, lower_limit: true },
    });
  }

  return Object.freeze({
    query,
    buckets,
    estimatedCells,
    estimatedBytes,
    coarsened,
    notes: Object.freeze(notes),
    severability,
  });
}

/**
 * §12.1 / §3.8: a list window is capped, and the cap is not a suggestion. The window is
 * half-open [from, to).
 */
function assertListWindow(query, source) {
  const maxDays = source.time?.maxDays ?? null;
  if (maxDays === null) return;
  const days = windowDaysOf(query);
  if (days > maxDays) {
    throw tooBroad(REASON.WINDOW_TOO_WIDE, `Source "${source.id}" caps its window at ${maxDays} days${narrowingHint(source) ? ` unless narrowed by ${narrowingHint(source)}` : ''}; this one is ${round(days)} days.`, {
      max_days: maxDays,
      requested_days: round(days),
      fix: narrowingHint(source) ? { add_filter: narrowingHint(source) } : { narrow_window: true },
    });
  }
}

function narrowingHint(source) {
  if (source.time?.name === 'received_at') return 'subject, tool, device or class';
  return null;
}

/**
 * §3.2: "the guard refuses an unscoped window beyond 7 days when `subject` is a grouping
 * dimension, naming the narrowing that would make it servable."
 */
function assertSubjectScope(query, source) {
  if (!query.dimensions.includes('subject')) return;
  const days = windowDaysOf(query);
  const narrowedBySubject = query.filters.some((f) => f.field === 'subject' && (f.op === 'eq' || f.op === 'in'));
  const narrowedByTool = query.filters.some((f) => f.field === 'tool' && (f.op === 'eq' || f.op === 'in' || f.op === 'starts_with'));
  if (days > MAX_UNNARROWED_SUBJECT_WINDOW_DAYS && !narrowedBySubject && !narrowedByTool) {
    throw tooBroad(REASON.WINDOW_TOO_WIDE, `A subject-grouped read is refused beyond ${MAX_UNNARROWED_SUBJECT_WINDOW_DAYS} days unless it is narrowed by a tool or a subject filter (docs/04 §3.2).`, {
      max_days: MAX_UNNARROWED_SUBJECT_WINDOW_DAYS,
      requested_days: round(days),
      fix: { add_filter: ['tool eq/in', 'subject eq/in'], or_narrow_window_to_days: MAX_UNNARROWED_SUBJECT_WINDOW_DAYS },
    });
  }
  void source;
}

/**
 * §12.2: "No shape may be served by a sequential scan of `ingest.submission`. Every list shape
 * declares the index it requires; if the submitted filter combination is not covered, the API
 * returns `unsupported_query_shape` naming the filters that would make it servable."
 *
 * The check that actually bites is prefix matching: `starts_with` on a tool fingerprint cannot
 * use the `submission_by_tool` btree, and there is no trigram or text-pattern index on
 * `ingest.submission` in §3.11's list. Prefix matching over tool fingerprints is served from
 * the mart instead, and that is the fix the error names.
 */
function assertSeverability(query, source) {
  const prefixFilters = query.filters.filter((f) => f.op === 'starts_with');
  if (prefixFilters.length > 0 && (source.id === 'ingest.submission' || source.id === 'mart.v_finding')) {
    throw unsupported(REASON.NO_COVERING_INDEX, `Source "${source.id}" has no index for a prefix predicate (${prefixFilters.map((f) => f.field).join(', ')}); it would be served by scanning the event relation.`, {
      source: source.id,
      offending_filters: prefixFilters.map((f) => ({ field: f.field, op: f.op })),
      fix: {
        alternative_source: 'mart.agg_tool_period',
        or_use: 'tool eq / in',
      },
    });
  }
  return Object.freeze({
    ok: true,
    required: source.indexes,
    // The window predicate is itself the covering index for every list shape here; it is
    // mandatory (validate.js refuses a document with no window), so this is a declaration that
    // cannot be satisfied by accident.
    covering_predicate: source.time
      ? `${source.time.sql} range`
      : (source.bucket ? `${source.bucket.startColumn} range` : 'cursor-bounded list (no event clock)'),
  });
}

function windowDaysOf(query) {
  if (!query.window) return 0;
  return (Date.parse(query.window.to) - Date.parse(query.window.from)) / MS_PER_DAY;
}

function countBuckets(days, bucket) {
  const unit = BUCKET_DAYS[bucket] ?? 1;
  return Math.max(1, Math.ceil(days / unit));
}

/**
 * Cardinality of each grouped dimension *after* filters, which is the whole point of a closed
 * DSL: a filter that pins a dimension collapses its cardinality to the number of values asked
 * for, so a narrow question is not refused on the shape of a broad one.
 */
function groupedCardinalities(query, source) {
  const out = [];
  for (const name of query.dimensions) {
    const dimension = source.dimensions[name];
    let cardinality = dimension.cardinality;
    for (const filter of query.filters) {
      if (filter.field !== name) continue;
      if (filter.op === 'eq' || filter.op === 'starts_with' || filter.op === 'is_null') cardinality = Math.min(cardinality, 1);
      else if (filter.op === 'in' || filter.op === 'not_in') cardinality = Math.min(cardinality, Math.max(1, filter.value.length));
      else if (filter.op === 'between') cardinality = Math.min(cardinality, Math.max(1, Math.ceil(cardinality / 4)));
    }
    if (name === 'subject' && cardinality > K * 100) {
      // Grouping by subject is only ever a subject-scoped lookup; the estimator keeps it from
      // pretending a 4,000-person enumeration is a small question.
      out.push(cardinality);
      continue;
    }
    out.push(cardinality);
  }
  return out;
}

/** Name the coarser bucket that would fit, or the dimension to drop (§12.2). */
function suggestCoarserBucket(query, source, cardinalities, maxCells) {
  if (query.bucket) {
    const days = windowDaysOf(query);
    for (const candidate of COARSENING_ORDER.slice(COARSENING_ORDER.indexOf(query.bucket) + 1)) {
      let cells = countBuckets(days, candidate);
      for (const c of cardinalities) cells *= c;
      if (cells <= maxCells) return { coarser_bucket: candidate, estimated_cells: cells };
    }
  }
  const sorted = [...cardinalities].sort((a, b) => b - a);
  const drop = sorted.length > 0 ? sorted[0] : null;
  return {
    drop_dimension: drop !== null ? `a dimension with estimated cardinality ${drop}` : 'narrow the window',
    or_narrow_window: true,
  };
}

export const ESTIMATE_BYTES_PER_CELL = ESTIMATED_BYTES_PER_CELL;

/** Exposed for tests: the bucket arithmetic the guard refuses on. */
export function bucketsForWindow(fromIso, toIso, bucket) {
  return countBuckets((Date.parse(toIso) - Date.parse(fromIso)) / MS_PER_DAY, bucket);
}

export { MAX_LIST_WINDOW_DAYS };

function round(value) {
  return Math.round(value * 100) / 100;
}
