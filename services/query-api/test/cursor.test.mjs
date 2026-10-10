// cursor.test.mjs — cursor pagination.

import test from 'node:test';
import assert from 'node:assert/strict';
import { decodeCursor, encodeCursor, paginate } from '../src/cursor.js';
import { plan, executePlan } from '../src/plan.js';
import { baseDoc, NOW } from './helpers.mjs';

const KEY = Buffer.from('a'.repeat(32));

// ---------------------------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------------------------

const CURSOR_INPUT = Object.freeze({
  tenant: 'tenant-a',
  dslHash: 'deadbeef',
  order: ['bucket', 'tool'],
  upper: '2026-09-07T23:59:59.000Z',
  position: { bucket: '2026-09-01T00:00:00.000Z', tool: 'claude_web' },
});

test('a cursor round-trips and is bound to tenant, query and ordering', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000 });
  const payload = decodeCursor(token, { tenant: 'tenant-a', dslHash: 'deadbeef', order: ['bucket', 'tool'] }, { key: KEY, now: 2000 });
  assert.deepEqual(payload.pos, { bucket: '2026-09-01T00:00:00.000Z', tool: 'claude_web' });
  assert.equal(payload.upper, CURSOR_INPUT.upper);
});

test('a tampered cursor is refused, and the failure is cursor_expired', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000 });
  const [body] = token.split('.');
  const forged = `${body}.${'A'.repeat(43)}`;
  for (const bad of [forged, `${body}.`, 'garbage', 'a.b']) {
    let error;
    try {
      decodeCursor(bad, { tenant: 'tenant-a', dslHash: 'deadbeef', order: ['bucket', 'tool'] }, { key: KEY, now: 2000 });
    } catch (e) {
      error = e;
    }
    assert.ok(error, `expected a rejection for ${bad}`);
    assert.equal(error.resultState, 'cursor_expired');
    assert.equal(error.http, 400);
  }
});

test('a cursor from another tenant, query or ordering is refused with the reason', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000 });
  const cases = [
    [{ tenant: 'tenant-b', dslHash: 'deadbeef', order: ['bucket', 'tool'] }, 'cursor_tenant_mismatch'],
    [{ tenant: 'tenant-a', dslHash: 'cafe', order: ['bucket', 'tool'] }, 'cursor_mismatch'],
    [{ tenant: 'tenant-a', dslHash: 'deadbeef', order: ['bucket', 'subject'] }, 'cursor_mismatch'],
  ];
  for (const [expected, reason] of cases) {
    let error;
    try {
      decodeCursor(token, expected, { key: KEY, now: 2000 });
    } catch (e) {
      error = e;
    }
    assert.equal(error.reason, reason);
    assert.ok(!error.message.includes('tenant-a') && !error.message.includes('tenant-b'), 'a cursor error never echoes a tenant');
  }
});

test('an expired cursor is refused, and the client is told to restart', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000, ttlMs: 60_000 });
  const error = (() => {
    try {
      decodeCursor(token, { tenant: 'tenant-a', dslHash: 'deadbeef', order: ['bucket', 'tool'] }, { key: KEY, now: 1000 + 60_001 });
    } catch (e) {
      return e;
    }
    return null;
  })();
  assert.equal(error.resultState, 'cursor_expired');
  assert.equal(error.reason, 'cursor_expired');
});

test('a cursor signed with another key does not verify', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000 });
  let error;
  try {
    decodeCursor(token, { tenant: 'tenant-a', dslHash: 'deadbeef', order: ['bucket', 'tool'] }, { key: Buffer.from('b'.repeat(32)), now: 2000 });
  } catch (e) {
    error = e;
  }
  assert.equal(error.resultState, 'cursor_expired');
});

test('a cursor is refused on an aggregate: aggregates are not paged', () => {
  const token = encodeCursor(CURSOR_INPUT, { key: KEY, now: 1000 });
  let error;
  try {
    plan(
      baseDoc({
        source: 'mart.agg_tool_user_period',
        measures: ['submissions'],
        dimensions: ['tool', 'subject'],
        filters: [{ field: 'tool', op: 'eq', value: 'claude_web' }],
        cursor: token,
        limit: 50,
      }),
      { now: NOW, tenant: 'tenant-a', cursorKey: KEY },
    );
  } catch (e) {
    error = e;
  }
  assert.equal(error.resultState, 'unsupported_query_shape');
  assert.equal(error.reason, 'aggregate_not_paged');
});

test('a cursor without a key is refused rather than signed with a made-up one', () => {
  assert.throws(() => encodeCursor(CURSOR_INPUT, {}), /cursor key/);
  assert.throws(() => decodeCursor('a.b', {}, {}), /cursor key/);
});

test('page two follows page one: the cursor resumes after the last row under the same snapshot', async () => {
  const doc = { query_version: '1', source: 'ingest.submission', filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' }, limit: 2 };
  const ctx = { now: NOW, tenant: 'tenant-a', actorId: 'a', cursorKey: KEY };
  const rows = [
    { submission_id: '00000000-0000-4000-8000-000000000003', received_at: new Date('2026-09-07T10:00:00Z') },
    { submission_id: '00000000-0000-4000-8000-000000000002', received_at: new Date('2026-09-07T09:00:00Z') },
    { submission_id: '00000000-0000-4000-8000-000000000001', received_at: new Date('2026-09-07T08:00:00Z') },
  ];
  const planned = plan(doc, ctx);
  const client = { async query(text) { return { rows: text === planned.compiled.text ? rows : [] }; } };
  const first = await executePlan(planned, { ...ctx, client });
  assert.equal(first.data.length, 2);
  const cursor = first.page.next_cursor;
  assert.ok(cursor, 'the probe row means there is a next page');

  const later = new Date(NOW.getTime() + 60_000);
  const second = plan({ ...doc, cursor }, { ...ctx, now: later });
  assert.equal(second.meta.snapshot_upper_bound, first.page.snapshot_upper_bound, 'page two keeps the snapshot of page one');
  assert.match(second.compiled.text, /s\.received_at < \$\d+::timestamptz\)/, 'page two resumes after the last row');
  assert.ok(second.compiled.params.includes('2026-09-07T09:00:00.000Z'));
  assert.ok(second.compiled.params.includes('00000000-0000-4000-8000-000000000002'));

  const otherTenant = (() => { try { plan({ ...doc, cursor }, { ...ctx, tenant: 'tenant-b' }); } catch (e) { return e; } return null; })();
  assert.equal(otherTenant.reason, 'cursor_tenant_mismatch');
  const otherQuery = (() => { try { plan({ ...doc, limit: 3, filters: [{ field: 'tool', op: 'eq', value: 'x' }], cursor }, ctx); } catch (e) { return e; } return null; })();
  assert.equal(otherQuery.reason, 'cursor_mismatch');
});

test('pagination stops only when there is no next page, never on a short page', () => {
  const rows = [{ n: 1 }, { n: 2 }, { n: 3 }];
  const short = paginate({ rows, limit: 10, resume: () => 'next', snapshotUpper: 'upper' });
  assert.equal(short.page.returned, 3);
  assert.equal(short.page.next_cursor, null, 'a short page with no probe row is the end');

  const probe = paginate({ rows: [{ n: 1 }, { n: 2 }, { n: 3 }, { n: 4 }], limit: 3, resume: () => 'next', snapshotUpper: 'upper' });
  assert.equal(probe.page.returned, 3);
  assert.equal(probe.page.next_cursor, 'next');

  const middle = paginate({ rows: [{ n: 1 }, { n: 2 }], limit: 2, resume: () => 'next' });
  assert.equal(middle.page.next_cursor, null);
});

test('a plan for a page two page carries the frozen snapshot bound', () => {
  const first = plan(baseDoc({ limit: 3 }), { now: NOW, tenant: 'tenant-a', cursorKey: KEY });
  assert.equal(first.meta.snapshot_upper_bound, '2026-09-08T00:00:00.000Z', 'an aggregate freezes on its window');
  const list = plan({ query_version: '1', source: 'ingest.submission', filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z' }, limit: 3 }, { now: NOW, tenant: 'tenant-a', cursorKey: KEY });
  assert.equal(list.meta.snapshot_upper_bound, NOW.toISOString(), 'a list freezes at the moment page one is served');
});
