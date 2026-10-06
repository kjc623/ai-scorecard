// dsl.test.mjs — the client can only construct a query the API admits.
//
// The server is the security boundary: it rejects anything outside the enumerated vocabulary with
// a 400. This file is about the usability boundary — a mistake becomes a red panel in the browser
// instead of a round trip — and about one safety property that has to hold on both sides: a
// per-person source cannot be read without naming the person.

import test from 'node:test';
import assert from 'node:assert/strict';
import { buildTemplate, windowFor, DashboardQueryError } from '../src/dsl.js';
import { SOURCES, TEMPLATES, TEMPLATE_NAMES, WINDOWS } from '../src/vocab.js';

const WINDOW = { from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' };

function refuses(build, reason) {
  let error;
  try {
    build();
  } catch (e) {
    error = e;
  }
  assert.ok(error, `expected a refusal (${reason})`);
  assert.ok(error instanceof DashboardQueryError, `expected a DashboardQueryError, got ${error}`);
  assert.equal(error.reason, reason);
  assert.equal(error.resultState, 'unsupported_query_shape');
  assert.equal(error.toEnvelope().result_state, 'unsupported_query_shape');
  return error;
}

test('a template parameter the template does not declare is refused', () => {
  refuses(() => buildTemplate('q1_tools_ranked', { window: WINDOW, windows: WINDOW }), 'unknown_key');
  refuses(() => buildTemplate('nope', {}), 'unknown_template');
  refuses(() => buildTemplate('q6_subject_series', { window: WINDOW }), 'subject_scope_required');
  const ok = buildTemplate('q6_subject_series', { window: WINDOW, subject: 'u_1' });
  assert.equal(ok.template, 'q6_subject_series');
  assert.equal(ok.params.subject, 'u_1');
});

test('window presets are named, and the label cannot disagree with the request', () => {
  const w = windowFor('d7', new Date('2026-10-01T12:00:00Z'));
  assert.equal(w.to, '2026-10-01T12:00:00.000Z');
  assert.equal(w.from, '2026-09-24T12:00:00.000Z');
  assert.equal(w.label, WINDOWS.d7.label);
  assert.equal(w.bucket, 'day');
  refuses(() => windowFor('d4000'), 'unknown_window');
});

test('the vocabularies here are complete enough to describe every source and template', () => {
  assert.equal(Object.keys(SOURCES).length, 12);
  assert.equal(TEMPLATE_NAMES.length, 10);
  for (const [name, spec] of Object.entries(SOURCES)) {
    assert.ok(spec.kind === 'aggregate' || spec.kind === 'list', `${name} has a kind`);
  }
  for (const name of TEMPLATE_NAMES) {
    assert.ok(SOURCES[TEMPLATES[name].source], `${name} names a known source`);
    assert.ok(SOURCES[TEMPLATES[name].source].answers, `${name}'s source says what it answers`);
  }
});
