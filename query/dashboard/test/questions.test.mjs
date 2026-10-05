// questions.test.mjs — the ten questions, and the pagination rule that keeps a page honest.

import test from 'node:test';
import assert from 'node:assert/strict';
import { QUESTIONS, QUESTION_NAMES, context, questionParams, questionSource } from '../src/questions.js';
import { collectPages, createQueryApi, TransportError } from '../src/transport.js';
import { scenarioTransport } from '../src/scenarios.js';
import { TEMPLATES, TEMPLATE_NAMES } from '../src/vocab.js';
import { STATE_ENVELOPES } from '../src/fixtures.js';
import { SCREENS } from '../src/app.js';

const NOW = new Date('2026-10-01T12:00:00Z');
const ctx = (over = {}) => context({ preset: 'd7', now: NOW, ...over });

test('there is one question builder per template, and no more', () => {
  assert.deepEqual([...QUESTION_NAMES].sort(), [...TEMPLATE_NAMES].sort());
  const numbers = QUESTION_NAMES.map((n) => QUESTIONS[n].number).sort((a, b) => a - b);
  assert.deepEqual(numbers, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
});

test('every question builds a request the DSL admits, with only declared parameters', () => {
  for (const name of QUESTION_NAMES) {
    const question = QUESTIONS[name];
    const request = question.request(ctx({
      filters: {
        subject: 'u_1',
        submission_id: '11111111-2222-4333-8444-555555555551',
        tool: 'claude_web',
        severity: 'critical',
        liveness: 'stale',
        class: 'customer_pii',
        department: 'Engineering',
        dimensions: ['class', 'severity'],
      },
    }));
    assert.equal(request.query_version, '1');
    assert.equal(request.template, name);
    assert.ok(Object.keys(request).every((k) => ['query_version', 'template', 'params'].includes(k)), `${name} is closed`);
    const declared = questionParams(name);
    for (const key of Object.keys(request.params)) {
      assert.ok(declared.includes(key), `${name} sent an undeclared parameter "${key}"`);
    }
    assert.equal(questionSource(name), TEMPLATES[name].source);
  }
});

test('a question omits a parameter the analyst did not set rather than sending an empty value', () => {
  const request = QUESTIONS.q1_tools_ranked.request(ctx({}));
  assert.ok(!('tool' in request.params));
  assert.ok(!('sanctioned_state' in request.params));
  assert.ok('window' in request.params && 'bucket' in request.params);
});

// The Unsanctioned screen is Q2's own question, so it asks for unsanctioned tools by default; the
// state is still overridable, because `unknown` gets its own list (docs/04 §3.2).
test('Q2 asks for unsanctioned tools by default and lets the state be overridden', () => {
  const byDefault = QUESTIONS.q2_unsanctioned_users.request(ctx({ filters: { tool: 'claude_web' } }));
  assert.equal(byDefault.params.sanctioned_state, 'unsanctioned');
  const unknown = QUESTIONS.q2_unsanctioned_users.request(ctx({ filters: { tool: 'claude_web', sanctioned_state: 'unknown' } }));
  assert.equal(unknown.params.sanctioned_state, 'unknown');
});

test('the person question carries its subject and refuses to be built without one', () => {  const withSubject = QUESTIONS.q6_subject_series.request(ctx({ filters: { subject: 'u_1' } }));
  assert.equal(withSubject.params.subject, 'u_1');
  let error;
  try {
    QUESTIONS.q6_subject_series.request(ctx({}));
  } catch (e) {
    error = e;
  }
  assert.ok(error, 'a person series without a person is refused client-side');
  assert.equal(error.reason, 'subject_scope_required');
});

test('the event question carries the received_at hint so a purged record can be told from a missing one', () => {
  const request = QUESTIONS.q9_event_detail.request(ctx({ filters: { submission_id: 's', received_at_hint: '2026-09-05T04:00:00Z' } }));
  assert.equal(request.params.received_at_hint, '2026-09-05T04:00:00Z');
});

test('every screen that claims a question has one', () => {
  for (const screen of SCREENS) {
    if (!screen.questionId) continue;
    assert.ok(QUESTIONS[screen.questionId], `${screen.id} names a real question`);
    assert.equal(QUESTIONS[screen.questionId].screen, screen.id, `${screen.id} matches its question's screen`);
  }
});

// ── pagination ───────────────────────────────────────────────────────────────────────────────

test('a short page is not the end: iteration stops only when next_cursor is null', async () => {
  const pages = [
    { result_state: 'ok', data: [{ n: 1 }, { n: 2 }], page: { returned: 2, next_cursor: 'c1' }, freshness: {}, coverage: {} },
    { result_state: 'ok', data: [{ n: 3 }], page: { returned: 1, next_cursor: 'c2' }, freshness: {}, coverage: {} },
    { result_state: 'ok', data: [], page: { returned: 0, next_cursor: null }, freshness: {}, coverage: {} },
  ];
  let i = 0;
  const seen = [];
  const result = await collectPages({
    fetchPage: async (body) => { seen.push(body); return pages[i++]; },
    body: { query_version: '1', template: 'q5_findings', params: {} },
  });
  assert.equal(seen.length, 3, 'the empty page in the middle did not end the iteration');
  assert.equal(result.rows.length, 3);
  assert.equal(result.truncated, false, 'the loop ended because next_cursor was null');
  assert.equal(seen[0].cursor, undefined, 'the first request carries no cursor');
  assert.equal(seen[1].cursor, 'c1');
  assert.equal(seen[2].cursor, 'c2');
});

test('a refusal ends the iteration and is returned rather than swallowed', async () => {
  const result = await collectPages({
    fetchPage: async () => STATE_ENVELOPES.cursor_expired,
    body: { query_version: '1', template: 'q5_findings', params: {} },
  });
  assert.equal(result.error, 'cursor_expired');
  assert.equal(result.rows.length, 0);
  assert.equal(result.envelope.result_state, 'cursor_expired');
});

test('the safety bound is a bound, not a page count: it reports truncation', async () => {
  const forever = { result_state: 'ok', data: [{ n: 1 }], page: { returned: 1, next_cursor: 'again' }, freshness: {}, coverage: {} };
  const result = await collectPages({ fetchPage: async () => forever, body: {}, maxPages: 3 });
  assert.equal(result.truncated, true);
  assert.equal(result.pages.length, 3);
});

test('the transport refuses a body that is not a query envelope rather than rendering it as empty', async () => {
  const api = createQueryApi({ transport: { async send() { return { hello: 'world' }; } } });
  await assert.rejects(() => api.run({}), (error) => error instanceof TransportError && error.resultState === 'audit_chain_broken');
  const unknown = createQueryApi({ transport: { async send() { return { result_state: 'fine_thanks' }; } } });
  await assert.rejects(() => unknown.run({}), /Unknown result_state/);
});

test('a network failure becomes a 429-style state, never a silent empty answer', async () => {
  const api = createQueryApi({ transport: { async send() { throw new Error('offline'); } } });
  await assert.rejects(() => api.run({}), (error) => error.resultState === 'busy' && error.network === true);
});

test('the stub answers page two for a cursor it issued, and refuses one it did not', async () => {
  const api = createQueryApi({ transport: scenarioTransport('realistic') });
  const first = await api.run({ query_version: '1', template: 'q8_activity', params: { limit: 50, window: { from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' } } });
  assert.ok(first.page.next_cursor);
  const second = await api.run({ query_version: '1', template: 'q8_activity', params: { limit: 50, window: { from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' }, cursor: first.page.next_cursor } });
  assert.equal(second.page.next_cursor, null, 'the second page ends the iteration');
  const bogus = await api.run({ query_version: '1', template: 'q8_activity', params: { limit: 50, cursor: 'not-a-cursor' } });
  assert.equal(bogus.result_state, 'cursor_expired');
});
