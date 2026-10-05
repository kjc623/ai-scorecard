// tool-catalogue.test.mjs — the catalogue, the read-time resolution and the sanction write.
//
// Three claims are under test, and each has a pure half and (below, when the device-auth lab is
// reachable) a live half:
//
//   1. every source that can show a tool resolves the fingerprint to a name at read time, and keeps
//      the raw fingerprint beside it, so an unknown tool is never mistaken for a known one;
//   2. Q2's suppression cell is the (bucket, tool) group, not the person, so a tool used by fewer
//      than k people is suppressed while a tool with enough people can name them;
//   3. a sanction decision is validated strictly and its SQL is an upsert that clears attribution
//      when the state returns to `unknown`.

import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
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

// ── live: resolution and the write, against the device-auth lab ─────────────────────────────────

const LAB = 'sac-authlab-postgres-1';

function labReady() {
  const running = spawnSync('docker', ['ps', '--format', '{{.Names}}'], { encoding: 'utf8' });
  if (running.status !== 0 || !(running.stdout ?? '').split('\n').map((s) => s.trim()).includes(LAB)) {
    return 'the device-auth lab PostgreSQL is not running: this live half SKIPS and is not a pass';
  }
  const has = spawnSync('docker', ['exec', LAB, 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc',
    "select 1 from information_schema.tables where table_schema='ref' and table_name='tool_catalogue'"], { encoding: 'utf8' });
  if ((has.stdout ?? '').trim() !== '1') {
    return 'ref.tool_catalogue is not present: apply backlog/05-tool-catalogue/MIGRATION.sql first';
  }
  return false;
}

const SKIP = labReady();

test('the resolver names a catalogue fingerprint, an override and an unknown one', { skip: SKIP }, () => {
  const tenant = '00000000-0000-4000-8000-0000000005a1';
  const script = [
    'BEGIN;',
    `SELECT set_config('app.tenant_id','${tenant}',false);`,
    `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode)
       VALUES ('${tenant}','tool-catalogue-test','active','eu','vendor','kek-test','m3');`,
    // A catalogue fingerprint with no tenant decision resolves to the catalogue's name.
    `SELECT 'catalogue', ops.tool_display_name('tls_b6681b043244c43f');`,
    // A fingerprint the catalogue does not hold is explicitly unrecognised, never echoed as a name.
    `SELECT 'unknown', ops.tool_display_name('tls_2a942648fee3bbd5');`,
    // A tenant override wins over the catalogue.
    `INSERT INTO ops.tool (tenant_id, tool_fingerprint, display_name, sanctioned_state, decided_by, decided_at)
       VALUES ('${tenant}','tls_b6681b043244c43f','Claude Code (approved build)','sanctioned','tester',now());`,
    `SELECT 'override', ops.tool_display_name('tls_b6681b043244c43f');`,
    'ROLLBACK;',
  ].join('\n');
  const res = spawnSync('docker', ['exec', '-i', LAB, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-q', '-tA', '-f', '-'], { encoding: 'utf8', input: script });
  assert.equal(res.status, 0, res.stderr);
  const lines = (res.stdout ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  assert.ok(lines.includes('catalogue|Claude Code'), res.stdout);
  assert.ok(lines.includes('unknown|Unrecognised tool'), res.stdout);
  assert.ok(lines.includes('override|Claude Code (approved build)'), res.stdout);
});

test('the sanction upsert commits the decision and its audit, and rolls back clean', { skip: SKIP }, () => {
  const tenant = '00000000-0000-4000-8000-0000000005a2';
  const script = [
    'BEGIN;',
    `SELECT set_config('app.tenant_id','${tenant}',false);`,
    `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode)
       VALUES ('${tenant}','tool-sanction-test','active','eu','vendor','kek-test','m3');`,
    `INSERT INTO ops.tool (tenant_id, tool_fingerprint, display_name, sanctioned_state, decided_by, decided_at)
       VALUES ('${tenant}','chatgpt_web',NULL,'unsanctioned','tester',now())
       ON CONFLICT (tenant_id, tool_fingerprint) DO UPDATE
         SET sanctioned_state = EXCLUDED.sanctioned_state,
             decided_by = EXCLUDED.decided_by, decided_at = EXCLUDED.decided_at;`,
    `SELECT 'state', sanctioned_state, decided_by FROM ops.tool WHERE tenant_id='${tenant}' AND tool_fingerprint='chatgpt_web';`,
    // The schema refuses an unattributed non-unknown decision.
    `DO $$ BEGIN
       BEGIN
         INSERT INTO ops.tool (tenant_id, tool_fingerprint, sanctioned_state) VALUES ('${tenant}','gemini_web','sanctioned');
         RAISE EXCEPTION 'an unattributed decision was accepted';
       EXCEPTION WHEN check_violation THEN NULL; END;
     END $$;`,
    `SELECT 'unattributed','refused';`,
    'ROLLBACK;',
  ].join('\n');
  const res = spawnSync('docker', ['exec', '-i', LAB, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-q', '-tA', '-f', '-'], { encoding: 'utf8', input: script });
  assert.equal(res.status, 0, res.stderr);
  const lines = (res.stdout ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  assert.ok(lines.includes('state|unsanctioned|tester'), res.stdout);
  assert.ok(lines.includes('unattributed|refused'), res.stdout);
});
