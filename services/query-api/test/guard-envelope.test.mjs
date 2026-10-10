// guard-envelope.test.mjs — the cost guard and the result states.
//
// The guard's contract is "rejection, not degradation": an over-budget request never runs, and
// the refusal names the fix. The envelope's contract is "no metric without its state": a
// data-bearing response cannot be built without freshness and coverage.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { guard, bucketsForWindow } from '../src/guard.js';
import { validate } from '../src/validate.js';
import { buildEnvelope, coverageBlock, freshnessBlock, resultStateFor } from '../src/envelope.js';
import { baseDoc, NOW, rejection } from './helpers.mjs';

const day = (n) => new Date(Date.UTC(2026, 8, 1 + n)).toISOString();

test('a four-year day-bucketed trend is refused, and the refusal names the bucket that fits', () => {
  let error;
  try {
    plan(baseDoc({ window: { from: '2023-01-01T00:00:00Z', to: '2026-01-01T00:00:00Z' }, dimensions: [] }), { now: NOW });
  } catch (e) {
    error = e;
  }
  assert.equal(error.resultState, 'query_too_broad');
  assert.equal(error.detail.fix.coarser_bucket, 'week', 'the fix names the coarser bucket that fits');
  assert.ok(error.detail.requested_points > 400);
});

test('a series beyond 400 points is auto-coarsened, and the applied bucket is reported', () => {
  const p = plan(baseDoc({ bucket: undefined, window: { from: '2025-06-01T00:00:00Z', to: '2026-10-01T00:00:00Z' }, dimensions: ['tool'] }), { now: NOW });
  assert.equal(p.meta.coarsened.from, 'day');
  assert.ok(['week', 'month'].includes(p.meta.coarsened.to));
  assert.equal(p.meta.applied_bucket, p.meta.coarsened.to);
  assert.ok(p.meta.notes.some((n) => n.includes('auto-coarsened')));
});

test('the cell estimate multiplies buckets by the grouped cardinalities, tightened by filters', () => {
  const wide = guard(validate(baseDoc({ dimensions: ['tool'] })));
  assert.equal(wide.estimatedCells, 7 * 200);
  const narrow = guard(validate(baseDoc({ dimensions: ['tool'], filters: [{ field: 'tool', op: 'eq', value: 'claude_web' }] })));
  assert.equal(narrow.estimatedCells, 7);
  const listed = guard(validate(baseDoc({ dimensions: ['tool'], filters: [{ field: 'tool', op: 'in', value: ['a', 'b', 'c'] }] })));
  assert.equal(listed.estimatedCells, 7 * 3);
});

test('a limited aggregate is bounded by its limit, and the estimate is disclosed rather than hidden', () => {
  const p = plan(baseDoc({ source: 'mart.agg_tool_user_period', measures: ['submissions'], dimensions: ['tool', 'subject'], filters: [{ field: 'tool', op: 'eq', value: 'x' }], limit: 500 }), { now: NOW });
  assert.equal(p.meta.guard.limited, true);
  assert.ok(p.meta.guard.estimated_cells > 2000);
  assert.equal(p.meta.guard.bounded_cells, 501);
  assert.ok(p.meta.notes.some((n) => n.includes('Limited aggregate')));
});

test('an unlimited aggregate over the same shape is refused', () => {
  const error = rejection(() =>
    plan(
      baseDoc({
        source: 'mart.agg_tool_user_period',
        measures: ['submissions'],
        dimensions: ['tool', 'subject'],
        filters: [{ field: 'tool', op: 'eq', value: 'x' }],
        limit: undefined,
      }),
      { now: NOW },
    ),
  );
  assert.equal(error.resultState, 'query_too_broad');
});

test('the list window caps are enforced per source, with the bound stated', () => {
  const event = rejection(() =>
    plan({ query_version: '1', source: 'ingest.submission', filters: [], window: { from: '2026-01-01T00:00:00Z', to: '2026-12-01T00:00:00Z' }, limit: 50 }, { now: NOW }),
  );
  assert.equal(event.detail.max_days, 31);
  const finding = rejection(() =>
    plan({ query_version: '1', source: 'mart.v_finding', filters: [], window: { from: '2026-01-01T00:00:00Z', to: '2026-12-01T00:00:00Z' }, limit: 50 }, { now: NOW }),
  );
  assert.equal(finding.detail.max_days, 31);
  const audit = rejection(() =>
    plan({ query_version: '1', source: 'ops.audit', filters: [], window: { from: '2024-09-01T00:00:00Z', to: '2026-09-01T00:00:00Z' }, limit: 50 }, { now: NOW }),
  );
  assert.equal(audit.detail.max_days, 366);
});

test('a subject-grouped window beyond seven days is refused, naming the narrowing', () => {
  const error = rejection(() =>
    plan(
      baseDoc({ source: 'mart.agg_tool_user_period', measures: ['submissions'], dimensions: ['tool', 'subject'], window: { from: '2026-08-01T00:00:00Z', to: '2026-09-01T00:00:00Z' } }),
      { now: NOW },
    ),
  );
  assert.equal(error.resultState, 'query_too_broad');
  assert.ok(JSON.stringify(error.detail.fix).includes('subject'));
});

test('a prefix predicate on the event table is refused: there is no index for it', () => {
  const error = rejection(() =>
    plan(
      {
        query_version: '1',
        source: 'ingest.submission',
        filters: [{ field: 'tool', op: 'starts_with', value: 'claude' }],
        window: { from: day(0), to: day(7) },
        limit: 50,
      },
      { now: NOW },
    ),
  );
  assert.equal(error.resultState, 'unsupported_query_shape');
  assert.equal(error.detail.fix.alternative_source, 'mart.agg_tool_period');
});

test('the bucket arithmetic matches the applied bucket', () => {
  assert.equal(bucketsForWindow('2026-09-01T00:00:00Z', '2026-09-08T00:00:00Z', 'day'), 7);
  assert.equal(bucketsForWindow('2026-09-01T00:00:00Z', '2026-09-08T00:00:00Z', 'hour'), 168);
  assert.equal(bucketsForWindow('2026-09-01T00:00:00Z', '2026-09-08T00:00:00Z', 'week'), 1);
});

// ---------------------------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------------------------

test('a data-bearing envelope cannot be built without freshness and coverage', () => {
  assert.throws(() => buildEnvelope({ data: [{ a: 1 }], freshness: freshnessBlock({ aggregate: 'x', row: null }) }), /freshness and coverage/);
  const ok = buildEnvelope({
    data: [],
    freshness: freshnessBlock({ aggregate: 'mart.agg_tool_period', bucketSize: 'day', row: null }),
    coverage: coverageBlock({ window: null, row: null }),
  });
  assert.equal(ok.api_version, '1');
  assert.equal(ok.query_version, '1');
  assert.ok(ok.freshness && ok.coverage);
});

test('freshness is stale only past three cadences, and a missing watermark is not_yet_covered', () => {
  const now = Date.parse('2026-10-02T12:00:00Z');
  const fresh = freshnessBlock({ aggregate: 'a', row: { last_run_at: new Date(now - 5 * 60_000).toISOString(), last_complete_bucket: '2026-10-02T11:00:00Z' }, now });
  assert.equal(fresh.state, 'fresh');
  assert.equal(fresh.lag_seconds, 300);
  const stale = freshnessBlock({ aggregate: 'a', row: { last_run_at: new Date(now - 16 * 60_000).toISOString() }, now });
  assert.equal(stale.state, 'stale');
  const missing = freshnessBlock({ aggregate: 'mart.agg_org_period', row: null, reason: 'directory_not_synced', now });
  assert.equal(missing.state, 'not_yet_covered');
  assert.equal(missing.reason, 'directory_not_synced');
});

test('coverage states the enrolled denominator and keeps unknown as a reason', () => {
  const partial = coverageBlock({
    window: ['2026-09-01', '2026-09-08'],
    row: { devices_enrolled: 100, devices_reporting: 80, expected_collector_days: 600, observed_collector_days: 540, gap_reasons: { unknown: 6, not_enrolled: 54 } },
  });
  assert.equal(partial.state, 'partial');
  assert.equal(partial.devices_enrolled, 100);
  assert.equal(partial.gap_reasons.unknown, 6, 'unknown is a reason value, not a blank');
  assert.equal(partial.gaps_total, 60);
  const complete = coverageBlock({ row: { devices_enrolled: 10, devices_reporting: 10, expected_collector_days: 60, observed_collector_days: 60, gap_reasons: {} } });
  assert.equal(complete.state, 'complete');
});

test('result states never merge "we looked and found nothing" with "we cannot say"', () => {
  const fresh = { state: 'fresh' };
  const complete = { state: 'complete' };
  assert.equal(resultStateFor({ rowCount: 0, freshness: fresh, coverage: complete }), 'empty');
  assert.equal(resultStateFor({ rowCount: 0, freshness: fresh, coverage: { state: 'not_yet_covered' } }), 'not_yet_covered');
  assert.equal(resultStateFor({ rowCount: 0, freshness: { state: 'stale' }, coverage: complete }), 'stale_aggregate');
  assert.equal(resultStateFor({ rowCount: 0, freshness: fresh, coverage: { state: 'partial' } }), 'coverage_degraded');
  assert.equal(resultStateFor({ rowCount: 5, freshness: fresh, coverage: { state: 'partial' } }), 'coverage_degraded');
  assert.equal(resultStateFor({ rowCount: 5, freshness: { state: 'stale' }, coverage: complete }), 'stale_aggregate');
  assert.equal(resultStateFor({ rowCount: 5, freshness: fresh, coverage: complete }), 'ok');
  assert.equal(resultStateFor({ rowCount: 5, freshness: fresh, coverage: complete }), 'ok');
});

test('the q3 shape reports not_yet_covered for a missing watermark rather than a zero line', () => {
  const p = plan({ query_version: '1', template: 'q3_team_growth', params: { window: { from: day(0), to: day(7) }, limit: 100 } }, { now: NOW });
  assert.equal(p.source.id, 'mart.agg_team_period');
  const freshness = p.statements.find((s) => s.id === 'freshness');
  assert.equal(freshness.params[0], 'mart.agg_team_period');
});
