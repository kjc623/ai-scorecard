// db.test.mjs — the compiled SQL and the read path against a real PostgreSQL with the schema.
//
// Runs only when SAC_TEST_PG_DSN names a database (a postgres:// URL for a role that may SET ROLE
// sac_query, on a scratch database with services/database/schema.sql applied); otherwise every test here
// skips and says so. Each test works inside one transaction under a random tenant and rolls it
// back, so the database is left exactly as it was found. Reads run as `sac_query`, so the grants
// and row-level security the service depends on are what is exercised.

import test, { after, before } from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import pg from 'pg';

import { plan, executePlan } from '../src/plan.js';
import { SOURCES, SOURCE_IDS } from '../src/registry.js';
import { TEMPLATE_NAMES } from '../src/templates.js';
import { TYPES } from '../src/db.js';

const DSN = process.env.SAC_TEST_PG_DSN ?? '';
const SKIP = DSN === ''
  ? 'SAC_TEST_PG_DSN is not set: the live-database tests skip, which is not a pass'
  : (/^postgres(ql)?:\/\//.test(DSN) ? false : 'SAC_TEST_PG_DSN must be a postgres:// URL');

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const NOW = new Date('2026-10-02T12:00:00Z');
const KEY = Buffer.from('d'.repeat(32));

let pool = null;
before(() => {
  if (!SKIP) pool = new pg.Pool({ connectionString: DSN, types: TYPES, max: 2 });
});
after(() => pool?.end());

/**
 * Run `fn(client, tenant)` in a transaction that is always rolled back, with a fresh tenant row
 * and that tenant set on the session. `seed` runs first, as the DSN's role; `fn` runs as sac_query.
 */
async function scratch(seed, fn) {
  const tenant = randomUUID();
  const client = await pool.connect();
  try {
    await client.query('BEGIN');
    await client.query("SELECT set_config('app.tenant_id', $1, true)", [tenant]);
    await client.query(
      "INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode) VALUES ($1, 'query-api-test', 'active', 'eastus', 'm3')",
      [tenant],
    );
    if (seed) await seed(client, tenant);
    await client.query('SET LOCAL ROLE sac_query');
    return await fn(client, tenant);
  } finally {
    await client.query('ROLLBACK');
    client.release();
  }
}

/** executePlan's BEGIN/COMMIT/ROLLBACK as a savepoint, so its work stays inside scratch(). */
function nested(client) {
  return {
    query(text, params) {
      if (text === 'BEGIN') return client.query('SAVEPOINT plan');
      if (text === 'COMMIT') return client.query('RELEASE SAVEPOINT plan');
      if (text === 'ROLLBACK') return client.query('ROLLBACK TO SAVEPOINT plan');
      return client.query(text, params);
    },
  };
}

const DEVICE = '00000000-0000-4000-8000-0000000000d1';

function submissionRow(id, { labels = null, action = 'logged', promptKind = null, receivedAt = '2026-09-02T00:00:00Z' } = {}) {
  return [id, `sha256:${id.replace(/-/g, '').padEnd(64, '0')}`, promptKind, labels === null ? null : JSON.stringify(labels), action, receivedAt];
}

async function seedSubmissions(client, rows) {
  await client.query("INSERT INTO ops.device (tenant_id, device_id, os, managed_state) VALUES (ops.current_tenant(), $1, 'linux', 'managed')", [DEVICE]);
  for (const [id, weakKey, promptKind, labels, action, receivedAt] of rows) {
    await client.query(
      `INSERT INTO ingest.submission
         (tenant_id, submission_id, dedup_weak_key, kind, prompt_kind, device_id, user_ref, tool_fingerprint,
          first_occurred_at, last_occurred_at, received_at, collection_mode, labels, policy_action,
          winning_source, winning_fidelity, observed_routes, content_state, expires_at)
       VALUES (ops.current_tenant(), $1, $2, 'prompt', $3, $4, 'user_ref_0001', 'tool_a',
               $6, $6, $6, 'm3', $5::jsonb, $7, 'proxy.tls', 50, ARRAY['proxy.tls'], 'not_captured', '2026-12-01T00:00:00Z')`,
      [id, weakKey, promptKind, DEVICE, labels, receivedAt, action],
    );
  }
}

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

/** Every request shape the package can plan: the ten templates and each registered source. */
function corpus() {
  const requests = TEMPLATE_NAMES.map((name) => ({ label: `template ${name}`, request: { query_version: '1', template: name, params: TEMPLATE_PARAMS[name] } }));
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    if (source.kind === 'aggregate') {
      const measures = Object.keys(source.measures).slice(0, 3);
      const dimensions = Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 2);
      const filters = [];
      if (source.dimensions.device) filters.push({ field: 'device', op: 'eq', value: DEVICE });
      if (source.requiresSubjectScope) filters.push({ field: 'subject', op: 'eq', value: 'user_ref_0001' });
      requests.push({ label: `source ${id}`, request: { query_version: '1', source: id, bucket: 'day', dimensions, measures, filters, window: WINDOW, limit: 20 } });
      requests.push({ label: `source ${id} (week, rollup)`, request: { query_version: '1', source: id, bucket: 'week', dimensions, measures, filters, window: WINDOW, rollup: true } });
    } else {
      requests.push({ label: `source ${id}`, request: { query_version: '1', source: id, filters: [], ...(source.time ? { window: WINDOW } : {}), limit: 20 } });
    }
  }
  return requests;
}

test('every compiled statement executes as sac_query against the schema', { skip: SKIP }, async () => {
  await scratch(null, async (client, tenant) => {
    const failures = [];
    let count = 0;
    for (const entry of corpus()) {
      let planned;
      try {
        planned = plan(entry.request, { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY });
      } catch (error) {
        // A shape the cost guard refuses is not executed; guard-envelope.test.mjs covers that.
        assert.equal(error.resultState, 'query_too_broad', `${entry.label}: ${error.message}`);
        continue;
      }
      for (const statement of planned.statements) {
        count += 1;
        await client.query('SAVEPOINT s');
        try {
          await client.query(statement.text, statement.params);
          await client.query('RELEASE SAVEPOINT s');
        } catch (error) {
          await client.query('ROLLBACK TO SAVEPOINT s');
          failures.push(`${entry.label} / ${statement.id}: ${error.message}`);
        }
      }
    }
    assert.ok(count > 50, `expected a substantial corpus, got ${count} statements`);
    assert.deepEqual(failures, []);
  });
});

test('every registered source exists in the schema', { skip: SKIP }, async () => {
  const { rows } = await pool.query(
    "SELECT n.nspname || '.' || c.relname AS name FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname || '.' || c.relname = ANY($1::text[])",
    [SOURCE_IDS],
  );
  assert.deepEqual(rows.map((r) => r.name).sort(), [...SOURCE_IDS].sort());
});

test('a read through executePlan returns typed values, pages, and writes its audit row', { skip: SKIP }, async () => {
  const ids = ['00000000-0000-4000-8000-0000000000e1', '00000000-0000-4000-8000-0000000000e2'];
  await scratch(
    (client) => seedSubmissions(client, [submissionRow(ids[0], { receivedAt: '2026-09-02T10:00:00Z' }), submissionRow(ids[1], { receivedAt: '2026-09-02T09:00:00Z' })]),
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const request = { query_version: '1', template: 'q8_activity', params: { window: WINDOW, limit: 1 } };
      const first = await executePlan(plan(request, ctx), { ...ctx, client: nested(client) });
      assert.equal(first.result_state === 'ok' || first.result_state === 'coverage_degraded', true, first.result_state);
      assert.equal(first.data.length, 1);
      assert.equal(first.data[0].submission_id, ids[0]);
      assert.ok(first.data[0].received_at instanceof Date, 'timestamptz arrives as a Date');
      assert.equal(first.data[0].received_at.toISOString(), '2026-09-02T10:00:00.000Z');
      assert.equal(typeof first.data[0].observation_count, 'number', 'an integer column arrives as a number');
      assert.ok(first.audit?.entry_id, 'the subject-level read wrote its audit row');
      assert.ok(first.page.next_cursor, 'a second row means a next page');

      const second = await executePlan(plan({ ...request, params: { ...request.params, cursor: first.page.next_cursor } }, ctx), { ...ctx, client: nested(client) });
      assert.deepEqual(second.data.map((r) => r.submission_id), [ids[1]], 'page two resumes after page one');
      assert.equal(second.page.next_cursor, null);
    },
  );
});

test('row-level security scopes every read to the session tenant', { skip: SKIP }, async () => {
  await scratch(
    (client) => seedSubmissions(client, [submissionRow('00000000-0000-4000-8000-0000000000e1')]),
    async (client, tenant) => {
      const read = plan({ query_version: '1', source: 'ingest.submission', filters: [], window: WINDOW, limit: 5 }, { now: NOW, tenant, actorId: 'db-test' })
        .statements.find((s) => s.id === 'read');
      assert.equal((await client.query(read.text, read.params)).rows.length, 1, 'the tenant sees its own row');
      await client.query("SELECT set_config('app.tenant_id', $1, true)", [randomUUID()]);
      assert.equal((await client.query(read.text, read.params)).rows.length, 0, 'another tenant sees nothing');
      await client.query("SELECT set_config('app.tenant_id', '', true)");
      assert.equal((await client.query(read.text, read.params)).rows.length, 0, 'no tenant sees nothing');
    },
  );
});

test('the class filter matches a labelled event and composes with another filter', { skip: SKIP }, async () => {
  await scratch(
    (client) => seedSubmissions(client, [
      submissionRow('00000000-0000-4000-8000-0000000000e1', { labels: [{ class: 'payment_card', score: 0.9 }], action: 'logged' }),
      submissionRow('00000000-0000-4000-8000-0000000000e2', { labels: [{ class: 'source_code', score: 1 }], action: 'blocked' }),
    ]),
    async (client, tenant) => {
      const count = async (filters) => {
        const read = plan({ query_version: '1', source: 'ingest.submission', filters, window: WINDOW, limit: 50 }, { now: NOW, tenant, actorId: 'db-test' })
          .statements.find((s) => s.id === 'read');
        return (await client.query(read.text, read.params)).rows.length;
      };
      assert.equal(await count([{ field: 'class', op: 'eq', value: 'payment_card' }]), 1);
      assert.equal(await count([{ field: 'class', op: 'eq', value: 'payment_card' }, { field: 'action', op: 'eq', value: 'logged' }]), 1);
      assert.equal(await count([{ field: 'class', op: 'eq', value: 'payment_card' }, { field: 'action', op: 'eq', value: 'blocked' }]), 0);
    },
  );
});

test('prompt_kind_not excludes client_generated and keeps undecided rows', { skip: SKIP }, async () => {
  await scratch(
    (client) => seedSubmissions(client, [
      submissionRow('00000000-0000-4000-8000-0000000000e1', { promptKind: 'client_generated' }),
      submissionRow('00000000-0000-4000-8000-0000000000e2', { promptKind: null }),
      submissionRow('00000000-0000-4000-8000-0000000000e3', { promptKind: 'user' }),
    ]),
    async (client, tenant) => {
      const read = plan({ query_version: '1', template: 'q8_activity', params: { window: WINDOW, prompt_kind_not: 'client_generated', limit: 50 } }, { now: NOW, tenant, actorId: 'db-test' })
        .statements.find((s) => s.id === 'read');
      const kinds = (await client.query(read.text, read.params)).rows.map((r) => r.prompt_kind ?? null).sort();
      assert.deepEqual(kinds, [null, 'user'].sort());
    },
  );
});

test('a bucket-size predicate keeps hour rows out of a day total, and counts arrive as numbers', { skip: SKIP }, async () => {
  await scratch(
    (client) => client.query(
      `INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, submissions, users, bytes_total)
       VALUES (ops.current_tenant(), '2026-09-02T00:00:00Z', 'day', 'tool_a', 5, 5, 10),
              (ops.current_tenant(), '2026-09-02T00:00:00Z', 'hour', 'tool_a', 99, 99, 99)`,
    ),
    async (client, tenant) => {
      const read = plan({ query_version: '1', source: 'mart.agg_tool_period', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW, limit: 5 }, { now: NOW, tenant, actorId: 'db-test' })
        .statements.find((s) => s.id === 'read');
      const { rows } = await client.query(read.text, read.params);
      assert.equal(rows.length, 1);
      assert.strictEqual(rows[0].submissions, 5, 'a summed bigint is the number 5, not the string "5"');
      assert.ok(rows[0].bucket instanceof Date);
    },
  );
});

test('the tool display name resolves a catalogue entry and an unknown fingerprint', { skip: SKIP }, async () => {
  await scratch(
    null,
    async (client) => {
      const { rows: [catalogued] } = await client.query('SELECT tool_fingerprint FROM ref.tool_catalogue LIMIT 1');
      assert.ok(catalogued, 'ref.tool_catalogue is seeded');
      const name = async (fp) => (await client.query('SELECT ops.tool_display_name($1) AS name', [fp])).rows[0].name;
      const { rows: [expected] } = await client.query('SELECT display_name FROM ref.tool_catalogue WHERE tool_fingerprint = $1', [catalogued.tool_fingerprint]);
      assert.equal(await name(catalogued.tool_fingerprint), expected.display_name);
      assert.equal(await name('tls_never_seen_anywhere'), 'Unrecognised tool');
    },
  );
});
