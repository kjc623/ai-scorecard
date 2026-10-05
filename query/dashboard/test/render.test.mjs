// render.test.mjs — every state renders, and the acceptance criterion is a test:
// "opening index.html against a stub transport renders every state".

import test from 'node:test';
import assert from 'node:assert/strict';
import { renderScreen, renderValue, renderTable, renderSeries, escapeHtml } from '../src/render.js';
import { readState } from '../src/states.js';
import { devicesView, classesView, teamsView, toolsView, activityView, auditView, toolsView as tv } from '../src/views.js';
import { unavailableView, noApiView } from '../src/unavailable.js';
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
  assert.match(html, /<td class="num"><span class="cell-bar"[^>]*><\/span><span class="v-number">900<\/span><\/td>/);
  assert.match(html, /<td class="num"><span class="v-number">0<\/span><\/td>/, 'a zero is a number, and draws no bar');
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
  assert.match(submissions.note, /^Floor · 1 suppressed$/);

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

test('the overview summarises usage, data classes and findings beside the enrolled denominator', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('posture', {});
  assert.equal(view.id, 'posture');
  assert.equal(view.title, 'Overview');
  assert.deepEqual(view.tiles.map((t) => t.label), ['Submissions', 'People (lower bound)', 'Open findings', 'Devices reporting']);
  assert.match(view.tiles[3].note, /of 4,620 enrolled devices/);
  assert.deepEqual(view.tables.map((t) => t.title), ['Tools', 'Data classes', 'Findings']);
  assert.equal(view.tables[0].href, '#tools');
  const titles = view.banners.map((b) => b.title);
  assert.equal(new Set(titles).size, titles.length, 'a warning every read carries is said once');
});

test('merged screens are a switch on the screen that absorbed them, and old addresses still resolve', async () => {
  const dashboard = dashboardFor();
  assert.equal((await dashboard.load('tools', { filters: { view: 'classes' } })).view.id, 'classes');
  assert.equal((await dashboard.load('tools', { filters: { view: 'unsanctioned' } })).view.id, 'unsanctioned');
  assert.equal((await dashboard.load('classes', {})).view.id, 'classes');
  const all = (await dashboard.load('devices', {})).view;
  const attention = (await dashboard.load('degraded', {})).view;
  assert.equal(attention.id, 'devices');
  assert.ok(attention.tables[0].rows.length < all.tables[0].rows.length);
  assert.ok(attention.tables[0].rows.every(({ row }) => row.status !== 'reporting'));
});

test('the devices screen says the fleet, what needs attention, and each device in plain words', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('devices', {});
  assert.deepEqual(view.tiles.map((x) => x.label), ['Devices enrolled', 'Need attention']);
  assert.equal(view.tiles[0].value.text, '4,620');
  assert.deepEqual(view.tiles[0].split.map((p) => [p.label, p.count]), [['Reporting', 4180], ['Not reporting', 440]]);
  assert.equal(view.tiles[1].href, '#devices?status=attention');
  assert.deepEqual(view.tables.map((x) => x.title), ['Devices']);
  assert.deepEqual(view.tables[0].columns.map((c) => c.label), ['Device', 'User', 'Status', 'Last seen', 'Agent version', 'Mode', 'OS', 'Management', '']);
  assert.deepEqual(view.filters.map((f) => f.label), ['Status', 'OS', 'Management']);
  const html = renderScreen(view, SHELL);
  assert.match(html, /Never checked in/);
  assert.match(html, /Quiet since 2026-09-29/);
  assert.match(html, /days ago/);
  assert.match(html, /href="explore\.html#events\?device=/);
  // The device is named by hostname, the UUID stays on hover, and the user, version and mode are
  // shown (ADR 0021; backlog/04-device-identity).
  assert.match(html, /FIN-LAPTOP-07/);
  assert.match(html, /title="9f1c0b6e-0000-4000-8000-000000000001"/);
  assert.match(html, /alice@contoso\.example/);
  assert.match(html, /1\.4\.2/);
  assert.match(html, /m3/);
  assert.ok(!/Spool|watermark|Collector/i.test(html), 'pipeline internals are not on this screen');
  const windowsOnly = (await dashboard.load('devices', { filters: { device_os: 'windows' } })).view;
  assert.ok(windowsOnly.tables[0].rows.every(({ row }) => row.device_os === 'windows'));
});

// docs/04 §3.7: the fleet card and "Need attention" must describe the same population. When the read
// returns fleet-wide counts by status (meta.extras.device_status), both cards use it; the loaded
// page is never the denominator.
test('the two Devices cards agree when the read returns fleet-wide counts by status', async () => {
  const state = readState(envelope('ok', {
    data: [
      { device: 'd1', liveness: 'reporting', collector: 'egress_proxy', collector_state: 'healthy', last_seen_at: '2026-10-04T11:00:00Z' },
      { device: 'd2', liveness: 'stale', collector: null, collector_state: null, last_seen_at: '2026-09-29T02:11:00Z' },
    ],
    freshness: FRESH,
    coverage: PARTIAL,
    meta: {
      source: 'mart.v_device_liveness',
      extras: {
        device_status: [{
          devices_enrolled: 10, reporting: 6, never_reported: 2, stale: 1, degraded: 1, tampered: 0, revoked: 3,
        }],
      },
    },
  }));
  const view = devicesView(state);
  assert.equal(view.tiles[0].value.text, '10');
  assert.deepEqual(view.tiles[0].split.map((p) => [p.label, p.count]), [['Reporting', 6], ['Not reporting', 4]]);
  assert.equal(view.tiles[1].value.text, '4');
  assert.equal(
    view.tiles[1].value.text,
    view.tiles[0].split.find((p) => p.label === 'Not reporting').count.toLocaleString('en-US'),
    'need attention and not-reporting must be the same population',
  );
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
      rendered.push(`${scenario}/${screen.id}`);
    }
  }
  assert.equal(rendered.length, SCENARIO_NAMES.length * (SCREENS.length - 1), 'every scenario × every screen');
  assert.ok(rendered.length >= 90, `expected a broad matrix, got ${rendered.length}`);
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
