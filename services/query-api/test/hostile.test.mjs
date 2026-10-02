// hostile.test.mjs — the hostile-input corpus.
//
// INV-3 says: "No part of a client-supplied value is ever concatenated into SQL text; a value is
// bound, and a dimension name that is not in the enumerated vocabulary is rejected rather than
// escaped."
//
// This file tries to break that in the ways that break real DSLs — quotes, statement
// terminators, comment sequences, UNION, nested JSON, oversized arrays, unknown operators,
// prototype pollution — and asserts one of exactly two outcomes for every attempt:
//
//   * a typed §13 rejection, and no statement at all; or
//   * a compiled statement whose TEXT is byte-identical to the compiled text of a benign
//     document of the same shape, with the hostile value present only in `params`.
//
// The second assertion is the strong one. Identical text means nothing from the request reached
// the statement — not even a value that happened to survive validation.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { QueryError, RESULT_STATES } from '../src/errors.js';
import { SOURCE_IDS, SOURCES } from '../src/registry.js';
import { baseDoc, NOW, rejection } from './helpers.mjs';

const HOSTILE = Object.freeze([
  "'; DROP TABLE ingest.submission; --",
  "1' OR '1'='1",
  '$$; SELECT pg_sleep(10); --',
  '/* comment */',
  "%' UNION SELECT * FROM ops.content_object --",
  "tool' OR 1=1 --",
  "\\'; SELECT 1",
  'claude"; DROP SCHEMA mart CASCADE; --',
  'x\u0000y',
  'ünïcødé; --',
  'e\u0301\u200bcombining',
  "'; SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000000'; --",
]);

/** Compile a document, returning either the statement or the typed rejection. */
function attempt(doc, ctx = {}) {
  try {
    const p = plan(doc, { now: NOW, actorId: 'hostile-test', ...ctx });
    const read = p.statements.find((s) => s.id === 'read');
    return { ok: true, plan: p, text: read.text, params: read.params };
  } catch (error) {
    assert.ok(error instanceof QueryError, `expected a typed rejection, got ${error}`);
    assert.ok(RESULT_STATES[error.resultState], `unknown result state ${error.resultState}`);
    return { ok: false, error };
  }
}

/** A benign document with the same shape as the hostile ones, for text comparison. */
function benign(value) {
  return baseDoc({ filters: [{ field: 'tool', op: 'eq', value: value ?? 'benign_tool' }] });
}

function hostileDoc(value, extra = {}) {
  return baseDoc({ filters: [{ field: 'tool', op: 'eq', value }], ...extra });
}

test('every hostile string is either rejected or bound, never written into SQL text', () => {
  const baseline = attempt(benign()).text;
  for (const value of HOSTILE) {
    const result = attempt(hostileDoc(value));
    if (!result.ok) {
      assert.equal(result.error.resultState, 'unsupported_query_shape');
      continue;
    }
    assert.equal(result.text, baseline, `hostile value changed the statement text: ${JSON.stringify(value)}`);
    assert.ok(
      result.params.some((p) => p === value),
      'the hostile value must be present as a bound parameter',
    );
    for (const token of [';', '--', '/*', '*/', '\\']) {
      assert.ok(!result.text.includes(token), `statement text contains ${token}`);
    }
    assert.ok(!result.text.includes(value), 'hostile value leaked into the statement text');
  }
});

test('a hostile value never changes the statement for any registered source', () => {
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    const field = source.dimensions.tool ? 'tool' : null;
    if (!field) continue;
    const doc = (value) => ({
      query_version: '1',
      source: id,
      ...(source.kind === 'aggregate' ? { bucket: 'day', dimensions: [], measures: Object.keys(source.measures).slice(0, 1) } : {}),
      filters: [{ field: 'tool', op: 'eq', value }],
      window: { ...baseDoc().window },
      limit: 10,
    });
    const good = attempt(doc('claude_web'));
    const bad = attempt(doc(HOSTILE[0]));
    if (!good.ok || !bad.ok) continue;
    assert.equal(bad.text, good.text, `value leaked into the text for ${id}`);
  }
});

test('hostile dimension, measure, operator, bucket and source names are rejected', () => {
  const cases = [
    () => baseDoc({ dimensions: ["tool; DROP TABLE ops.audit"] }),
    () => baseDoc({ measures: ['submissions) FROM ops.tenant --'] }),
    () => baseDoc({ filters: [{ field: "tool' OR '1'='1", op: 'eq', value: 'x' }] }),
    () => baseDoc({ filters: [{ field: 'tool', op: "eq' OR '1'='1", value: 'x' }] }),
    () => baseDoc({ bucket: "day'; DROP TABLE ops.audit; --" }),
    () => baseDoc({ source: "mart.v_tool_usage; DROP TABLE ops.audit; --" }),
    () => baseDoc({ order: [{ by: 'submissions); DROP TABLE ops.audit; --', dir: 'desc' }] }),
  ];
  for (const build of cases) {
    const error = rejection(() => plan(build(), { now: NOW }));
    assert.equal(error.resultState, 'unsupported_query_shape', String(error.message));
  }
});

test('SQL-shaped keys are rejected even when their value is harmless', () => {
  for (const key of ['sql', 'where', 'expression', 'order_by', 'raw', 'filter_sql']) {
    const error = rejection(() => plan(baseDoc({ [key]: '1=1' }), { now: NOW }));
    assert.equal(error.resultState, 'unsupported_query_shape');
  }
});

test('nested JSON in a value is rejected rather than passed through', () => {
  const nested = { a: { b: { c: { d: { e: 'deep' } } } } };
  const error = rejection(() => plan(hostileDoc(nested), { now: NOW }));
  assert.ok(['value_not_scalar', 'max_depth_exceeded', 'type_mismatch'].includes(error.reason));
  const arrayValue = rejection(() => plan(hostileDoc(['a', 'b']), { now: NOW }));
  assert.ok(['value_not_scalar', 'type_mismatch'].includes(arrayValue.reason));
});

test('oversized arrays are refused with the bound stated, not truncated', () => {
  const values = Array.from({ length: 10_000 }, (_, i) => `v${i}`);
  const error = rejection(() => plan(baseDoc({ filters: [{ field: 'tool', op: 'in', value: values }] }), { now: NOW }));
  assert.equal(error.resultState, 'query_too_broad');
  assert.equal(error.detail.max_values, 200);
  assert.equal(error.detail.requested, 10_000);
});

test('a JSON body that is not an object is refused', () => {
  for (const value of ['SELECT 1', 7, true, [baseDoc()], null]) {
    const error = rejection(() => plan(value, { now: NOW }));
    assert.equal(error.resultState, 'unsupported_query_shape');
  }
});

test('prototype pollution through the document is impossible', () => {
  const doc = JSON.parse('{"query_version":"1","__proto__":{"polluted":"yes"},"source":"mart.v_tool_usage"}');
  const error = rejection(() => plan(doc, { now: NOW }));
  assert.equal(error.reason, 'not_closed');
  assert.equal({}.polluted, undefined);
  assert.equal(Object.prototype.polluted, undefined);
});

test('every compiled statement in the corpus stays inside a printable-SQL alphabet', () => {
  const documents = [];
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    if (source.kind === 'aggregate') {
      documents.push({
        query_version: '1',
        source: id,
        bucket: 'day',
        dimensions: Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 1),
        measures: Object.keys(source.measures).slice(0, 2),
        filters: [],
        window: { ...baseDoc().window },
        limit: 10,
      });
    } else {
      documents.push({
        query_version: '1',
        source: id,
        filters: [],
        ...(source.time ? { window: { ...baseDoc().window } } : {}),
        limit: 10,
      });
    }
  }
  for (const doc of documents) {
    const result = attempt(doc);
    if (!result.ok) continue;
    assert.match(result.text, /^[\x20-\x7e\n\t]*$/, `non-printable characters in a statement for ${doc.source}`);
    for (const token of [';', '--', '/*', '*/', '\u0000']) {
      assert.ok(!result.text.includes(token), `${doc.source} statement contains ${JSON.stringify(token)}`);
    }
    // The one legitimate backslash in the whole compiler is ops.audit's chain separator, which
    // the trigger owns. Nothing else may contain one.
    const withoutChainSeparator = result.text.replace(/E'\\x1f'/g, '');
    assert.ok(!withoutChainSeparator.includes('\\'), `${doc.source} statement contains a backslash`);
    assert.ok(!withoutChainSeparator.includes(HOSTILE[0]), 'a hostile value leaked into the statement text');
  }
});

test('a hostile cursor is refused as cursor_expired, never parsed as a position', () => {
  for (const cursor of [HOSTILE[0], 'not-a-cursor', 'aaaa.bbbb', 'x'.repeat(5000)]) {
    const error = rejection(() =>
      plan(baseDoc({ cursor }), { now: NOW, tenant: 'tenant-a', cursorKey: Buffer.from('k'.repeat(32)) }),
    );
    assert.equal(error.resultState, 'cursor_expired', `cursor ${cursor.slice(0, 20)}`);
  }
});
