/**
 * test/queue-health.test.mjs — §3.4's bounded queue and §4.3's closed counter set.
 *
 * The two claims under test:
 *   - the bound is enforced on write with **drop-oldest**, and every drop increments `dropped` by
 *     exactly the number evicted (C22: "a drop is counted, never silent");
 *   - a dead channel is reported as capture-core `absent` **plus** extension-side `degraded` —
 *     never as "no observations" (§3.4). That distinction is the whole of check 2 below.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createQueue } from '../src/queue.js';
import { COUNTER, createCounterSet, createCounters, createHealthReporter } from '../src/health.js';
import { COUNTERS } from '../src/messages.js';

test('the queue drops the OLDEST entry when full, and counts exactly what it dropped', () => {
  const queue = createQueue({ capacity: 3 });
  const seqs = [];
  for (let i = 1; i <= 5; i++) seqs.push(queue.enqueue('observation', { i }, 10));

  assert.equal(queue.size(), 3, 'the bound is enforced on write');
  const kept = queue.peek(10).map((e) => e.payload.i);
  assert.deepEqual(kept, [3, 4, 5], 'the newest survive and the oldest are evicted');
  assert.deepEqual(seqs, [0, 1, 2, 3, 4]);

  const stats = queue.stats();
  assert.equal(stats.dropped_total, 2, 'two entries were evicted, so two are counted');
  assert.equal(stats.dropped_bytes_total, 20);
  assert.deepEqual(stats.dropped_by_kind, { observation: 2 });
  assert.equal(stats.depth, 3);
  assert.equal(stats.capacity, 3);
});

test('a drop is reported to the counter set, so §4.3\'s `dropped` moves with the queue', () => {
  const counters = createCounterSet();
  const queue = createQueue({ capacity: 2, onDrop: () => counters.inc(COUNTER.DROPPED, 1) });
  for (let i = 0; i < 5; i++) queue.enqueue('observation', { i }, 1);
  assert.equal(counters.snapshot().counters.dropped, 3);
  assert.equal(queue.stats().dropped_total, 3, 'the queue and the counter agree exactly');
});

test('the bound changes nothing about the entries that survive', () => {
  const queue = createQueue({ capacity: 2 });
  queue.enqueue('observation', { id: 'a' }, 1);
  queue.enqueue('observation', { id: 'b' }, 1);
  queue.enqueue('observation', { id: 'c' }, 1);
  assert.deepEqual(queue.peek(2).map((e) => e.payload.id), ['b', 'c']);
});

test('ack removes exactly one entry and a peek removes nothing', () => {
  const queue = createQueue({ capacity: 10 });
  const a = queue.enqueue('observation', { id: 'a' }, 1);
  queue.enqueue('observation', { id: 'b' }, 1);
  assert.equal(queue.size(), 2);
  queue.peek(1);
  assert.equal(queue.size(), 2, 'peek must not consume: a failed send has to retry');
  assert.equal(queue.ack(a, 1), 1);
  assert.equal(queue.size(), 1);
  assert.equal(queue.stats().delivered_total, 1);
  assert.equal(queue.ack(999, 1), 0, 'acking something already gone is not an error');
});

test('a capacity below one is refused rather than silently accepted', () => {
  assert.throws(() => createQueue({ capacity: 0 }), RangeError);
  assert.throws(() => createQueue({ capacity: -1 }), RangeError);
});

test('counters are the closed seven, at zero, and an unknown name is refused rather than sent', () => {
  const c = createCounters();
  assert.deepEqual(Object.keys(c).sort(), [...COUNTERS].sort());
  assert.equal(COUNTERS.length, 7, '§4.3/A15 closes the set at seven');
  for (const v of Object.values(c)) assert.equal(v, 0);

  const set = createCounterSet();
  assert.equal(set.inc('observed'), true);
  assert.equal(set.inc('a_counter_this_provider_invented'), false, 'a name outside the set is not recorded');
  assert.equal(set.snapshot().counters.observed, 1);
});

test('counters are cumulative since start plus a windowed delta (§4.3)', () => {
  const set = createCounterSet();
  set.inc('observed', 3);
  set.inc('emitted', 2);
  assert.equal(set.snapshot().counters.observed, 3);
  assert.equal(set.snapshot().window.observed, 3);
  set.inc('observed', 1);
  const previous = set.rollWindow();
  assert.equal(previous.observed, 4, 'the rolled window is everything since the last roll, including the last increment');
  assert.equal(set.snapshot().window.observed, 0, 'and the new window starts clean');
  assert.equal(set.snapshot().counters.observed, 4, 'cumulatively it only grows');
});

test('a failed connect is reported as capture-core absent AND extension-side degraded, never as "no observations"', () => {
  const counters = createCounterSet();
  const queue = createQueue({ capacity: 2, onDrop: () => counters.inc(COUNTER.DROPPED, 1) });
  const health = createHealthReporter({
    device_id: 'dev-1',
    queueStats: () => queue.stats(),
    policySnapshot: () => ({ policy_version: 'v3', present: true, stale: false }),
  });
  health.counters.inc(COUNTER.OBSERVED, 4);
  health.onChannelAbsent();

  queue.enqueue('observation', { a: 1 }, 10);
  queue.enqueue('observation', { b: 1 }, 10);
  queue.enqueue('observation', { c: 1 }, 10); // evicts one

  const report = health.report();
  assert.equal(report.core, 'absent');
  assert.equal(report.state, 'degraded', 'never "healthy", and never "no observations"');
  assert.equal(report.collector, 'capture_extension');
  assert.equal(report.counters.observed, 4, 'what was observed is still reported');
  assert.equal(report.queue.depth, 2);
  assert.equal(report.queue.capacity, 2);
  assert.equal(report.queue.dropped_total, 1);
  assert.equal(report.policy.version, 'v3', '§11.3 mode-change attribution');
});

test('the queue\'s drop counter and the health counter are the same number, because the queue drives it', () => {
  // In the running extension this is `bootstrap`'s `onDrop: () => health.counters.inc('dropped', 1)`.
  const queue = createQueue({ capacity: 1 });
  const health = createHealthReporter({ device_id: 'dev-1', queueStats: () => queue.stats() });
  const wired = createQueue({ capacity: 1, onDrop: () => health.counters.inc(COUNTER.DROPPED, 1) });
  wired.enqueue('observation', { a: 1 }, 1);
  wired.enqueue('observation', { b: 1 }, 1);
  assert.equal(wired.stats().dropped_total, 1);
  assert.equal(health.counters.snapshot().counters.dropped, 1);
  assert.equal(health.report().counters.dropped, 1);
});

test('errors are counted with a cause, so an operator can group without reading prose', () => {
  const set = createCounterSet();
  set.countError('native_unavailable');
  set.countError('native_unavailable');
  set.countError('evaluation_error');
  set.countError(undefined);
  const snap = set.snapshot();
  assert.equal(snap.counters.errors, 4);
  assert.deepEqual(snap.errors_by_code, { native_unavailable: 2, evaluation_error: 1, internal_error: 1 });
});

test('§3.4: a dead channel reports capture-core absent AND extension-side degraded', () => {
  const health = createHealthReporter({
    device_id: 'dev-1',
    queueStats: () => queue.stats(),
    policySnapshot: () => ({ policy_version: 'v3', present: true, stale: false }),
  });
  // The queue drives the counter, exactly as `bootstrap` wires it, so the report and the queue
  // cannot disagree about how much was lost.
  const queue = createQueue({ capacity: 2, onDrop: () => health.counters.inc(COUNTER.DROPPED, 1) });
  health.counters.inc(COUNTER.OBSERVED, 4);
  health.onChannelAbsent();

  queue.enqueue('observation', { a: 1 }, 10);
  queue.enqueue('observation', { b: 1 }, 10);
  queue.enqueue('observation', { c: 1 }, 10); // evicts one

  const report = health.report();
  assert.equal(report.core, 'absent');
  assert.equal(report.state, 'degraded', 'never "healthy", and never "no observations"');
  assert.equal(report.collector, 'capture_extension');
  assert.equal(report.counters.observed, 4, 'what was observed is still reported');
  assert.equal(report.counters.dropped, 1, 'and what was lost is reported with it');
  assert.equal(report.queue.depth, 2);
  assert.equal(report.queue.capacity, 2);
  assert.equal(report.queue.dropped_total, 1);
  assert.equal(report.policy.version, 'v3', '§11.3 mode-change attribution');
});

test('a connected channel reports healthy and a successful send is recorded as emitted', () => {
  const queue = createQueue({ capacity: 10 });
  const health = createHealthReporter({ device_id: 'dev-1', queueStats: () => queue.stats() });
  health.onChannelConnected();
  health.markEmitted(2);
  const report = health.report();
  assert.equal(report.core, 'connected');
  assert.equal(report.state, 'healthy');
  assert.equal(report.counters.emitted, 2);
  assert.ok(report.last_success_at, 'last_success_at is set when something actually got through');
});

test('the health report carries every counter, so a missing one is never read as zero', () => {
  const health = createHealthReporter({ device_id: 'dev-1', queueStats: () => createQueue({ capacity: 1 }).stats() });
  const report = health.report();
  assert.deepEqual(Object.keys(report.counters).sort(), [...COUNTERS].sort());
});

test('before anything connects, the state is absent rather than healthy or empty', () => {
  const health = createHealthReporter({ device_id: 'dev-1', queueStats: () => createQueue({ capacity: 1 }).stats() });
  const report = health.report();
  assert.equal(report.core, 'absent');
  assert.equal(report.state, 'degraded');
  assert.equal(report.counters.observed, 0, 'zero observed is a fact; it is not the same fact as "not connected"');
});
