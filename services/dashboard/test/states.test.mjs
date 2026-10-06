// states.test.mjs — the consumer side of the contract reader pinned.
//
// The single most important behaviour in this package: a suppressed small cell and a genuine zero
// must never be collapsed into one another. `reader` pinned the producer side (a suppressed cell
// carries no measure key); this file pins the consumer side, which is where the merge would happen
// if a renderer ever wrote `value ?? 0`.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  measureOf, isSuppressed, assertStateful, readState, bannersFor,
  coverageText, freshnessText, emptyStateFor, vocabOf,
} from '../src/states.js';
import { sumMeasure } from '../src/views.js';
import { COMPLETE, FRESH, PARTIAL, envelope } from './helpers.mjs';
import { STATE_ENVELOPES } from './fixtures.mjs';

const SUPPRESSED = Object.freeze({ bucket: '2026-09-30T00:00:00Z', tool: 'shadow_llm_gateway', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 });
const ZERO = Object.freeze({ bucket: '2026-09-30T00:00:00Z', tool: 'legacy_summariser', submissions: 0, users: 0 });
const VALUE = Object.freeze({ bucket: '2026-09-30T00:00:00Z', tool: 'claude_web', submissions: 812, users: 214 });
const ABSENT = Object.freeze({ bucket: '2026-09-30T00:00:00Z', tool: 'copilot_chat' });

test('a suppressed cell yields suppressed, never a number', () => {
  const value = measureOf(SUPPRESSED, 'submissions');
  assert.equal(value.kind, 'suppressed');
  assert.equal(value.k, 5);
  assert.equal(value.reason, 'fewer_than_k_subjects');
  assert.equal(value.value, undefined);
});

test('a genuine zero yields the number 0', () => {
  const value = measureOf(ZERO, 'submissions');
  assert.equal(value.kind, 'number');
  assert.equal(value.value, 0);
});

test('suppression wins even if a measure key is present on the row', () => {
  // A producer bug must not become a leak: the marker is authoritative, not the stray key.
  const contradictory = { result_state: 'suppressed', k: 5, submissions: 900 };
  assert.equal(measureOf(contradictory, 'submissions').kind, 'suppressed');
});

test('an absent measure is absent, not zero', () => {
  assert.equal(measureOf(ABSENT, 'submissions').kind, 'absent');
  assert.equal(measureOf(null, 'submissions').kind, 'absent');
  assert.equal(measureOf({ submissions: null }, 'submissions').kind, 'absent');
});

test('the suppressed marker and the zero are distinguishable by every predicate', () => {
  assert.equal(isSuppressed(SUPPRESSED), true);
  assert.equal(isSuppressed(ZERO), false);
  assert.notEqual(measureOf(SUPPRESSED, 'submissions').kind, measureOf(ZERO, 'submissions').kind);
});

test('a total over a set containing a suppressed cell is a floor, not a total', () => {
  const state = readState(envelope('ok', { data: [VALUE, SUPPRESSED], freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 } }));
  const total = sumMeasure(state, 'submissions');
  assert.equal(total.kind, 'floor');
  assert.equal(total.value, 812);
  assert.equal(total.suppressedCells, 1);
});

test('a total where every cell is suppressed is suppressed, not zero', () => {
  const state = readState(envelope('ok', { data: [SUPPRESSED, { ...SUPPRESSED, tool: 'b' }], freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 2 } }));
  assert.equal(sumMeasure(state, 'submissions').kind, 'suppressed');
});

test('a total of genuine zeroes is the number zero', () => {
  const state = readState(envelope('ok', { data: [ZERO, { ...ZERO, tool: 'b' }], freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 0 } }));
  const total = sumMeasure(state, 'submissions');
  assert.equal(total.kind, 'number');
  assert.equal(total.value, 0);
});

test('a data-bearing response without freshness or coverage is refused, not rendered', () => {
  assert.throws(() => readState(envelope('ok', { data: [VALUE], coverage: COMPLETE })), /freshness/);
  assert.throws(() => readState(envelope('ok', { data: [VALUE], freshness: FRESH })), /coverage/);
  assert.throws(() => assertStateful({ data: [] }), /refusing to render a number without its state/);
  // A refusal carries no data, so it needs neither block.
  assert.doesNotThrow(() => readState(STATE_ENVELOPES.refused_too_broad));
});

test('the coverage sentence always carries the enrolled denominator', () => {
  const partial = coverageText(PARTIAL);
  assert.match(partial.text, /4,180 of 4,620 enrolled devices reporting/);
  assert.equal(partial.gaps.length, 2);
  const complete = coverageText(COMPLETE);
  assert.match(complete.text, /4,620 of 4,620 enrolled devices reporting/);
  const blind = coverageText({ state: 'not_yet_covered', reason: 'no_coverage_snapshot' });
  assert.match(blind.text, /not yet measured/);
  assert.equal(coverageText(null).text, 'Coverage unknown');
});

test('freshness always states an age, and a stale one says so', () => {
  assert.match(freshnessText(FRESH).text, /Updated 180 s ago/);
  assert.equal(freshnessText({ ...FRESH, lag_seconds: 5400, state: 'stale' }).state, 'stale');
  assert.match(freshnessText({ state: 'not_yet_covered', reason: 'directory_not_synced' }).text, /directory_not_synced/);
});

test('every result state has a treatment, and the ones that say "we cannot say" do not read as zero', () => {
  const notCovered = readState(envelope('not_yet_covered', { data: [], freshness: { ...FRESH, state: 'not_yet_covered', reason: 'directory_not_synced' }, coverage: COMPLETE }));
  assert.equal(notCovered.resultState, 'not_yet_covered');
  const empty = readState(envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE }));
  assert.notEqual(empty.resultState, notCovered.resultState);
  assert.equal(emptyStateFor(empty).kind, 'empty');
  assert.equal(emptyStateFor(notCovered).kind, 'not_yet_covered');
  const degraded = readState(envelope('coverage_degraded', { data: [VALUE], freshness: FRESH, coverage: PARTIAL }));
  assert.equal(emptyStateFor(degraded).kind, 'coverage_degraded');
});

test('banners name the reason rather than leaving a bare state', () => {
  const stale = bannersFor(envelope('stale_aggregate', { data: [], freshness: { ...FRESH, state: 'stale' }, coverage: COMPLETE }));
  assert.ok(stale.some((b) => b.title === 'Stale aggregate'));
  const partial = bannersFor(envelope('coverage_degraded', { data: [], freshness: FRESH, coverage: PARTIAL }));
  assert.ok(partial.some((b) => /floor/.test(b.text)));
  const refusal = bannersFor(STATE_ENVELOPES.refused_too_broad);
  assert.ok(refusal.some((b) => /coarsen the bucket to week/.test(b.text)), 'the fix is rendered, not just the failure');
  const gone = bannersFor(STATE_ENVELOPES.no_longer_available);
  assert.ok(gone.some((b) => /Receipt r_88/.test(b.text)));
  const missing = bannersFor(STATE_ENVELOPES.not_found);
  assert.ok(missing.some((b) => /cannot be told apart/.test(b.text)), 'not_found states its own ambiguity');
  const unavailable = bannersFor(STATE_ENVELOPES.audit_unavailable);
  assert.ok(unavailable.some((b) => /does not happen/.test(b.text)));
});

test('state pairs are kept apart on the row itself', () => {
  const row = { liveness: 'never_reported', collector_state: 'absent', content_state: 'shredded', policy_action: 'blocked', review_state: 'open', sanctioned_state: 'unknown', merge_confidence: 'low' };
  const vocab = vocabOf(row);
  assert.equal(Object.keys(vocab).length, 7);
  assert.equal(vocab.liveness, 'never_reported');
  assert.equal(vocab.review_state, 'open');
});

test('every fixture envelope is renderable, and each carries a documented state', () => {
  for (const [name, env] of Object.entries(STATE_ENVELOPES)) {
    const state = readState(env);
    assert.ok(state.resultState, `${name} has a state`);
    assert.ok(Array.isArray(state.banners), `${name} has banners`);
  }
});
