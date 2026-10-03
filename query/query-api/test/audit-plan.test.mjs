// audit-plan.test.mjs — §5's audit-on-read and the executor's transaction shape.
//
// The two properties that matter: the audit decision is made before suppression, and a read that
// cannot be proved to have happened does not happen (fail closed, zero rows, 503).

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan, executePlan } from '../src/plan.js';
import { auditDecision, auditStatement, verifyAuditPage } from '../src/audit.js';
import { validate } from '../src/validate.js';
import { REASON } from '../src/errors.js';
import { resolveMissingRecord } from '../src/blocks.js';
import { baseDoc, NOW } from './helpers.mjs';

/** A client that records statements and returns canned rows per statement id. */
function fakeClient(canned = {}, options = {}) {
  const log = [];
  let inTransaction = false;
  return {
    log,
    get transaction() {
      return inTransaction;
    },
    async begin() {
      if (inTransaction) throw new Error('nested transaction');
      inTransaction = true;
      log.push({ kind: 'BEGIN' });
    },
    async commit() {
      inTransaction = false;
      log.push({ kind: 'COMMIT' });
    },
    async rollback() {
      inTransaction = false;
      log.push({ kind: 'ROLLBACK' });
    },
    async query(text, params) {
      if (!inTransaction) throw new Error('statement outside a transaction');
      const id = options.idOf ? options.idOf(text) : idFromText(text);
      log.push({ kind: 'query', id, text, params });
      if (options.failOn && options.failOn(id)) throw new Error('simulated database failure');
      return { rows: canned[id] ?? [] };
    },
  };
}

function idFromText(text) {
  if (text.startsWith('INSERT INTO ops.audit')) return 'audit_insert';
  if (text.includes('FROM ops.aggregate_watermark')) return 'freshness';
  if (text.includes('AS devices_enrolled')) return 'coverage';
  if (text.includes('newer_events_exist')) return 'newer_events';
  return 'read';
}

test('a query that returns a subject reference is audited, and the audit row is written first', async () => {
  const p = plan(
    { query_version: '1', source: 'ingest.submission', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'analyst-1' },
  );
  assert.equal(p.audit.decision.required, true);
  assert.equal(p.audit.decision.phase, 'pre_read');
  assert.equal(p.statements[0].id, 'audit_insert');
  assert.equal(p.statements[1].id, 'read');

  const client = fakeClient({ read: [] });
  await executePlan(p, { client, now: NOW });
  const kinds = client.log.map((e) => e.kind === 'query' ? e.id : e.kind);
  assert.deepEqual(kinds.slice(0, 3), ['BEGIN', 'audit_insert', 'read']);
  assert.equal(kinds[kinds.length - 1], 'COMMIT');
});

test('the audit insert takes the tenant from the session function and everything else as a parameter', () => {
  const validated = validate(baseDoc());
  const decision = auditDecision(validated, {});
  const statement = auditStatement(decision, { actorId: "o'brien", detail: { a: 1 } });
  assert.ok(statement.text.includes('VALUES (ops.current_tenant(), $1::text'));
  assert.equal(statement.params[1], "o'brien", 'a quote in an actor id stays a value');
  assert.ok(!statement.text.includes("o'brien"));
  assert.ok(statement.text.includes('RETURNING audit_seq, occurred_at, row_hash'));
});

test('a read of the audit trail writes one row and is not re-audited', async () => {
  const rows = [
    { audit_seq: 2, occurred_at: new Date('2026-09-02T00:00:00Z'), row_hash: 'b', prev_hash: 'a', __recomputed_hash: 'b', __newer_prev_hash: null },
    { audit_seq: 1, occurred_at: new Date('2026-09-01T00:00:00Z'), row_hash: 'a', prev_hash: null, __recomputed_hash: 'a', __newer_prev_hash: null },
  ];
  const p = plan(
    { query_version: '1', source: 'ops.audit', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'auditor-1' },
  );
  assert.equal(p.audit.decision.selfAudited, false);
  const client = fakeClient({ read: rows, audit_insert: [{ audit_seq: 99, occurred_at: new Date('2026-10-02T12:00:00Z') }] });
  const envelope = await executePlan(p, { client, now: NOW });
  const auditQueries = client.log.filter((e) => e.kind === 'query' && e.id === 'audit_insert');
  assert.equal(auditQueries.length, 1, 'exactly one audit row per query, no recursion');
  assert.equal(envelope.audit.entry_id, '99');
  assert.equal(envelope.data.length, 2);
  assert.equal(envelope.data[0].__recomputed_hash, undefined, 'internal columns never reach the wire');
});

test('a broken hash link returns audit_chain_broken rather than a list that looks fine', async () => {
  const rows = [
    { audit_seq: 2, occurred_at: new Date('2026-09-02T00:00:00Z'), row_hash: 'b', prev_hash: 'X', __recomputed_hash: 'b', __newer_prev_hash: null },
    { audit_seq: 1, occurred_at: new Date('2026-09-01T00:00:00Z'), row_hash: 'a', prev_hash: null, __recomputed_hash: 'a', __newer_prev_hash: 'X' },
  ];
  const p = plan(
    { query_version: '1', source: 'ops.audit', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'auditor-1' },
  );
  const client = fakeClient({ read: rows, audit_insert: [{ audit_seq: 1, occurred_at: new Date('2026-10-02T12:00:00Z') }] });
  const envelope = await executePlan(p, { client, now: NOW });
  assert.equal(envelope.result_state, 'audit_chain_broken');
  assert.equal(envelope.data, undefined);
  assert.equal(envelope.error.code, REASON.LINK_MISMATCH);
});

test('verifyAuditPage checks recomputation and linkage within the page', () => {
  const ok = verifyAuditPage([
    { audit_seq: 2, row_hash: 'b', prev_hash: 'a', __recomputed_hash: 'b', __newer_prev_hash: null },
    { audit_seq: 1, row_hash: 'a', prev_hash: null, __recomputed_hash: 'a', __newer_prev_hash: 'a' },
  ]);
  assert.equal(ok.ok, true);
  const recomputed = verifyAuditPage([{ audit_seq: 1, row_hash: 'a', __recomputed_hash: 'z' }]);
  assert.equal(recomputed.ok, false);
  assert.equal(recomputed.broken[0].reason, REASON.CHAIN_MISMATCH);
  const linked = verifyAuditPage([{ audit_seq: 1, row_hash: 'a', __recomputed_hash: 'a', __newer_prev_hash: 'wrong' }]);
  assert.equal(linked.broken[0].reason, REASON.LINK_MISMATCH);
});

test('a failing audit insert serves zero rows and returns 503 audit_unavailable', async () => {
  const p = plan(
    { query_version: '1', source: 'ingest.submission', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'analyst-1' },
  );
  const client = fakeClient({ read: [{ submission_id: 'x', "subject": 'user-1' }] }, { failOn: (id) => id === 'audit_insert' });
  let error;
  try {
    await executePlan(p, { client, now: NOW });
  } catch (e) {
    error = e;
  }
  assert.ok(error, 'the read must not be served');
  assert.equal(error.resultState, 'audit_unavailable');
  assert.equal(error.http, 503);
  assert.equal(error.resultState === 'audit_unavailable' && error.toEnvelope().data, undefined);
  assert.ok(client.log.some((e) => e.kind === 'ROLLBACK'), 'the transaction is rolled back');
  assert.ok(!client.log.some((e) => e.kind === 'query' && e.id === 'read'), 'the read never ran');
});

test('a failure after the read still rolls back and serves nothing', async () => {
  const p = plan(
    { query_version: '1', source: 'ops.coverage_snapshot', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'analyst-1' },
  );
  const client = fakeClient({}, { failOn: (id) => id === 'coverage' });
  let error;
  try {
    await executePlan(p, { client, now: NOW });
  } catch (e) {
    error = e;
  }
  assert.equal(error.resultState, 'audit_unavailable');
  assert.ok(client.log.some((e) => e.kind === 'ROLLBACK'));
  assert.equal(client.transaction, false);
});

test('the small-cell trigger is post-read: the audit row is written after the cells exist', async () => {
  const p = plan(baseDoc({ measures: ['submissions', 'users'] }), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  assert.equal(p.audit.decision.phase, 'post_read');
  const rows = [{ bucket: '2026-09-01T00:00:00Z', tool: 'a', submissions: 3, users: 1, __k_subjects: 1 }];
  const client = fakeClient({
    read: rows,
    freshness: [{ aggregate_name: 'mart.agg_tool_period', bucket_size: 'day', last_run_at: new Date(NOW.getTime() - 60_000), last_complete_bucket: '2026-10-02T11:00:00Z' }],
    coverage: [{ devices_enrolled: 100, devices_reporting: 100, expected_collector_days: 600, observed_collector_days: 600, gap_reasons: {} }],
    audit_insert: [{ audit_seq: 7, occurred_at: new Date('2026-10-02T12:00:00Z') }],
  });
  const envelope = await executePlan(p, { client, now: NOW });
  const order = client.log.map((e) => (e.kind === 'query' ? e.id : e.kind));
  assert.ok(order.indexOf('read') < order.indexOf('audit_insert'), 'the cells decide the trigger');
  assert.equal(envelope.audit.entry_id, '7');
  assert.equal(envelope.suppression.suppressed_cells, 1);
  assert.equal(envelope.data[0].result_state, 'suppressed');
  assert.equal(envelope.result_state, 'suppressed');
});

test('a wide aggregate cell writes no audit row at all', async () => {
  const p = plan(baseDoc({ measures: ['submissions', 'users'] }), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  const client = fakeClient({
    read: [{ bucket: '2026-09-01T00:00:00Z', tool: 'a', submissions: 300, users: 40, __k_subjects: 40 }],
    freshness: [{ aggregate_name: 'mart.agg_tool_period', bucket_size: 'day', last_run_at: new Date(NOW.getTime() - 60_000), last_complete_bucket: '2026-10-02T11:00:00Z' }],
    coverage: [{ devices_enrolled: 100, devices_reporting: 100, expected_collector_days: 600, observed_collector_days: 600, gap_reasons: {} }],
  });
  const envelope = await executePlan(p, { client, now: NOW });
  assert.ok(!client.log.some((e) => e.kind === 'query' && e.id === 'audit_insert'));
  assert.equal(envelope.audit, undefined);
  assert.equal(envelope.suppression.suppressed_cells, 0);
  assert.equal(envelope.result_state, 'ok');
});

test('device and coverage reads are not subject-level and are not audited (§5.2)', () => {
  const device = validate({ query_version: '1', source: 'mart.v_device_liveness', filters: [], limit: 10 });
  const coverage = validate({ query_version: '1', source: 'ops.coverage_snapshot', filters: [], window: { ...baseDoc().window }, limit: 10 });
  for (const validated of [device, coverage]) {
    const decision = auditDecision(validated, {});
    assert.equal(decision.required, false, `${validated.source.id} is not subject-level`);
    assert.equal(decision.phase, 'none');
  }
});

test('the audit detail describes the question and puts subject values in subject_ref', () => {
  const validated = validate(
    baseDoc({ filters: [{ field: 'tool', op: 'eq', value: 'claude_web' }, { field: 'sanctioned_state', op: 'eq', value: 'unsanctioned' }] }),
  );
  const decision = auditDecision(validated, {});
  const statement = auditStatement(decision, {
    actorId: 'analyst-1',
    subjectRef: 'user_ref_9',
    detail: { source: validated.source.id, filters: validated.query.filters.map((f) => ({ field: f.field, op: f.op })) },
  });
  const detail = JSON.parse(statement.params[7]);
  assert.equal(detail.source, 'mart.v_tool_usage');
  assert.equal(statement.params[5], 'user_ref_9');
});

test('§13\'s not_found rule: a purge receipt makes the answer no_longer_available, not 404', () => {
  const receipt = { receipt_id: 'r1', scope_kind: 'subject', completed_at: '2026-09-06T00:00:00Z', mechanisms: ['db_delete'], removed_counts: { submission: 12 }, remaining_counts: {} };
  const purged = resolveMissingRecord({ found: false, receivedAtHint: '2026-09-05T04:00:00Z', receipts: [receipt] });
  assert.equal(purged.result_state, 'no_longer_available');
  assert.equal(purged.http, 410);
  assert.equal(purged.reason, 'erasure');
  assert.equal(purged.receipt.receipt_id, 'r1');

  const unknownWindow = resolveMissingRecord({ found: false, receivedAtHint: null, receipts: [] });
  assert.equal(unknownWindow.result_state, 'not_found');
  assert.equal(unknownWindow.detail.purge_window_unknown, true);

  const noEvidence = resolveMissingRecord({ found: false, receivedAtHint: '2026-09-05T04:00:00Z', receipts: [] });
  assert.equal(noEvidence.result_state, 'not_found');
  assert.equal(noEvidence.detail.retention_evidence, 'no_ledger_in_schema');

  assert.equal(resolveMissingRecord({ found: true }).result_state, 'ok');
});

test('an event list page is cursored and reports newer events without hiding them', async () => {
  const rows = [
    { submission_id: 's1', received_at: new Date('2026-09-07T10:00:00Z'), "subject": 'u1' },
    { submission_id: 's2', received_at: new Date('2026-09-07T09:00:00Z'), "subject": 'u2' },
    { submission_id: 's3', received_at: new Date('2026-09-07T08:00:00Z'), "subject": 'u3' },
  ];
  const p = plan(
    { query_version: '1', source: 'ingest.submission', filters: [], window: { ...baseDoc().window }, limit: 2 },
    { now: NOW, tenant: 't1', actorId: 'analyst-1', cursorKey: Buffer.from('k'.repeat(32)) },
  );
  const client = fakeClient(
    { read: rows, newer_events: [{ newer_events_exist: true }],
      coverage: [{ devices_enrolled: 10, devices_reporting: 10, expected_collector_days: 60, observed_collector_days: 60, gap_reasons: {} }],
      audit_insert: [{ audit_seq: 1, occurred_at: new Date('2026-10-02T12:00:00Z') }] },
  );
  const envelope = await executePlan(p, { client, now: NOW, tenant: 't1', cursorKey: Buffer.from('k'.repeat(32)) });
  assert.equal(envelope.page.returned, 2);
  assert.ok(envelope.page.next_cursor, 'the probe row means there is a next page');
  assert.equal(envelope.page.newer_events_exist, true);
  assert.equal(envelope.page.snapshot_upper_bound, NOW.toISOString());
  assert.equal(envelope.data.length, 2);
});
