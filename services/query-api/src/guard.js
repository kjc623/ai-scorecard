// guard.js — the cost guard: rejection, not degradation.
//
// The DSL is closed, so cost is knowable before execution. This module never runs EXPLAIN and
// never touches the database. It multiplies the declared
// cardinality of each grouped dimension by the number of buckets in the window, and compares
// the product to the class's cap. Where a filter pins a dimension, the estimate is tightened, so
// that a well-narrowed question is not refused for the shape of a broad one.
//
// Every refusal names the fix: the coarser bucket, the narrower window, or the dimension to drop.

import {
  COARSENING_ORDER,
  MAX_RESPONSE_BYTES,
  MAX_SERIES_POINTS,
  MAX_UNNARROWED_SUBJECT_WINDOW_DAYS,
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
 * @property {object|null} coarsened {from, to, reason} when the series was auto-coarsened
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
  // A time series is at most 400 points. Who chose the resolution decides what happens beyond
  // that: a series whose bucket the server picked (none was asked for) is auto-coarsened and says
  // so; a caller who pinned a bucket that cannot fit is refused and told which bucket would fit.
  // Silently changing a resolution a caller asked for would be degrading instead of rejecting.
  // -------------------------------------------------------------------------------------------
  let coarsened = null;
  const maxSeriesPoints = opts.maxSeriesPoints ?? MAX_SERIES_POINTS;
  if (query.bucket) {
    const bucketsAtRequested = countBuckets(windowDays, query.bucket);
    if (bucketsAtRequested > maxSeriesPoints) {
      const fitting = fittingBucket(windowDays, query.bucket, maxSeriesPoints);
      if (query.bucket_explicit || !fitting) {
        throw tooBroad(
          REASON.SERIES_TOO_LONG,
          `A ${query.bucket}-bucketed window of ${round(windowDays)} days is about ${bucketsAtRequested} points, above the ${maxSeriesPoints}-point series bound.`,
          {
            max_points: maxSeriesPoints,
            requested_points: bucketsAtRequested,
            window_days: round(windowDays),
            ...(fitting ? { fix: { coarser_bucket: fitting } } : { fix: { narrow_window: true, drop_dimensions: true } }),
          },
        );
      }
      coarsened = {
        from: query.bucket,
        to: fitting,
        reason: `series_exceeds_${maxSeriesPoints}_points`,
      };
      notes.push(`A ${query.bucket}-bucketed window of ${round(windowDays)} days exceeds ${maxSeriesPoints} points; the server-chosen bucket was auto-coarsened to ${fitting} and the applied bucket is reported in freshness.`);
      query = Object.freeze({ ...query, bucket: fitting });
    }
  }

  const buckets = query.bucket ? countBuckets(windowDays, query.bucket) : 1;
  const cardinalities = groupedCardinalities(query, source);
  let estimatedCells = buckets;
  for (const c of cardinalities) estimatedCells *= c;
  if (query.kind === 'list') estimatedCells = query.limit ?? 0;
  if (query.rollup) estimatedCells += 1;

  const maxCells = opts.maxCells ?? klass.maxCells;
  // An aggregate response carries at most 2,000 cells. The cap applies to what the response can
  // carry:
  //   * no limit  -> the whole estimated result set must fit;
  //   * a limit   -> the limited response must fit, and the estimate is reported rather than
  //                  hidden (Q2's tool-by-subject list is bounded this way).
  const limited = query.kind === 'aggregate' && query.limit !== null;
  const boundedCells = limited ? Math.min(estimatedCells, query.limit + 1) : estimatedCells;
  if (maxCells !== null && query.kind === 'aggregate' && boundedCells > maxCells) {
    const suggestion = suggestCoarserBucket(query, source, cardinalities, maxCells);
    throw tooBroad(REASON.COST_ESTIMATE_EXCEEDED, `This shape may return about ${estimatedCells} cells, above the ${maxCells}-cell cap for a ${source.costClass} read.`, {
      estimated_cells: estimatedCells,
      max_cells: maxCells,
      window_days: round(windowDays),
      grouped_cardinalities: cardinalities,
      fix: suggestion,
    });
  }
  if (limited && estimatedCells > maxCells) {
    notes.push(
      `Limited aggregate: the response is bounded by its ${query.limit}-row limit, and the full result is estimated at ${estimatedCells} cells. The estimate is reported so it is not mistaken for a total.`,
    );
  }

  const estimatedBytes = query.kind === 'list'
    ? (query.limit ?? 0) * ESTIMATED_BYTES_PER_CELL + ESTIMATED_BYTES_OVERHEAD
    : boundedCells * ESTIMATED_BYTES_PER_CELL + ESTIMATED_BYTES_OVERHEAD;
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
    boundedCells,
    limited,
    estimatedBytes,
    coarsened,
    notes: Object.freeze(notes),
    severability,
  });
}

/**
 * A list window is capped, and the cap is not a suggestion. The window is half-open [from, to).
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
 * A window beyond 7 days is refused when `subject` is a grouping dimension and no tool, team or
 * subject filter narrows it; the refusal names the narrowing that would make it servable.
 */
function assertSubjectScope(query, source) {
  if (!query.dimensions.includes('subject')) return;
  const days = windowDaysOf(query);
  const narrowedBySubject = query.filters.some((f) => f.field === 'subject' && (f.op === 'eq' || f.op === 'in'));
  const narrowedByTool = query.filters.some((f) => f.field === 'tool' && (f.op === 'eq' || f.op === 'in' || f.op === 'starts_with'));
  const narrowedByTeam = query.filters.some((f) => f.field === 'team' && (f.op === 'eq' || f.op === 'in'));
  if (days > MAX_UNNARROWED_SUBJECT_WINDOW_DAYS && !narrowedBySubject && !narrowedByTool && !narrowedByTeam) {
    throw tooBroad(REASON.WINDOW_TOO_WIDE, `A subject-grouped read is refused beyond ${MAX_UNNARROWED_SUBJECT_WINDOW_DAYS} days unless it is narrowed by a tool, team or subject filter.`, {
      max_days: MAX_UNNARROWED_SUBJECT_WINDOW_DAYS,
      requested_days: round(days),
      fix: { add_filter: ['tool eq/in', 'team eq/in', 'subject eq/in'], or_narrow_window_to_days: MAX_UNNARROWED_SUBJECT_WINDOW_DAYS },
    });
  }
  void source;
}

/**
 * No shape may be served by a sequential scan of `ingest.submission`. Every list shape declares
 * the index it requires, and an uncovered filter combination is `unsupported_query_shape`
 * naming the filters that would make it servable.
 *
 * The check that bites is prefix matching: `starts_with` on a tool fingerprint cannot use the
 * `submission_by_tool` btree, and there is no text-pattern index on `ingest.submission`. Prefix
 * matching over tool fingerprints is served from the mart instead, and that is the fix the error
 * names.
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

/** The next coarser bucket that fits the point bound, or null when none does. */
function fittingBucket(days, from, maxPoints) {
  for (const candidate of COARSENING_ORDER.slice(COARSENING_ORDER.indexOf(from) + 1)) {
    if (countBuckets(days, candidate) <= maxPoints) return candidate;
  }
  return null;
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
    if (name === 'subject' && cardinality > 500) {
      // Grouping by subject is only ever a subject-scoped lookup; the estimator keeps it from
      // pretending a 4,000-person enumeration is a small question.
      out.push(cardinality);
      continue;
    }
    out.push(cardinality);
  }
  return out;
}

/** Name the coarser bucket that would fit, or the dimension to drop. */
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

function round(value) {
  return Math.round(value * 100) / 100;
}
