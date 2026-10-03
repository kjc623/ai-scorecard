// render.test.mjs — every state renders, and the acceptance criterion is a test:
// "opening index.html against a stub transport renders every state".

import test from 'node:test';
import assert from 'node:assert/strict';
import { renderScreen, renderValue, renderStrip, renderTable, renderSeries, escapeHtml } from '../src/render.js';
import { readState } from '../src/states.js';
import { postureView, devicesView, classesView, teamsView, toolsView, activityView, auditView, toolsView as tv } from '../src/views.js';
import { degradedCollectionView, unavailableView, noApiView } from '../src/unavailable.js';
import { STATE_ENVELOPES, SCENARIO_NAMES, SCENARIOS } from '../src/fixtures.js';
import { createDashboard, SCREENS, refusalFrom } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { scenarioTransport } from '../src/scenarios.js';
import { COMPLETE, FRESH, PARTIAL, envelope } from './helpers.mjs';
import { K } from '../src/vocab.js';

const SHELL = { coverage: PARTIAL, freshness: FRESH };

function dashboardFor(scenario = 'realistic') {
  return createDashboard({ api: createQueryApi({ transport: scenarioTransport(scenario) }), now: () => new Date('2026-10-01T12:00:00Z') });
}

test('every documented result state renders without throwing, and none renders as a blank', () => {
  for (const [name, env] of Object.entries(STATE_ENVELOPES)) {
    const state = readState(env);
    const view = {
      id: name, title: name, question: null, source: null, sourceLabel: null,
      banners: state.banners,
      tiles: [{ label: 'Rows', value: { kind: 'number', text: String(state.rowCount) }, note: null }],
      tables: [], series: [], notes: [],
    };
    const html = renderScreen(view, SHELL);
    assert.ok(html.length > 200, `${name} rendered something`);
    assert.ok(/<div class="strip/.test(html), `${name} carries the strip`);
    if (state.isRefusal) assert.ok(state.banners.length > 0, `${name} explains itself`);
  }
});

test('a refusal screen names the state, the code and the fix', () => {
  const state = readState(STATE_ENVELOPES.refused_too_broad);
  const html = renderScreen(refusalFrom({ resultState: 'query_too_broad', envelope: STATE_ENVELOPES.refused_too_broad }), SHELL);
  assert.ok(/query_too_broad/.test(html));
  assert.ok(/coarsen the bucket to week/.test(html));
  assert.equal(state.rowCount, 0);
});

test('the coverage strip distinguishes complete, partial and not-yet-covered', () => {
  const complete = renderStrip({ coverage: COMPLETE, freshness: FRESH });
  const partial = renderStrip({ coverage: PARTIAL, freshness: FRESH });
  const blind = renderStrip({ coverage: { state: 'not_yet_covered', reason: 'no_coverage_snapshot' }, freshness: FRESH });
  assert.match(complete, /strip-complete/);
  assert.match(partial, /strip-partial/);
  assert.match(blind, /strip-not_yet_covered/);
  assert.equal(new Set([complete, partial, blind]).size, 3);
  for (const html of [complete, partial, blind]) assert.ok(!/class="v-number"/.test(html) || /4620|4180/.test(html));
});

test('a suppressed cell and a zero render into different markup on the same table', () => {
  const state = readState(envelope('ok', {
    data: [
      { tool: 'a', submissions: 900, users: 40 },
      { tool: 'b', submissions: 0, users: 0 },
      { tool: 'c', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 },
    ],
    freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 },
  }));
  const html = renderTable({
    title: 'Tools',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'submissions', label: 'Submissions', kind: 'measure' }],
    rows: state.data.map((row) => ({ row, vocab: {}, suppressed: row.result_state === 'suppressed' })),
    emptyText: 'none', suppressedCells: 1,
  });
  assert.match(html, /<td><span class="v-number">900<\/span><\/td>/);
  assert.match(html, /<td><span class="v-number">0<\/span><\/td>/);
  assert.match(html, /v-suppressed[^>]*>suppressed<span class="v-k">k=5/);
  const suppressedCell = /<tr class="row-suppressed">.*?<\/tr>/s.exec(html)[0];
  assert.ok(!/>0</.test(suppressedCell), 'the suppressed row contains no zero');
});

test('the tile for a floored total says it is a floor, and the tile for an all-suppressed measure says suppressed', () => {
  const floored = readState(envelope('ok', { data: [{ submissions: 10 }, { result_state: 'suppressed', k: 5 }], freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 } }));
  const rows = toolsView(floored).tiles;
  const submissions = rows.find((t) => t.label === 'Submissions');
  assert.equal(submissions.value.kind, 'floor');
  assert.match(submissions.value.text, /≥ 10/);
  assert.match(submissions.note, /floor, not a total/);

  const all = readState(envelope('ok', { data: [{ result_state: 'suppressed', k: 5 }], freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 } }));
  assert.equal(toolsView(all).tiles.find((t) => t.label === 'Submissions').value.kind, 'suppressed');
});

test('a series refuses to exist for an event source and renders hatched floors where cells are suppressed', () => {
  const events = readState(envelope('ok', { data: [{ bucket: '2026-09-30T00:00:00Z', submissions: 1 }], freshness: FRESH, coverage: COMPLETE, meta: { source: 'ingest.submission' } }));
  const series = toolsView(events).series[0];
  assert.equal(series.unavailable, true);
  assert.match(renderSeries(series), /No chart/);

  const agg = readState(envelope('ok', {
    data: [
      { bucket: '2026-09-29T00:00:00Z', tool: 'a', submissions: 10 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'a', result_state: 'suppressed', k: 5 },
    ],
    freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 }, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' },
  }));
  const html = renderSeries(toolsView(agg).series[0]);
  assert.match(html, /bar-floor/);
  assert.match(html, /Hatched columns are floors, not zeroes/);
});

test('the posture screen leads with coverage, freshness and the devices that are not reporting', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('posture', {});
  assert.equal(view.id, 'posture');
  assert.equal(view.tiles[0].label, 'Devices reporting');
  assert.match(view.tiles[0].note, /of 4,620 enrolled devices/);
  assert.equal(view.tables[0].title, 'Devices not reporting');
  assert.ok(view.notes.some((n) => /absence of events is ambiguous/.test(n)));
  const html = renderScreen(view, SHELL);
  assert.ok(/never_reported/.test(html) && /revoked/.test(html) && /stale/.test(html));
});

test('the degraded-collection screen renders what is available and lists what is not, in one panel', () => {
  const devices = readState(envelope('ok', { data: [
    { device: 'd1', liveness: 'reporting', collector_state: 'healthy', spool_depth: 0, spool_dropped_total: 0 },
    { device: 'd2', liveness: 'stale', collector_state: 'degraded', spool_depth: 812, spool_dropped_total: 4412 },
  ], freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }));
  const view = degradedCollectionView({ devices });
  assert.equal(view.tables.length, 2);
  assert.equal(view.tables[0].title, 'Signals this API supports');
  assert.equal(view.tables[1].title, 'Signals this API does not expose');
  assert.ok(view.tables[1].rows.length >= 4, 'the missing signals are listed, not omitted');
  const html = renderScreen(view, SHELL);
  assert.ok(/rejected-envelope histogram|Rejected-envelope/.test(html));
  assert.ok(/Reconciliation drift/.test(html));
  assert.ok(/812/.test(html), 'the spool depth that IS available is shown');
});

test('the catalogue screen renders every gap with its reason, and the no-api screens render theirs', () => {
  const catalogue = renderScreen(unavailableView(), SHELL);
  for (const needle of ['Content search', 'Exports', 'Settings', 'Rejected-envelope', 'Anchored chain head']) {
    assert.ok(catalogue.includes(needle), `${needle} is reported`);
  }
  for (const id of ['search', 'exports', 'settings']) {
    const html = renderScreen(noApiView({ id, title: id, subtitle: '' }), SHELL);
    assert.ok(/No API behind this screen/.test(html), `${id} says why it is empty`);
    assert.ok(!/row-empty/.test(html), `${id} does not render an empty result table`);
  }
});

test('the audit screen reports an integrity alert as an alert, not as a list', async () => {
  const dashboard = dashboardFor('chain_broken');
  const { view } = await dashboard.load('audit', {});
  const html = renderScreen(view, SHELL);
  assert.ok(/audit_chain_broken/.test(html));
  assert.ok(/integrity alert/i.test(html));
  assert.ok(!/Audit entries/.test(html), 'no list is rendered for a broken chain');
});

test('HTML escaping: a hostile tool name or case reference cannot become markup', () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const html = renderTable({
    title: 'T',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'case', label: 'Case' }],
    rows: [{ row: { tool: hostile, case: "'><script>alert(2)</script>" }, vocab: {}, suppressed: false }],
    emptyText: 'none', suppressedCells: 0,
  });
  assert.ok(!html.includes('<img'), 'the tag is escaped');
  assert.ok(!html.includes('<script>'), 'the script tag is escaped');
  assert.match(html, /&lt;img/);
  assert.equal(escapeHtml('&<>"\''), '&amp;&lt;&gt;&quot;&#39;');
});

test('a vocabulary value is never blank: null renders as unknown, which is its own answer', () => {
  const html = renderTable({
    title: 'T',
    columns: [{ key: 'collector_state', label: 'State', kind: 'vocab' }],
    rows: [{ row: { collector_state: null }, vocab: {}, suppressed: false }],
    emptyText: 'none', suppressedCells: 0,
  });
  assert.match(html, /v-vocab-unknown">unknown</);
});

test('the acceptance run: every scenario renders every screen without throwing', async () => {
  const rendered = [];
  for (const scenario of SCENARIO_NAMES) {
    const dashboard = dashboardFor(scenario);
    for (const screen of SCREENS) {
      const { view, shell, gallery } = await dashboard.load(screen.id, { filters: { submission_id: '11111111-2222-4333-8444-555555555551', subject: 'u_1' } });
      if (gallery) continue;
      const html = renderScreen(view, shell);
      assert.ok(typeof html === 'string' && html.length > 0, `${scenario}/${screen.id} rendered`);
      assert.ok(/<div class="strip/.test(html) || view.needsInput, `${scenario}/${screen.id} carries the strip`);
      rendered.push(`${scenario}/${screen.id}`);
    }
  }
  assert.equal(rendered.length, SCENARIO_NAMES.length * (SCREENS.length - 1), 'every scenario × every screen');
  assert.ok(rendered.length >= 180, `expected a broad matrix, got ${rendered.length}`);
});

test('every scenario has a label and is reachable from the gallery', () => {
  for (const name of SCENARIO_NAMES) {
    assert.ok(SCENARIOS[name].label, `${name} has a label`);
    assert.ok(SCENARIOS[name].forced || SCENARIOS[name].answers, `${name} has answers`);
  }
  assert.ok(SCENARIO_NAMES.includes('realistic'));
  assert.ok(SCENARIO_NAMES.includes('blind'), 'the "cannot say" state is in the gallery');
  assert.ok(SCENARIO_NAMES.includes('suppressed'), 'the suppression state is in the gallery');
});

test('k is displayed wherever a suppression is explained', () => {
  const state = readState(STATE_ENVELOPES.all_suppressed);
  const html = renderScreen({ ...toolsView(state), id: 'x', title: 'x' }, SHELL);
  assert.ok(html.includes(`k = ${K}`) || html.includes(`k=${K}`), 'the threshold is on screen');
});

test('classes and teams screens carry their two-measure honesty note', () => {
  const classes = readState(envelope('ok', {
    data: [{ class: 'customer_pii', severity: 'high', submissions: 10, users: 9, max_score: 0.9, degraded_events: 3 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_class_period', extras: { class_total: [{ submissions_total: 4210 }] } },
  }));
  const classHtml = renderScreen(classesView(classes), SHELL);
  assert.ok(/non-additive total/.test(classHtml));
  assert.ok(/could not be classified/.test(classHtml));

  const teams = readState(envelope('ok', {
    data: [{ department: 'Engineering', submissions: 10, users: 9 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_org_period', extras: { org_coverage: [{ users_all: 4210, users_mapped: 2680, org_rows: 96 }] } },
  }));
  const teamHtml = renderScreen(teamsView(teams), SHELL);
  assert.ok(/Unmapped people are an explicit series/.test(teamHtml));
  assert.ok(/1,530 without/.test(teamHtml));
});

test('the activity screen states that its low-merge figure is page-local', () => {
  const state = readState(envelope('ok', {
    data: [{ submission_id: 's', received_at: '2026-09-30T14:00:00Z', first_occurred_at: '2026-09-23T00:00:00Z', merge_confidence: 'low' }],
    freshness: FRESH, coverage: PARTIAL, page: { returned: 1, next_cursor: null }, meta: { source: 'ingest.submission' },
  }));
  const html = renderScreen(activityView(state), SHELL);
  assert.ok(/page count, not a window total/.test(html));
  assert.ok(/Flushed late/.test(html));
});

export { tv };
