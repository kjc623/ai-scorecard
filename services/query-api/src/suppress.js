// suppress.js — §6 k-suppression, applied on the read path and nowhere else.
//
// The rule (docs/04 §6.2): k = 5 distinct subjects per cell; k applies to distinct subjects,
// not to rows; a suppressed cell suppresses every measure in it; and a suppressed cell is a
// third kind of value — neither a zero nor a null.
//
// The one thing this module must never do is merge the two facts §6.3 keeps apart:
//
//   0          means "we looked and there was none"
//   suppressed means "there was something and we are not telling you the number"
//
// So a cell whose subject count is below k but whose measures are all genuinely zero is
// published as a zero. A zero cell is not a secret; it is an answer.

import { K as DEFAULT_K } from './registry.js';

/** The marker object a suppressed cell carries instead of its measures. */
export function suppressedCell(k, extra = {}) {
  return Object.freeze({
    result_state: 'suppressed',
    reason: 'fewer_than_k_subjects',
    k,
    ...extra,
  });
}

/**
 * @typedef {object} SuppressionResult
 * @property {ReadonlyArray<object>} rows          published rows, suppressed ones marked
 * @property {number} suppressedCells
 * @property {boolean} totalSuppressed
 * @property {ReadonlyArray<object>} suppressed    one entry per suppressed cell, for the envelope
 * @property {ReadonlyArray<string>} notes
 */

/**
 * Apply k-suppression to a page of aggregate cells.
 *
 * @param {ReadonlyArray<object>} rows     result rows, each carrying the hidden `__k_subjects`
 * @param {object} options
 * @param {ReadonlyArray<string>} options.measures    measure names to blank out when suppressed
 * @param {number} [options.k]
 * @param {boolean} [options.subjectScoped]  §6.4: explicitly subject-scoped reads are not suppressed
 * @param {string} [options.totalRowMarker]  dimension key that is NULL on the rollup total row
 * @param {ReadonlyArray<string>} [options.groupKeys] grouping keys, used to find the total row
 * @returns {SuppressionResult}
 */
export function applySuppression(rows, options) {
  const k = options.k ?? DEFAULT_K;
  const measures = options.measures ?? [];
  const groupKeys = options.groupKeys ?? [];
  const notes = [];

  // §6.4: "Does not apply to explicitly subject-scoped reads — a list filtered by user_ref, or
  // a single detail. Those answer questions about a named person, which is the product's
  // purpose (Q6, Q8, Q9); the controls there are authorisation and audit, not anonymity."
  if (options.subjectScoped) {
    return Object.freeze({
      rows: Object.freeze(rows.map((row) => stripInternal(row))),
      suppressedCells: 0,
      totalSuppressed: false,
      suppressed: Object.freeze([]),
      notes: Object.freeze(['k-suppression does not apply to an explicitly subject-scoped read (docs/04 §6.4).']),
    });
  }

  const suppressed = [];
  let totalIndex = -1;

  const marked = rows.map((row, index) => {
    if (isTotalRow(row, groupKeys)) totalIndex = index;
    const subjects = numberOrNull(row.__k_subjects);
    const allZero = measures.length > 0 && measures.every((name) => isGenuineZero(row[name]));
    if (subjects === null || subjects >= k || allZero) {
      // Either the cell is wide enough, or it is a genuine zero — which is an answer, not a
      // secret, and must not be hidden behind the suppression state.
      return stripInternal(row);
    }
    const kept = {};
    for (const [key, value] of Object.entries(row)) {
      if (key.startsWith('__')) continue;
      if (measures.includes(key)) continue;
      kept[key] = value;
    }
    const marker = suppressedCell(k, kept);
    suppressed.push(Object.freeze({ cell: Object.freeze({ ...kept }), reason: 'fewer_than_k_subjects', k }));
    return marker;
  });

  // -------------------------------------------------------------------------------------------
  // §6.2 complementary suppression: "If a response contains exactly one suppressed cell and a
  // published total over the same dimension, the total is suppressed too; otherwise the total
  // minus the published cells recovers the hidden value."
  // -------------------------------------------------------------------------------------------
  let totalSuppressed = false;
  if (totalIndex >= 0 && suppressed.length === 1) {
    const row = marked[totalIndex];
    const kept = {};
    for (const [key, value] of Object.entries(row)) {
      if (key.startsWith('__')) continue;
      if (measures.includes(key)) continue;
      kept[key] = value;
    }
    marked[totalIndex] = suppressedCell(k, { ...kept, reason: 'complementary_suppression' });
    totalSuppressed = true;
    suppressed.push(Object.freeze({ cell: Object.freeze({ ...kept }), reason: 'complementary_suppression', k }));
    notes.push('The published total was suppressed as well: with exactly one suppressed cell it would otherwise have recovered the hidden value (docs/04 §6.2).');
  }

  return Object.freeze({
    rows: Object.freeze(marked),
    suppressedCells: suppressed.length,
    totalSuppressed,
    suppressed: Object.freeze(suppressed),
    notes: Object.freeze(notes),
  });
}

/** The rollup row: every grouping key is NULL (GROUPING SETS (…) , ()). */
export function isTotalRow(row, groupKeys) {
  if (groupKeys.length === 0) return false;
  return groupKeys.every((key) => row[key] === null || row[key] === undefined);
}

function stripInternal(row) {
  const out = {};
  for (const [key, value] of Object.entries(row)) {
    if (key.startsWith('__')) continue;
    out[key] = value;
  }
  return Object.freeze(out);
}

function numberOrNull(value) {
  if (value === null || value === undefined) return null;
  const n = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(n) ? n : null;
}

/**
 * A genuine zero: the measure is present and numerically zero. NULL is not a zero — a NULL
 * measure means the source had nothing to say (`max_score` on an empty cell), which is a
 * different fact again and is not treated as zero here.
 */
function isGenuineZero(value) {
  if (value === null || value === undefined) return true;
  const n = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(n) && n === 0;
}

/**
 * The audit trigger of §5.2 is decided *before* suppression: "a query answered with `suppressed`
 * still writes an audit entry, because the attempt to resolve a small group is the fact worth
 * recording". This is the predicate the executor applies to the raw rows, ahead of marking.
 */
export function anyCellBelowK(rows, k = DEFAULT_K) {
  return rows.some((row) => {
    const subjects = numberOrNull(row.__k_subjects);
    if (subjects === null || subjects >= k) return false;
    const measures = Object.keys(row).filter((key) => !key.startsWith('__') && typeof row[key] === 'number');
    return !measures.every((name) => isGenuineZero(row[name]));
  });
}

export { DEFAULT_K as K };
