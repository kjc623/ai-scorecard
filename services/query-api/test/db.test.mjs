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

function submissionRow(id, { labels = null, action = 'logged', promptKind = null, receivedAt = '2026-09-02T00:00:00Z', attachments = null } = {}) {
  return [id, `sha256:${id.replace(/-/g, '').padEnd(64, '0')}`, promptKind, labels === null ? null : JSON.stringify(labels), action, receivedAt, attachments];
}

async function seedSubmissions(client, rows) {
  await client.query("INSERT INTO ops.device (tenant_id, device_id, os, managed_state) VALUES (ops.current_tenant(), $1, 'linux', 'managed')", [DEVICE]);
  for (const [id, weakKey, promptKind, labels, action, receivedAt, attachments] of rows) {
    await client.query(
      `INSERT INTO ingest.submission
         (tenant_id, submission_id, dedup_weak_key, kind, prompt_kind, device_id, user_ref, tool_fingerprint,
          first_occurred_at, last_occurred_at, received_at, collection_mode, labels, policy_action,
          winning_source, winning_fidelity, observed_routes, content_state, expires_at, attachment_count)
       VALUES (ops.current_tenant(), $1, $2, 'prompt', $3, $4, 'user_ref_0001', 'tool_a',
               $6, $6, $6, 'm3', $5::jsonb, $7, 'proxy.tls', 50, ARRAY['proxy.tls'], 'not_captured', '2026-12-01T00:00:00Z', $8)`,
      [id, weakKey, promptKind, DEVICE, labels, receivedAt, action, attachments],
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
    if (source.dimensions.tool) {
      // Every form of a tool filter: the fingerprints of the tools named, or their complement.
      const base = requests[requests.length - 1].request;
      const forms = [['eq', 'app:chatgpt_web'], ['in', ['app:chatgpt_web', 'tool_a']], ['ne', 'tool_a'], ['not_in', ['tool_a']]];
      if (source.kind === 'aggregate') forms.push(['starts_with', 'app:']);
      for (const [op, value] of forms) {
        requests.push({ label: `source ${id} (tool ${op})`, request: { ...base, filters: [...base.filters, { field: 'tool', op, value }] } });
      }
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

test('attachments: yes lists the events that described an attachment, with their count', { skip: SKIP }, async () => {
  await scratch(
    (client) => seedSubmissions(client, [
      submissionRow('00000000-0000-4000-8000-0000000000e1', { attachments: 2 }),
      submissionRow('00000000-0000-4000-8000-0000000000e2', { attachments: null }),
    ]),
    async (client, tenant) => {
      const read = plan({ query_version: '1', template: 'q8_activity', params: { window: WINDOW, attachments: 'yes', limit: 50 } }, { now: NOW, tenant, actorId: 'db-test' })
        .statements.find((s) => s.id === 'read');
      const rows = (await client.query(read.text, read.params)).rows;
      assert.deepEqual(rows.map((r) => [r.submission_id, Number(r.attachment_count)]), [['00000000-0000-4000-8000-0000000000e1', 2]]);
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

test('the tool display name resolves a catalogue entry, a site no app claims and an unknown fingerprint', { skip: SKIP }, async () => {
  await scratch(
    null,
    async (client) => {
      const { rows: [catalogued] } = await client.query('SELECT tool_fingerprint FROM ref.tool_catalogue LIMIT 1');
      assert.ok(catalogued, 'ref.tool_catalogue is seeded');
      const name = async (fp) => (await client.query('SELECT ops.tool_display_name($1) AS name', [fp])).rows[0].name;
      const { rows: [expected] } = await client.query('SELECT display_name FROM ref.tool_catalogue WHERE tool_fingerprint = $1', [catalogued.tool_fingerprint]);
      assert.equal(await name(catalogued.tool_fingerprint), expected.display_name);
      assert.equal(await name('tls_never_seen_anywhere'), 'Unrecognised tool');
      assert.equal(await name('host:claude.ai'), 'claude.ai', 'a destination no app claims is shown as its host');
    },
  );
});

test('one tool is one row in every listing, however many fingerprints it was seen under', { skip: SKIP }, async () => {
  // ChatGPT (web) under an extension host hash, its own app id and a site its domain covers, beside
  // a fingerprint no app claims, on two days, in every aggregate that carries a tool.
  const chatgpt = ['tf2:a64fvqypthxcqzcwgpxxeeo4kvhk5onujlq22qafww3ql422pxyq', 'app:chatgpt_web', 'host:dbtest.chatgpt.com'];
  const fingerprints = [...chatgpt, 'tls_never_seen_anywhere'];
  const days = ['2026-09-02', '2026-09-03'];
  const user = `u_${'e'.repeat(32)}`;
  let classCode = null;
  await scratch(
    async (client) => {
      classCode = (await client.query('SELECT class_code FROM ref.data_class ORDER BY class_code LIMIT 1')).rows[0].class_code;
      const team = (await client.query(
        "INSERT INTO ops.team (tenant_id, name, created_by) VALUES (ops.current_tenant(), 'Pilot', 'db-test') RETURNING team_id::text",
      )).rows[0].team_id;
      await client.query(
        "INSERT INTO ops.device (tenant_id, device_id, os, managed_state) VALUES (ops.current_tenant(), $1, 'linux', 'managed')",
        [DEVICE],
      );
      for (const day of days) {
        for (const fingerprint of fingerprints) {
          const args = [day, fingerprint];
          await client.query("INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, submissions, users, bytes_total) VALUES (ops.current_tenant(), $1, 'day', $2, 1, 1, 10)", args);
          await client.query("INSERT INTO mart.agg_tool_user_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref, submissions, bytes_total) VALUES (ops.current_tenant(), $1, 'day', $2, $3, 1, 10)", [...args, user]);
          await client.query("INSERT INTO mart.agg_org_period (tenant_id, bucket_start, bucket_size, department, population, tool_fingerprint, submissions, users) VALUES (ops.current_tenant(), $1, 'day', 'Engineering', 'directory', $2, 1, 1)", args);
          await client.query("INSERT INTO mart.agg_team_period (tenant_id, bucket_start, bucket_size, team_id, tool_fingerprint, submissions, users) VALUES (ops.current_tenant(), $1, 'day', $3, $2, 1, 1)", [...args, team]);
          await client.query("INSERT INTO mart.agg_class_period (tenant_id, bucket_start, bucket_size, class_code, tool_fingerprint, severity, classifier_version, submissions, users) VALUES (ops.current_tenant(), $1, 'day', $3, $2, 'high', 'v1', 1, 1)", [...args, classCode]);
          await client.query("INSERT INTO mart.agg_discovery_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, discovery_type, item, devices, last_seen) VALUES (ops.current_tenant(), $1, 'day', $2, 'app_installed', '', 1, $1::timestamptz + interval '1 hour')", args);
          await client.query(
            `INSERT INTO ingest.submission
               (tenant_id, submission_id, dedup_weak_key, kind, device_id, user_ref, tool_fingerprint,
                first_occurred_at, last_occurred_at, received_at, collection_mode, policy_action,
                winning_source, winning_fidelity, observed_routes, content_state, expires_at)
             VALUES (ops.current_tenant(), gen_random_uuid(), 'sha256:' || md5($1 || $2) || md5($2 || $1), 'prompt', $3, $4, $2,
                     $1::timestamptz + interval '1 hour', $1::timestamptz + interval '1 hour', $1::timestamptz + interval '1 hour',
                     'm3', 'logged', 'proxy.tls', 50, ARRAY['proxy.tls'], 'not_captured', '2026-12-01T00:00:00Z')`,
            [day, fingerprint, DEVICE, user],
          );
        }
      }
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const run = async (request) => executePlan(plan(request, ctx), { ...ctx, client: nested(client) });
      const once = (label, rows, cell, expected = ['app:chatgpt_web', 'tls_never_seen_anywhere']) => {
        const seen = new Set();
        for (const row of rows) {
          const key = JSON.stringify(cell(row));
          assert.ok(!seen.has(key), `${label} shows ${key} twice`);
          seen.add(key);
        }
        const tools = new Set(rows.map((row) => row.tool));
        assert.deepEqual([...tools].sort(), expected, `${label} shows the tools ${[...tools]}`);
        for (const row of rows) if (row.tool === 'app:chatgpt_web' && 'tool_name' in row) assert.equal(row.tool_name, 'ChatGPT (web)', label);
      };

      for (const id of SOURCE_IDS.filter((name) => SOURCES[name].kind === 'aggregate' && SOURCES[name].dimensions.tool)) {
        const source = SOURCES[id];
        const groupings = [['tool'], ...Object.keys(source.dimensions).filter((d) => !['tool', 'bucket', 'subject'].includes(d)).map((d) => ['tool', d])];
        for (const dimensions of groupings) {
          const measure = Object.keys(source.measures)[0];
          const result = await run({ query_version: '1', source: id, bucket: 'day', dimensions, measures: [measure], filters: [], window: WINDOW, limit: 100 });
          const label = `${id} by ${dimensions.join(', ')}`;
          once(label, result.data, (row) => [row.bucket, ...dimensions.map((d) => row[d])]);
          const total = result.data.filter((row) => row.tool === 'app:chatgpt_web').reduce((sum, row) => sum + Number(row[measure]), 0);
          // A sum adds the tool's fingerprints; a distinct count takes the busiest of them each day.
          const expected = source.measures[measure].agg === 'max' ? days.length : measure === 'bytes_total' ? 60 : 6;
          assert.equal(total, expected, `${label}: ChatGPT (web) folds its fingerprints`);
          const filtered = await run({ query_version: '1', source: id, bucket: 'day', dimensions, measures: [measure], filters: [{ field: 'tool', op: 'eq', value: chatgpt[2] }], window: WINDOW, limit: 100 });
          assert.deepEqual([...new Set(filtered.data.map((row) => row.tool))], ['app:chatgpt_web'], `${label}: a fingerprint names its whole tool`);
          assert.equal(filtered.data.length, days.length, `${label}: one row a day`);
        }
      }

      const ranked = await run({ query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, limit: 100 } });
      once('Q1', ranked.data, (row) => [row.bucket, row.tool]);
      const people = await run({ query_version: '1', template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100, tool: chatgpt[0] } });
      once('Q2', people.data, (row) => [row.bucket, row.tool, row.subject], ['app:chatgpt_web']);
      const series = await run({ query_version: '1', template: 'q6_subject_series', params: { window: WINDOW, subject: user } });
      assert.equal(Number(series.meta.extras.subject_tools[0].tools), 2, 'a person used two tools, not four');

      const firstSeen = await run({ query_version: '1', source: 'mart.v_tool_first_seen', filters: [], window: WINDOW, limit: 50 });
      once('first seen', firstSeen.data, (row) => row.tool);
      const events = await run({ query_version: '1', source: 'ingest.submission', filters: [], window: WINDOW, limit: 50 });
      assert.deepEqual([...new Set(events.data.map((row) => row.tool))].sort(), ['app:chatgpt_web', 'tls_never_seen_anywhere'], 'each event names the tool it counts as');
      const named = await run({ query_version: '1', source: 'ingest.submission', filters: [{ field: 'tool', op: 'eq', value: 'app:chatgpt_web' }], window: WINDOW, limit: 50 });
      assert.equal(named.data.length, chatgpt.length * days.length, 'the tool\'s events under every fingerprint');
    },
  );
});

test('teams and their people read under their current names, and people list by name with a case-insensitive prefix', { skip: SKIP }, async () => {
  const ada = `u_${'a'.repeat(32)}`;
  const bob = `u_${'b'.repeat(32)}`;
  let team = null;
  await scratch(
    async (client) => {
      await client.query(
        `INSERT INTO ops.user_dim (tenant_id, user_ref, department, display_name, status)
         VALUES (ops.current_tenant(), $1, 'Engineering', 'Ada Lovelace', 'active'),
                (ops.current_tenant(), $2, 'Sales', NULL, 'active')`,
        [ada, bob],
      );
      team = (await client.query(
        "INSERT INTO ops.team (tenant_id, name, created_by) VALUES (ops.current_tenant(), 'Pilot', 'db-test') RETURNING team_id::text",
      )).rows[0].team_id;
      await client.query("INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by) VALUES (ops.current_tenant(), $1, $2, 'db-test')", [team, ada]);
      for (const user of [ada, bob]) {
        await client.query(
          "INSERT INTO mart.agg_user_period (tenant_id, bucket_start, bucket_size, user_ref, submissions, bytes_total, tools_used) VALUES (ops.current_tenant(), '2026-09-02', 'day', $1, 3, 30, 1)",
          [user],
        );
      }
      await client.query(
        "INSERT INTO mart.agg_team_period (tenant_id, bucket_start, bucket_size, team_id, tool_fingerprint, submissions, users) VALUES (ops.current_tenant(), '2026-09-02', 'day', $1, 'tool_a', 3, 1)",
        [team],
      );
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const teams = await executePlan(plan({ query_version: '1', template: 'q3_team_growth', params: { window: WINDOW, limit: 100 } }, ctx), { ...ctx, client: nested(client) });
      assert.equal(teams.data.length, 1);
      assert.equal(teams.data[0].team, team);
      assert.equal(teams.data[0].team_name, 'Pilot');
      const coverage = teams.meta.extras.team_coverage[0];
      assert.deepEqual([Number(coverage.users_all), Number(coverage.users_in_teams), Number(coverage.teams)], [2, 1, 1]);

      const roster = await executePlan(
        plan({ query_version: '1', source: 'mart.v_team_member_period', dimensions: ['team', 'subject'], measures: ['submissions'], filters: [{ field: 'team', op: 'eq', value: team }], window: WINDOW, limit: 100 }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.deepEqual(roster.data.map((r) => [r.team_name, r.name, Number(r.submissions)]), [['Pilot', 'Ada Lovelace', 3]], 'only the team\'s member, by name');
      assert.ok(roster.audit?.entry_id, 'reading a team\'s people is audited');

      const people = await executePlan(plan({ query_version: '1', source: 'mart.v_person', filters: [], limit: 50 }, ctx), { ...ctx, client: nested(client) });
      assert.deepEqual(people.data.map((p) => p.name), ['Ada Lovelace', bob], 'by name; a person with no name is listed by reference');
      assert.ok(people.audit?.entry_id, 'listing people is audited');
      const found = await executePlan(
        plan({ query_version: '1', source: 'mart.v_person', filters: [{ field: 'name_key', op: 'starts_with', value: 'ada l' }], limit: 50 }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.deepEqual(found.data.map((p) => p.subject), [ada]);
      assert.equal(found.data[0].department, 'Engineering');
    },
  );
});

test('the rules list names the custom labels and tools each rule names, and the matches per rule read beside it', { skip: SKIP }, async () => {
  await scratch(
    async (client) => {
      await client.query(
        `INSERT INTO ops.custom_label (tenant_id, label, display_name, terms, updated_by)
         VALUES (ops.current_tenant(), 'custom_falcon', 'Project Falcon',
                 '{"words":["falcon"],"customer_names":[],"file_extensions":[],"file_names":[]}', 'db-test')`,
      );
      await client.query(
        `INSERT INTO ops.enforcement_rule (tenant_id, position, rule_id, action, match, message, updated_by) VALUES
           (ops.current_tenant(), 0, 'falcon.block', 'block', '{"labels":["custom_falcon","credential"]}', 'No.', 'db-test'),
           (ops.current_tenant(), 1, 'chatgpt.warn', 'warn', '{"tools":["chatgpt_web"]}', 'Careful.', 'db-test')`,
      );
      await client.query(
        `INSERT INTO mart.agg_rule_period (tenant_id, bucket_start, bucket_size, rule_id, matches, blocked, warned, logged) VALUES
           (ops.current_tenant(), '2026-09-02', 'day', 'falcon.block', 3, 3, 0, 0),
           (ops.current_tenant(), '2026-09-03', 'day', 'falcon.block', 2, 1, 0, 1),
           (ops.current_tenant(), '2026-09-03', 'hour', 'falcon.block', 2, 1, 0, 1),
           (ops.current_tenant(), '2026-09-04', 'day', 'removed.rule', 1, 0, 1, 0)`,
      );
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const rules = await executePlan(plan({ query_version: '1', source: 'ops.enforcement_rule', filters: [], limit: 100 }, ctx), { ...ctx, client: nested(client) });
      assert.deepEqual(rules.data.map((r) => [r.position, r.rule, r.action]), [[0, 'falcon.block', 'block'], [1, 'chatgpt.warn', 'warn']]);
      assert.deepEqual(rules.data[0].label_names, { custom_falcon: 'Project Falcon' }, 'a built-in label needs no name');
      assert.deepEqual(rules.data[1].tool_names, { chatgpt_web: 'ChatGPT (web)' });
      assert.ok(!('updated_by' in rules.data[0]), 'who edited a rule is not read');

      const matches = await executePlan(
        plan({ query_version: '1', source: 'mart.agg_rule_period', bucket: 'month', dimensions: ['rule'], measures: ['matches', 'blocked', 'warned', 'logged'], window: WINDOW }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.deepEqual(
        matches.data.map((r) => [r.rule, r.matches, r.blocked, r.warned, r.logged]),
        [['falcon.block', 5, 4, 0, 1], ['removed.rule', 1, 0, 1, 0]],
        'day rows only, summed per rule; a removed rule keeps its count',
      );
    },
  );
});

test('what discovery found reads per tool under all its fingerprints, kind and item, named and placed by the catalogue, beside where the inventory scan stands', { skip: SKIP }, async () => {
  const devices = ['d2', 'd3', 'd4', 'd5'].map((n) => `00000000-0000-4000-8000-0000000000${n}`);
  await scratch(
    async (client) => {
      await client.query(
        `INSERT INTO ops.device (tenant_id, device_id, os, revoked_at, revoked_by, revoked_reason) VALUES
           (ops.current_tenant(), $1, 'windows', NULL, NULL, NULL),
           (ops.current_tenant(), $2, 'windows', NULL, NULL, NULL),
           (ops.current_tenant(), $3, 'windows', NULL, NULL, NULL),
           (ops.current_tenant(), $4, 'windows', now(), 'db-test', 'retired')`,
        devices,
      );
      await client.query(
        `INSERT INTO ops.collector_state (tenant_id, device_id, collector, state, error_code, last_report_at) VALUES
           (ops.current_tenant(), $1, 'inventory_scanner', 'healthy', NULL, now()),
           (ops.current_tenant(), $2, 'inventory_scanner', 'absent', 'disabled_by_policy', now()),
           (ops.current_tenant(), $3, 'inventory_scanner', 'healthy', NULL, now() - interval '3 days')`,
        devices.slice(0, 3),
      );
      await client.query("INSERT INTO ops.tool_sanction (tenant_id, tool_key, sanctioned_state, decided_by, decided_at) VALUES (ops.current_tenant(), 'ollama', 'unsanctioned', 'db-test', now())");
      await client.query(
        `INSERT INTO mart.agg_discovery_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, discovery_type, item, devices, last_seen) VALUES
           (ops.current_tenant(), '2026-08-10', 'day', 'proc_ollama', 'local_model', 'llama3', 1, '2026-08-10T09:00:00Z'),
           (ops.current_tenant(), '2026-08-20', 'day', 'app:ollama', 'local_model', 'llama3', 1, '2026-08-20T09:00:00Z'),
           (ops.current_tenant(), '2026-09-02', 'day', 'app:ollama', 'local_model', 'llama3', 3, '2026-09-02T09:00:00Z'),
           (ops.current_tenant(), '2026-09-03', 'day', 'app:ollama', 'local_model', 'llama3', 2, '2026-09-03T10:00:00Z'),
           (ops.current_tenant(), '2026-09-03', 'hour', 'app:ollama', 'local_model', 'llama3', 9, '2026-09-03T10:00:00Z'),
           (ops.current_tenant(), '2026-09-04', 'day', 'proc_ollama', 'local_model', 'llama3', 4, '2026-09-04T12:00:00Z'),
           (ops.current_tenant(), '2026-09-03', 'day', 'exe:0123456789abcdef', 'inference_connection', 'api.example.test', 1, '2026-09-03T11:00:00Z')`,
      );
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const found = await executePlan(
        plan({ query_version: '1', source: 'mart.agg_discovery_period', bucket: 'month', dimensions: ['tool', 'discovery_type', 'item'], measures: ['devices'], window: WINDOW, limit: 100 }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.deepEqual(
        found.data.map((r) => [r.tool, r.discovery_type, r.item, r.devices, r.tool_name, r.category, r.sanctioned_state, r.first_seen, r.last_seen]),
        [
          ['app:ollama', 'local_model', 'llama3', 4, 'Ollama', 'local_runtime', 'unsanctioned', '2026-08-10T00:00:00.000Z', '2026-09-04T12:00:00.000Z'],
          ['exe:0123456789abcdef', 'inference_connection', 'api.example.test', 1, 'Unrecognised tool', null, null, '2026-09-03T00:00:00.000Z', '2026-09-03T11:00:00.000Z'],
        ].map((row) => row.map((v) => (typeof v === 'string' && /^\d{4}-\d\d-\d\dT/.test(v) ? new Date(v) : v))),
        'day rows only, the busiest day per item under either fingerprint; first seen from before the window, under either',
      );
      assert.equal(found.data.some((r) => 'tool_key' in r), false);
      assert.equal(found.meta.measure_semantics.devices, 'distinct_lower_bound');
      const scan = found.meta.extras.inventory_scan[0];
      assert.deepEqual([scan.devices_enrolled, scan.scanned, scan.not_scanned, scan.switched_off].map(Number), [3, 1, 1, 1],
        'a stale scan is not scanned, a scanner the policy switched off is apart, and a revoked device is not counted');
    },
  );
});

test('Q1 counts the people behind the usage once each, by the sanction of their tools, beside the people they are out of', { skip: SKIP }, async () => {
  const [ada, bob, cara, dan] = ['a', 'b', 'c', 'd'].map((c) => `u_${c.repeat(32)}`);
  let sanctioned = null;
  let unsanctioned = null;
  await scratch(
    async (client) => {
      sanctioned = (await client.query(
        'SELECT app_key, array_agg(tool_fingerprint ORDER BY tool_fingerprint) AS fingerprints FROM ref.tool_catalogue GROUP BY app_key HAVING count(*) >= 2 ORDER BY app_key LIMIT 1',
      )).rows[0];
      unsanctioned = (await client.query(
        'SELECT app_key, min(tool_fingerprint) AS fingerprint FROM ref.tool_catalogue WHERE app_key <> $1 GROUP BY app_key ORDER BY app_key LIMIT 1',
        [sanctioned.app_key],
      )).rows[0];
      await client.query(
        `INSERT INTO ops.tool_sanction (tenant_id, tool_key, sanctioned_state, decided_by, decided_at)
         VALUES (ops.current_tenant(), $1, 'sanctioned', 'db-test', now()), (ops.current_tenant(), $2, 'unsanctioned', 'db-test', now())`,
        [sanctioned.app_key, unsanctioned.app_key],
      );
      await client.query(
        `INSERT INTO ops.user_dim (tenant_id, user_ref, status) VALUES
           (ops.current_tenant(), $1, 'active'), (ops.current_tenant(), $2, 'active'),
           (ops.current_tenant(), $3, 'active'), (ops.current_tenant(), $4, 'inactive')`,
        [ada, bob, cara, dan],
      );
      await client.query(
        `INSERT INTO ops.device (tenant_id, device_id, os, last_user_ref, revoked_at, revoked_by, revoked_reason) VALUES
           (ops.current_tenant(), gen_random_uuid(), 'windows', $1, NULL, NULL, NULL),
           (ops.current_tenant(), gen_random_uuid(), 'windows', $2, NULL, NULL, NULL),
           (ops.current_tenant(), gen_random_uuid(), 'windows', $2, NULL, NULL, NULL),
           (ops.current_tenant(), gen_random_uuid(), 'windows', $3, now(), 'db-test', 'lost')`,
        [ada, bob, cara],
      );
      // ada uses only the sanctioned tool, under both its fingerprints; bob uses both tools; cara a
      // tool with no decision; dan only before the window.
      const [first, second] = sanctioned.fingerprints;
      for (const [day, fingerprint, user] of [
        ['2026-09-02', first, ada], ['2026-09-03', second, ada], ['2026-09-03', first, bob],
        ['2026-09-03', unsanctioned.fingerprint, bob], ['2026-09-05', 'tls_never_seen_anywhere', cara],
        ['2026-08-20', first, dan], ['2026-09-20', unsanctioned.fingerprint, dan],
      ]) {
        await client.query(
          "INSERT INTO mart.agg_tool_user_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref, submissions, bytes_total) VALUES (ops.current_tenant(), $1, 'day', $2, $3, 1, 10)",
          [day, fingerprint, user],
        );
      }
      for (const [day, fingerprint, submissions] of [
        ['2026-08-20', first, 1], ['2026-09-02', first, 1], ['2026-09-03', second, 1], ['2026-09-03', first, 1],
        ['2026-09-03', unsanctioned.fingerprint, 1], ['2026-09-20', unsanctioned.fingerprint, 1],
        ['2026-09-05', 'tls_never_seen_anywhere', 1], ['2026-09-04', 'tls_only_detected', 0],
      ]) {
        await client.query(
          "INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, submissions, users, bytes_total, detections) VALUES (ops.current_tenant(), $1, 'day', $2, $3, $3, 10, 1)",
          [day, fingerprint, submissions],
        );
      }
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const tools = await executePlan(plan({ query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, limit: 100 } }, ctx), { ...ctx, client: nested(client) });
      const people = tools.meta.extras.people[0];
      assert.deepEqual(
        ['people', 'people_sanctioned_only', 'people_both', 'people_unsanctioned_only', 'directory_people', 'device_people'].map((k) => Number(people[k])),
        [3, 1, 1, 1, 3, 2],
        'a tool with no decision is not sanctioned; an inactive person and a revoked device are not counted',
      );
      const one = await executePlan(
        plan({ query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, limit: 100, tool: unsanctioned.fingerprint } }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.equal(one.meta.extras?.people, undefined, 'one tool\'s read does not count everyone');

      const firstSeen = await executePlan(plan({ query_version: '1', source: 'mart.v_tool_first_seen', filters: [], window: WINDOW, limit: 50 }, ctx), { ...ctx, client: nested(client) });
      assert.deepEqual(
        firstSeen.data.map((r) => [r.first_seen.toISOString?.() ?? r.first_seen, r.tool, r.sanctioned_state, Number(r.people)]),
        [
          ['2026-09-05T00:00:00.000Z', 'tls_never_seen_anywhere', null, 1],
          ['2026-09-03T00:00:00.000Z', `app:${unsanctioned.app_key}`, 'unsanctioned', 2],
        ],
        'a tool first seen before the window is not new however new its fingerprint; a detection is not a prompt; its people include those since the window',
      );
    },
  );
});

test('teams crossed with tools carry the people per team for each tool not sanctioned, a tool once across its fingerprints', { skip: SKIP }, async () => {
  const [ada, bob, cara, dan, eve] = ['a', 'b', 'c', 'd', 'e'].map((c) => `u_${c.repeat(32)}`);
  await scratch(
    async (client) => {
      await client.query(
        `INSERT INTO ops.tool_sanction (tenant_id, tool_key, sanctioned_state, decided_by, decided_at) VALUES
           (ops.current_tenant(), 'claude_code', 'unsanctioned', 'db-test', now()),
           (ops.current_tenant(), 'cursor', 'sanctioned', 'db-test', now())`,
      );
      const teams = {};
      for (const name of ['Platform', 'Data', 'Quiet']) {
        teams[name] = (await client.query("INSERT INTO ops.team (tenant_id, name, created_by) VALUES (ops.current_tenant(), $1, 'db-test') RETURNING team_id", [name])).rows[0].team_id;
      }
      for (const [team, user] of [['Platform', ada], ['Platform', bob], ['Data', bob], ['Data', cara], ['Quiet', dan]]) {
        await client.query("INSERT INTO ops.team_member (tenant_id, team_id, user_ref, added_by) VALUES (ops.current_tenant(), $1, $2, 'db-test')", [teams[team], user]);
      }
      // ada uses Claude Code under both its fingerprints; bob uses it and the sanctioned Cursor;
      // cara a tool outside the catalogue; eve, in no team, Claude Code; dan only before the window.
      for (const [day, fingerprint, user, submissions] of [
        ['2026-09-02', 'tls_b6681b043244c43f', ada, 2], ['2026-09-03', 'app:claude_code', ada, 1],
        ['2026-09-03', 'tls_b6681b043244c43f', bob, 5], ['2026-09-03', 'app:cursor', bob, 9],
        ['2026-09-05', 'tls_never_seen_anywhere', cara, 1], ['2026-09-04', 'app:claude_code', eve, 1],
        ['2026-08-20', 'tls_b6681b043244c43f', dan, 4],
      ]) {
        await client.query(
          "INSERT INTO mart.agg_tool_user_period (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref, submissions, bytes_total) VALUES (ops.current_tenant(), $1, 'day', $2, $3, $4, 10)",
          [day, fingerprint, user, submissions],
        );
      }
    },
    async (client, tenant) => {
      const ctx = { now: NOW, tenant, actorId: 'db-test', cursorKey: KEY };
      const read = await executePlan(
        plan({ query_version: '1', source: 'mart.agg_team_period', bucket: 'month', dimensions: ['team', 'tool'], measures: ['submissions'], window: WINDOW, limit: 100 }, ctx),
        { ...ctx, client: nested(client) },
      );
      const rows = read.meta.extras.demand.map((r) => [r.team_name, r.tool, r.tool_name, r.sanctioned_state,
        r.team_size === null ? null : Number(r.team_size), Number(r.people), r.people_in_teams === null ? null : Number(r.people_in_teams)]);
      const order = (a, b) => String(a[0]).localeCompare(String(b[0])) || a[1].localeCompare(b[1]);
      assert.deepEqual(rows.sort(order), [
        ['Data', 'app:claude_code', 'Claude Code', 'unsanctioned', 2, 1, null],
        ['Data', 'tls_never_seen_anywhere', 'Unrecognised tool', 'unknown', 2, 1, null],
        ['Platform', 'app:claude_code', 'Claude Code', 'unsanctioned', 2, 2, null],
        [null, 'app:claude_code', 'Claude Code', 'unsanctioned', null, 3, 2],
        [null, 'tls_never_seen_anywhere', 'Unrecognised tool', 'unknown', null, 1, 1],
      ].sort(order), 'a person counts once per tool across its fingerprints; a sanctioned tool and use before the window are left out');
      assert.equal(Number(read.meta.extras.team_coverage[0].teams), 3);

      const teamsOnly = await executePlan(
        plan({ query_version: '1', source: 'mart.agg_team_period', bucket: 'month', dimensions: ['team'], measures: ['submissions'], window: WINDOW, limit: 100 }, ctx),
        { ...ctx, client: nested(client) },
      );
      assert.equal(teamsOnly.meta.extras?.demand, undefined, 'a read of teams alone does not count the demand');
    },
  );
});
