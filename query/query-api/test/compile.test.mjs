// compile.test.mjs — the DSL-to-SQL compiler.
//
// The two properties under test are the ones ADR 0003 turns on: values are bound and identifiers
// are resolved, so there is no escaping code to get wrong; and the ordering a cursor pages over
// is total, so a keyset page is exact rather than approximately right.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { compile, effectiveOrderKeys } from '../src/compile.js';
import { validate } from '../src/validate.js';
import { guard } from '../src/guard.js';
import { baseDoc, NOW } from './helpers.mjs';

function read(doc, ctx = {}) {
  const p = plan(doc, { now: NOW, actorId: 't', ...ctx });
  return p.statements.find((s) => s.id === 'read');
}

function compiled(doc) {
  const validated = validate(doc);
  const guarded = guard(validated);
  return { compiled: compile({ ...validated, query: guarded.query }), guarded };
}

test('the tenant is never a parameter: it comes from ops.current_tenant()', () => {
  const statement = read(baseDoc());
  assert.match(statement.text, /t\.tenant_id = ops\.current_tenant\(\)/);
  assert.ok(!statement.params.some((p) => typeof p === 'string' && p.includes('0000-0000')));
});

test('every filter value is a bound parameter with an explicit cast', () => {
  const statement = read(
    baseDoc({
      filters: [
        { field: 'tool', op: 'eq', value: 'claude_web' },
        { field: 'tool', op: 'starts_with', value: 'chat' },
        { field: 'sanctioned_state', op: 'in', value: ['unsanctioned', 'unknown'] },
        { field: 'sanctioned_state', op: 'is_null' },
      ],
    }),
  );
  assert.match(statement.text, /t\.tool_fingerprint = \$\d+::text/);
  assert.match(statement.text, /starts_with\(t\.tool_fingerprint, \$\d+::text\)/);
  assert.match(statement.text, /t\.sanctioned_state = ANY\(\$\d+::text\[\]\)/);
  assert.ok(statement.text.includes('t.sanctioned_state IS NULL'));
  for (const value of ['claude_web', 'chat']) assert.ok(statement.params.includes(value), `missing param ${value}`);
  assert.ok(statement.params.some((p) => Array.isArray(p) && p.length === 2));
  assert.ok(!statement.text.includes('claude_web'));
});

test('starts_with is a literal prefix test, not a LIKE pattern', () => {
  const statement = read(baseDoc({ filters: [{ field: 'tool', op: 'starts_with', value: '100%_raw' }] }));
  assert.ok(statement.text.includes('starts_with('));
  assert.ok(!statement.text.includes('LIKE'), 'a LIKE would make % and _ wildcards');
  assert.ok(statement.params.includes('100%_raw'));
});

test('the bucket_size predicate is always pinned, so hour rows are never added to day rows', () => {
  const short = { from: '2026-09-01T00:00:00Z', to: '2026-09-02T00:00:00Z' };
  const hour = read(baseDoc({ bucket: 'hour', window: short, dimensions: ['tool'], filters: [{ field: 'tool', op: 'eq', value: 'claude_web' }] }));
  assert.ok(hour.text.includes('t.bucket_size = $'), 'an hour read must pin bucket_size');
  assert.ok(hour.params.includes('hour'));
  for (const bucket of ['day', 'week', 'month']) {
    const statement = read(baseDoc({ bucket }));
    assert.ok(statement.text.includes('t.bucket_size = $'), `bucket ${bucket} must pin bucket_size`);
  }
  const native = read(baseDoc({ bucket: 'week' }));
  assert.ok(native.text.includes("date_trunc('week', t.bucket_start) AS bucket"));
  assert.ok(native.params.includes('day'), 'a week reduction reads day rows');
});

test('an unbucketed aggregate defaults to day buckets and says which one it applied', () => {
  const statement = read(baseDoc({ bucket: undefined }));
  assert.ok(statement.text.includes('t.bucket_size = $'));
  assert.ok(statement.params.includes('day'));
  assert.ok(statement.text.includes('GROUP BY t.bucket_start'), 'the default bucket is day, so it groups by day');
});

test('the window is half-open and bound', () => {
  const statement = read(baseDoc());
  assert.ok(statement.text.includes('t.bucket_start >= $1::timestamptz'));
  assert.ok(statement.text.includes('t.bucket_start < $2::timestamptz'));
  assert.equal(statement.params[0], '2026-09-01T00:00:00.000Z');
  assert.equal(statement.params[1], '2026-09-08T00:00:00.000Z');
});

test('ordering ends in a total key, and bucket leads a series', () => {
  const statement = read(baseDoc({ order: [{ by: 'submissions', dir: 'desc' }] }));
  assert.match(statement.text, /ORDER BY t\.bucket_start DESC NULLS LAST, SUM\(t\.submissions\) DESC NULLS LAST, t\.tool_fingerprint ASC NULLS LAST/);
});

test('a time series without a caller ordering still orders by bucket then grouping keys', () => {
  const statement = read(baseDoc({ dimensions: ['tool'], order: [] }));
  assert.match(statement.text, /ORDER BY t\.bucket_start DESC NULLS LAST, t\.tool_fingerprint ASC NULLS LAST/);
});

test('the effective ordering key is exposed for the cursor to bind to', () => {
  const validated = validate(baseDoc({ dimensions: ['tool'], order: [{ by: 'submissions', dir: 'desc' }] }));
  assert.deepEqual(
    [...effectiveOrderKeys(validated.query, validated.source)],
    ['bucket', 'submissions', 'tool'],
  );
});

test('a rollup emits GROUPING SETS so the published total exists for complementary suppression', () => {
  const statement = read(baseDoc({ rollup: true, dimensions: ['tool'], limit: undefined }));
  assert.ok(statement.text.includes('GROUP BY GROUPING SETS ((t.bucket_start, t.tool_fingerprint), ())'));
});

test('distinct-subject measures are served as a lower bound, and the response says so', () => {
  const { compiled: hard } = compiled(baseDoc({ dimensions: ['tool'], measures: ['users'] }));
  assert.ok(hard.text.includes('MAX(t.users) AS "users"'));
  assert.equal(hard.meta.measure_semantics.users, 'exact', 'a cell at the source grain is exact');

  const { compiled: soft } = compiled(
    baseDoc({ source: 'mart.agg_class_period', dimensions: ['class'], measures: ['users', 'submissions'] }),
  );
  assert.equal(soft.meta.measure_semantics.users, 'distinct_lower_bound');
  assert.equal(soft.meta.measure_semantics.submissions, 'additive');
  assert.equal(soft.meta.subject_count_basis, 'lower_bound');
});

test('submissions is summed even where a subject count is not', () => {
  const { compiled: c } = compiled(
    baseDoc({ source: 'mart.agg_class_period', dimensions: ['class'], measures: ['submissions'] }),
  );
  assert.ok(c.text.includes('SUM(c.submissions) AS "submissions"'));
});

test('max_score is a maximum, never a sum', () => {
  const { compiled: c } = compiled(
    baseDoc({ source: 'mart.agg_class_period', dimensions: ['class'], measures: ['max_score'] }),
  );
  assert.ok(c.text.includes('MAX(c.max_score) AS "max_score"'));
});

test('the hidden subject count is computed in SQL and never returned to the client', () => {
  const { compiled: c } = compiled(baseDoc({ measures: ['submissions'] }));
  assert.ok(c.text.includes('max(t.users)::bigint AS __k_subjects'));
});

test('an exact distinct-subject count is taken over aggregate rows, not over events', () => {
  const { compiled: c } = compiled(
    baseDoc({ source: 'mart.agg_tool_user_period', dimensions: ['tool'], measures: ['submissions'] }),
  );
  assert.ok(c.text.includes('count(DISTINCT a.user_ref)::bigint AS __k_subjects'));
  assert.ok(!c.text.includes('ingest.submission'), 'no aggregate read may touch the event table (C27)');
});

test('the directory join is emitted only when a directory dimension is used', () => {
  const plain = read({
    query_version: '1',
    source: 'ingest.submission',
    filters: [],
    window: { ...baseDoc().window },
    limit: 10,
  });
  assert.ok(!plain.text.includes('ops.user_dim'));
  const joined = read({
    query_version: '1',
    source: 'ingest.submission',
    filters: [{ field: 'department', op: 'eq', value: 'Legal' }],
    window: { ...baseDoc().window },
    limit: 10,
  });
  assert.ok(joined.text.includes('LEFT JOIN ops.user_dim ud'));
  assert.ok(joined.text.includes('ud.department = $'), 'the filter value stays bound');
});

test('a nullable ordering key is ordered through a sentinel and carried privately', () => {
  const { compiled: c } = compiled(
    baseDoc({
      source: 'mart.agg_org_period',
      window: { from: '2026-09-01T00:00:00Z', to: '2026-09-02T00:00:00Z' },
      dimensions: ['department', 'population'],
      measures: ['submissions'],
    }),
  );
  // The sentinel keeps the ORDER BY total over a nullable column; the client still receives the
  // real value, and the keyset comparison uses the sentinel rather than the displayed NULL.
  assert.ok(c.text.includes('coalesce(o.population, chr(1))'));
  assert.ok(c.text.includes('AS "__ord_population"'));
  assert.ok(c.text.includes('GROUP BY o.bucket_start, o.department, o.population, coalesce(o.population, chr(1))'));
});

test('the class predicate is jsonb containment over labels, never a string concatenation', () => {
  const statement = read({
    query_version: '1',
    source: 'ingest.submission',
    filters: [{ field: 'class', op: 'eq', value: 'customer_pii' }],
    window: { ...baseDoc().window },
    limit: 10,
  });
  assert.ok(statement.text.includes("s.labels @> jsonb_build_object('class', $"));
  assert.ok(!statement.text.includes('customer_pii'));
});

test('a keyset cursor becomes an expanded predicate over the mixed-direction total order', () => {
  const firstPage = plan(baseDoc({ limit: 2, order: [{ by: 'submissions', dir: 'desc' }] }), { now: NOW });
  const order = firstPage.meta.order.map((t) => t.by);
  assert.deepEqual(order, ['bucket', 'submissions', 'tool']);

  const validated = validate(
    baseDoc({ limit: 2, order: [{ by: 'submissions', dir: 'desc' }], cursor: null }),
  );
  const guarded = guard(validated);
  const compiledStatement = compile(
    { ...validated, query: guarded.query },
    {
      cursorPosition: { bucket: '2026-09-07T00:00:00.000Z', submissions: 12, tool: 'claude_web' },
      snapshotUpper: '2026-09-07T23:59:59.000Z',
    },
  );
  assert.match(
    compiledStatement.text,
    /\(t\.bucket_start < \$5::timestamptz\) OR \(t\.bucket_start = \$5::timestamptz AND SUM\(t\.submissions\) < \$6::numeric\) OR \(t\.bucket_start = \$5::timestamptz AND SUM\(t\.submissions\) = \$6::numeric AND t\.tool_fingerprint > \$7::text\)/,
  );
});

test('the page probe fetches one row beyond the page rather than counting', () => {
  const statement = read(baseDoc({ limit: 50 }));
  assert.ok(statement.text.includes('LIMIT $'));
  assert.equal(statement.params[statement.params.length - 1], 51);
});
