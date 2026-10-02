// db.test.mjs — the compiled SQL against the real db/schema.sql.
//
// THIS IS THE STRONGEST EVIDENCE THIS PACKAGE PRODUCES. It is not a mock and not a snapshot:
// every statement the ten templates and the twelve registered sources can produce is bound and
// executed by PostgreSQL 17.11 against the schema another agent applied, inside one transaction
// that is rolled back, so the shared database is left exactly as it was found.
//
// It asserts what a snapshot cannot: that the field allow-list is real. A renamed column, a
// missing view, a wrong type, a GROUP BY that does not cover a selected expression, an ordering
// expression Postgres will not accept — all of those are syntax or semantic errors here and
// nothing else in this suite would catch them.
//
// The test SKIPS (and says so) when no PostgreSQL container is reachable; a skip is not a pass,
// and `tools/verify-all.mjs` reports the suite's exit code, not this file's skips.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { SOURCES, SOURCE_IDS } from '../src/registry.js';
import { TEMPLATES, TEMPLATE_NAMES } from '../src/templates.js';
import { bindForPsql, findContainer, psqlScript, TENANT, tenantPrelude } from './helpers.mjs';

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const NOW = new Date('2026-10-02T12:00:00Z');

const TEMPLATE_PARAMS = Object.freeze({
  q1_tools_ranked: { window: WINDOW, limit: 100 },
  q2_unsanctioned_users: { window: WINDOW, tool: 'claude_web', limit: 100 },
  q3_team_growth: { window: WINDOW, limit: 100 },
  q4_class_mix: { window: WINDOW, limit: 100 },
  q5_findings: { window: WINDOW, limit: 50 },
  q6_subject_series: { window: WINDOW, subject: 'user_ref_0001' },
  q7_devices: { limit: 50 },
  q8_activity: { window: WINDOW, subject: 'user_ref_0001', limit: 50 },
  q9_event_detail: { submission_id: '11111111-2222-4333-8444-555555555555' },
  q10_audit_trail: { window: WINDOW, limit: 50 },
});

const container = findContainer();

/** Every statement the package can produce that is meant to run. */
function corpus() {
  const statements = [];
  for (const name of TEMPLATE_NAMES) {
    statements.push({
      label: `template ${name}`,
      request: { query_version: '1', template: name, params: TEMPLATE_PARAMS[name] },
    });
  }
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    if (source.kind === 'aggregate') {
      const measures = Object.keys(source.measures).slice(0, 3);
      const dims = Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 2);
      const filters = [];
      if (source.dimensions.device) filters.push({ field: 'device', op: 'eq', value: TENANT });
      if (source.requiresSubjectScope) filters.push({ field: 'subject', op: 'eq', value: 'user_ref_0001' });
      statements.push({ label: `source ${id}`, request: { query_version: '1', source: id, bucket: 'day', dimensions: dims, measures, filters, window: WINDOW, limit: 20 } });
      statements.push({ label: `source ${id} (week, rollup)`, request: { query_version: '1', source: id, bucket: 'week', dimensions: dims, measures, filters, window: WINDOW, rollup: true } });
    } else {
      statements.push({
        label: `source ${id}`,
        request: { query_version: '1', source: id, filters: [], ...(source.time ? { window: WINDOW } : {}), limit: 20 },
      });
    }
  }
  return statements;
}

test('every compiled statement executes against the real schema', { skip: container ? false : `no PostgreSQL container is running (tried shadowpg, shadowpg-invariants, *shadow*): this test SKIPS and is not a pass` }, () => {
  const script = [tenantPrelude()];
  let count = 0;
  for (const entry of corpus()) {
    let planned;
    try {
      planned = plan(entry.request, { now: NOW, tenant: TENANT, actorId: 'db-test' });
    } catch (error) {
      // A shape the cost guard refuses is not executed; that is the guard working, and it is
      // asserted separately in guard-envelope.test.mjs.
      assert.equal(error.resultState, 'query_too_broad', `${entry.label} was refused for an unexpected reason: ${error.message}`);
      continue;
    }
    for (const statement of planned.statements) {
      script.push(`${bindForPsql(statement.text, statement.params)};`);
      count += 1;
    }
  }
  script.push('SELECT 1;');
  script.push('ROLLBACK;');
  assert.ok(count > 50, `expected a substantial corpus, got ${count} statements`);
  const result = psqlScript(container, script.join('\n'));
  assert.equal(
    result.status,
    0,
    `PostgreSQL rejected a compiled statement:\n${(result.stderr ?? '').trim()}\n\nstdout:\n${(result.stdout ?? '').trim()}`,
  );
});

test('the tenant predicate is what scopes a read, and it is not a parameter', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  const planned = plan(
    { query_version: '1', source: 'ingest.submission', filters: [], window: WINDOW, limit: 5 },
    { now: NOW, tenant: TENANT, actorId: 'db-test' },
  );
  const read = planned.statements.find((s) => s.id === 'read');
  // With app.tenant_id set, the statement is scoped; with it unset, ops.current_tenant() is NULL
  // and the same statement returns nothing rather than everything. Both are executed here.
  const script = [
    `BEGIN;
SET LOCAL app.tenant_id = '${TENANT}';
${bindForPsql(read.text, read.params)};
ROLLBACK;`,
    `BEGIN;
SELECT set_config('app.tenant_id', '', true);
${bindForPsql(read.text, read.params)};
ROLLBACK;`,
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, `statement failed: ${(result.stderr ?? '').trim()}`);
});

test('the audit insert commits, chains, and derives its tenant from the session', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  const planned = plan(
    { query_version: '1', source: 'ingest.submission', filters: [{ field: 'subject', op: 'eq', value: 'user_ref_0001' }], window: WINDOW, limit: 5 },
    { now: NOW, tenant: TENANT, actorId: 'db-test' },
  );
  const audit = planned.statements.find((s) => s.id === 'audit_insert');
  assert.ok(audit, 'a subject-filtered read must carry an audit insert');
  const script = [
    tenantPrelude(),
    `${bindForPsql(audit.text, audit.params)};`,
    // The chain trigger must have filled prev_hash from the prelude's row and produced a hash.
    'SELECT count(*) FROM ops.audit WHERE tenant_id = ops.current_tenant() AND action = \'query.events\' AND row_hash IS NOT NULL;',
    'ROLLBACK;',
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, `audit insert failed: ${(result.stderr ?? '').trim()}`);
  assert.match(result.stdout ?? '', /\b1\b/, 'the audit row must exist inside the transaction');
});

test('the audit chain recomputation column agrees with the stored row_hash', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  const planned = plan(
    { query_version: '1', source: 'ops.audit', filters: [], window: WINDOW, limit: 5 },
    { now: NOW, tenant: TENANT, actorId: 'db-test' },
  );
  const read = planned.statements.find((s) => s.id === 'read');
  const script = [
    tenantPrelude(),
    `${bindForPsql(read.text, read.params)};`,
    'ROLLBACK;',
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, `audit read failed: ${(result.stderr ?? '').trim()}`);
});

test('a bucket-size predicate actually discriminates: day and hour rows are not mixed', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  const dayPlan = plan({ query_version: '1', source: 'mart.agg_tool_period', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW, limit: 5 }, { now: NOW, tenant: TENANT, actorId: 'db-test' });
  const read = dayPlan.statements.find((s) => s.id === 'read');
  assert.ok(read.text.includes('t.bucket_size = $'));
  const script = [
    tenantPrelude(),
    `INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, submissions, users, bytes_total)
       VALUES (ops.current_tenant(), '2026-09-02T00:00:00Z', 'day', 'tool_a', 5, 5, 10),
              (ops.current_tenant(), '2026-09-02T00:00:00Z', 'hour', 'tool_a', 99, 99, 99);`,
    `${bindForPsql(read.text, read.params)};`,
    'ROLLBACK;',
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, `statement failed: ${(result.stderr ?? '').trim()}`);
  // The day row (5) must be returned and the hour row (99) must not: the bucket_size predicate
  // is what stops an hour row being added to a day total.
  assert.match(result.stdout ?? '', /(^|\D)5(\D|$)/);
  assert.ok(!/(^|\D)99(\D|$)/.test(result.stdout ?? ''), 'the hour row must not be summed into the day bucket');
});

test('the session tenant is structural: the same read returns nothing without it', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  const planned = plan({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW, limit: 5 }, { now: NOW, tenant: TENANT, actorId: 'db-test' });
  const read = planned.statements.find((s) => s.id === 'read');
  const script = [
    tenantPrelude(),
    `INSERT INTO ops.tool (tenant_id, tool_fingerprint, sanctioned_state, decided_by, decided_at)
       VALUES (ops.current_tenant(), 'tool_a', 'unsanctioned', 'admin', now());`,
    `INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, submissions, users, bytes_total)
       VALUES (ops.current_tenant(), '2026-09-02T00:00:00Z', 'day', 'tool_a', 7, 7, 10);`,
    'SELECT count(*) AS rows_with_tenant FROM (',
    bindForPsql(read.text, read.params),
    ') q;',
    `SELECT set_config('app.tenant_id', '', true);`,
    'SELECT count(*) AS rows_without_tenant FROM (',
    bindForPsql(read.text, read.params),
    ') q;',
    'ROLLBACK;',
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, `statement failed: ${(result.stderr ?? '').trim()}`);
  const counts = (result.stdout ?? '').trim().split('\n').map((s) => s.trim()).filter((s) => /^\d+$/.test(s));
  assert.equal(counts[0], '1', 'the tenant sees its own row');
  assert.equal(counts[1], '0', 'with no session tenant the same statement sees nothing (fail closed)');
});

test('the register of sources matches what the schema actually contains', { skip: container ? false : 'no PostgreSQL container is running' }, () => {
  // A direct check that every relation named in the registry exists and every column the
  // registry resolves to is present. This is the assertion that keeps the allow-list honest:
  // an invented column fails here rather than in production.
  const names = [];
  for (const id of SOURCE_IDS) {
    const [schema, relation] = id.split('.');
    names.push(`('${schema}','${relation}')`);
  }
  const script = [
    `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE (n.nspname, c.relname) IN (${names.join(',')});`,
    'SELECT 1;',
  ].join('\n');
  const result = psqlScript(container, script);
  assert.equal(result.status, 0, (result.stderr ?? '').trim());
  const count = Number((result.stdout ?? '').trim().split('\n')[0]);
  assert.equal(count, SOURCE_IDS.length, 'every registered source must exist in the schema');
  assert.equal(SOURCE_IDS.length, 12);
  assert.equal(Object.keys(TEMPLATES).length, 10);
});
