// audit-plan.test.mjs — audit-on-read and the executor's transaction shape.
//
// The two properties that matter: the audit decision is made before the read is served, and a read that
// cannot be proved to have happened does not happen (fail closed, zero rows, 503).

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan, executePlan } from '../src/plan.js';
import { auditDecision, auditStatement, verifyAuditPage } from '../src/audit.js';
import { validate } from '../src/validate.js';
import { REASON } from '../src/errors.js';
import { resolveMissingRecord } from '../src/blocks.js';
import { baseDoc, NOW } from './helpers.mjs';

/**
 * A connection that records statements and returns canned rows per statement id. BEGIN, COMMIT,
 * ROLLBACK and the tenant scope arrive through query(), as they do on a pooled pg client.
 */
function fakeClient(canned = {}, options = {}) {
  const log = [];
  let inTransaction = false;
  return {
    log,
    get transaction() {
      return inTransaction;
    },
    async query(text, params) {
      if (text === 'BEGIN') {
        if (inTransaction) throw new Error('nested transaction');
        inTransaction = true;
        log.push({ kind: 'BEGIN' });
        return { rows: [] };
      }
      if (text === 'COMMIT' || text === 'ROLLBACK') {
        inTransaction = false;
        log.push({ kind: text });
        return { rows: [] };
      }
      if (!inTransaction) throw new Error('statement outside a transaction');
      if (text.startsWith("SELECT set_config('app.tenant_id'")) {
        log.push({ kind: 'SCOPE', params });
        return { rows: [] };
      }
      const id = options.idOf ? options.idOf(text) : idFromText(text);
      log.push({ kind: 'query', id, text, params });
      if (options.failOn && options.failOn(id)) {
        const error = new Error('simulated database failure');
        if (options.code) error.code = options.code;
        throw error;
      }
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
  await executePlan(p, { client, now: NOW, tenant: 't1' });
  const kinds = client.log.map((e) => e.kind === 'query' ? e.id : e.kind);
  assert.deepEqual(kinds.slice(0, 4), ['BEGIN', 'SCOPE', 'audit_insert', 'read']);
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
  const envelope = await executePlan(p, { client, now: NOW, tenant: 't1' });
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
  const envelope = await executePlan(p, { client, now: NOW, tenant: 't1' });
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
    await executePlan(p, { client, now: NOW, tenant: 't1' });
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
    await executePlan(p, { client, now: NOW, tenant: 't1' });
  } catch (e) {
    error = e;
  }
  assert.equal(error.resultState, 'audit_unavailable');
  assert.ok(client.log.some((e) => e.kind === 'ROLLBACK'));
  assert.equal(client.transaction, false);
});

test('an aggregate cell writes no audit row, however few people it covers', async () => {
  const p = plan(baseDoc({ measures: ['submissions', 'users'] }), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  assert.equal(p.audit.decision.phase, 'none');
  const client = fakeClient({
    read: [{ bucket: '2026-09-01T00:00:00Z', tool: 'a', submissions: 3, users: 1 }],
    freshness: [{ aggregate_name: 'mart.agg_tool_period', bucket_size: 'day', last_run_at: new Date(NOW.getTime() - 60_000), last_complete_bucket: '2026-10-02T11:00:00Z' }],
    coverage: [{ devices_enrolled: 100, devices_reporting: 100, expected_collector_days: 600, observed_collector_days: 600, gap_reasons: {} }],
  });
  const envelope = await executePlan(p, { client, now: NOW, tenant: 't1' });
  assert.ok(!client.log.some((e) => e.kind === 'query' && e.id === 'audit_insert'));
  assert.equal(envelope.audit, undefined);
  assert.equal(envelope.suppression, undefined);
  assert.equal(envelope.data[0].users, 1, 'a small count is served as the number it is');
  assert.equal(envelope.result_state, 'ok');
});

test('the device read is subject-level and audited; the coverage read is not', () => {
  const device = validate({ query_version: '1', source: 'mart.v_device_liveness', filters: [], limit: 10 });
  const coverage = validate({ query_version: '1', source: 'ops.coverage_snapshot', filters: [], window: { ...baseDoc().window }, limit: 10 });

  // The device row returns the most recent user (user_ref, subject_name), so it is subject-level:
  // a read that identifies a subject is recorded as served.
  const deviceDecision = auditDecision(device, {});
  assert.equal(deviceDecision.required, true, 'a device read that names a user is subject-level');
  assert.equal(deviceDecision.phase, 'pre_read');
  assert.ok(deviceDecision.reasons.includes('returns_subject_reference'));

  const coverageDecision = auditDecision(coverage, {});
  assert.equal(coverageDecision.required, false, 'coverage carries no subject reference');
  assert.equal(coverageDecision.phase, 'none');
});

test("one device's collector rows name no person, so the read writes no audit row", async () => {
  const device = '00000000-0000-4000-8000-0000000000d1';
  const p = plan(
    { query_version: '1', source: 'ops.collector_state', filters: [{ field: 'device', op: 'eq', value: device }], limit: 100 },
    { now: NOW, tenant: 't1', actorId: 'viewer-1' },
  );
  assert.equal(p.audit.decision.required, false);
  assert.equal(p.audit.decision.phase, 'none');
  const read = p.statements.find((s) => s.id === 'read');
  for (const column of ['user_ref', 'subject_name', 'hostname', 'detail']) {
    assert.ok(!new RegExp(`\\b${column}\\b`).test(read.text.split('FROM')[0]), `the collector rows select ${column}`);
  }
  assert.match(read.text, /cs\.device_id = \$1::uuid/);
  assert.deepEqual(read.params.slice(0, 1), [device]);

  const rows = [
    { device, collector: 'tool_config_claude_code', collector_state: 'degraded', error_code: 'config_tampered', last_report_at: '2026-10-02T11:00:00Z', last_success_at: '2026-10-02T10:00:00Z' },
    { device, collector: 'tool_config_cursor', collector_state: 'absent', error_code: 'disabled_by_policy', last_report_at: '2026-10-02T11:00:00Z', last_success_at: null },
  ];
  const client = fakeClient({
    read: rows,
    coverage: [{ devices_enrolled: 1, devices_reporting: 1, expected_collector_days: 1, observed_collector_days: 1, gap_reasons: {} }],
  });
  const envelope = await executePlan(p, { client, now: NOW, tenant: 't1' });
  assert.ok(!client.log.some((e) => e.kind === 'query' && e.id === 'audit_insert'), 'no audit row');
  assert.equal(envelope.audit, undefined);
  assert.deepEqual(envelope.data.map((r) => [r.collector, r.collector_state, r.error_code]), [
    ['tool_config_claude_code', 'degraded', 'config_tampered'],
    ['tool_config_cursor', 'absent', 'disabled_by_policy'],
  ]);
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

test('a missing record covered by an erasure receipt is no_longer_available, not 404', () => {
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

test('the transaction is scoped to the session tenant with the query class statement timeout', async () => {
  const p = plan(baseDoc({ measures: ['submissions'] }), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  const client = fakeClient({});
  await executePlan(p, { client, now: NOW, tenant: 't1' });
  const scope = client.log.find((e) => e.kind === 'SCOPE');
  assert.deepEqual(scope.params, ['t1', '3000ms', '1500ms'], 'aggregate class: 3 s statement timeout, half of it for locks');
  assert.equal(client.log[0].kind, 'BEGIN');
  assert.equal(client.log.at(-1).kind, 'COMMIT');
});

test('a read without a tenant is refused before anything runs', async () => {
  const p = plan(baseDoc(), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  const client = fakeClient({});
  await assert.rejects(executePlan(p, { client, now: NOW }));
  assert.equal(client.log.length, 0);
});

test('a statement timeout is busy and retryable, not an audit failure', async () => {
  const p = plan(
    { query_version: '1', source: 'ingest.submission', filters: [], window: { ...baseDoc().window }, limit: 10 },
    { now: NOW, tenant: 't1', actorId: 'analyst-1' },
  );
  for (const [code, reason] of [['57014', 'statement_timeout'], ['55P03', 'lock_timeout'], ['40001', 'serialization_failure'], ['40P01', 'serialization_failure']]) {
    const client = fakeClient({}, { failOn: (id) => id === 'read', code });
    const error = await executePlan(p, { client, now: NOW, tenant: 't1' }).then(() => null, (e) => e);
    assert.equal(error.resultState, 'busy', code);
    assert.equal(error.http, 429);
    assert.equal(error.reason, reason);
    assert.ok(client.log.some((e) => e.kind === 'ROLLBACK'));
  }
  const other = fakeClient({}, { failOn: (id) => id === 'read', code: '42P01' });
  const error = await executePlan(p, { client: other, now: NOW, tenant: 't1' }).then(() => null, (e) => e);
  assert.equal(error.resultState, 'audit_unavailable');
  assert.equal(error.reason, 'read_failed', 'only the audit insert itself is audit_write_failed');
});

test('an aggregate is cut at its limit and says whether the limit truncated it', async () => {
  const p = plan(baseDoc({ measures: ['submissions'], limit: 2 }), { now: NOW, tenant: 't1', actorId: 'analyst-1' });
  const rows = [1, 2, 3].map((n) => ({ bucket: '2026-09-01T00:00:00Z', tool: `t${n}`, submissions: 100, __ord_1: n }));
  const truncated = await executePlan(p, { client: fakeClient({ read: rows }), now: NOW, tenant: 't1' });
  assert.equal(truncated.data.length, 2, 'the probe row is never served');
  assert.equal(truncated.meta.truncated, true);
  assert.equal(truncated.page, undefined, 'an aggregate is not paged');
  const whole = await executePlan(p, { client: fakeClient({ read: rows.slice(0, 2) }), now: NOW, tenant: 't1' });
  assert.equal(whole.data.length, 2);
  assert.equal(whole.meta.truncated, false);
});
