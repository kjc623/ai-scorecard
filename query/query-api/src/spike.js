// spike.js — Q6's "has this person's usage changed, or spiked" rule (§3.6).
//
// The document fixes the rule and this module implements it exactly, including the two ways the
// flag lies and the state that must not be merged with "no change":
//
//   * flag a day above median(trailing 28 daily buckets) + 4 x MAD AND >= 3 x the median,
//     requiring >= 14 observed buckets first;
//   * below 14 buckets the answer is `not_yet_covered`, not "no change";
//   * no peer comparison, ever. The subject is compared against their own baseline: no cohort
//     percentile, no ranking, no people sorted by volume (brief §1.2).
//
// The two lies are returned as separate annotations rather than folded into the flag:
// `coverage_degraded` when a device was not reporting (a dip is not a behaviour change), and
// `late_flush` when received_at - first_occurred_at exceeded an hour (a spool flush is not a
// burst of activity).

const MIN_OBSERVED_BUCKETS = 14;
const TRAILING_BUCKETS = 28;
const MAD_MULTIPLIER = 4;
const MEDIAN_MULTIPLIER = 3;

/**
 * @typedef {object} SpikeReport
 * @property {string} state            ok | not_yet_covered
 * @property {number} observed_buckets
 * @property {number} required_buckets
 * @property {ReadonlyArray<object>} spikes
 * @property {ReadonlyArray<string>} notes
 */

/**
 * @param {ReadonlyArray<{bucket_start:string, submissions:number}>} series  ascending by bucket
 * @param {object} [options]
 * @returns {SpikeReport}
 */
export function detectSpikes(series, options = {}) {
  const minBuckets = options.minObservedBuckets ?? MIN_OBSERVED_BUCKETS;
  const trailing = options.trailingBuckets ?? TRAILING_BUCKETS;
  const madMultiplier = options.madMultiplier ?? MAD_MULTIPLIER;
  const medianMultiplier = options.medianMultiplier ?? MEDIAN_MULTIPLIER;
  const measure = options.measure ?? 'submissions';

  const points = [...series]
    .map((row) => ({ bucket_start: row.bucket_start, value: Number(row[measure] ?? 0) }))
    .sort((a, b) => Date.parse(a.bucket_start) - Date.parse(b.bucket_start));

  if (points.length < minBuckets) {
    return Object.freeze({
      state: 'not_yet_covered',
      observed_buckets: points.length,
      required_buckets: minBuckets,
      reason: 'fewer_than_14_observed_buckets',
      spikes: Object.freeze([]),
      notes: Object.freeze([
        'Below 14 observed buckets the answer is `not_yet_covered`, not "no change": there is no baseline to compare against yet (docs/04 §3.6).',
      ]),
    });
  }

  const spikes = [];
  for (let i = minBuckets; i < points.length; i += 1) {
    const history = points.slice(Math.max(0, i - trailing), i).map((p) => p.value);
    if (history.length < minBuckets) continue;
    const median = medianOf(history);
    const mad = medianOf(history.map((v) => Math.abs(v - median)));
    const threshold = median + madMultiplier * mad;
    const candidate = points[i];
    // Both conditions, as written: above the MAD threshold AND at least 3x the median. The
    // second matters when a subject has a flat baseline and MAD is zero.
    if (candidate.value > threshold && candidate.value >= medianMultiplier * median && median > 0) {
      spikes.push(Object.freeze({
        bucket_start: candidate.bucket_start,
        value: candidate.value,
        median,
        mad,
        threshold,
        ratio: median === 0 ? null : candidate.value / median,
      }));
    }
  }

  return Object.freeze({
    state: 'ok',
    observed_buckets: points.length,
    required_buckets: minBuckets,
    spikes: Object.freeze(spikes),
    notes: Object.freeze([
      'Compared against the subject\'s own trailing baseline only. No cohort percentile, no ranking, no peer comparison (brief §1.2).',
    ]),
  });
}

/**
 * §3.6: the two ways the flag lies, both checked before it is shown.
 *
 * @param {object} input
 * @param {SpikeReport} input.report
 * @param {object|null} input.coverage          the envelope's coverage block, if the caller has one
 * @param {{rows_in_window:number, late_flush_rows:number}|null} input.flush
 * @returns {{result_state:string, coverage_degraded:boolean, late_flush:boolean, notes:ReadonlyArray<string>}}
 */
export function annotateSpikes(input) {
  const notes = [];
  let coverageDegraded = false;
  if (input.coverage && (input.coverage.state === 'partial' || input.coverage.state === 'not_yet_covered')) {
    coverageDegraded = true;
    notes.push(
      'A device was not reporting inside this window, so a dip here is a coverage gap as much as a behaviour change (docs/04 §3.6).',
    );
  }
  const lateFlushRows = Number(input.flush?.late_flush_rows ?? 0);
  const lateFlush = lateFlushRows > 0;
  if (lateFlush) {
    notes.push(
      `${lateFlushRows} row(s) in this window were received more than an hour after they occurred: a spool flush spikes received time, not behaviour (brief §3.6, two clocks).`,
    );
  }
  const state = input.report.spikes.length > 0
    ? (coverageDegraded ? 'coverage_degraded' : 'ok')
    : input.report.state;
  return Object.freeze({
    result_state: state,
    coverage_degraded: coverageDegraded,
    late_flush: lateFlush,
    late_flush_rows: lateFlushRows,
    notes: Object.freeze(notes),
  });
}

export function medianOf(values) {
  if (values.length === 0) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 0 ? (sorted[mid - 1] + sorted[mid]) / 2 : sorted[mid];
}

export { MIN_OBSERVED_BUCKETS, TRAILING_BUCKETS, MAD_MULTIPLIER, MEDIAN_MULTIPLIER };
