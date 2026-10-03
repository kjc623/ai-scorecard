// spike.test.mjs — Q6's "changed, or spiked" rule (§3.6).
//
// The rule is fixed by the document: median(trailing 28 daily buckets) + 4 x MAD AND >= 3x the
// median, requiring >= 14 observed buckets first. Below 14 buckets the answer is not_yet_covered,
// not "no change".

import test from 'node:test';
import assert from 'node:assert/strict';
import { annotateSpikes, detectSpikes, medianOf } from '../src/spike.js';

/** A daily series of `days` buckets, flat at `base` except the given overrides. */
function series(days, base, overrides = {}) {
  return Array.from({ length: days }, (_, i) => ({
    bucket_start: new Date(Date.UTC(2026, 0, 1 + i)).toISOString(),
    submissions: overrides[i] ?? base,
  }));
}

test('median and MAD are computed on the values, not the mean', () => {
  assert.equal(medianOf([1, 2, 3, 4, 100]), 3, 'the flush day does not move the median');
  assert.equal(medianOf([]), 0);
});

test('below 14 observed buckets the answer is not_yet_covered, never "no change"', () => {
  const report = detectSpikes(series(13, 10));
  assert.equal(report.state, 'not_yet_covered');
  assert.equal(report.observed_buckets, 13);
  assert.equal(report.required_buckets, 14);
  assert.equal(report.reason, 'fewer_than_14_observed_buckets');
  assert.ok(report.notes[0].includes('not "no change"'));
});

test('a flat series with 14 buckets has no spikes and is answerable', () => {
  const report = detectSpikes(series(20, 10));
  assert.equal(report.state, 'ok');
  assert.deepEqual(report.spikes, []);
});

test('a day above the MAD threshold and at least 3x the median is flagged', () => {
  const report = detectSpikes(series(30, 10, { 25: 60 }));
  assert.equal(report.spikes.length, 1);
  const spike = report.spikes[0];
  assert.equal(spike.value, 60);
  assert.equal(spike.median, 10);
  assert.equal(spike.ratio, 6);
  assert.ok(spike.value > spike.threshold);
});

test('a day above the MAD threshold but below 3x the median is not flagged', () => {
  // Median 10, MAD 0, so the MAD threshold is 10: 25 > 10 but 25 < 30.
  const report = detectSpikes(series(30, 10, { 25: 25 }));
  assert.deepEqual(report.spikes, []);
});

test('a bump that is a natural spread, not a spike, is not flagged', () => {
  const wobble = {};
  for (let i = 0; i < 30; i += 1) wobble[i] = 10 + (i % 5);
  const report = detectSpikes(series(30, 10, wobble));
  assert.deepEqual(report.spikes, []);
});

test('the baseline is the subject\'s own trailing window, never a peer group', () => {
  const report = detectSpikes(series(40, 10, { 35: 90 }), { trailingBuckets: 28 });
  assert.equal(report.spikes.length, 1);
  assert.ok(report.notes.some((n) => n.includes('No cohort percentile')));
});

test('a coverage gap in the window degrades the answer rather than presenting it as behaviour', () => {
  const report = detectSpikes(series(30, 10, { 25: 60 }));
  const annotated = annotateSpikes({
    report,
    coverage: { state: 'partial', gap_reasons: { not_enrolled: 4 } },
    flush: { rows_in_window: 900, late_flush_rows: 0 },
  });
  assert.equal(annotated.result_state, 'coverage_degraded');
  assert.equal(annotated.coverage_degraded, true);
  assert.ok(annotated.notes.some((n) => n.includes('dip here is a coverage gap')));
});

test('a late flush annotates the flag as a flush rather than as a behaviour change', () => {
  const report = detectSpikes(series(30, 10, { 25: 60 }));
  const annotated = annotateSpikes({
    report,
    coverage: { state: 'complete' },
    flush: { rows_in_window: 900, late_flush_rows: 120 },
  });
  assert.equal(annotated.late_flush, true);
  assert.equal(annotated.late_flush_rows, 120);
  assert.equal(annotated.result_state, 'ok', 'the data is still served; it is annotated');
  assert.ok(annotated.notes.some((n) => n.includes('spool flush')));
});

test('a not_yet_covered report stays not_yet_covered through annotation', () => {
  const report = detectSpikes(series(5, 10));
  const annotated = annotateSpikes({ report, coverage: { state: 'complete' }, flush: null });
  assert.equal(annotated.result_state, 'not_yet_covered');
});
