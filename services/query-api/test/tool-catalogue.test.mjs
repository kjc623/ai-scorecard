// tool-catalogue.test.mjs — the catalogue and the read-time resolution of names and sanction state.
//
// Two claims are under test (db.test.mjs runs the resolver against a live database):
//
//   1. every source that can show a tool resolves the fingerprint to a name at read time, and keeps
//      the raw fingerprint beside it, so an unknown tool is never mistaken for a known one;
//   2. Q2 joins the present-tense sanction state through the catalogue and filters on it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { compile } from '../src/compile.js';
import { validate } from '../src/validate.js';
import { guard } from '../src/guard.js';
import { SOURCES } from '../src/registry.js';
import { expandTemplate } from '../src/templates.js';
import { NOW, rejection } from './helpers.mjs';

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };

function readText(doc) {
  const p = plan(doc, { now: NOW, actorId: 'tool-test' });
  return p.statements.find((s) => s.id === 'read').text;
}

function compiledText(doc) {
  const validated = validate(doc);
  const guarded = guard(validated);
  return compile({ ...validated, query: guarded.query }).text;
}

// ── resolution: the three sources that show a tool ─────────────────────────────────────────────

test('every tool-bearing source adds a read-time display name and keeps the fingerprint', () => {
  for (const id of ['mart.v_tool_usage', 'mart.agg_tool_user_period', 'ingest.submission']) {
    const source = SOURCES[id];
    const entries = [...(source.extraSelect ?? []), ...(source.listSelect ?? [])];
    assert.ok(
      entries.some((e) => {
        const sql = typeof e === 'string' ? e : e.sql;
        return sql.includes('ops.tool_display_name(') && sql.includes('AS "tool_name"');
      }),
      `${id} does not resolve a tool display name`,
    );
  }
  // The raw fingerprint stays the `tool` dimension/column on every one of them.
  assert.equal(SOURCES['mart.v_tool_usage'].dimensions.tool.sql, 't.tool_fingerprint');
  assert.equal(SOURCES['mart.agg_tool_user_period'].dimensions.tool.sql, 'a.tool_fingerprint');
  assert.ok(SOURCES['ingest.submission'].listSelect.includes('s.tool_fingerprint AS "tool"'));
});

test('the display name is selected only when tool is grouped, so an unrelated grouping still compiles', () => {
  // Q4 groups by class and severity: tool is not grouped, and a `tool_name` in the SELECT list
  // would be a Postgres grouping error. It must be absent.
  const classMix = readText({ query_version: '1', template: 'q4_class_mix', params: { window: WINDOW, limit: 100 } });
  assert.ok(!classMix.includes('tool_name'), 'q4 must not select a name it cannot group');
  // Q1 groups by tool, so the name is present.
  const tools = readText({ query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, limit: 100 } });
  assert.match(tools, /"tool_name"/);
});

test('Q1 returns the name without changing the fingerprint it groups on', () => {
  const text = readText({ query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW, limit: 100 } });
  assert.match(text, /t\.tool_fingerprint AS "tool"/);
  assert.match(text, /ops\.tool_display_name\(t\.tool_fingerprint\) AS "tool_name"/);
});

test('the event list returns the name beside the fingerprint', () => {
  const text = readText({ query_version: '1', template: 'q8_activity', params: { window: WINDOW, subject: 'u_1', limit: 50 } });
  assert.match(text, /s\.tool_fingerprint AS "tool"/);
  assert.match(text, /ops\.tool_display_name\(s\.tool_fingerprint\) AS "tool_name"/);
});

// ── Q2: the sanctioned join ──────────────────────────────────────────────────────────────────

test('Q2 joins the present-tense sanction state and filters to it', () => {
  const document = expandTemplate({ template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100 } }).document;
  assert.deepEqual(document.dimensions, ['tool', 'subject']);
  assert.ok(document.filters.some((f) => f.field === 'sanctioned_state' && f.value === 'unsanctioned'), 'Q2 does not default to unsanctioned');
  assert.equal(SOURCES['mart.agg_tool_user_period'].dimensions.sanctioned_state.sql, 'ot.sanctioned_state');
});

test('Q2 compiles to the sanction join through the catalogue', () => {
  const text = readText({ query_version: '1', template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100 } });
  assert.match(text, /LEFT JOIN ref\.tool_catalogue tc ON tc\.tool_fingerprint = a\.tool_fingerprint LEFT JOIN ops\.tool_sanction ot ON ot\.tenant_id = a\.tenant_id AND ot\.tool_key = tc\.app_key/);
  assert.match(text, /ot\.sanctioned_state = \$\d+::text/);
});

