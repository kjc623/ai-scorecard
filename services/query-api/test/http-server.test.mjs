// http-server.test.mjs — the HTTP surface, driven by real ES256 tokens through the real verifier.
//
// The database is a recording double with the shape of a pg pool: connect() hands out a client
// whose query() sees every statement, BEGIN, COMMIT and ROLLBACK included, so each test can assert
// what reached the database and in what order.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createQueryServer, createGate, PATHS, MAX_BODY_BYTES } from '../src/http/server.js';
import { loadConfig } from '../src/http/config.js';
import { createVerifier } from '../src/http/auth.js';
import { createContentForwarder, CONTENT_PATHS } from '../src/http/content.js';
import { FINDING_FOR_REVIEW_SQL, UPSERT_REVIEW_SQL } from '../src/review.js';
import { createTestIssuer, TENANT } from './helpers.mjs';

const SILENT = Object.freeze({ info() {}, warn() {}, error() {} });
const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const AGGREGATE = { query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW };
const EVENTS = { query_version: '1', template: 'q8_activity', params: { window: WINDOW } };
const DEVICES = { query_version: '1', template: 'q7_devices', params: {} };
const AUDIT = { query_version: '1', template: 'q10_audit_trail', params: { window: WINDOW } };
const REVIEW_SUBMISSION = '80385a3a-ed3d-4950-bd21-3606c6f97fc5';
const REVIEW_BODY = { submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'confirmed' };
const RETRIEVAL_EVENT = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const HIT = '06f2b95e-def2-42ea-824b-926d74b59b97';

/** Canned rows for the statements the writes and the hit lookup issue, by their exact text. */
function defaultAnswer(text) {
  if (text === FINDING_FOR_REVIEW_SQL) return [{ user_ref: 'u_4f21' }];
  if (text === UPSERT_REVIEW_SQL) return [{ review_state: 'confirmed', reviewed_by: 'reader@lab.test', reviewed_at: new Date('2026-10-05T01:00:00Z') }];
  if (text.startsWith('INSERT INTO ops.audit')) return [{ audit_seq: 42, occurred_at: new Date('2026-10-05T01:00:01Z') }];
  return [];
}

/**
 * A pool double. `fail` throws a driver error (with `code`) from the first statement whose text
 * includes `fail.on`; `connectError` makes connect() itself fail.
 */
function fakePool({ answer = defaultAnswer, fail = null, connectError = null, hold = null } = {}) {
  const calls = { query: [], connected: 0, released: 0 };
  const client = {
    async query(text, params) {
      calls.query.push({ text, params });
      if (hold) await hold(text);
      if (fail && text.includes(fail.on)) {
        const error = new Error('simulated driver failure');
        error.code = fail.code;
        throw error;
      }
      return { rows: answer(text, params) ?? [] };
    },
    release() {
      calls.released += 1;
    },
  };
  return {
    calls,
    async connect() {
      if (connectError) throw connectError;
      calls.connected += 1;
      return client;
    },
    async query(text) {
      if (connectError) throw connectError;
      return { rows: text === 'SELECT 1' ? [{ '?column?': 1 }] : [] };
    },
    async end() {},
  };
}

/** A vault double that records what it was sent and answers both content routes. */
function vaultDouble(searchHits = []) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    const path = new URL(url).pathname;
    calls.push({ path, headers: { ...init.headers }, body: JSON.parse(init.body) });
    const json = path === '/v1/content-search'
      ? { state: 'available', hits: searchHits, truncated: false }
      : { state: 'available', grant_id: 'g-1', raw_digest: 'sha256:x', expires_at: '2026-10-05T00:05:00Z', retrieval_url: `/v1/content/retrieval/${TENANT}/g-1` };
    return { status: 200, json: async () => json };
  };
  return { calls, forwarder: createContentForwarder({ vaultUrl: 'http://vault.internal:8080', fetchImpl, log: null }) };
}

/** Start a server on an ephemeral port with a test issuer, and return helpers bound to it. */
async function withServer(t, { pool = fakePool(), vault = vaultDouble(), gate } = {}) {
  const issuer = createTestIssuer();
  const cfg = loadConfig({
    SAC_PG_HOST: 'db.test',
    SAC_PG_DATABASE: 'shadow',
    SAC_PG_USER: 'query-api',
    SAC_PG_PASSWORD: 'test-only',
    SAC_PG_SSLMODE: 'disable',
    SAC_CONTENT_VAULT_URL: 'http://vault.internal:8080',
    SAC_CURSOR_KEY: 'c'.repeat(32),
    SAC_AUTH_ISSUER: issuer.issuer,
  });
  const verifier = createVerifier({ issuer: issuer.issuer, fetchImpl: issuer.fetchImpl });
  const service = createQueryServer({ cfg, pool, verifier, contentForwarder: vault.forwarder, log: SILENT, ...(gate ? { gate } : {}) });
  const address = await service.listen({ host: '127.0.0.1', port: 0 });
  const base = `http://127.0.0.1:${address.port}`;
  t.after(() => service.close());
  const bearer = (roles = ['content_reader'], claims = {}) => ({ authorization: `Bearer ${issuer.mint({ roles, ...claims })}` });
  const call = (path, body, headers = bearer()) => fetch(`${base}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...headers },
    body: typeof body === 'string' ? body : JSON.stringify(body),
  });
  return { base, pool, vault, issuer, bearer, call };
}

// ---------------------------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------------------------

test('liveness answers 200 without touching the database', async (t) => {
  const { base, pool } = await withServer(t, { pool: fakePool({ connectError: new Error('down') }) });
  const res = await fetch(`${base}${PATHS.LIVENESS}`);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { status: 'ok' });
  assert.equal(pool.calls.connected, 0);
});

test('readiness is one database round trip: 200 when it answers, 503 without the reason when not', async (t) => {
  const up = await withServer(t);
  const ready = await fetch(`${up.base}${PATHS.READINESS}`);
  assert.equal(ready.status, 200);
  assert.deepEqual(await ready.json(), { status: 'ready' });

  const down = await withServer(t, { pool: fakePool({ connectError: new Error('connect ECONNREFUSED db.internal.example:5432') }) });
  const res = await fetch(`${down.base}${PATHS.READINESS}`);
  assert.equal(res.status, 503);
  const text = await res.text();
  assert.deepEqual(JSON.parse(text), { status: 'not-ready' });
  assert.ok(!text.includes('db.internal'), 'a driver error never reaches the probe body');
});

// ---------------------------------------------------------------------------------------------
// Who is asking
// ---------------------------------------------------------------------------------------------

test('a read without a bearer token is refused 401 with the challenge, and touches nothing', async (t) => {
  const { call, pool } = await withServer(t);
  const res = await call(PATHS.QUERY, AGGREGATE, {});
  assert.equal(res.status, 401);
  assert.equal(res.headers.get('www-authenticate'), 'Bearer realm="sac-query"');
  const body = await res.json();
  assert.equal(body.result_state, 'unauthorised_role');
  assert.equal(body.error.code, 'unauthenticated');
  assert.equal(pool.calls.connected, 0);
});

test('a refused token is 401 invalid_token, and the development headers are not a session', async (t) => {
  const { call, issuer, pool } = await withServer(t);
  const expired = await call(PATHS.QUERY, AGGREGATE, { authorization: `Bearer ${issuer.mint({ exp: 1, iat: 0 })}` });
  assert.equal(expired.status, 401);
  assert.match(expired.headers.get('www-authenticate') ?? '', /error="invalid_token"/);
  const dev = await call(PATHS.QUERY, AGGREGATE, { 'x-sac-dev-tenant': TENANT, 'x-sac-dev-role': 'admin' });
  assert.equal(dev.status, 401);
  assert.equal(pool.calls.connected, 0);
});

test('a read runs in one transaction scoped to the token tenant, with the class timeout', async (t) => {
  const { call, bearer, pool } = await withServer(t);
  const answered = await call(PATHS.QUERY, AGGREGATE, bearer(['viewer']));
  assert.equal(answered.status, 200);
  const texts = pool.calls.query.map((c) => c.text);
  assert.equal(texts[0], 'BEGIN');
  assert.match(texts[1], /^SELECT set_config\('app\.tenant_id', \$1, true\)/);
  assert.deepEqual(pool.calls.query[1].params, [TENANT, '3000ms', '1500ms']);
  assert.equal(texts.at(-1), 'COMMIT');
  assert.ok(texts.every((text) => !text.includes(TENANT)), 'the tenant is bound, never interpolated');
  assert.equal(pool.calls.released, 1, 'the connection goes back to the pool');
});

test('a tenant in any request body is refused as a validation error, not honoured', async (t) => {
  const { call, bearer, pool } = await withServer(t);
  for (const [path, body, roles] of [
    [PATHS.QUERY, { ...AGGREGATE, tenant_id: TENANT }, ['viewer']],
    [PATHS.FINDING_REVIEW, { ...REVIEW_BODY, tenant_id: TENANT }, ['analyst']],
  ]) {
    const res = await call(path, body, bearer(roles));
    assert.equal(res.status, 400, path);
    assert.equal((await res.json()).error.code, 'tenant_in_request');
  }
  assert.equal(pool.calls.connected, 0, 'a rejected body never reaches the database');
});

// ---------------------------------------------------------------------------------------------
// Reading the request
// ---------------------------------------------------------------------------------------------

test('a body that is not JSON is 400 malformed_document; an oversized one is 413', async (t) => {
  const { call } = await withServer(t);
  const bad = await call(PATHS.QUERY, '{not json');
  assert.equal(bad.status, 400);
  assert.deepEqual((await bad.json()).error.code, 'malformed_document');
  const big = await call(PATHS.QUERY, 'x'.repeat(MAX_BODY_BYTES + 1));
  assert.equal(big.status, 413);
  assert.equal((await big.json()).error.code, 'body_too_large');
});

test('an unknown path is 404, a wrong method is 405, and every answer is no-store', async (t) => {
  const { base, call } = await withServer(t);
  const missing = await fetch(`${base}/v1/nope`);
  assert.equal(missing.status, 404);
  assert.equal((await missing.json()).result_state, 'not_found');
  const wrongMethod = await fetch(`${base}${PATHS.QUERY}`, { method: 'GET' });
  assert.equal(wrongMethod.status, 405);
  assert.equal(wrongMethod.headers.get('allow'), 'GET, POST');
  const res = await call(PATHS.QUERY, {});
  assert.equal(res.headers.get('cache-control'), 'no-store');
  assert.equal(res.headers.get('x-content-type-options'), 'nosniff');
});

test('a pipeline rejection keeps its state, reason and status, and is decided before a connection is taken', async (t) => {
  const { call, pool } = await withServer(t);
  const res = await call(PATHS.QUERY, { query_version: '1', source: 'mart.v_tool_usage', sql: 'SELECT 1' });
  assert.equal(res.status, 400);
  const body = await res.json();
  assert.equal(body.result_state, 'unsupported_query_shape');
  assert.equal(body.error.code, 'prohibited_field');
  assert.equal('data' in body, false);
  assert.equal(pool.calls.connected, 0);
});

// ---------------------------------------------------------------------------------------------
// Database failures
// ---------------------------------------------------------------------------------------------

test('a statement timeout is 429 busy and rolls back; any other driver error is 503 with nothing served', async (t) => {
  const timeout = await withServer(t, { pool: fakePool({ fail: { on: 'FROM mart.v_tool_usage', code: '57014' } }) });
  const busy = await timeout.call(PATHS.QUERY, AGGREGATE, timeout.bearer(['viewer']));
  assert.equal(busy.status, 429);
  const busyBody = await busy.json();
  assert.equal(busyBody.result_state, 'busy');
  assert.equal(busyBody.error.code, 'statement_timeout');
  const texts = timeout.pool.calls.query.map((c) => c.text);
  assert.ok(texts.includes('ROLLBACK') && !texts.includes('COMMIT'));

  const broken = await withServer(t, { pool: fakePool({ fail: { on: 'FROM mart.v_tool_usage', code: 'XX000' } }) });
  const res = await broken.call(PATHS.QUERY, AGGREGATE, broken.bearer(['viewer']));
  assert.equal(res.status, 503);
  const body = await res.json();
  assert.equal(body.result_state, 'audit_unavailable');
  assert.equal(body.error.code, 'read_failed');
  assert.equal('data' in body, false);
});

test('an unreachable database is 503, not a 500 or a hang', async (t) => {
  const { call, bearer } = await withServer(t, { pool: fakePool({ connectError: Object.assign(new Error('token endpoint unreachable'), { code: 'ECONNREFUSED' }) }) });
  const res = await call(PATHS.QUERY, AGGREGATE, bearer(['viewer']));
  assert.equal(res.status, 503);
  assert.equal((await res.json()).result_state, 'audit_unavailable');
});

test('a write whose audit row cannot be written is rolled back and reported as audit_write_failed', async (t) => {
  const { call, bearer, pool } = await withServer(t, { pool: fakePool({ fail: { on: 'INSERT INTO ops.audit', code: '23514' } }) });
  const res = await call(PATHS.FINDING_REVIEW, REVIEW_BODY, bearer(['analyst']));
  assert.equal(res.status, 503);
  assert.equal((await res.json()).error.code, 'audit_write_failed');
  const texts = pool.calls.query.map((c) => c.text);
  assert.ok(texts.includes('ROLLBACK') && !texts.includes('COMMIT'), 'the review is not kept without its audit row');
});

// ---------------------------------------------------------------------------------------------
// The admission gate
// ---------------------------------------------------------------------------------------------

test('the gate admits up to its concurrency limit and rejects past its queue depth with 429', async () => {
  const gate = createGate({ maxConcurrency: 2, maxQueue: 1 });
  await gate.acquire();
  await gate.acquire();
  assert.equal(gate.active, 2);
  const queued = gate.acquire();
  assert.equal(gate.queued, 1);
  await assert.rejects(() => gate.acquire(), (error) => error.status === 429 && error.payload.result_state === 'busy');
  gate.release();
  await queued;
  assert.equal(gate.active, 2, 'the queued waiter took the freed slot');
});

test('a request beyond the gate is answered 429 busy at once, and the one holding the slot is still served', async (t) => {
  let release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  let reached;
  const inGate = new Promise((resolve) => {
    reached = resolve;
  });
  let first = true;
  const pool = fakePool({
    hold: async (text) => {
      if (text === 'BEGIN' && first) {
        first = false;
        reached();
        await held;
      }
    },
  });
  const { call, bearer } = await withServer(t, { pool, gate: createGate({ maxConcurrency: 1, maxQueue: 0 }) });
  const inFlight = call(PATHS.QUERY, AGGREGATE, bearer(['viewer']));
  await inGate;
  const shed = await call(PATHS.QUERY, AGGREGATE, bearer(['viewer']));
  assert.equal(shed.status, 429);
  assert.equal((await shed.json()).result_state, 'busy');
  release();
  assert.equal((await inFlight).status, 200);
});

// ---------------------------------------------------------------------------------------------
// Search hits say who, where and which tool
// ---------------------------------------------------------------------------------------------

test('a search hit carries the person, the device and the tool of its submission', async (t) => {
  const row = { submission_id: HIT, user_ref: 'u_4f21', subject_name: 'alice@example', directory_name: 'Alice Smith', tool: 'claude_code', tool_name: 'Claude Code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: 'LAPTOP-7' };
  const pool = fakePool({ answer: (text) => (text.includes('AS subject_name') ? [row] : []) });
  const vault = vaultDouble([{ submission_id: HIT, snippet: 'the <em>capital</em>', rank: 1 }, { submission_id: 'not-a-uuid', snippet: 'x', rank: 0 }]);
  const { call, bearer } = await withServer(t, { pool, vault });
  const res = await call(CONTENT_PATHS.SEARCH, { query: 'capital' }, bearer(['analyst']));
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.deepEqual(body.hits[0], { submission_id: HIT, snippet: 'the <em>capital</em>', rank: 1, subject: 'alice@example', directory_name: 'Alice Smith', tool: 'claude_code', tool_name: 'Claude Code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: 'LAPTOP-7' });
  assert.deepEqual(body.hits[1], { submission_id: 'not-a-uuid', snippet: 'x', rank: 0 }, 'a hit with no submission row is served as the vault sent it');
  const lookup = pool.calls.query.find((q) => q.text.includes('AS subject_name'));
  assert.deepEqual(lookup.params, [TENANT, [HIT]], 'the lookup is tenant-scoped and its ids are a bound parameter');
  assert.equal(pool.calls.query[0].text, 'BEGIN', 'inside a tenant-scoped transaction');
});

test('a search hit falls back to the pseudonymous reference when there is no clear name', async (t) => {
  const pool = fakePool({ answer: (text) => (text.includes('AS subject_name') ? [{ submission_id: HIT, user_ref: 'u_4f21', subject_name: null, tool: 'claude_code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: null }] : []) });
  const { call, bearer } = await withServer(t, { pool, vault: vaultDouble([{ submission_id: HIT, snippet: 's', rank: 1 }]) });
  const body = await (await call(CONTENT_PATHS.SEARCH, { query: 's' }, bearer(['analyst']))).json();
  assert.equal(body.hits[0].subject, 'u_4f21');
  assert.equal(body.hits[0].hostname, null);
  assert.equal(body.hits[0].directory_name, null);
});

test('a search still answers when its hits cannot be described', async (t) => {
  const { call, bearer } = await withServer(t, { pool: fakePool({ fail: { on: 'AS subject_name', code: 'XX000' } }), vault: vaultDouble([{ submission_id: HIT, snippet: 's', rank: 1 }]) });
  const res = await call(CONTENT_PATHS.SEARCH, { query: 's' }, bearer(['analyst']));
  assert.equal(res.status, 200);
  assert.deepEqual((await res.json()).hits, [{ submission_id: HIT, snippet: 's', rank: 1 }]);
});

test("a content request forwards the caller's own bearer token, and nothing else that names them", async (t) => {
  const { call, issuer, vault } = await withServer(t);
  const token = issuer.mint({ roles: ['content_reader'], actor: 'reader@lab.test' });
  assert.equal((await call(CONTENT_PATHS.SEARCH, { query: 'capital' }, { authorization: `Bearer ${token}` })).status, 200);
  assert.equal((await call(CONTENT_PATHS.RETRIEVAL, { event_ids: [RETRIEVAL_EVENT] }, { authorization: `Bearer ${token}` })).status, 200);
  assert.equal(vault.calls.length, 2);
  for (const sent of vault.calls) {
    assert.deepEqual(sent.headers, { 'content-type': 'application/json', authorization: `Bearer ${token}` }, sent.path);
  }
});

// ---------------------------------------------------------------------------------------------
// The two audited writes
// ---------------------------------------------------------------------------------------------

test('a review is stored and audited in one transaction, with the actor from the token', async (t) => {
  const { call, bearer, pool } = await withServer(t);
  const res = await call(PATHS.FINDING_REVIEW, { ...REVIEW_BODY, note: 'real card number' }, bearer(['analyst']));
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.data.review_state, 'confirmed');
  assert.equal(body.audit.entry_id, '42', 'the audit entry id makes the write traceable');
  const upsert = pool.calls.query.find((c) => c.text === UPSERT_REVIEW_SQL);
  assert.deepEqual(upsert.params, [REVIEW_SUBMISSION, 'PAYMENT_CARD_PAN', 'confirmed', 'reader@lab.test', 'real card number']);
  const texts = pool.calls.query.map((c) => c.text);
  assert.ok(texts.indexOf(UPSERT_REVIEW_SQL) < texts.findIndex((x) => x.startsWith('INSERT INTO ops.audit')));
  assert.equal(texts.at(-1), 'COMMIT', 'review and audit commit together');
});

test('reviewing a finding that does not exist is 404 and writes nothing; open is not an action', async (t) => {
  const { call, bearer, pool } = await withServer(t, { pool: fakePool({ answer: (text) => (text === FINDING_FOR_REVIEW_SQL ? [] : defaultAnswer(text)) }) });
  const missing = await call(PATHS.FINDING_REVIEW, REVIEW_BODY, bearer(['analyst']));
  assert.equal(missing.status, 404);
  assert.equal((await missing.json()).error.code, 'no_such_finding');
  assert.ok(!pool.calls.query.some((c) => c.text === UPSERT_REVIEW_SQL || c.text.startsWith('INSERT INTO ops.audit')));
  const open = await call(PATHS.FINDING_REVIEW, { ...REVIEW_BODY, review_state: 'open' }, bearer(['analyst']));
  assert.equal(open.status, 400);
  assert.equal((await open.json()).error.code, 'type_mismatch');
});

test("the token's sid is written into the audit row of a read and of a write", async (t) => {
  const { call, bearer, pool } = await withServer(t);
  const analyst = bearer(['analyst'], { sid: 'ABCDEF0123456789' });
  assert.equal((await call(PATHS.QUERY, EVENTS, analyst)).status, 200);
  assert.equal((await call(PATHS.FINDING_REVIEW, REVIEW_BODY, analyst)).status, 200);
  const audits = pool.calls.query.filter((c) => c.text.startsWith('INSERT INTO ops.audit'));
  assert.equal(audits.length, 2);
  for (const audit of audits) {
    assert.equal(audit.params[1], 'reader@lab.test');
    assert.equal(JSON.parse(audit.params[7]).sid, 'abcdef0123456789');
  }
});

// ---------------------------------------------------------------------------------------------
// Role boundaries
// ---------------------------------------------------------------------------------------------

/** Every route class, the body that reaches its role gate, and the roles that may pass it. */
const ROUTE_CLASSES = [
  { name: 'aggregate read', path: PATHS.QUERY, body: AGGREGATE, allowed: ['viewer', 'analyst', 'content_reader', 'admin'] },
  { name: 'device read', path: PATHS.QUERY, body: DEVICES, allowed: ['viewer', 'analyst', 'content_reader', 'admin'] },
  { name: 'subject-level read', path: PATHS.QUERY, body: EVENTS, allowed: ['analyst', 'content_reader', 'admin'] },
  { name: 'audit trail read', path: PATHS.QUERY, body: AUDIT, allowed: ['admin'] },
  { name: 'finding review write', path: PATHS.FINDING_REVIEW, body: REVIEW_BODY, allowed: ['analyst', 'content_reader', 'admin'] },
  { name: 'prompt-text search', path: CONTENT_PATHS.SEARCH, body: { query: 'capital' }, allowed: ['analyst', 'content_reader', 'admin'] },
  { name: 'content retrieval mint', path: CONTENT_PATHS.RETRIEVAL, body: { event_ids: [RETRIEVAL_EVENT] }, allowed: ['content_reader', 'admin'] },
];

test('every role boundary holds on every route class', async (t) => {
  const { call, bearer } = await withServer(t);
  for (const role of ['viewer', 'analyst', 'content_reader', 'admin']) {
    for (const route of ROUTE_CLASSES) {
      const res = await call(route.path, route.body, bearer([role]));
      const body = await res.json();
      if (route.allowed.includes(role)) {
        assert.equal(res.status, 200, `${role} must be served the ${route.name} (got ${JSON.stringify(body.error)})`);
      } else {
        assert.equal(res.status, 403, `${role} must be refused the ${route.name}`);
        assert.equal(body.error?.code, 'role');
      }
    }
  }
});

test('a token carrying several roles has the union of their reach', async (t) => {
  const { call, bearer } = await withServer(t);
  const both = bearer(['viewer', 'analyst']);
  assert.equal((await call(PATHS.QUERY, DEVICES, both)).status, 200, 'device list via viewer');
  assert.equal((await call(PATHS.QUERY, EVENTS, both)).status, 200, 'subject-level read via analyst');
  assert.equal((await call(PATHS.QUERY, AUDIT, both)).status, 403, 'neither carries the audit trail');
});

test('without a valid session every route class answers 401 and touches nothing', async (t) => {
  const { call, issuer, pool, vault } = await withServer(t);
  const presented = [
    {},
    { authorization: `Bearer ${issuer.mint({ iss: 'http://elsewhere' })}` },
    { authorization: `Bearer ${issuer.mint({ roles: ['superuser'] })}` },
    { 'x-sac-dev-tenant': TENANT, 'x-sac-dev-role': 'admin' },
  ];
  for (const headers of presented) {
    for (const route of ROUTE_CLASSES) {
      const res = await call(route.path, route.body, headers);
      assert.equal(res.status, 401, `${route.name} with ${Object.keys(headers).join(',') || 'nothing'}`);
      assert.equal((await res.json()).error.code, 'unauthenticated');
      assert.ok(res.headers.get('www-authenticate')?.startsWith('Bearer'));
    }
  }
  assert.equal(pool.calls.connected, 0, 'no unauthenticated request reached the database');
  assert.equal(vault.calls.length, 0, 'or the vault');
});
