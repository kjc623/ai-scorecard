// questions.test.mjs — the ten questions, and the transport's refusal to render a non-envelope.

import test from 'node:test';
import assert from 'node:assert/strict';
import { QUESTIONS, QUESTION_NAMES, context } from '../src/questions.js';
import { createQueryApi, TransportError } from '../src/transport.js';
import { TEMPLATES, TEMPLATE_NAMES } from '../src/vocab.js';
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
        tool: 'tls_b6681b043244c43f',
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
    const declared = TEMPLATES[name].params;
    for (const key of Object.keys(request.params)) {
      assert.ok(declared.includes(key), `${name} sent an undeclared parameter "${key}"`);
    }
  }
});

test('a question omits a parameter the analyst did not set rather than sending an empty value', () => {
  const request = QUESTIONS.q1_tools_ranked.request(ctx({}));
  assert.ok(!('tool' in request.params));
  assert.ok(!('sanctioned_state' in request.params));
  assert.ok('window' in request.params && 'bucket' in request.params);
});

// The Unsanctioned screen is Q2's own question, so it asks for unsanctioned tools by default; the
// state is still overridable, because `unknown` gets its own list.
test('Q2 asks for unsanctioned tools by default and lets the state be overridden', () => {
  const byDefault = QUESTIONS.q2_unsanctioned_users.request(ctx({ filters: { tool: 'tls_b6681b043244c43f' } }));
  assert.equal(byDefault.params.sanctioned_state, 'unsanctioned');
  const unknown = QUESTIONS.q2_unsanctioned_users.request(ctx({ filters: { tool: 'tls_b6681b043244c43f', sanctioned_state: 'unknown' } }));
  assert.equal(unknown.params.sanctioned_state, 'unknown');
});

test('the person question carries its subject and refuses to be built without one', () => {
  const withSubject = QUESTIONS.q6_subject_series.request(ctx({ filters: { subject: 'u_1' } }));
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
