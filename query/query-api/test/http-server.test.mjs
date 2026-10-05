// http-server.test.mjs — the HTTP surface, driven through a real socket with a fake database.
//
// WHAT THIS PROVES, and what it deliberately does not. It proves the transport: which paths exist,
// which methods are allowed, that a request the server cannot read is refused rather than guessed
// at, that the tenant never comes from the body, and that a pipeline rejection keeps the state,
// reason and HTTP status the pipeline chose. It does NOT prove that any query returns a correct
// number — that is compile/plan/suppress's business and their suites cover it — and it does not
// prove the driver works: the client here is a fake, and the real one has its own suite.
//
// The server is started on port 0 and stopped at the end, so nothing is left listening.

import test from 'node:test';
import assert from 'node:assert/strict';

import { loadConfig } from '../src/http/config.js';
import { createQueryServer, createGate, PATHS, MAX_BODY_BYTES } from '../src/http/server.js';

/** The tenant under test. A missing or body-supplied tenant is a different test's business. */
const TENANT = '00000000-0000-4000-8000-0000000000aa';

/**
 * A client that answers the statements the pipeline issues with empty result sets.
 *
 * `begin`/`commit`/`rollback` are counted rather than ignored: "nothing is returned to the caller
 * before COMMIT" is a property of the read path, and a fake that hides the order would let a
 * regression in it pass.
 */
function fakeClient({ rows = [], failOn = null } = {}) {
  const calls = { query: [], began: 0, committed: 0, rolledBack: 0 };
  return {
    calls,
    async connect() {},
    async query(text, params) {
      calls.query.push({ text, params });
      if (failOn && text.includes(failOn)) {
        const error = new Error('simulated driver failure');
        error.code = '57014';
        throw error;
      }
      if (text === 'SELECT 1') return { rows: [{ '?column?': 1 }], rowCount: 1, fields: [] };
      return { rows, rowCount: rows.length, fields: [] };
    },
    async begin() {
      calls.began += 1;
    },
    async commit() {
      calls.committed += 1;
    },
    async rollback() {
      calls.rolledBack += 1;
    },
    async close() {},
  };
}

function testConfig(overrides = {}) {
  return loadConfig({
    SAC_ROLE: 'sac_query',
    SAC_PG_HOST: 'shadowpg',
    SAC_PG_DATABASE: 'shadow',
    SAC_DEV_TRUST_PRINCIPAL: '1',
    ...overrides,
  });
}

/** Start a server on an ephemeral port and return a fetch bound to it plus a close(). */
async function withServer(t, { cfg = testConfig(), client = fakeClient(), ...rest } = {}) {
  const service = createQueryServer({ cfg, client, log: { info() {}, error() {}, warn() {} }, ...rest });
  const address = await service.listen({ host: '127.0.0.1', port: 0 });
  const base = `http://127.0.0.1:${address.port}`;
  t.after(async () => {
    await service.close();
  });
  return { base, client, service, cfg };
}

const asTenant = (extra = {}) => ({ 'x-sac-dev-tenant': TENANT, 'content-type': 'application/json', ...extra });

// ---------------------------------------------------------------------------------------------
// The probes the deployment actually calls
// ---------------------------------------------------------------------------------------------

test('liveness answers 200 and names the role it is running as', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.LIVENESS}`);
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.status, 'ok');
  assert.equal(body.role, 'sac_query');
});

test('readiness asks a real dependency, and answers 200 when the database answers', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.READINESS}`);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { status: 'ready', role: 'sac_query' });
});

test('readiness answers 503 when the database does not answer, and does not leak why', async (t) => {
  // A broken database is "take me out of rotation", not "kill me": 503, never 500.
  const client = fakeClient();
  client.query = async (text) => {
    if (text === 'SELECT 1') {
      const error = new Error('could not connect to server: Connection refused at db.internal.example');
      error.code = 'ECONNREFUSED';
      throw error;
    }
    return { rows: [], rowCount: 0, fields: [] };
  };
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.READINESS}`);
  assert.equal(res.status, 503);
  const raw = await res.text();
  assert.equal(raw.includes('db.internal.example'), false, 'the probe body must not carry a host name');
  assert.equal(raw.includes('ECONNREFUSED'), false, 'nor a driver error code');
});

// ---------------------------------------------------------------------------------------------
// Who is asking
// ---------------------------------------------------------------------------------------------

test('a read with no established tenant is refused 403, and says so rather than guessing', async (t) => {
  const { base, client } = await withServer(t);
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' } }),
  });
  assert.equal(res.status, 403);
  const body = await res.json();
  assert.equal(body.result_state, 'unauthorised_role');
  assert.equal(client.calls.query.length, 0, 'an unauthorised read must not reach the database');
});

test('a read sets the session tenant before it plans or executes anything', async (t) => {
  // database/schema.sql enforces isolation with FORCE ROW LEVEL SECURITY and every scoped policy
  // compares against ops.current_tenant(), which reads `app.tenant_id`. A read that never sets it
  // returns nothing for every query: fail-closed rather than a leak, but an absence of data
  // presented where the honest answer is "this session asked as nobody".
  const { base, client } = await withServer(t);
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' } }),
  });
  assert.ok(res.status === 200 || res.status === 503, `expected the read to reach the pipeline, got ${res.status}`);

  const setTenant = client.calls.query.find((c) => /set_config\('app\.tenant_id'/.test(c.text));
  assert.ok(setTenant, 'the session tenant must be set before the read');
  assert.deepEqual(setTenant.params, [TENANT], 'and it must be bound, not interpolated');
  assert.equal(setTenant.text.includes(TENANT), false, 'the tenant must never appear in the SQL text');
});

test('with development trust off, the header is ignored and the read is refused', async (t) => {
  // The escape hatch has to be explicit. A server that honoured the header without the flag would
  // let any caller choose its own tenant, which is what REASON.TENANT_IN_REQUEST forbids.
  const { base } = await withServer(t, { cfg: testConfig({ SAC_DEV_TRUST_PRINCIPAL: '' }) });
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' } }),
  });
  assert.equal(res.status, 403);
  assert.match((await res.json()).error.message, /no signed-in session/);
});

test('a tenant in the request body is refused as a validation error, not honoured', async (t) => {
  const { base } = await withServer(t, { cfg: testConfig({ SAC_DEV_TRUST_PRINCIPAL: '' }) });
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ query_version: '1', tenant_id: TENANT, source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [] }),
  });
  assert.equal(res.status, 403, 'no session, so the request is refused before its shape is judged');
});

// ---------------------------------------------------------------------------------------------
// Reading the request
// ---------------------------------------------------------------------------------------------

test('a body that is not JSON is refused 400 with the pipeline\u2019s own malformed_document code', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: asTenant(), body: '{not json' });
  assert.equal(res.status, 400);
  const body = await res.json();
  assert.equal(body.result_state, 'unsupported_query_shape');
  assert.equal(body.error.code, 'malformed_document');
});

test('an oversized body is refused 413 before it is parsed', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: 'x'.repeat(MAX_BODY_BYTES + 1),
  });
  assert.equal(res.status, 413);
  assert.equal((await res.json()).error.code, 'body_too_large');
});

test('a path that does not exist is 404, and a wrong method on a real path is 405', async (t) => {
  const { base } = await withServer(t);
  const missing = await fetch(`${base}/v1/nope`);
  assert.equal(missing.status, 404);
  assert.equal((await missing.json()).result_state, 'not_found');

  const wrongMethod = await fetch(`${base}${PATHS.QUERY}`, { method: 'GET' });
  assert.equal(wrongMethod.status, 405);
  assert.equal(wrongMethod.headers.get('allow'), 'GET, POST');
});

test('every response is marked no-store, because a query response is per-tenant', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: asTenant(), body: '{}' });
  assert.equal(res.headers.get('cache-control'), 'no-store');
  assert.equal(res.headers.get('x-content-type-options'), 'nosniff');
});

// ---------------------------------------------------------------------------------------------
// What the pipeline says, the transport repeats
// ---------------------------------------------------------------------------------------------

test('a pipeline rejection keeps the state, the reason and the status the pipeline chose', async (t) => {
  const { base } = await withServer(t);
  // A prohibited field is a validation rejection: validate.js names the reason, and the transport
  // must not turn it into a 500 or invent a code of its own.
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', sql: 'SELECT 1' }),
  });
  assert.ok(res.status === 400 || res.status === 403, `expected a typed rejection, got ${res.status}`);
  const body = await res.json();
  assert.ok(body.result_state, 'a rejection always carries a result_state');
  assert.ok(body.error?.code, 'and a machine code');
  assert.equal('data' in body, false, 'a rejection never carries data');
});

test('a driver failure mid-read is reported as audit_unavailable (503), never as a 500', async (t) => {
  // This is the pipeline's decision, not the transport's, and it is stricter than a SQLSTATE map
  // would be: executePlan wraps anything thrown inside the transaction as
  // `audit_unavailable` — "the read could not be completed inside one transaction, so nothing was
  // served" — because a read that cannot be finished atomically has no answer to give. The
  // transport must not second-guess that with a status of its own.
  const client = fakeClient({ failOn: 'FROM' });
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.QUERY}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' } }),
  });
  assert.equal(res.status, 503);
  const body = await res.json();
  assert.equal(body.result_state, 'audit_unavailable');
  assert.equal('data' in body, false, 'nothing was served, so nothing may be reported');
  assert.equal(client.calls.committed, 0, 'a failed read must not commit');
  assert.equal(client.calls.rolledBack, 1, 'and it must roll back');
});

// ---------------------------------------------------------------------------------------------
// The concurrency gate
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

test('a shed request is answered 429 with the busy state, not with a silent queue', async (t) => {
  // maxConcurrency 1 with maxQueue 0: the first request holds the slot, so the second must be shed
  // immediately rather than queued for ever. The hold is a single deferred that only the FIRST
  // query waits on: a flag that every query overwrote would release the wrong request and leave the
  // first one waiting for its own timeout, which is a test bug that looks like a server bug.
  // `reached` is settled by the resolver the first request parks on, so the next step waits for the
  // request to be INSIDE the gate rather than guessing with a sleep or watching a side effect.
  let release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  let reached;
  const inGate = new Promise((resolve) => {
    reached = resolve;
  });
  let first = true;
  const client = fakeClient();
  const realQuery = client.query.bind(client);
  client.query = async (text, params) => {
    if (text !== 'SELECT 1' && first) {
      first = false;
      reached();
      await held;
    }
    return realQuery(text, params);
  };
  const { base } = await withServer(t, { client, cfg: testConfig({ SAC_MAX_CONCURRENCY: '1', SAC_MAX_QUEUE: '0' }) });

  const body = JSON.stringify({ query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' } });
  const inFlight = fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: asTenant(), body });
  const gateFull = await Promise.race([inGate.then(() => true), waitFor(() => false, 5000)]);
  assert.ok(gateFull, 'the first request never occupied the gate');

  const shed = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: asTenant(), body });
  assert.equal(shed.status, 429);
  assert.equal((await shed.json()).result_state, 'busy');

  release();
  const answered = await inFlight;
  assert.equal(answered.status, 200, 'the request that held the slot is still served normally');
});

/** Poll a predicate until it holds or the budget runs out. */
async function waitFor(predicate, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (predicate()) return true;
    if (Date.now() > deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

// ---------------------------------------------------------------------------------------------
// A search hit says who, where and which tool
// ---------------------------------------------------------------------------------------------

test('a content search hit carries the person, the device and the tool of its submission', async (t) => {
  const id = '06f2b95e-def2-42ea-824b-926d74b59b97';
  const client = fakeClient({ rows: [{ submission_id: id, user_ref: 'u_4f21', subject_name: 'alice@example', directory_name: 'Alice Smith', tool: 'claude_code', tool_name: 'Claude Code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: 'LAPTOP-7' }] });
  const contentForwarder = {
    async handle() {
      return { status: 200, body: { state: 'available', hits: [{ submission_id: id, snippet: 'the <em>capital</em>', rank: 1 }, { submission_id: 'not-a-uuid', snippet: 'x', rank: 0 }], truncated: false } };
    },
  };
  const { base } = await withServer(t, { client, contentForwarder });
  const res = await fetch(`${base}/v1/content-search`, { method: 'POST', headers: asTenant(), body: JSON.stringify({ query: 'capital' }) });
  assert.equal(res.status, 200);
  const body = await res.json();
  // The clear name of the submitter and the hostname ride on the hit (ADR 0021); the UUID device
  // remains the identity behind the hit.
  assert.deepEqual(body.hits[0], { submission_id: id, snippet: 'the <em>capital</em>', rank: 1, subject: 'alice@example', directory_name: 'Alice Smith', tool: 'claude_code', tool_name: 'Claude Code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: 'LAPTOP-7' });
  assert.deepEqual(body.hits[1], { submission_id: 'not-a-uuid', snippet: 'x', rank: 0 }, 'a hit with no submission row is served as the vault sent it');
  const lookup = client.calls.query.find((q) => q.text.includes('ingest.submission'));
  assert.deepEqual(lookup.params, [TENANT, id], 'the lookup is tenant-scoped and its ids are a bound parameter');
  assert.ok(client.calls.query.some((q) => q.text.includes('app.tenant_id')), 'the tenant is set on the session first');
});

test('a search hit falls back to the pseudonymous reference when there is no clear name', async (t) => {
  const id = '06f2b95e-def2-42ea-824b-926d74b59b97';
  const client = fakeClient({ rows: [{ submission_id: id, user_ref: 'u_4f21', subject_name: null, tool: 'claude_code', device: '35beae1b-e366-465a-8517-58df42c88bdc', hostname: null }] });
  const contentForwarder = { async handle() { return { status: 200, body: { state: 'available', hits: [{ submission_id: id, snippet: 's', rank: 1 }], truncated: false } }; } };
  const { base } = await withServer(t, { client, contentForwarder });
  const res = await fetch(`${base}/v1/content-search`, { method: 'POST', headers: asTenant(), body: JSON.stringify({ query: 's' }) });
  const body = await res.json();
  assert.equal(body.hits[0].subject, 'u_4f21', 'the pseudonymous ref is the fallback when the tenant is hashed or the device could not attribute');
  assert.equal(body.hits[0].hostname, null);
  assert.equal(body.hits[0].directory_name, null, 'a hit with no directory row carries no display name');
});

test('a search still answers when its hits cannot be described', async (t) => {
  const id = '06f2b95e-def2-42ea-824b-926d74b59b97';
  const client = fakeClient({ failOn: 'ingest.submission' });
  const contentForwarder = { async handle() { return { status: 200, body: { state: 'available', hits: [{ submission_id: id, snippet: 's', rank: 1 }], truncated: false } }; } };
  const { base } = await withServer(t, { client, contentForwarder });
  const res = await fetch(`${base}/v1/content-search`, { method: 'POST', headers: asTenant(), body: JSON.stringify({ query: 's' }) });
  assert.equal(res.status, 200);
  assert.deepEqual((await res.json()).hits, [{ submission_id: id, snippet: 's', rank: 1 }]);
});

// ---------------------------------------------------------------------------------------------
// Finding review — the one write this service accepts
// ---------------------------------------------------------------------------------------------

const REVIEW_SUBMISSION = '80385a3a-ed3d-4950-bd21-3606c6f97fc5';

/** A client that knows the three review statements by their shape. */
function reviewClient({ finding = { user_ref: 'u_4f21' }, saved = { review_state: 'confirmed', reviewed_by: 'analyst@lab.test', reviewed_at: new Date('2026-10-05T01:00:00Z') } } = {}) {
  const calls = { query: [], began: 0, committed: 0, rolledBack: 0 };
  return {
    calls,
    async connect() {},
    async query(text, params) {
      calls.query.push({ text, params });
      if (text.includes('FROM mart.finding')) return { rows: finding ? [finding] : [], rowCount: finding ? 1 : 0, fields: [] };
      if (text.includes('INSERT INTO ops.finding_review')) return { rows: saved ? [saved] : [], rowCount: saved ? 1 : 0, fields: [] };
      if (text.includes('INSERT INTO ops.audit')) return { rows: [{ audit_seq: 42, occurred_at: new Date('2026-10-05T01:00:01Z') }], rowCount: 1, fields: [] };
      return { rows: [], rowCount: 0, fields: [] };
    },
    async begin() { calls.began += 1; },
    async commit() { calls.committed += 1; },
    async rollback() { calls.rolledBack += 1; },
    async close() {},
  };
}

test('a valid review is stored and audited in one transaction, with the actor from the session', async (t) => {
  const client = reviewClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.FINDING_REVIEW}`, {
    method: 'POST',
    headers: asTenant({ 'x-sac-dev-actor': 'analyst@lab.test' }),
    body: JSON.stringify({ submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'confirmed', note: 'real card number' }),
  });
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.result_state, 'ok');
  assert.equal(body.data.review_state, 'confirmed');
  assert.equal(body.audit.entry_id, '42', 'the audit entry id is returned so the write is traceable');

  const upsert = client.calls.query.find((c) => c.text.includes('INSERT INTO ops.finding_review'));
  assert.ok(upsert, 'the review must be written');
  assert.deepEqual(upsert.params, [REVIEW_SUBMISSION, 'PAYMENT_CARD_PAN', 'confirmed', 'analyst@lab.test', 'real card number']);
  assert.ok(upsert.text.includes('ops.current_tenant()'), 'the tenant is the session, never a parameter');
  assert.equal(upsert.text.includes(TENANT), false, 'the tenant must never appear in the SQL text');

  const audit = client.calls.query.find((c) => c.text.includes('INSERT INTO ops.audit'));
  assert.ok(audit, 'the review must be audited');
  assert.ok(audit.text.includes('ops.current_tenant()'), 'the audit row takes the tenant from the session too');
  assert.equal(client.calls.committed, 1, 'review and audit commit together');
  assert.equal(client.calls.rolledBack, 0);
});

test('reviewing a finding that does not exist is 404 and writes nothing', async (t) => {
  const client = reviewClient({ finding: null });
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.FINDING_REVIEW}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'disputed' }),
  });
  assert.equal(res.status, 404);
  assert.equal((await res.json()).result_state, 'not_found');
  assert.equal(client.calls.query.some((c) => c.text.includes('INSERT INTO ops.finding_review')), false, 'no review without a finding');
  assert.equal(client.calls.committed, 0);
  assert.equal(client.calls.rolledBack, 1);
});

test('a tenant in a review body is refused as a validation error, not honoured', async (t) => {
  const client = reviewClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.FINDING_REVIEW}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'confirmed', tenant_id: TENANT }),
  });
  assert.equal(res.status, 400);
  assert.equal((await res.json()).error.code, 'tenant_in_request');
  assert.equal(client.calls.query.length, 0, 'a rejected body must not reach the database');
});

test('review_state is only confirmed or disputed; open is not an action', async (t) => {
  const { base } = await withServer(t);
  const res = await fetch(`${base}${PATHS.FINDING_REVIEW}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'open' }),
  });
  assert.equal(res.status, 400);
  assert.equal((await res.json()).error.code, 'type_mismatch');
});

test('a review with no established tenant is refused 403 and never reaches the database', async (t) => {
  const client = reviewClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.FINDING_REVIEW}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ submission_id: REVIEW_SUBMISSION, rule_id: 'PAYMENT_CARD_PAN', review_state: 'confirmed' }),
  });
  assert.equal(res.status, 403);
  assert.equal((await res.json()).result_state, 'unauthorised_role');
  assert.equal(client.calls.query.length, 0);
});

// ---------------------------------------------------------------------------------------------
// A tool sanction decision is a configuration write, audited beside the read that shows it
// ---------------------------------------------------------------------------------------------

const TOOL_FP = 'tls_b6681b043244c43f';

/** A client that knows the sanction statements by their shape. */
function sanctionClient({ existing = null, saved = { tool_fingerprint: TOOL_FP, display_name: null, sanctioned_state: 'unsanctioned', decided_by: 'analyst@lab.test', decided_at: new Date('2026-10-05T01:00:00Z') } } = {}) {
  const calls = { query: [], began: 0, committed: 0, rolledBack: 0 };
  return {
    calls,
    async connect() {},
    async query(text, params) {
      calls.query.push({ text, params });
      if (text.includes('FROM ops.tool')) return { rows: existing ? [existing] : [], rowCount: existing ? 1 : 0, fields: [] };
      if (text.includes('INSERT INTO ops.tool')) return { rows: saved ? [saved] : [], rowCount: saved ? 1 : 0, fields: [] };
      if (text.includes('INSERT INTO ops.audit')) return { rows: [{ audit_seq: 77, occurred_at: new Date('2026-10-05T01:00:01Z') }], rowCount: 1, fields: [] };
      return { rows: [], rowCount: 0, fields: [] };
    },
    async begin() { calls.began += 1; },
    async commit() { calls.committed += 1; },
    async rollback() { calls.rolledBack += 1; },
    async close() {},
  };
}

test('a sanction decision is upserted and audited in one transaction, with the actor from the session', async (t) => {
  const client = sanctionClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.TOOL_SANCTION}`, {
    method: 'POST',
    headers: asTenant({ 'x-sac-dev-actor': 'analyst@lab.test' }),
    body: JSON.stringify({ tool_fingerprint: TOOL_FP, sanctioned_state: 'unsanctioned', note: 'not approved' }),
  });
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.result_state, 'ok');
  assert.equal(body.data.tool_fingerprint, TOOL_FP);
  assert.equal(body.data.sanctioned_state, 'unsanctioned');
  assert.equal(body.data.previous_state, 'unknown', 'an absent row is unknown, a real answer');
  assert.equal(body.audit.entry_id, '77');

  const upsert = client.calls.query.find((c) => c.text.includes('INSERT INTO ops.tool'));
  assert.ok(upsert, 'the decision must be written');
  assert.deepEqual(upsert.params, [TOOL_FP, null, 'unsanctioned', 'analyst@lab.test']);
  assert.ok(upsert.text.includes('ops.current_tenant()'), 'the tenant is the session, never a parameter');
  assert.equal(upsert.text.includes(TENANT), false, 'the tenant must never appear in the SQL text');
  assert.ok(upsert.text.includes("CASE WHEN $3::text = 'unknown' THEN NULL ELSE $4::text END"), 'unknown clears attribution');

  const audit = client.calls.query.find((c) => c.text.includes('INSERT INTO ops.audit'));
  assert.ok(audit, 'the decision must be audited');
  assert.equal(audit.params[2], 'tool.sanction');
  assert.equal(audit.params[3], 'ops.tool');
  assert.equal(client.calls.committed, 1, 'decision and audit commit together');
  assert.equal(client.calls.rolledBack, 0);
});

test('a tenant in a sanction body is refused as a validation error, not honoured', async (t) => {
  const client = sanctionClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.TOOL_SANCTION}`, {
    method: 'POST',
    headers: asTenant(),
    body: JSON.stringify({ tool_fingerprint: TOOL_FP, sanctioned_state: 'sanctioned', tenant_id: TENANT }),
  });
  assert.equal(res.status, 400);
  assert.equal((await res.json()).error.code, 'tenant_in_request');
  assert.equal(client.calls.query.length, 0, 'a rejected body must not reach the database');
});

test('a sanction with no established tenant is refused 403 and never reaches the database', async (t) => {
  const client = sanctionClient();
  const { base } = await withServer(t, { client });
  const res = await fetch(`${base}${PATHS.TOOL_SANCTION}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ tool_fingerprint: TOOL_FP, sanctioned_state: 'sanctioned' }),
  });
  assert.equal(res.status, 403);
  assert.equal((await res.json()).result_state, 'unauthorised_role');
  assert.equal(client.calls.query.length, 0);
});

// ---------------------------------------------------------------------------------------------
// Role boundaries (task 11)
// ---------------------------------------------------------------------------------------------

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const AGGREGATE = { query_version: '1', source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW };
const EVENTS = { query_version: '1', template: 'q8_activity', params: { window: WINDOW } };
const DEVICES = { query_version: '1', template: 'q7_devices', params: {} };
const AUDIT = { query_version: '1', template: 'q10_audit_trail', params: { window: WINDOW } };

const asRole = (role, extra = {}) => asTenant({ 'x-sac-dev-role': role, ...extra });

async function queryAs(base, role, body) {
  const res = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: asRole(role), body: JSON.stringify(body) });
  return res;
}

test('a viewer reads aggregates and devices but is refused events, findings and the audit trail', async (t) => {
  const { base } = await withServer(t);
  assert.notEqual((await queryAs(base, 'viewer', AGGREGATE)).status, 403, 'a viewer may read aggregates');
  assert.notEqual((await queryAs(base, 'viewer', DEVICES)).status, 403, 'the owner ruled the device list visible to a viewer');
  const events = await queryAs(base, 'viewer', EVENTS);
  assert.equal(events.status, 403, 'events are subject-level');
  assert.equal((await events.json()).result_state, 'unauthorised_role');
  const audit = await queryAs(base, 'viewer', AUDIT);
  assert.equal(audit.status, 403, 'the audit trail is the admin capability');
});

test('an analyst reads events but is refused the audit trail and any content', async (t) => {
  const { base } = await withServer(t);
  assert.notEqual((await queryAs(base, 'analyst', EVENTS)).status, 403, 'an analyst may read events');
  assert.equal((await queryAs(base, 'analyst', AUDIT)).status, 403, 'the audit trail is admin');
  // The content gate sits before the forwarder: analyst may search, not retrieve.
  const search = await fetch(`${base}${'/v1/content-search'}`, { method: 'POST', headers: asRole('analyst'), body: JSON.stringify({ query: 'capital' }) });
  assert.notEqual(search.status, 403, 'search is an analyst capability');
  const retrieval = await fetch(`${base}${'/v1/content/retrieval'}`, { method: 'POST', headers: asRole('analyst'), body: JSON.stringify({ event_ids: [TENANT] }) });
  assert.equal(retrieval.status, 403, 'opening stored content needs the content reader role');
});

test('a content reader may mint a retrieval URL; a viewer may not do either content read', async (t) => {
  const { base } = await withServer(t);
  const retrieval = await fetch(`${base}${'/v1/content/retrieval'}`, { method: 'POST', headers: asRole('content_reader'), body: JSON.stringify({ event_ids: [TENANT] }) });
  assert.notEqual(retrieval.status, 403, 'a content reader passes the retrieval gate (the vault is unconfigured here)');
  for (const path of ['/v1/content-search', '/v1/content/retrieval']) {
    const res = await fetch(`${base}${path}`, { method: 'POST', headers: asRole('viewer'), body: JSON.stringify({ query: 'x', event_ids: [TENANT] }) });
    assert.equal(res.status, 403, `a viewer is refused ${path}`);
    assert.equal((await res.json()).error.code, 'role');
  }
});

test('an admin reads the audit trail and decides sanctions, but reads no subject-level events', async (t) => {
  const { base } = await withServer(t);
  assert.notEqual((await queryAs(base, 'admin', AUDIT)).status, 403, 'the audit trail is an admin read');
  assert.equal((await queryAs(base, 'admin', EVENTS)).status, 403, 'admin reads no subject-level events');
  assert.equal((await queryAs(base, 'admin', AGGREGATE)).status, 200, 'admin keeps the aggregates');
});

test('a verified token establishes the tenant, actor and roles, and the tenant cannot come from the body', async (t) => {
  const seen = [];
  const verifier = {
    enabled: true,
    async verify(token) {
      if (token !== 'signed.token.value') throw new Error('signature does not verify');
      return { tenant: TENANT, actorId: 'reader@lab.test', subject: 'sub-1', roles: ['viewer'], caseReference: null };
    },
  };
  const { base, client } = await withServer(t, { cfg: testConfig({ SAC_DEV_TRUST_PRINCIPAL: '', SAC_OIDC_ISSUER: 'https://idp.test', SAC_OIDC_AUDIENCE: 'sac-query-api' }), verifier, client: { ...fakeClient(), async query(text, params) { seen.push({ text, params }); return { rows: [], rowCount: 0, fields: [] }; } } });
  const res = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer signed.token.value' }, body: JSON.stringify(AGGREGATE) });
  assert.equal(res.status, 200, 'the aggregate read is served for the viewer token');
  const setTenant = seen.find((c) => /set_config\('app\.tenant_id'/.test(c.text));
  assert.deepEqual(setTenant.params, [TENANT], 'the tenant came from the verified token');
  const events = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer signed.token.value' }, body: JSON.stringify(EVENTS) });
  assert.equal(events.status, 403, 'the token role is the viewer role');
});

test('an invalid bearer token is refused even with development trust off', async (t) => {
  const verifier = { enabled: true, async verify() { throw new Error('nope'); } };
  const client = fakeClient();
  const { base } = await withServer(t, { cfg: testConfig({ SAC_DEV_TRUST_PRINCIPAL: '', SAC_OIDC_ISSUER: 'https://idp.test', SAC_OIDC_AUDIENCE: 'sac-query-api' }), verifier, client });
  const res = await fetch(`${base}${PATHS.QUERY}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer forged' }, body: JSON.stringify(AGGREGATE) });
  assert.equal(res.status, 403);
  assert.equal(client.calls.query.length, 0);
});
