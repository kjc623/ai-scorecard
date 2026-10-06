// validate.test.mjs — the closed-document gate.
//
// The property under test: an unknown key, field, operator or bucket is REJECTED, never ignored,
// because ignoring an unrecognised filter answers a different question than the one asked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { validate } from '../src/validate.js';
import { QueryError, RESULT_STATES } from '../src/errors.js';
import { baseDoc, rejection } from './helpers.mjs';

test('a well-formed document validates and is frozen', () => {
  const { query, source } = validate(baseDoc());
  assert.equal(query.source, 'mart.v_tool_usage');
  assert.equal(source.kind, 'aggregate');
  assert.deepEqual([...query.dimensions], ['tool']);
  assert.ok(Object.isFrozen(query));
});

test('every rejection carries a result_state and an HTTP status', () => {
  const error = rejection(() => validate(baseDoc({ nope: 1 })));
  assert.ok(error instanceof QueryError);
  assert.ok(RESULT_STATES[error.resultState], `unknown result state ${error.resultState}`);
  assert.equal(error.resultState, 'unsupported_query_shape');
  assert.equal(error.http, 400);
  const envelope = error.toEnvelope();
  assert.equal(envelope.result_state, 'unsupported_query_shape');
  assert.equal(envelope.data, undefined, 'a rejection never carries data');
  assert.equal(envelope.error.code, 'unknown_key');
});

test('unknown top-level keys are rejected with the known set named', () => {
  const error = rejection(() => validate(baseDoc({ groupby: ['tool'] })));
  assert.equal(error.reason, 'unknown_key');
  assert.ok(error.detail.known_keys.includes('dimensions'));
});

test('prohibited SQL-ish keys are rejected by name, not treated as unknown', () => {
  for (const key of ['sql', 'raw', 'where', 'expression', 'order_by', 'filter_sql', 'having', 'select', 'from', 'join']) {
    const error = rejection(() => validate(baseDoc({ [key]: 'anything' })));
    assert.equal(error.reason, 'prohibited_field', `${key} should be prohibited`);
    assert.equal(error.resultState, 'unsupported_query_shape');
  }
});

test('a tenant anywhere in the request is rejected, not ignored', () => {
  for (const key of ['tenant_id', 'tenant']) {
    const error = rejection(() => validate(baseDoc({ [key]: 'other-tenant' })));
    assert.equal(error.reason, 'tenant_in_request');
    assert.equal(error.http, 400);
  }
});

test('a content predicate on /v1/query is rejected rather than ignored', () => {
  for (const key of ['content_match', 'snippet', 'search', 'match', 'tsv', 'body']) {
    const error = rejection(() => validate(baseDoc({ [key]: 'wire transfer' })));
    assert.equal(error.reason, 'text_predicate_in_request');
    assert.equal(error.detail.endpoint, '/v1/content-search');
  }
});

test('unknown dimensions, measures, operators and buckets are rejected', () => {
  assert.equal(rejection(() => validate(baseDoc({ dimensions: ['salary'] }))).reason, 'unknown_dimension');
  assert.equal(rejection(() => validate(baseDoc({ measures: ['revenue'] }))).reason, 'unknown_measure');
  assert.equal(rejection(() => validate(baseDoc({ bucket: 'fortnight' }))).reason, 'unknown_bucket');
  assert.equal(
    rejection(() => validate(baseDoc({ filters: [{ field: 'tool', op: 'regexp', value: 'x' }] }))).reason,
    'unknown_operator',
  );
  assert.equal(
    rejection(() => validate(baseDoc({ filters: [{ field: 'nope', op: 'eq', value: 'x' }] }))).reason,
    'unknown_dimension',
  );
});

test('unsupported query_version is rejected, never silently downgraded', () => {
  assert.equal(rejection(() => validate(baseDoc({ query_version: '2' }))).reason, 'unsupported_query_version');
  assert.equal(rejection(() => validate(baseDoc({ query_version: undefined }))).reason, 'unsupported_query_version');
});

test('starts_with is permitted only on tool fingerprints', () => {
  const error = rejection(() => validate(baseDoc({ filters: [{ field: 'sanctioned_state', op: 'starts_with', value: 'un' }] })));
  assert.equal(error.reason, 'operator_not_applicable');
  assert.equal(error.http, 400);
  const ok = validate(baseDoc({ filters: [{ field: 'tool', op: 'starts_with', value: 'claude' }] }));
  assert.equal(ok.query.filters[0].op, 'starts_with');
});

test('operator applicability is per type: lt on a text dimension is refused', () => {
  assert.equal(
    rejection(() => validate(baseDoc({ filters: [{ field: 'tool', op: 'lt', value: 'z' }] }))).reason,
    'operator_not_applicable',
  );
});

test('filter value types are checked, and eq null is refused with the fix named', () => {
  const doc = (filters) => baseDoc({ filters });
  assert.equal(rejection(() => validate(doc([{ field: 'tool', op: 'eq', value: null }]))).reason, 'type_mismatch');
  assert.equal(rejection(() => validate(doc([{ field: 'tool', op: 'in', value: [] }]))).reason, 'type_mismatch');
  assert.equal(
    rejection(() =>
      validate({
        query_version: '1',
        source: 'mart.v_device_liveness',
        filters: [{ field: 'spool_depth', op: 'between', value: [1] }],
        limit: 10,
      }),
    ).reason,
    'type_mismatch',
  );
  assert.equal(rejection(() => validate(doc([{ field: 'tool', op: 'eq', value: { a: 1 } }]))).reason, 'value_not_scalar');
  assert.equal(rejection(() => validate(doc([{ field: 'tool', op: 'eq', value: ['a', 'b'] }]))).reason, 'value_not_scalar');
});

test('oversized membership lists and oversized values are refused with the bound stated', () => {
  const many = Array.from({ length: 500 }, (_, i) => `t${i}`);
  const listError = rejection(() => validate(baseDoc({ filters: [{ field: 'tool', op: 'in', value: many }] })));
  assert.equal(listError.resultState, 'query_too_broad');
  assert.equal(listError.detail.max_values, 200);
  const longError = rejection(() => validate(baseDoc({ filters: [{ field: 'tool', op: 'eq', value: 'x'.repeat(300) }] })));
  assert.equal(longError.detail.max_length, 256);
});

test('too many filters and too many dimensions are refused with the limit stated', () => {
  const filters = Array.from({ length: 20 }, () => ({ field: 'tool', op: 'eq', value: 'x' }));
  assert.equal(rejection(() => validate(baseDoc({ filters }))).detail.max_filters, 16);
  // agg_class_period has enough dimensions to exceed the cap of three.
  const dims = ['class', 'tool', 'severity', 'classifier_version'];
  assert.equal(
    rejection(() => validate(baseDoc({ source: 'mart.agg_class_period', dimensions: dims, measures: ['submissions'] }))).detail.max_dimensions,
    3,
  );
});

test('a window is mandatory, half-open, and bounded', () => {
  assert.equal(rejection(() => validate(baseDoc({ window: undefined }))).reason, 'missing_window');
  assert.equal(
    rejection(() => validate(baseDoc({ window: { from: '2026-09-08T00:00:00Z', to: '2026-09-01T00:00:00Z' } }))).reason,
    'bad_window',
  );
  assert.equal(rejection(() => validate(baseDoc({ window: { from: '2026-09-01', to: '2026-09-08' } }))).reason, 'bad_window');
  assert.equal(
    rejection(() => validate(baseDoc({ window: { from: '2020-01-01T00:00:00Z', to: '2026-01-01T00:00:00Z' } }))).resultState,
    'query_too_broad',
  );
});

test('the device-liveness list needs no window, but every other source does', () => {
  const { query } = validate({ query_version: '1', source: 'mart.v_device_liveness', filters: [], limit: 50 });
  assert.equal(query.window, null);
  assert.equal(rejection(() => validate({ query_version: '1', source: 'ingest.submission', filters: [], limit: 50 })).reason, 'missing_window');
});

test('a list source does not group: grouping an event list is refused', () => {
  const error = rejection(() => validate({ query_version: '1', source: 'ingest.submission', dimensions: ['tool'], window: { ...baseDoc().window }, limit: 50 }));
  assert.equal(error.resultState, 'unsupported_query_shape');
});

test('ordering is only on a returned measure or a grouped dimension', () => {
  assert.equal(rejection(() => validate(baseDoc({ order: [{ by: 'bytes_total', dir: 'desc' }] }))).reason, 'unknown_measure');
  const ok = validate(baseDoc({ order: [{ by: 'submissions', dir: 'desc' }] }));
  assert.deepEqual(ok.query.order.map((o) => o.by), ['submissions']);
  // A list has a fixed total ordering; a caller cannot reorder it.
  assert.equal(
    rejection(() => validate({ query_version: '1', source: 'mart.v_device_liveness', order: [{ by: 'device', dir: 'desc' }], limit: 10 })).reason,
    'cursor_requires_total_order',
  );
});

test('an aggregate takes no cursor, and a list takes no rollup', () => {
  assert.equal(rejection(() => validate(baseDoc({ rollup: true, cursor: 'abc.def' }))).reason, 'aggregate_not_paged');
  assert.equal(rejection(() => validate({ query_version: '1', source: 'mart.v_device_liveness', rollup: true, limit: 10 })).reason, 'malformed_document');
});

test('a per-subject source cannot be read without naming the subject', () => {
  const bare = {
    query_version: '1',
    source: 'mart.agg_user_period',
    bucket: 'day',
    dimensions: ['subject'],
    measures: ['submissions'],
    filters: [],
    window: { ...baseDoc().window },
    limit: 200,
  };
  const error = rejection(() => validate(bare));
  assert.equal(error.reason, 'subject_scope_required');
  assert.equal(error.resultState, 'unsupported_query_shape');
  assert.equal(error.detail.fix.add_filter.field, 'subject');

  const named = validate({ ...bare, filters: [{ field: 'subject', op: 'eq', value: 'user_ref_1' }] });
  assert.equal(named.query.filters[0].field, 'subject');
});

test('prototype-laden documents are rejected before anything else looks at them', () => {
  const error = rejection(() => validate(JSON.parse('{"__proto__":{"polluted":true},"query_version":"1"}')));
  assert.equal(error.reason, 'not_closed');
  assert.equal({}.polluted, undefined, 'no prototype pollution');
});

test('non-object documents are rejected, including arrays and scalars', () => {
  for (const value of [null, 42, 'SELECT 1', [baseDoc()], true]) {
    const error = rejection(() => validate(value));
    assert.equal(error.reason, 'malformed_document');
  }
});

test('deeply nested JSON is refused at the depth bound', () => {
  const deep = { a: { b: { c: { d: { e: { f: 1 } } } } } };
  const error = rejection(() => validate({ ...baseDoc(), extra: deep }));
  // `extra` is an unknown key; depth is checked first, which is the point of the bound.
  assert.ok(['max_depth_exceeded', 'unknown_key'].includes(error.reason));
  const nestedInFilter = rejection(() =>
    validate(baseDoc({ filters: [{ field: 'tool', op: 'eq', value: deep }] })),
  );
  assert.ok(['max_depth_exceeded', 'value_not_scalar'].includes(nestedInFilter.reason));
});

test('a limit above the class cap is refused with the cap named', () => {
  const error = rejection(() =>
    validate({ query_version: '1', source: 'mart.v_device_liveness', filters: [], limit: 5000 }),
  );
  assert.equal(error.resultState, 'query_too_broad');
  assert.equal(error.detail.max_limit, 500);
});

test('class on an event row is a predicate with eq only, served by the labels GIN', () => {
  const ok = validate({
    query_version: '1',
    source: 'ingest.submission',
    filters: [{ field: 'class', op: 'eq', value: 'customer_pii' }],
    window: { ...baseDoc().window },
    limit: 50,
  });
  assert.equal(ok.query.filters[0].field, 'class');
  const error = rejection(() =>
    validate({
      query_version: '1',
      source: 'ingest.submission',
      filters: [{ field: 'class', op: 'in', value: ['a', 'b'] }],
      window: { ...baseDoc().window },
      limit: 50,
    }),
  );
  assert.equal(error.reason, 'operator_not_applicable');
});
