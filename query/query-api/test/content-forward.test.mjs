// content-forward.test.mjs — the two content reads query-api forwards to content-vault.
//
// query-api cannot see content and decides nothing about it. What it owns, and what is asserted
// here, is that the vault is told WHO is asking from the session and never from the body, that the
// vault's own refusal reaches the caller with its reason, and that a retrieval relays the vault's
// single-use retrieval URL — never the content, which the browser fetches from the vault itself.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createContentForwarder, CONTENT_PATHS } from '../src/http/content.js';

const PRINCIPAL = Object.freeze({ tenant: '11111111-1111-1111-1111-111111111111', actorId: 'analyst@example.test' });
const EVENT_A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const EVENT_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

/** A vault double: answers by path, and records every call it was sent. */
function vault(answers) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    const path = new URL(url).pathname;
    const body = JSON.parse(init.body);
    calls.push({ path, headers: init.headers, body });
    const answer = answers(path, body);
    return { status: answer.status ?? 200, json: async () => answer.json };
  };
  return { calls, forwarder: createContentForwarder({ vaultUrl: 'http://vault.internal:8080/', scope: 'lab', fetchImpl, log: null }) };
}

test('a search carries the session principal and the configured scope, never a tenant from the body', async () => {
  const { calls, forwarder } = vault(() => ({ json: { state: 'available', hits: [{ submission_id: 's1', snippet: 'a <em>word</em>', rank: 0.1, unit_kind: 'prompt_body' }], truncated: false } }));
  const answer = await forwarder.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, { query: 'word', tenant_id: 'ffffffff-ffff-4fff-8fff-ffffffffffff', scope: 'everything' });
  assert.equal(answer.status, 200);
  assert.deepEqual(answer.body.hits, [{ submission_id: 's1', snippet: 'a <em>word</em>', rank: 0.1 }]);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, '/v1/content-search');
  assert.equal(calls[0].headers['x-sac-tenant'], PRINCIPAL.tenant);
  assert.equal(calls[0].headers['x-sac-subject'], PRINCIPAL.actorId);
  assert.equal(calls[0].headers['x-sac-service'], 'query-api');
  assert.deepEqual(calls[0].body, { scope: 'lab', form: 'terms', query: 'word', limit: 20 });
});

test('a search forwards the person, tool, device, mode and window filters, and the vault\'s page cursor', async () => {
  const { calls, forwarder } = vault(() => ({ json: { state: 'available', hits: [], truncated: true, next_cursor: 'page-2' } }));
  const answer = await forwarder.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, {
    query: 'australia', limit: 5, cursor: 'page-2',
    subject: 'lab-user', tool: 'claude_web', device: '35beae1b-e366-465a-8517-58df42c88bdc', mode: 'm3',
    window: { from: '2026-10-01T00:00:00.000Z', to: '2026-10-05T00:00:00.000Z' },
  });
  assert.equal(answer.status, 200);
  assert.equal(answer.body.next_cursor, 'page-2');
  assert.deepEqual(calls[0].body, {
    scope: 'lab', form: 'terms', query: 'australia', limit: 5, cursor: 'page-2',
    subject: 'lab-user', tool: 'claude_web', device: '35beae1b-e366-465a-8517-58df42c88bdc',
    mode: 'm3', received_from: '2026-10-01T00:00:00.000Z', received_to: '2026-10-05T00:00:00.000Z',
  });
});

test('a malformed filter is refused here and never reaches the vault', async () => {
  const { calls, forwarder } = vault(() => ({ json: {} }));
  for (const body of [
    { query: 'x', mode: 'm9' },
    { query: 'x', window: { from: 'yesterday' } },
    { query: 'x', window: 'today' },
    { query: 'x', subject: 'a'.repeat(513) },
  ]) {
    assert.equal((await forwarder.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, body)).status, 400, JSON.stringify(body));
  }
  assert.equal(calls.length, 0, 'nothing malformed reaches the vault');
});

test('an open window bound is left out rather than sent as an empty value', async () => {
  const { calls, forwarder } = vault(() => ({ json: { state: 'available', hits: [] } }));
  await forwarder.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, { query: 'x', window: { from: '', to: '' } });
  assert.deepEqual(calls[0].body, { scope: 'lab', form: 'terms', query: 'x', limit: 20 });
});

test('a retrieval relays the vault\'s single-use URL and never the content', async () => {
  const { calls, forwarder } = vault(() => ({ json: {
    state: 'available', grant_id: 'g1', raw_digest: 'sha256:x', expires_at: '2026-10-05T12:05:00Z',
    retrieval_url: '/v1/content/retrieval/11111111-1111-1111-1111-111111111111/g1',
  } }));
  const answer = await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, {
    event_ids: [EVENT_A], case_reference: 'CASE-1', second_approver: 'other@example.test', justification: 'why',
  });
  assert.equal(answer.status, 200);
  assert.equal(answer.body.state, 'available');
  assert.equal(answer.body.retrieval_url, '/v1/content/retrieval/11111111-1111-1111-1111-111111111111/g1');
  assert.ok(!('content' in answer.body), 'the answer carries no content byte');
  assert.deepEqual(calls.map((c) => c.path), ['/v1/content/retrieval']);
  assert.deepEqual(calls[0].body, { event_id: EVENT_A, case_reference: 'CASE-1', second_approver: 'other@example.test', justification: 'why' });
});

test('a vault that authorises a read but mints no URL is reported, not rendered as empty content', async () => {
  const { forwarder } = vault(() => ({ json: { state: 'available', grant_id: 'g1' } }));
  const answer = await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, {
    event_ids: [EVENT_A], case_reference: 'C', second_approver: 'o@example.test',
  });
  assert.equal(answer.status, 502);
  assert.equal(answer.body.error.code, 'retrieval_url_missing');
});

test('the vault\'s refusal reaches the caller with its reason, and nothing is redeemed', async () => {
  const { calls, forwarder } = vault(() => ({ status: 403, json: { error: { code: 'second_approver_not_distinct', detail: 'the second approver must be someone other than the requester', closed: true } } }));
  const answer = await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, { event_ids: [EVENT_A], case_reference: 'CASE-1', second_approver: PRINCIPAL.actorId });
  assert.equal(answer.status, 403);
  assert.equal(answer.body.error.code, 'second_approver_not_distinct');
  assert.match(answer.body.error.message, /someone other than the requester/);
  assert.equal(calls.length, 1, 'a refused retrieval mints no URL');
});

test('the object is held against one of a submission\'s events: the next is asked only when one has none', async () => {
  const { calls, forwarder } = vault((path, body) => {
    if (path === '/v1/content/retrieval' && body.event_id === EVENT_A) return { status: 403, json: { error: { code: 'no_content_object', detail: 'no object', closed: true } } };
    return { json: { state: 'available', grant_id: 'g2', retrieval_url: '/v1/content/retrieval/t/g2' } };
  });
  const answer = await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, { event_ids: [EVENT_A, EVENT_B], case_reference: 'C', second_approver: 'o@example.test' });
  assert.equal(answer.body.retrieval_url, '/v1/content/retrieval/t/g2');
  assert.equal(answer.body.event_id, EVENT_B);
  assert.deepEqual(calls.map((c) => c.body.event_id), [EVENT_A, EVENT_B]);
});

test('content that is gone is a result with its reason, not an error', async () => {
  const { forwarder } = vault(() => ({ json: { state: 'no_longer_available', reason: 'retention_expired', receipt_ref: 'r1' } }));
  const answer = await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, { event_ids: [EVENT_A], case_reference: 'C', second_approver: 'o@example.test' });
  assert.equal(answer.status, 200);
  assert.deepEqual(answer.body, { state: 'no_longer_available', reason: 'retention_expired', receipt_ref: 'r1' });
});

test('a malformed request is refused here, and a deployment with no vault says so', async () => {
  const { calls, forwarder } = vault(() => ({ json: {} }));
  assert.equal((await forwarder.handle(CONTENT_PATHS.RETRIEVAL, PRINCIPAL, { event_ids: ['not-a-uuid'] })).status, 400);
  assert.equal((await forwarder.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, {})).status, 400);
  assert.equal(calls.length, 0, 'nothing malformed reaches the vault');
  assert.equal(await forwarder.handle('/v1/query', PRINCIPAL, {}), null, 'any other path is not this module\'s');

  const none = createContentForwarder({ vaultUrl: '', log: null });
  const answer = await none.handle(CONTENT_PATHS.SEARCH, PRINCIPAL, { query: 'x' });
  assert.equal(answer.status, 503);
  assert.equal(answer.body.error.code, 'content_vault_not_configured');
});
