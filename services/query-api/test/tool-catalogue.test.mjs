// tool-catalogue.test.mjs — the catalogue, the read-time resolution and the sanction write.
//
// Three claims are under test (db.test.mjs runs the resolver against a live database):
//
//   1. every source that can show a tool resolves the fingerprint to a name at read time, and keeps
//      the raw fingerprint beside it, so an unknown tool is never mistaken for a known one;
//   2. Q2's suppression cell is the (bucket, tool) group, not the person, so a tool used by fewer
//      than k people is suppressed while a tool with enough people can name them;
//   3. a sanction decision is validated strictly and its SQL is an upsert that clears attribution
//      when the state returns to `unknown`.

import test from 'node:test';
import assert from 'node:assert/strict';
import { plan } from '../src/plan.js';
import { compile } from '../src/compile.js';
import { validate } from '../src/validate.js';
import { guard } from '../src/guard.js';
import { SOURCES } from '../src/registry.js';
import { expandTemplate } from '../src/templates.js';
import {
  SANCTION_STATES,
  TOOL_SANCTION_ACTION,
  validateSanctionRequest,
  UPSERT_TOOL_SANCTION_SQL,
  toolSanctionAuditStatement,
} from '../src/sanction.js';
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

// ── Q2: the sanctioned join and the tool-level suppression cell ────────────────────────────────

test('Q2 joins the present-tense sanction state and filters to it', () => {
  const document = expandTemplate({ template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100 } }).document;
  assert.deepEqual(document.dimensions, ['tool', 'subject']);
  assert.ok(document.filters.some((f) => f.field === 'sanctioned_state' && f.value === 'unsanctioned'), 'Q2 does not default to unsanctioned');
  assert.equal(SOURCES['mart.agg_tool_user_period'].dimensions.sanctioned_state.sql, 'ot.sanctioned_state');
});

test('Q2 compiles to the tool-cell suppression count and the ops.tool join', () => {
  const text = readText({ query_version: '1', template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100 } });
  assert.match(text, /LEFT JOIN ops\.tool ot ON ot\.tenant_id = a\.tenant_id AND ot\.tool_fingerprint = a\.tool_fingerprint/);
  assert.match(text, /ot\.sanctioned_state = \$\d+::text/);
  assert.match(text, /count\(\*\) OVER \(PARTITION BY a\.bucket_start, a\.tool_fingerprint\)::bigint AS __k_subjects/);
  assert.ok(!text.includes('count(DISTINCT'), 'the tool-cell count must not fall back to the per-row distinct count');
});

test('the tool-cell count falls back to the exact distinct count when subject is not grouped', () => {
  // A query that groups only by tool cannot count grouped rows as subjects: the fallback keeps the
  // number exact rather than pinning every cell at one.
  const text = compiledText({
    query_version: '1',
    source: 'mart.agg_tool_user_period',
    bucket: 'day',
    dimensions: ['tool'],
    measures: ['submissions', 'bytes_total'],
    filters: [],
    window: WINDOW,
    limit: 20,
  });
  assert.match(text, /count\(DISTINCT a\.user_ref\)::bigint AS __k_subjects/);
  assert.ok(!text.includes('OVER (PARTITION BY'));
});

// ── the sanction write ─────────────────────────────────────────────────────────────────────────

test('a sanction request accepts the three states and refuses anything else', () => {
  for (const state of SANCTION_STATES) {
    const ok = validateSanctionRequest({ tool_fingerprint: 'tls_b6681b043244c43f', sanctioned_state: state });
    assert.equal(ok.sanctionedState, state);
  }
  assert.equal(
    rejection(() => validateSanctionRequest({ tool_fingerprint: 'x', sanctioned_state: 'maybe' })).reason,
    'type_mismatch',
  );
  assert.equal(
    rejection(() => validateSanctionRequest({ tool_fingerprint: '', sanctioned_state: 'sanctioned' })).reason,
    'type_mismatch',
  );
  assert.equal(
    rejection(() => validateSanctionRequest({ tool_fingerprint: 'x', sanctioned_state: 'sanctioned', case_reference: 'c' })).reason,
    'unknown_key',
  );
  assert.equal(
    rejection(() => validateSanctionRequest({ tool_fingerprint: 'x', sanctioned_state: 'sanctioned', tenant_id: 'other' })).reason,
    'tenant_in_request',
  );
});

test('the sanction upsert attributes a decision and clears attribution on unknown', () => {
  assert.match(UPSERT_TOOL_SANCTION_SQL, /INSERT INTO ops\.tool/);
  assert.match(UPSERT_TOOL_SANCTION_SQL, /ON CONFLICT \(tenant_id, tool_fingerprint\) DO UPDATE/);
  // `unknown` is the absence of a decision: the schema's tool_decision_attributed check accepts it
  // only because the attribution is cleared in the same statement.
  assert.match(UPSERT_TOOL_SANCTION_SQL, /CASE WHEN \$3::text = 'unknown' THEN NULL ELSE \$4::text END/);
  assert.match(UPSERT_TOOL_SANCTION_SQL, /CASE WHEN \$3::text = 'unknown' THEN NULL ELSE now\(\) END/);
  // A policy decision is not an observation, so last_seen_at is not touched.
  assert.ok(!UPSERT_TOOL_SANCTION_SQL.includes('last_seen_at'));
});

test('a sanction writes a tool.sanction audit row naming the previous state', () => {
  const audit = toolSanctionAuditStatement({
    actorId: 'analyst@example',
    toolFingerprint: 'tls_b6681b043244c43f',
    sanctionedState: 'unsanctioned',
    previousState: 'unknown',
    displayName: null,
    note: 'not approved',
    caseReference: 'CASE-1',
  });
  assert.equal(TOOL_SANCTION_ACTION, 'tool.sanction');
  assert.ok(audit.text.includes('INSERT INTO ops.audit'));
  assert.deepEqual(audit.params.slice(1, 5), ['analyst@example', 'tool.sanction', 'ops.tool', 'tls_b6681b043244c43f']);
  assert.match(String(audit.params[7]), /"previous_state":"unknown"/);
  assert.match(String(audit.params[7]), /"sanctioned_state":"unsanctioned"/);
});
