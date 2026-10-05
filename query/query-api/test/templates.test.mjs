// templates.test.mjs — §3's ten questions as named templates.
//
// Each of the ten must (a) expand to a document that validates, (b) compile to a statement whose
// text contains no interpolated value, and (c) carry the honesty notes the document attaches to
// that question. The no-interpolation assertion is run against every statement in the plan,
// including the side reads.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { TEMPLATES, TEMPLATE_NAMES, expandTemplate } from '../src/templates.js';
import { SOURCES, SOURCE_IDS } from '../src/registry.js';

/** Registry constants that can legitimately appear both as a parameter and in SQL text. */
const REGISTRY_CONSTANTS = new Set([...SOURCE_IDS, 'hour', 'day', 'week', 'month', 'ingest.submission']);
import { QueryError } from '../src/errors.js';
import { NOW, rejection } from './helpers.mjs';

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };

/** Parameters that make each template's read narrow enough for the cost guard to allow it. */
const PARAMS = Object.freeze({
  q1_tools_ranked: { window: WINDOW, limit: 100 },
  q2_unsanctioned_users: { window: WINDOW, tool: 'claude_web', limit: 100 },
  q3_team_growth: { window: WINDOW, limit: 100 },
  q4_class_mix: { window: WINDOW, limit: 100 },
  q5_findings: { window: WINDOW, limit: 50 },
  q6_subject_series: { window: WINDOW, subject: 'user_ref_0001' },
  q7_devices: { limit: 50 },
  q8_activity: { window: WINDOW, subject: 'user_ref_0001', limit: 50 },
  q9_event_detail: { submission_id: '11111111-2222-4333-8444-555555555555' },
  q10_audit_trail: { window: WINDOW, limit: 50 },
});

test('there are exactly ten templates, one per question of §3.6', () => {
  assert.equal(TEMPLATE_NAMES.length, 10);
  const questions = TEMPLATE_NAMES.map((n) => TEMPLATES[n].question).sort((a, b) => a - b);
  assert.deepEqual(questions, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
});

test('every template names a registered source', () => {
  for (const name of TEMPLATE_NAMES) {
    assert.ok(SOURCES[TEMPLATES[name].source], `${name} names ${TEMPLATES[name].source}`);
  }
});

for (const name of TEMPLATE_NAMES) {
  test(`${name} compiles with no interpolated value and states its limits`, () => {
    const p = plan({ query_version: '1', template: name, params: PARAMS[name] }, { now: NOW, actorId: 'template-test' });
    assert.ok(p.statements.length > 0, 'a template must produce at least one statement');
    for (const statement of p.statements) {
      assert.match(statement.text, /^[\x20-\x7e\n\t]*$/, `${name}/${statement.id} has non-printable characters`);
      for (const token of [';', '--', '/*', '*/']) {
        assert.ok(!statement.text.includes(token), `${name}/${statement.id} text contains ${token}`);
      }
      for (const value of statement.params) {
        if (typeof value !== 'string' || value.length < 3) continue;
        // A registry constant that happens to be a parameter (the source name in an audit
        // detail, the applied bucket) is not request-derived; everything else must be bound.
        if (REGISTRY_CONSTANTS.has(value)) continue;
        assert.ok(!statement.text.includes(value), `${name}/${statement.id} interpolated ${value}`);
      }
      // Every value that reached the statement did so as a placeholder.
      const placeholders = new Set(statement.text.match(/\$\d+/g) ?? []);
      assert.equal(placeholders.size, statement.params.length, `${name}/${statement.id} placeholder count`);
    }
    assert.ok(p.meta.notes.length > 0 || name === 'q9_event_detail', `${name} should carry its notes`);
  });
}

test('q1 ranks by submissions with a deterministic tie-break on tool, and never by sanction', () => {
  const p = plan({ query_version: '1', template: 'q1_tools_ranked', params: PARAMS.q1_tools_ranked }, { now: NOW });
  const read = p.statements.find((s) => s.id === 'read');
  assert.match(read.text, /ORDER BY t\.bucket_start DESC NULLS LAST, SUM\(t\.submissions\) DESC NULLS LAST, t\.tool_fingerprint ASC NULLS LAST/);
  assert.ok(p.meta.warnings.some((w) => w.includes('detection-only')), 'the Q1 gap must be stated');
});

test('q2 refuses an unscoped subject window beyond seven days, naming the narrowing', () => {
  const error = rejection(() =>
    plan(
      { query_version: '1', template: 'q2_unsanctioned_users', params: { window: { from: '2026-08-01T00:00:00Z', to: '2026-09-30T00:00:00Z' } } },
      { now: NOW },
    ),
  );
  assert.equal(error.resultState, 'query_too_broad');
  assert.equal(error.detail.max_days, 7);
  assert.ok(error.detail.fix.add_filter.includes('tool eq/in'));
});

test('q3 carries the unmapped residual and the mapped-user arithmetic', () => {
  const p = plan({ query_version: '1', template: 'q3_team_growth', params: PARAMS.q3_team_growth }, { now: NOW });
  const org = p.statements.find((s) => s.id === 'org_coverage');
  assert.ok(org, 'the unmapped series must travel with the per-team series');
  assert.ok(org.text.includes('users_all') && org.text.includes('users_mapped'));
  assert.equal(org.params.length, 3);
});

test('q4 carries the non-additive total as a second, separately-labelled number', () => {
  const p = plan({ query_version: '1', template: 'q4_class_mix', params: PARAMS.q4_class_mix }, { now: NOW });
  const total = p.statements.find((s) => s.id === 'class_total');
  assert.ok(total, 'the class total must be computed once, from a different measure');
  assert.ok(total.text.includes('mart.agg_tool_period'));
  assert.ok(p.meta.notes.some((n) => n.includes('CARRYING that class')));
});

test('q4 lets the analyst choose which three of the four grain dimensions to show', () => {
  const p = plan(
    { query_version: '1', template: 'q4_class_mix', params: { ...PARAMS.q4_class_mix, dimensions: ['class', 'classifier_version'] } },
    { now: NOW },
  );
  const read = p.statements.find((s) => s.id === 'read');
  assert.ok(read.text.includes('c.classifier_version'));
  const error = rejection(() =>
    plan({ query_version: '1', template: 'q4_class_mix', params: { ...PARAMS.q4_class_mix, dimensions: ['class', 'tool', 'severity', 'classifier_version'] } }, { now: NOW }),
  );
  assert.equal(error.resultState, 'unsupported_query_shape');
});

test('q5 lists findings with the three-column total key §7.1 requires', () => {
  const p = plan({ query_version: '1', template: 'q5_findings', params: PARAMS.q5_findings }, { now: NOW });
  const read = p.statements.find((s) => s.id === 'read');
  assert.match(
    read.text,
    /ORDER BY f\.detected_at DESC NULLS LAST, f\.submission_id DESC NULLS LAST, f\.rule_id ASC NULLS LAST/,
  );
  assert.ok(p.meta.notes.some((n) => n.includes('open')));
});

test('q6 is subject-scoped, always audited, and carries the flush check', () => {
  const p = plan({ query_version: '1', template: 'q6_subject_series', params: PARAMS.q6_subject_series }, { now: NOW });
  assert.equal(p.audit.decision.required, true);
  assert.equal(p.audit.decision.phase, 'pre_read');
  assert.ok(p.audit.decision.reasons.includes('filters_on_subject'));
  const flush = p.statements.find((s) => s.id === 'flush_check');
  assert.ok(flush.text.includes("interval '1 hour'"));
  assert.deepEqual(flush.params, ['user_ref_0001', '2026-09-01T00:00:00.000Z', '2026-09-08T00:00:00.000Z']);
});

test('q6 without a subject is refused: the Person screen is a lookup, not a list', () => {
  const error = rejection(() => plan({ query_version: '1', template: 'q6_subject_series', params: { window: WINDOW } }, { now: NOW }));
  assert.equal(error.resultState, 'unsupported_query_shape');
});

test('q7 needs no window and keeps four liveness values distinct', () => {
  const p = plan({ query_version: '1', template: 'q7_devices', params: PARAMS.q7_devices }, { now: NOW });
  const read = p.statements.find((s) => s.id === 'read');
  assert.ok(!read.text.includes('received_at'), 'device state is not an event stream');
  for (const value of ['revoked', 'never_reported', 'stale', 'reporting']) {
    assert.ok(read.text.includes(`'${value}'`), `liveness must keep ${value}`);
  }
  assert.match(read.text, /ORDER BY d\.device_id ASC NULLS LAST, coalesce\(cs\.collector, chr\(1\)\) ASC NULLS LAST/);
  // Fleet-wide counts by status, so the Devices cards describe the same population as the cursor
  // page (docs/04 §3.7). If this disappears, "Need attention" silently reverts to the page count.
  const status = p.statements.find((s) => s.id === 'device_status');
  assert.ok(status, 'q7 must return fleet-wide counts by status');
  for (const bucket of ['devices_enrolled', 'reporting', 'never_reported', 'stale', 'degraded', 'tampered', 'revoked']) {
    assert.ok(status.text.includes(bucket), `device_status must count ${bucket}`);
  }
});

test('q8 is a bounded, cursor-paged list with both clocks and no grouping', () => {
  const p = plan({ query_version: '1', template: 'q8_activity', params: PARAMS.q8_activity }, { now: NOW });
  const read = p.statements.find((s) => s.id === 'read');
  assert.ok(read.text.includes('s.first_occurred_at AS first_occurred_at'));
  assert.ok(read.text.includes('s.last_occurred_at AS last_occurred_at'));
  assert.ok(!read.text.includes('GROUP BY'));
  assert.equal(p.meta.k, null, 'k-suppression does not apply to an event list');
});

test('q8 caps its window at 31 days and refuses an unbounded read', () => {
  const error = rejection(() =>
    plan({ query_version: '1', template: 'q8_activity', params: { window: { from: '2026-01-01T00:00:00Z', to: '2026-06-01T00:00:00Z' } } }, { now: NOW }),
  );
  assert.equal(error.resultState, 'query_too_broad');
  assert.equal(error.detail.max_days, 31);
});

test('q8 filters on prompt_kind and prompt_kind_not, the not form default-hiding client_generated', () => {
  const p = plan(
    {
      query_version: '1',
      template: 'q8_activity',
      params: { ...PARAMS.q8_activity, prompt_kind: 'user', prompt_kind_not: 'client_generated' },
    },
    { now: NOW },
  );
  const read = p.statements.find((s) => s.id === 'read');
  // Both compile through the coalesce, so the `ne` form never drops a NULL row.
  assert.ok(read.text.includes("coalesce(s.prompt_kind, 'unknown') = $"), 'prompt_kind must compile as an eq through the coalesce');
  assert.ok(read.text.includes("coalesce(s.prompt_kind, 'unknown') <> $"), 'prompt_kind_not must compile as a ne through the coalesce');
  assert.ok(read.params.includes('user'), 'prompt_kind value must be bound');
  assert.ok(read.params.includes('client_generated'), 'prompt_kind_not value must be bound');
});

test('q8 refuses a prompt_kind outside the closed vocabulary', () => {
  assert.equal(
    rejection(() => plan({ query_version: '1', template: 'q8_activity', params: { ...PARAMS.q8_activity, prompt_kind: 'bogus' } }, { now: NOW })).reason,
    'malformed_document',
  );
  assert.equal(
    rejection(() => plan({ query_version: '1', template: 'q8_activity', params: { ...PARAMS.q8_activity, prompt_kind_not: 'bogus' } }, { now: NOW })).reason,
    'malformed_document',
  );
});

test('q9 is a single record read: submission and observations, audited first', () => {
  const p = plan({ query_version: '1', template: 'q9_event_detail', params: PARAMS.q9_event_detail }, { now: NOW });
  assert.equal(p.mode, 'single');
  assert.equal(p.statements[0].id, 'audit_insert', 'the audit row is written before the detail read');
  assert.equal(p.statements[1].id, 'submission_detail');
  assert.ok(p.statements[1].text.includes('LEFT JOIN ingest.observation o'));
  assert.ok(!p.statements[1].text.includes('content_excerpt'), 'no query path returns content');
  assert.deepEqual(p.statements[1].params, [PARAMS.q9_event_detail.submission_id]);
  assert.ok(p.statements[2].id === 'erasure_evidence');
});

test('q9 refuses a malformed submission id at the door', () => {
  const error = rejection(() => plan({ query_version: '1', template: 'q9_event_detail', params: { submission_id: "1' OR 1=1 --" } }, { now: NOW }));
  assert.equal(error.resultState, 'unsupported_query_shape');
});

test('q10 reads the audit trail, verifies the chain in SQL, and does not re-audit itself', () => {
  const p = plan({ query_version: '1', template: 'q10_audit_trail', params: PARAMS.q10_audit_trail }, { now: NOW });
  const read = p.statements.find((s) => s.id === 'read');
  assert.ok(read.text.includes('__recomputed_hash'));
  assert.ok(read.text.includes('__newer_prev_hash'));
  assert.equal(p.audit.decision.selfAudited, false, 'one audit row per query, bounded recursion');
  assert.equal(p.audit.decision.phase, 'pre_read');
});

test('an unknown template or parameter is rejected, not ignored', () => {
  assert.equal(rejection(() => expandTemplate({ template: 'q11_whatever' })).reason, 'unknown_template');
  assert.equal(
    rejection(() => expandTemplate({ template: 'q1_tools_ranked', params: { windows: WINDOW } })).reason,
    'unknown_key',
  );
  assert.equal(
    rejection(() => expandTemplate({ template: 'q1_tools_ranked', params: { window: WINDOW, bucket: 'fortnight' } })).reason,
    'unknown_bucket',
  );
});

test('a hostile template parameter never reaches statement text', () => {
  const hostile = "'; DROP TABLE ops.audit; --";
  const p = plan(
    { query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, tool: hostile, limit: 10 } },
    { now: NOW },
  );
  const read = p.statements.find((s) => s.id === 'read');
  assert.ok(!read.text.includes(hostile));
  assert.ok(read.params.includes(hostile));
  const q8 = plan(
    { query_version: '1', template: 'q8_activity', params: { window: WINDOW, subject: hostile, limit: 10 } },
    { now: NOW },
  );
  const read8 = q8.statements.find((s) => s.id === 'read');
  assert.ok(!read8.text.includes(hostile));
  assert.ok(read8.params.includes(hostile));
});

test('a template request is closed too: a prohibited key beside a template is rejected, not dropped', () => {
  const base = { query_version: '1', template: 'q1_tools_ranked', params: PARAMS.q1_tools_ranked };
  assert.equal(rejection(() => plan({ ...base, sql: 'DROP TABLE ops.audit' }, { now: NOW })).reason, 'prohibited_field');
  assert.equal(rejection(() => plan({ ...base, tenant_id: 'other' }, { now: NOW })).reason, 'tenant_in_request');
  assert.equal(rejection(() => plan({ ...base, content_match: 'iban' }, { now: NOW })).reason, 'text_predicate_in_request');
  assert.equal(rejection(() => plan({ ...base, unexpected: 1 }, { now: NOW })).reason, 'unknown_key');
  assert.equal(rejection(() => plan({ template: 'q1_tools_ranked', params: PARAMS.q1_tools_ranked }, { now: NOW })).reason, 'unsupported_query_version');
  assert.equal(rejection(() => plan({ ...base, query_version: '2' }, { now: NOW })).reason, 'unsupported_query_version');
  assert.ok(plan(base, { now: NOW }).ok);
});

test('q9 carries the received_at hint into its missing-record resolution', () => {
  const hint = '2026-09-05T04:00:00Z';
  const p = plan(
    { query_version: '1', template: 'q9_event_detail', params: { ...PARAMS.q9_event_detail, received_at_hint: hint } },
    { now: NOW },
  );
  assert.equal(p.meta.received_at_hint, hint);
  assert.deepEqual(p.statements.find((s) => s.id === 'erasure_evidence').params, [hint]);
  const withoutHint = plan({ query_version: '1', template: 'q9_event_detail', params: PARAMS.q9_event_detail }, { now: NOW });
  assert.equal(withoutHint.meta.received_at_hint, null);
});

test('every template against every hostile string keeps its statement text unchanged', () => {
  const hostile = "x' UNION SELECT 1 --";
  for (const name of TEMPLATE_NAMES) {
    const params = { ...PARAMS[name] };
    const textOf = (p) => p.statements.map((s) => `${s.id}:${s.text}`).join('\u0000');
    let good;
    let bad;
    try {
      good = plan({ query_version: '1', template: name, params }, { now: NOW });
    } catch {
      continue;
    }
    const hostileParams = { ...params };
    if (hostileParams.subject) hostileParams.subject = hostile;
    else if (hostileParams.tool) hostileParams.tool = hostile;
    else if (name === 'q9_event_detail') continue;
    else hostileParams.window = { from: hostile, to: WINDOW.to };
    try {
      bad = plan({ query_version: '1', template: name, params: hostileParams }, { now: NOW });
    } catch (error) {
      assert.ok(error instanceof QueryError, `${name} threw a non-typed error`);
      continue;
    }
    assert.equal(textOf(bad), textOf(good), `${name} changed its SQL when given a hostile value`);
  }
});
