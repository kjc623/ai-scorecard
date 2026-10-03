// dsl.test.mjs — the client can only construct a query the API admits.
//
// The server is the security boundary: it rejects anything outside the enumerated vocabulary with
// a 400. This file is about the usability boundary — a mistake becomes a red panel in the browser
// instead of a round trip — and about one safety property that has to hold on both sides: a
// per-person source cannot be read without naming the person.

import test from 'node:test';
import assert from 'node:assert/strict';
import { buildDocument, buildTemplate, windowFor, estimatedPoints, maxPageSize, DashboardQueryError } from '../src/dsl.js';
import { OPERATORS, SOURCES, SOURCE_NAMES, TEMPLATES, TEMPLATE_NAMES, WINDOWS } from '../src/vocab.js';

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

test('a valid document is closed: versioned, no tenant, no SQL, no free text', () => {
  const doc = buildDocument({ source: 'mart.v_tool_usage', bucket: 'day', dimensions: ['tool'], measures: ['submissions'], filters: [], window: WINDOW, limit: 50 });
  assert.equal(doc.query_version, '1');
  assert.deepEqual(Object.keys(doc).sort(), ['bucket', 'dimensions', 'filters', 'limit', 'measures', 'query_version', 'source', 'window']);
  const text = JSON.stringify(doc);
  assert.ok(!/tenant/i.test(text));
  assert.ok(!/select|insert|update|delete|drop/i.test(text));
});

test('unknown top-level keys are refused, not ignored', () => {
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [], sql: 'x' }), 'unknown_key');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [], tenant_id: 'other' }), 'unknown_key');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [], order_by: 'tool' }), 'unknown_key');
});

test('unknown source, dimension, measure, operator and bucket are refused with the known set', () => {
  const source = (s) => refuses(() => buildDocument({ source: s, window: WINDOW, measures: ['submissions'], dimensions: [] }), 'unknown_source');
  source('mart.agg_users');
  const error = refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: ['salary'] }), 'unknown_dimension');
  assert.ok(error.detail.known_dimensions.includes('tool'));
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['revenue'], dimensions: [] }), 'unknown_measure');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [], filters: [{ field: 'tool', op: 'regexp', value: 'x' }] }), 'unknown_operator');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [], bucket: 'fortnight' }), 'unknown_bucket');
});

test('starts_with is refused anywhere but a tool fingerprint', () => {
  refuses(() => buildDocument({
    source: 'mart.v_tool_usage', window: WINDOW, measures: ['submissions'], dimensions: [],
    filters: [{ field: 'sanctioned_state', op: 'starts_with', value: 'un' }],
  }), 'operator_not_applicable');
});

test('a per-person source requires the person, on both sides of the wire', () => {
  const withoutSubject = () => buildDocument({
    source: 'mart.agg_user_period', bucket: 'day', dimensions: ['subject'], measures: ['submissions'], filters: [], window: WINDOW,
  });
  const error = refuses(withoutSubject, 'subject_scope_required');
  assert.equal(error.detail.fix.add_filter.field, 'subject');
  const withSubject = buildDocument({
    source: 'mart.agg_user_period', bucket: 'day', dimensions: ['subject'], measures: ['submissions'],
    filters: [{ field: 'subject', op: 'eq', value: 'u_1' }], window: WINDOW,
  });
  assert.equal(withSubject.filters[0].field, 'subject');
});

test('a bounded list does not group, and an aggregate needs a measure', () => {
  refuses(() => buildDocument({ source: 'ingest.submission', window: WINDOW, filters: [], dimensions: ['tool'] }), 'no_measures');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', window: WINDOW, dimensions: [], measures: [] }), 'no_measures');
});

test('more than three dimensions is refused before it is sent', () => {
  refuses(() => buildDocument({
    source: 'mart.agg_class_period', window: WINDOW, measures: ['submissions'],
    dimensions: ['class', 'tool', 'severity', 'classifier_version'],
  }), 'too_many_dimensions');
});

test('windows are required, ordered and UTC', () => {
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', measures: ['submissions'], dimensions: [] }), 'missing_window');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', measures: ['submissions'], dimensions: [], window: { from: WINDOW.to, to: WINDOW.from } }), 'bad_window');
  refuses(() => buildDocument({ source: 'mart.v_tool_usage', measures: ['submissions'], dimensions: [], window: { from: '2026-09-24', to: '2026-10-01' } }), 'bad_window');
  const ok = buildDocument({ source: 'mart.v_tool_usage', measures: ['submissions'], dimensions: [], window: { from: '2026-09-24T00:00:00+02:00', to: WINDOW.to } });
  assert.equal(ok.window.from, '2026-09-23T22:00:00.000Z', 'an offset is normalised, not rejected');
});

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

test('the client knows the page cap for each source kind', () => {
  assert.equal(maxPageSize('ingest.submission'), 500);
  assert.equal(maxPageSize('mart.v_tool_usage'), 2000);
  assert.equal(estimatedPoints(WINDOW, 'day'), 7);
  assert.equal(estimatedPoints(WINDOW, 'hour'), 168);
  assert.equal(estimatedPoints(WINDOW, 'week'), 1);
});

test('the vocabularies here are complete enough to describe every source and template', () => {
  assert.equal(SOURCE_NAMES.length, 12);
  assert.equal(TEMPLATE_NAMES.length, 10);
  for (const [name, spec] of Object.entries(SOURCES)) {
    assert.ok(spec.kind === 'aggregate' || spec.kind === 'list', `${name} has a kind`);
  }
  for (const name of TEMPLATE_NAMES) {
    assert.ok(SOURCES[TEMPLATES[name].source], `${name} names a known source`);
    assert.ok(SOURCES[TEMPLATES[name].source].answers, `${name}'s source says what it answers`);
  }
  assert.ok(OPERATORS.includes('is_null'));
  assert.ok(!OPERATORS.includes('regexp'));
});
