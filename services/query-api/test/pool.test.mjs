// pool.test.mjs — how the database pool authenticates, encrypts, and reads values.

import test from 'node:test';
import assert from 'node:assert/strict';
import { ENTRA_POSTGRES_RESOURCE, TYPES, createPool, entraTokenSource, inTransaction } from '../src/db.js';

const IDENTITY = Object.freeze({ endpoint: 'http://localhost:42356/msi/token', header: 'identity-header', clientId: '5d1c0de0-0000-4000-8000-000000000001' });

function identityEndpoint({ expiresIn = 3600, status = 200, now = () => Date.now() } = {}) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url: new URL(url), headers: init.headers });
    const n = calls.length;
    return {
      status,
      json: async () => ({ access_token: `token-${n}`, expires_on: String(Math.floor(now() / 1000) + expiresIn), resource: ENTRA_POSTGRES_RESOURCE, token_type: 'Bearer' }),
    };
  };
  return { calls, fetchImpl };
}

test('the Entra token is fetched for the managed identity from the Container Apps endpoint', async () => {
  const endpoint = identityEndpoint();
  const password = entraTokenSource(IDENTITY, { fetchImpl: endpoint.fetchImpl });
  assert.equal(await password(), 'token-1');
  const [call] = endpoint.calls;
  assert.equal(call.url.origin + call.url.pathname, IDENTITY.endpoint);
  assert.equal(call.url.searchParams.get('api-version'), '2019-08-01');
  assert.equal(call.url.searchParams.get('resource'), 'https://ossrdbms-aad.database.windows.net');
  assert.equal(call.url.searchParams.get('client_id'), IDENTITY.clientId);
  assert.deepEqual(call.headers, { 'X-IDENTITY-HEADER': 'identity-header' });
});

test('the token is cached until five minutes before it expires, and concurrent connections share one fetch', async () => {
  let clock = Date.parse('2026-10-05T12:00:00Z');
  const now = () => clock;
  const endpoint = identityEndpoint({ expiresIn: 3600, now });
  const password = entraTokenSource(IDENTITY, { fetchImpl: endpoint.fetchImpl, now });
  assert.deepEqual(await Promise.all([password(), password(), password()]), ['token-1', 'token-1', 'token-1']);
  assert.equal(endpoint.calls.length, 1);
  clock += 54 * 60_000;
  assert.equal(await password(), 'token-1', 'still more than five minutes left');
  clock += 2 * 60_000;
  assert.equal(await password(), 'token-2', 'inside the five-minute margin a new token is fetched');
  assert.equal(endpoint.calls.length, 2);
});

test('an identity endpoint failure is an error for that connection, and the next one retries', async () => {
  const failing = identityEndpoint({ status: 500 });
  const password = entraTokenSource(IDENTITY, { fetchImpl: failing.fetchImpl });
  await assert.rejects(password(), /answered 500/);
  await assert.rejects(password(), /answered 500/);
  assert.equal(failing.calls.length, 2, 'a failure is not cached');
});

test('the pool verifies TLS unless told sslmode=disable, and uses a password only when one is set', async () => {
  const base = { host: 'db.example', port: 5432, database: 'shadow', user: 'query-api', identity: IDENTITY };
  const azure = createPool({ ...base, password: '', sslMode: 'require' });
  const lab = createPool({ ...base, password: 'lab-only', sslMode: 'disable' });
  try {
    assert.deepEqual(azure.options.ssl, { rejectUnauthorized: true });
    assert.equal(typeof azure.options.password, 'function', 'no password: every connection fetches an Entra token');
    assert.equal(azure.options.application_name, 'query-api');
    assert.equal(lab.options.ssl, false);
    assert.equal(lab.options.password, 'lab-only');
  } finally {
    await azure.end();
    await lab.end();
  }
});

test('values are read losslessly: exact numbers become numbers, the rest stay text, timestamps are UTC', () => {
  const parse = (oid, text) => TYPES.getTypeParser(oid, 'text')(text);
  assert.strictEqual(parse(20, '42'), 42);
  assert.strictEqual(parse(20, '9007199254740993'), '9007199254740993', 'an int8 past 2^53 stays exact text');
  assert.strictEqual(parse(1700, '1.10'), 1.1);
  assert.strictEqual(parse(1700, '123456789012345678901234567890'), '123456789012345678901234567890');
  assert.equal(parse(1114, '2026-09-01 10:00:00.123456').toISOString(), '2026-09-01T10:00:00.123Z', 'a zoneless timestamp is UTC');
  assert.equal(parse(1184, '2026-09-01 10:00:00+02').toISOString(), '2026-09-01T08:00:00.000Z');
  assert.equal(parse(1082, '2026-09-01').toISOString(), '2026-09-01T00:00:00.000Z', 'a date is UTC midnight');
  assert.strictEqual(parse(1184, 'infinity'), 'infinity');
});

test('a tenant-scoped transaction sets the tenant and timeouts first, and rolls back on failure', async () => {
  const log = [];
  const client = { async query(text, params) { log.push([text, params]); if (text === 'boom') throw new Error('x'); return { rows: [] }; } };
  await assert.rejects(inTransaction(client, { tenant: 't1', statementTimeoutMs: 5000 }, () => client.query('boom')));
  assert.deepEqual(log.map(([text]) => text.split(',')[0]), ['BEGIN', "SELECT set_config('app.tenant_id'", 'boom', 'ROLLBACK']);
  assert.deepEqual(log[1][1], ['t1', '5000ms', '2500ms']);
  await assert.rejects(inTransaction(client, { tenant: '', statementTimeoutMs: 5000 }, async () => {}), /tenant/);
});
