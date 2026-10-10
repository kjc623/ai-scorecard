// envelope.js — the response envelope: freshness, coverage, suppression and the result state.
//
// Three properties matter more than the field names, and all three are encoded here rather than
// left to a caller's discipline:
//
//   * `freshness` and `coverage` are always present on a data-bearing response, so the UI cannot
//     render a number and omit its state without visibly discarding fields it was given.
//   * `result_state` is a first-class answer, not an error channel.
//   * `audit.entry_id` is returned to the caller, so a screenshot of a number traces to the
//     audited read that produced it.
//
// `buildEnvelope` throws when a data-bearing envelope is constructed without its two honesty
// blocks: a programming error here must surface as a 500, never as a number on a dashboard with
// no denominator.

import {
  API_VERSION,
  FRESHNESS_CADENCE_MS,
  FRESHNESS_STALE_MULTIPLIER,
  QUERY_VERSION,
} from './registry.js';

/**
 * @param {object} input
 * @param {object|null} input.row   a row of ops.aggregate_watermark, or null when none exists
 * @param {string} input.aggregate  the aggregate the numbers came from
 * @param {string} input.bucketSize the applied bucket_size
 * @param {number} [input.now]
 */
export function freshnessBlock(input) {
  const now = input.now ?? Date.now();
  const stalenessMs = FRESHNESS_CADENCE_MS * FRESHNESS_STALE_MULTIPLIER;
  if (!input.row) {
    // Where a measure is absent for a tenant the panel returns `not_yet_covered` with the
    // reason, never a partial number presented as a whole one.
    return Object.freeze({
      aggregate: input.aggregate,
      bucket_size: input.bucketSize ?? null,
      last_run_at: null,
      last_complete_bucket: null,
      lag_seconds: null,
      state: 'not_yet_covered',
      reason: input.reason ?? 'no_watermark_row',
    });
  }
  const lastRunAt = toIso(input.row.last_run_at);
  const lagSeconds = lastRunAt === null ? null : Math.max(0, Math.round((now - Date.parse(lastRunAt)) / 1000));
  const stale = lastRunAt === null || (now - Date.parse(lastRunAt)) > stalenessMs;
  return Object.freeze({
    aggregate: input.aggregate,
    bucket_size: input.row.bucket_size ?? input.bucketSize ?? null,
    last_run_at: lastRunAt,
    last_complete_bucket: toIso(input.row.last_complete_bucket),
    last_run_rows: input.row.last_run_rows ?? null,
    lag_seconds: lagSeconds,
    // `stale` when now() - last_run_at exceeds 3x the cadence (15 minutes). A stale
    // aggregate is never silently recomputed on the read path.
    state: stale ? 'stale' : 'fresh',
    ...(stale ? { stale_after_seconds: Math.round(stalenessMs / 1000) } : {}),
  });
}

/**
 * The denominator is stated, never implied. Coverage is measured over the
 * ENROLLED fleet, and `unknown` shows as a reason rather than a blank.
 *
 * @param {object} input
 * @param {object|null} input.row  { devices_enrolled, devices_reporting, expected_collector_days,
 *                                   observed_collector_days, gap_reasons }
 */
export function coverageBlock(input) {
  const row = input.row;
  if (!row) {
    return Object.freeze({
      window: input.window ?? null,
      devices_reporting: null,
      devices_enrolled: null,
      expected_collector_days: null,
      observed_collector_days: null,
      gap_reasons: Object.freeze({}),
      state: 'not_yet_covered',
      reason: input.reason ?? 'no_coverage_snapshot',
    });
  }
  const enrolled = numberOrNull(row.devices_enrolled);
  const reporting = numberOrNull(row.devices_reporting);
  const expected = numberOrNull(row.expected_collector_days);
  const observed = numberOrNull(row.observed_collector_days);
  const gaps = normaliseGaps(row.gap_reasons);
  const missing = Object.values(gaps).reduce((a, b) => a + b, 0);
  let state = 'complete';
  if (expected !== null && observed !== null) {
    if (expected === 0) state = 'not_yet_covered';
    else if (observed < expected) state = 'partial';
  }
  if (state === 'complete' && (enrolled === null || reporting === null || reporting < enrolled)) state = 'partial';
  return Object.freeze({
    window: input.window ?? null,
    devices_reporting: reporting,
    devices_enrolled: enrolled,
    expected_collector_days: expected,
    observed_collector_days: observed,
    // gap_reason is a closed vocabulary and `unknown` is a value in it, not an absence, because
    // blank and unknown look identical on a dashboard and mean different things.
    gap_reasons: gaps,
    gaps_total: missing,
    state,
  });
}

function normaliseGaps(value) {
  if (!value || typeof value !== 'object') return Object.freeze({});
  const out = {};
  for (const [key, raw] of Object.entries(value)) {
    const n = numberOrNull(raw);
    if (n !== null) out[key] = n;
  }
  return Object.freeze(out);
}

/**
 * The result state when several are true at once (a stale watermark AND partial coverage, say),
 * from strongest claim to weakest:
 *
 *   1. `not_yet_covered` — the system has no source for this window or dimension at all.
 *   2. `coverage_degraded` — we have numbers, and they are a floor.
 *   3. `stale_aggregate` — we have numbers and we know how old they are.
 *   4. `empty` — we looked, coverage was adequate, there is nothing.
 *   5. `ok`.
 *
 * A floor beats an age because it changes what the number means; the age is carried in
 * `freshness` either way, so nothing is lost by choosing.
 *
 * @param {object} input
 * @returns {string}
 */
export function resultStateFor(input) {
  const rows = input.rowCount ?? 0;
  const freshnessState = input.freshness?.state ?? 'fresh';
  const coverageState = input.coverage?.state ?? 'complete';

  if (rows === 0) {
    // Nothing to show, and we can say why: no source for the window or dimension at all beats
    // every weaker statement, because it is the one that says "do not read this as zero".
    if (coverageState === 'not_yet_covered') return 'not_yet_covered';
    if (freshnessState === 'not_yet_covered') return 'not_yet_covered';
    if (freshnessState === 'stale') return 'stale_aggregate';
    if (coverageState === 'partial') return 'coverage_degraded';
    return 'empty';
  }
  // Data is present. Coverage first: a floor is a stronger statement about the number than its
  // age is, and where coverage cannot be stated at all the number must be treated as a floor
  // (a path that cannot say must not read as a clean bill of health).
  if (coverageState === 'partial' || coverageState === 'not_yet_covered') return 'coverage_degraded';
  if (freshnessState === 'stale') return 'stale_aggregate';
  return 'ok';
}

/**
 * Build the envelope. Throws if a data-bearing envelope is missing its freshness or coverage, so a
 * code path cannot forget them.
 */
export function buildEnvelope(input) {
  const resultState = input.resultState ?? resultStateFor(input);
  const carryData = input.data !== undefined && input.data !== null;
  if (carryData && (!input.freshness || !input.coverage)) {
    throw new Error(
      'Refusing to build a data-bearing envelope without freshness and coverage: a number must never travel without its state.',
    );
  }
  const envelope = {
    api_version: API_VERSION,
    query_version: QUERY_VERSION,
    result_state: resultState,
  };
  if (carryData) envelope.data = input.data;
  if (input.page) envelope.page = input.page;
  if (input.freshness) envelope.freshness = input.freshness;
  if (input.coverage) envelope.coverage = input.coverage;
  if (input.audit) envelope.audit = input.audit;
  if (input.meta) envelope.meta = input.meta;
  return Object.freeze(envelope);
}

function toIso(value) {
  if (value === null || value === undefined) return null;
  const ms = value instanceof Date ? value.getTime() : Date.parse(String(value));
  return Number.isFinite(ms) ? new Date(ms).toISOString() : null;
}

function numberOrNull(value) {
  if (value === null || value === undefined) return null;
  const n = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(n) ? n : null;
}
