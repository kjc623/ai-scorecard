// states.js — what an envelope means, decided once.
//
// This is the consumer half of the contract the read path pins. Two facts must never be merged,
// and this module is where that is enforced rather than hoped for:
//
//   a suppressed cell  — {result_state:"suppressed", reason:"fewer_than_k_subjects", k, …}, with
//                        NO measure key at all. "There was something and we are not telling you
//                        the number."
//   a genuine zero     — {submissions: 0}. "We looked and there was none."
//
// `measureOf()` is the only way a screen may obtain a value, and it returns a discriminated
// result, never a number that might be a lie. `render.js` has no other source of numbers.
//
// The other rules encoded here, all from docs/04 §13 and §14:
//   * a data-bearing envelope must carry freshness and coverage — if it does not, this module
//     refuses to treat it as an answer (`assertStateful`), because a number without its state is
//     the failure the envelope exists to make impossible;
//   * `empty` is an answer and says so; `not_yet_covered` and `coverage_degraded` say the system
//     cannot say, and are rendered as hatched states, not as zeroes;
//   * `coverage_degraded` always names the gap and always carries the enrolled denominator.

import { K, RESULT_STATES, STATE_PAIRS } from './vocab.js';

/** Keys that are part of a row's envelope rather than its data. */
const SYSTEM_ROW_KEYS = Object.freeze([
  'result_state', 'reason', 'k', 'suppressed_measures', 'bucket', 'snapshot_day',
]);

/** The vocabulary columns a row may carry, so a renderer can show states side by side. */
export const VOCAB_COLUMNS = Object.freeze(Object.keys(STATE_PAIRS));

/**
 * A value, or the fact that there is not one. Never a number standing in for a suppression.
 *
 * @param {object} row
 * @param {string} measure
 * @returns {{kind:'number', value:number}|{kind:'suppressed', k:number, reason:string}|{kind:'absent'}}
 */
export function measureOf(row, measure) {
  if (!row || typeof row !== 'object') return { kind: 'absent' };
  const suppressed = row.result_state === 'suppressed';
  const present = Object.prototype.hasOwnProperty.call(row, measure) && row[measure] !== null && row[measure] !== undefined;
  if (suppressed) {
    // Even if a measure key were present on a suppressed row — a bug at the producer — the
    // suppression wins. Publishing it would be the exact merge §6.3 forbids.
    return { kind: 'suppressed', k: typeof row.k === 'number' ? row.k : K, reason: row.reason ?? 'fewer_than_k_subjects' };
  }
  if (!present) return { kind: 'absent' };
  const value = row[measure];
  if (typeof value === 'number') return { kind: 'number', value };
  const parsed = Number(value);
  return Number.isFinite(parsed) ? { kind: 'number', value: parsed } : { kind: 'absent' };
}

/** True when the row is a suppression marker rather than a measurement. */
export function isSuppressed(row) {
  return row?.result_state === 'suppressed';
}

/** True when the row carries any real measure: a zero is real, an absent measure is not. */
export function hasAnyMeasure(row) {
  if (!row || typeof row !== 'object' || isSuppressed(row)) return false;
  return Object.entries(row).some(([key, value]) => !SYSTEM_ROW_KEYS.includes(key) && typeof value === 'number');
}

/** The closed-vocabulary values on a row, grouped, so the renderer can keep pairs distinct. */
export function vocabOf(row) {
  const out = {};
  for (const column of VOCAB_COLUMNS) {
    if (row && typeof row === 'object' && column in row) out[column] = row[column];
  }
  return out;
}

/**
 * Refuse to treat a body as an answer when it carries data without its state.
 *
 * The API's envelope builder cannot produce one; this client refuses to render one, so a proxy, a
 * cache or a stub cannot introduce the failure the envelope was designed to prevent.
 */
export function assertStateful(envelope) {
  const carriesData = Array.isArray(envelope?.data) || envelope?.data !== undefined;
  if (!carriesData) return;
  const missing = [];
  if (!envelope.freshness || typeof envelope.freshness !== 'object') missing.push('freshness');
  if (!envelope.coverage || typeof envelope.coverage !== 'object') missing.push('coverage');
  if (missing.length > 0) {
    const error = new Error(`A data-bearing response is missing ${missing.join(' and ')}: refusing to render a number without its state.`);
    error.reason = 'missing_state_block';
    error.resultState = 'audit_chain_broken';
    error.missing = missing;
    throw error;
  }
}

/** A human sentence for a freshness block, always stating the age. */
export function freshnessText(freshness) {
  if (!freshness) return { text: 'Freshness unknown', state: 'unknown' };
  const state = freshness.state ?? 'fresh';
  if (state === 'not_yet_covered') {
    return { text: `No aggregate has run for this window (${freshness.reason ?? 'no watermark row'})`, state };
  }
  const age = freshness.lag_seconds === null || freshness.lag_seconds === undefined
    ? 'unknown age'
    : `${freshness.lag_seconds} s ago`;
  const complete = freshness.last_complete_bucket ? ` · complete to ${String(freshness.last_complete_bucket).slice(0, 16).replace('T', ' ')}Z` : '';
  if (state === 'stale') return { text: `Updated ${age}${complete} · stale`, state };
  return { text: `Updated ${age}${complete}`, state };
}

/**
 * A human sentence for a coverage block. The enrolled denominator is always part of the sentence:
 * no fleet figure is shown without it (§11.3, brief §5.5).
 */
export function coverageText(coverage) {
  if (!coverage) return { text: 'Coverage unknown', state: 'unknown', gaps: [] };
  const state = coverage.state ?? 'complete';
  if (state === 'not_yet_covered') {
    return { text: `Coverage not yet measured (${coverage.reason ?? 'no snapshot'})`, state, gaps: [] };
  }
  const reporting = coverage.devices_reporting;
  const enrolled = coverage.devices_enrolled;
  const gaps = Object.entries(coverage.gap_reasons ?? {}).map(([reason, count]) => ({ reason, count }));
  if (reporting === null || reporting === undefined || enrolled === null || enrolled === undefined) {
    return { text: 'Coverage denominator unavailable', state, gaps };
  }
  const base = `${reporting} of ${enrolled} enrolled devices reporting`;
  return {
    text: state === 'partial' ? `${base} · gaps in ${gaps.length} categor${gaps.length === 1 ? 'y' : 'ies'}` : base,
    state,
    gaps,
  };
}

/**
 * The banners a result state demands. A refusal is not an empty answer: it is a state with a
 * sentence and, where the API supplied one, the fix.
 */
export function bannersFor(envelope) {
  const state = envelope?.result_state;
  const banners = [];
  if (!state) return banners;

  if (envelope.freshness?.state === 'stale') {
    banners.push({
      level: 'warning',
      title: 'Stale aggregate',
      text: `These numbers are old: ${freshnessText(envelope.freshness).text}`,
    });
  }
  if (envelope.coverage?.state === 'partial') {
    banners.push({
      level: 'warning',
      title: 'Coverage is partial',
      text: `${coverageText(envelope.coverage).text}. Every figure here is a floor, not a total.`,
    });
  }
  if (envelope.coverage?.state === 'not_yet_covered') {
    banners.push({
      level: 'hatched',
      title: 'Not yet covered',
      text: coverageText(envelope.coverage).text,
    });
  }
  if (state === 'not_yet_covered') {
    banners.push({
      level: 'hatched',
      title: 'Not yet covered',
      text: envelope.freshness?.reason === 'directory_not_synced'
        ? 'The directory has not been synchronised, so no team is known for any person. This is not a quiet team.'
        : `The system cannot answer for this window (${envelope.freshness?.reason ?? envelope.coverage?.reason ?? 'no source'}).`,
    });
  }
  if (state === 'empty') {
    banners.push({ level: 'info', title: 'No data', text: 'We looked, coverage was adequate, and there is nothing in this window.' });
  }
  if (state === 'suppressed') {
    banners.push({
      level: 'hatched',
      title: 'Every cell suppressed',
      text: `Every cell in this response had fewer than k = ${envelope.suppression?.k ?? K} distinct subjects. There was something here; the number is withheld.`,
    });
  }
  if (state === 'stale_aggregate') banners.push({ level: 'warning', title: 'Stale aggregate', text: freshnessText(envelope.freshness).text });
  if (state === 'coverage_degraded') {
    banners.push({
      level: 'warning',
      title: 'Coverage degraded: this is a floor',
      text: `${coverageText(envelope.coverage).text}. Parts of the fleet are not reporting, so the value shown understates reality.`,
    });
  }
  if (state === 'audit_unavailable') {
    banners.push({
      level: 'refusal',
      title: 'Read not served',
      text: 'The audit entry could not be committed, so no rows were returned. A read that cannot be proved to have happened does not happen.',
    });
  }
  if (state === 'cursor_expired') {
    banners.push({
      level: 'restart',
      title: 'Restart from page one',
      text: `${envelope.error?.message ?? 'The cursor is no longer valid.'} Do not resume from an offset.`,
    });
  }
  if (state === 'busy') {
    banners.push({
      level: 'retry',
      title: 'Busy',
      text: `${envelope.error?.message ?? 'The read was shed.'} ${envelope.error?.detail?.retry_after_seconds ? `Retry in about ${envelope.error.detail.retry_after_seconds}s.` : ''}`.trim(),
    });
  }
  if (state === 'audit_chain_broken') {
    banners.push({
      level: 'refusal',
      title: 'Audit integrity alert',
      text: 'The hash links in this page do not verify. This is not a list to read; it is an incident.',
    });
  }
  if (state === 'query_too_broad' || state === 'unsupported_query_shape') {
    banners.push({
      level: 'refusal',
      title: state === 'query_too_broad' ? 'Too broad to serve' : 'Shape not servable',
      text: fixSentence(envelope) ?? (envelope.error?.message ?? 'The API refused this read.'),
    });
  }
  if (state === 'unauthorised_role' || state === 'key_unavailable' || state === 'content_search_not_enabled') {
    banners.push({ level: 'refusal', title: 'Not available to this session', text: envelope.error?.message ?? state });
  }
  if (state === 'no_longer_available') {
    banners.push({
      level: 'gone',
      title: 'No longer available',
      text: `${envelope.error?.message ?? ''} ${envelope.error?.receipt ? `Receipt ${envelope.error.receipt.receipt_id} (${envelope.error.receipt.scope_kind}) completed ${String(envelope.error.receipt.completed_at).slice(0, 10)}.` : ''}`.trim(),
    });
  }
  if (state === 'not_found') {
    const detail = envelope.error?.detail ?? {};
    banners.push({
      level: 'info',
      title: 'No such record',
      text: detail.retention_evidence === 'no_ledger_in_schema'
        ? 'No purge record covers this window, and there is no retention ledger to prove otherwise. "Never existed" and "was purged" cannot be told apart from the id alone.'
        : detail.purge_window_unknown
          ? 'The record is not here. Without the window it would have been received in, "never existed" and "existed and was purged" cannot be told apart.'
          : 'The record is not here, and no purge covers its window.',
    });
  }
  return banners;
}

/** The fix the API named, as a sentence a person can act on. */
export function fixSentence(envelope) {
  const fix = envelope?.error?.detail?.fix;
  const detail = envelope?.error?.detail ?? {};
  if (!fix && !detail.max_cells) {
    const bits = [];
    if (detail.max_days) bits.push(`window ≤ ${detail.max_days} days`);
    if (detail.max_values) bits.push(`≤ ${detail.max_values} values in a list`);
    if (detail.max_limit) bits.push(`limit ≤ ${detail.max_limit}`);
    return bits.length > 0 ? `The bound is ${bits.join(', ')}.` : null;
  }
  const parts = [];
  if (fix?.coarser_bucket) parts.push(`coarsen the bucket to ${fix.coarser_bucket}`);
  if (fix?.drop_dimension) parts.push(`drop ${fix.drop_dimension}`);
  if (fix?.narrow_window || fix?.or_narrow_window) parts.push('narrow the window');
  if (fix?.lower_limit) parts.push('lower the page size');
  if (fix?.add_filter) parts.push(`add a filter on ${Array.isArray(fix.add_filter) ? fix.add_filter.join(' or ') : fix.add_filter}`);
  if (fix?.alternative_source) parts.push(`read ${fix.alternative_source} instead`);
  if (fix?.or_use) parts.push(`or use ${fix.or_use}`);
  if (detail.max_cells && detail.estimated_cells) parts.push(`this shape is estimated at ${detail.estimated_cells} cells against a ${detail.max_cells} cap`);
  return parts.length > 0 ? `Fix: ${parts.join('; ')}.` : null;
}

/**
 * Normalise an envelope (or a locally-produced refusal) into the shape a screen renders.
 *
 * @param {object} envelope
 * @returns {object} view state
 */
export function readState(envelope) {
  assertStateful(envelope);
  const state = envelope.result_state;
  const spec = RESULT_STATES[state] ?? { http: 0, carriesData: false, treatment: 'refusal', label: 'Unknown state' };
  const data = Array.isArray(envelope.data) ? envelope.data : [];
  return Object.freeze({
    resultState: state,
    spec,
    isRefusal: !spec.carriesData,
    data,
    rowCount: data.length,
    page: envelope.page ?? null,
    freshness: envelope.freshness ?? null,
    coverage: envelope.coverage ?? null,
    suppression: envelope.suppression ?? null,
    audit: envelope.audit ?? null,
    meta: envelope.meta ?? {},
    error: envelope.error ?? null,
    banners: Object.freeze(bannersFor(envelope)),
    suppressedCells: envelope.suppression?.suppressed_cells ?? data.filter(isSuppressed).length,
    allSuppressed: data.length > 0 && data.every(isSuppressed),
    freshnessText: freshnessText(envelope.freshness),
    coverageText: coverageText(envelope.coverage),
  });
}

/** The floor of an empty result: an answer, distinguishable from a state that cannot say. */
export function emptyStateFor(state) {
  if (state.resultState === 'empty') {
    return { kind: 'empty', title: 'No data', text: 'We looked, coverage was adequate, and there is nothing in this window.' };
  }
  if (state.resultState === 'not_yet_covered') {
    return { kind: 'not_yet_covered', title: 'Not yet covered', text: 'The system cannot answer for this window. This is not the same as zero.' };
  }
  if (state.resultState === 'coverage_degraded') {
    return { kind: 'coverage_degraded', title: 'Nothing found, on partial coverage', text: 'No rows came back, and parts of the fleet were not reporting: the absence is not a total.' };
  }
  if (state.resultState === 'suppressed') {
    return { kind: 'suppressed', title: 'All cells suppressed', text: `Every cell had fewer than k = ${state.suppression?.k ?? K} distinct subjects.` };
  }
  return null;
}
