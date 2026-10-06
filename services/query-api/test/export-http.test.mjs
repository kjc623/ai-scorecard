// export-http.test.mjs — the export and erasure routes, driven by real tokens through the real
// verifier against a recording database double.
import test from 'node:test';
import assert from 'node:assert/strict';

import { createQueryServer, PATHS } from '../src/http/server.js';
import { loadConfig } from '../src/http/config.js';
import { createVerifier } from '../src/http/auth.js';
import { createContentForwarder } from '../src/http/content.js';
import {
  RESOLVE_SUBJECT_SQL, SUBJECT_SUBMISSIONS_SQL, SUBJECT_OBSERVATIONS_SQL, SUBJECT_FINDINGS_SQL,
  INSERT_EXPORT_SQL, CLAIM_EXPORT_SQL, EXPORT_FOR_DOWNLOAD_SQL, INSERT_ERASURE_REQUEST_SQL,
} from '../src/subject.js';
import { createTestIssuer, TENANT } from './helpers.mjs';

const SILENT = Object.freeze({ info() {}, warn() {}, error() {} });
const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const EXPORT_ID = '80385a3a-ed3d-4950-bd21-3606c6f97fc5';

function defaultAnswer(text) {
  if (text.startsWith('INSERT INTO ops.audit')) return [{ audit_seq: 42, occurred_at: new Date('2026-10-05T01:00:01Z') }];
  if (text === INSERT_EXPORT_SQL) return [{ export_id: EXPORT_ID, expires_at: new Date('2026-10-05T01:15:00Z') }];
  if (text === RESOLVE_SUBJECT_SQL) return [{ user_ref: 'u_canonical', display_name: 'Ada Lovelace' }];
  if (text === SUBJECT_SUBMISSIONS_SQL) return [{ submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', received_at: new Date('2026-10-01T00:00:00Z'), subject: 'u_canonical', tool: 'tls_x', tool_name: 'Claude Code', labels: [], observed_routes: [] }];
  if (text === SUBJECT_OBSERVATIONS_SQL) return [{ event_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' }];
  if (text === SUBJECT_FINDINGS_SQL) return [];
  if (text === CLAIM_EXPORT_SQL) return [{ kind: 'list', source: 'ingest.submission', subject_ref: null, content_type: 'text/csv; charset=utf-8', payload: Buffer.from('a,b\n1,2\n') }];
  if (text === EXPORT_FOR_DOWNLOAD_SQL) return [{ kind: 'list', content_type: 'text/csv', payload: Buffer.from('x'), expires_at: new Date(), used_at: new Date() }];
  if (text === INSERT_ERASURE_REQUEST_SQL) return [{ request_id: EXPORT_ID }];
  return [{ submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', received_at: new Date('2026-10-01T00:00:00Z'), first_occurred_at: new Date('2026-10-01T00:00:00Z'), last_occurred_at: new Date('2026-10-01T00:00:00Z'), subject: 'u_4f21', tool: 'tls_x', tool_name: 'Claude Code', device: 'd', mode: 'm1', action: 'logged', content_state: 'not_captured', route: 'ext.page_context', detection_basis: 'prompt', prompt_kind: 'user', merge_confidence: 'high', confidence: 'high', observation_count: 1, size_bytes: 10, labels: [], observed_routes: [] }];
}

function fakePool({ answer = defaultAnswer } = {}) {
  const calls = { query: [] };
  const client = {
    async query(text, params) {
      calls.query.push({ text, params });
      return { rows: answer(text) ?? [] };
    },
    release() {},
  };
  return { calls, async connect() { return client; }, async query(text) { return { rows: [] }; }, async end() {} };
}

function vaultDouble() {
  const calls = [];
  const fetchImpl = async (url, init) => {
    const path = new URL(url).pathname;
    calls.push({ path, body: JSON.parse(init.body) });
    const json = path === '/v1/content/subject-export'
      ? { state: 'available', prompts: [{ event_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', prompt_kind: 'user', raw_digest: 'sha256:x', size_bytes: 5, plaintext: Buffer.from('hello').toString('base64') }], skipped: 0 }
      : { state: 'available', hits: [] };
    return { status: 200, json: async () => json };
  };
  return { calls, forwarder: createContentForwarder({ vaultUrl: 'http://vault.internal:8080', fetchImpl, log: null }) };
}

async function withServer(t, { pool = fakePool(), vault = vaultDouble() } = {}) {
  const issuer = createTestIssuer();
  const cfg = loadConfig({
    SAC_PG_HOST: 'db.test', SAC_PG_DATABASE: 'shadow', SAC_PG_USER: 'query-api',
    SAC_PG_PASSWORD: 'test-only', SAC_PG_SSLMODE: 'disable',
    SAC_CONTENT_VAULT_URL: 'http://vault.internal:8080', SAC_CURSOR_KEY: 'c'.repeat(32),
    SAC_AUTH_ISSUER: issuer.issuer,
  });
  const verifier = createVerifier({ issuer: issuer.issuer, fetchImpl: issuer.fetchImpl });
  const service = createQueryServer({ cfg, pool, verifier, contentForwarder: vault.forwarder, log: SILENT });
  const address = await service.listen({ host: '127.0.0.1', port: 0 });
  const base = `http://127.0.0.1:${address.port}`;
  t.after(() => service.close());
  const bearer = (roles, claims = {}) => ({ authorization: `Bearer ${issuer.mint({ roles, ...claims })}` });
  const post = (path, body, headers = bearer(['content_reader'])) => fetch(`${base}${path}`, {
    method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body: JSON.stringify(body),
  });
  const get = (path, headers = bearer(['content_reader'])) => fetch(`${base}${path}`, { headers });
  return { base, pool, vault, bearer, post, get };
}

test('a list export runs the read, writes the audit and returns a download link', async (t) => {
  const { pool, post } = await withServer(t);
  const res = await post(PATHS.LIST_EXPORT, { query_version: '1', template: 'q8_activity', params: { window: WINDOW } });
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.result_state, 'ok');
  assert.equal(body.export.row_count, 1);
  assert.equal(body.export.download_url, `${PATHS.EXPORT_DOWNLOAD}/${EXPORT_ID}`);

  const texts = pool.calls.query.map((c) => c.text);
  assert.ok(texts.some((t) => t.startsWith('INSERT INTO ops.audit')), 'audit written');
  assert.ok(texts.some((t) => t === INSERT_EXPORT_SQL), 'export stored');
  assert.ok(texts.some((t) => t.includes('FROM ingest.submission')), 'the list was read');
});

test('a list export of per-person rows requires the analyst role', async (t) => {
  const { post, bearer } = await withServer(t);
  const res = await post(PATHS.LIST_EXPORT, { query_version: '1', template: 'q8_activity', params: { window: WINDOW } }, bearer(['viewer']));
  assert.equal(res.status, 403);
  const body = await res.json();
  assert.equal(body.result_state, 'unauthorised_role');
});

test('a list export refuses an aggregate template', async (t) => {
  const { post } = await withServer(t);
  const res = await post(PATHS.LIST_EXPORT, { query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW } });
  assert.equal(res.status, 400);
  const body = await res.json();
  assert.equal(body.result_state, 'unsupported_query_shape');
});

test('a subject export asks the vault for prompts and returns an archive link', async (t) => {
  const { pool, vault, post, bearer } = await withServer(t);
  const res = await post(PATHS.SUBJECT_EXPORT, { subject_ref: 'u_alias' }, bearer(['admin']));
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.result_state, 'ok');
  assert.equal(body.export.download_url, `${PATHS.EXPORT_DOWNLOAD}/${EXPORT_ID}`);
  assert.ok(vault.calls.some((c) => c.path === '/v1/content/subject-export'), 'the vault was asked for prompts');
  assert.ok(pool.calls.query.some((c) => c.text === RESOLVE_SUBJECT_SQL), 'the subject was resolved');
});

test('a subject export requires the admin role', async (t) => {
  const { post, bearer } = await withServer(t);
  const res = await post(PATHS.SUBJECT_EXPORT, { subject_ref: 'u_x' }, bearer(['analyst']));
  assert.equal(res.status, 403);
});

test('a subject erasure records the request', async (t) => {
  const { pool, post, bearer } = await withServer(t);
  const res = await post(PATHS.SUBJECT_ERASURE, { subject_ref: 'u_alias' }, bearer(['admin']));
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.erasure.request_id, EXPORT_ID);
  assert.ok(pool.calls.query.some((c) => c.text === INSERT_ERASURE_REQUEST_SQL), 'the request was recorded');
  const erasureAudit = pool.calls.query.find((c) => c.text.startsWith('INSERT INTO ops.audit'));
  assert.equal(erasureAudit.params[2], 'subject.erasure', 'the audit names the erasure action');
});

test('a download serves the bytes once and refuses the second time', async (t) => {
  const { get } = await withServer(t);
  const first = await get(`${PATHS.EXPORT_DOWNLOAD}/${EXPORT_ID}`);
  assert.equal(first.status, 200);
  assert.match(first.headers.get('content-type'), /text\/csv/);
  assert.equal(await first.text(), 'a,b\n1,2\n');

  // The second download of a used link is refused.
  const { get: get2, pool } = await withServer(t, { pool: fakePool({ answer: (text) => (text === CLAIM_EXPORT_SQL ? [] : (text === EXPORT_FOR_DOWNLOAD_SQL ? [{ kind: 'list', content_type: 'text/csv', payload: Buffer.from('x'), expires_at: new Date(), used_at: new Date() }] : [])) }) });
  const second = await get2(`${PATHS.EXPORT_DOWNLOAD}/${EXPORT_ID}`);
  assert.equal(second.status, 410);
  const body = await second.json();
  assert.equal(body.error.code, 'export_used');
  void pool;
});
