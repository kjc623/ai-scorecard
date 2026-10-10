// render.test.mjs — every documented state renders on every screen, and the screens say what they show.

import test from 'node:test';
import assert from 'node:assert/strict';
import { renderScreen, renderValue, renderTable, renderSeries, escapeHtml } from '../src/render.js';
import { readState } from '../src/states.js';
import { devicesView, classesView, postureView, teamsView, toolsView, eventView } from '../src/views.js';
import { STATE_ENVELOPES, SCENARIO_NAMES, RECORD_ROWS, fixtureTransport } from './fixtures.mjs';
import { createDashboard, SCREENS, refusalFrom } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { COMPLETE, FRESH, PARTIAL, envelope } from './helpers.mjs';

const SHELL = { coverage: PARTIAL, freshness: FRESH };

function dashboardFor(scenario = 'realistic') {
  return createDashboard({ api: createQueryApi({ transport: fixtureTransport(scenario) }), now: () => new Date('2026-10-01T12:00:00Z') });
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

test('a small count, a zero and an absent measure render into different markup on the same table', () => {
  const state = readState(envelope('ok', {
    data: [
      { tool: 'a', submissions: 900, users: 40 },
      { tool: 'b', submissions: 0, users: 0 },
      { tool: 'c', submissions: 3, users: 2 },
      { tool: 'd' },
    ],
    freshness: FRESH, coverage: COMPLETE,
  }));
  const html = renderTable({
    title: 'Tools',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'submissions', label: 'Submissions', kind: 'measure' }],
    rows: state.data.map((row) => ({ row, vocab: {} })),
    emptyText: 'none',
  });
  assert.match(html, /<td class="num"><span class="cell-bar"[^>]*><\/span><span class="v-number">900<\/span><\/td>/);
  assert.match(html, /<td class="num"><span class="v-number">0<\/span><\/td>/, 'a zero is a number, and draws no bar');
  assert.match(html, /<td class="num"><span class="cell-bar"[^>]*><\/span><span class="v-number">3<\/span><\/td>/, 'a small count is a number like any other');
  assert.match(html, /<td class="num"><span class="v-absent"[^>]*>—<\/span><\/td>/, 'an absent measure is neither a number nor a zero');
});

test('a tool cell shows the name, and the fingerprint only beside an unrecognised tool', () => {
  const state = readState(envelope('ok', {
    data: [
      { tool: 'tls_b6681b043244c43f', tool_name: 'Claude Code', submissions: 40 },
      { tool: 'tls_2a942648fee3bbd5', tool_name: 'Unrecognised tool', submissions: 3 },
      { tool: 'legacy_fingerprint', submissions: 1 },
    ],
    freshness: FRESH, coverage: COMPLETE,
  }));
  const html = renderTable({
    title: 'Tools',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'submissions', label: 'Submissions', kind: 'measure' }],
    rows: state.data.map((row) => ({ row })),
    emptyText: 'none',
  });
  // A known tool shows its name, with the fingerprint on hover only.
  assert.match(html, /<span class="v-text" title="Tool fingerprint tls_b6681b043244c43f">Claude Code<\/span><\/td>/);
  // An unknown fingerprint is not silently echoed as a plausible name: it says so, and exposes the raw.
  assert.match(html, /Unrecognised tool<\/span> <span class="v-mono v-tool-fp">tls_2a942648fee3bbd5/);
  // A row with no resolved name falls back to the fingerprint, which is all there is.
  assert.match(html, /<span class="v-text v-mono" title="Tool fingerprint">legacy_fingerprint<\/span>/);
});

test('the tile for a total counts a small cell in full, and says absent when no cell carries the measure', () => {
  const mixed = readState(envelope('ok', { data: [{ submissions: 10 }, { submissions: 2 }], freshness: FRESH, coverage: COMPLETE }));
  const submissions = toolsView(mixed).tiles.find((t) => t.label === 'Submissions');
  assert.equal(submissions.value.kind, 'number');
  assert.equal(submissions.value.text, '12');
  assert.equal(submissions.note, null);

  const none = readState(envelope('ok', { data: [{ tool: 'a' }], freshness: FRESH, coverage: COMPLETE }));
  assert.equal(toolsView(none).tiles.find((t) => t.label === 'Submissions').value.kind, 'absent');
});

test('a series refuses to exist for an event source and draws every bucket to scale, small ones included', () => {
  const events = readState(envelope('ok', { data: [{ bucket: '2026-09-30T00:00:00Z', submissions: 1 }], freshness: FRESH, coverage: COMPLETE, meta: { source: 'ingest.submission' } }));
  const series = toolsView(events).series[0];
  assert.equal(series.unavailable, true);
  assert.match(renderSeries(series), /No chart/);

  const agg = readState(envelope('ok', {
    data: [
      { bucket: '2026-09-29T00:00:00Z', tool: 'a', submissions: 10 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'a', submissions: 2 },
    ],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' },
  }));
  const html = renderSeries(toolsView(agg).series[0]);
  assert.match(html, /data-tip="2026-09-29 · 10"[^>]*><span class="bar-fill" style="--h:100"/);
  assert.match(html, /data-tip="2026-09-30 · 2"[^>]*><span class="bar-fill" style="--h:20"/, 'a small bucket is drawn at its own height');
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
  assert.ok(!view.banners.some((b) => b.about === 'coverage'), 'coverage is said by the devices reporting tile, not a banner');
});

test('an overview panel whose read is not yet covered says so instead of "none"', () => {
  const blind = { resultState: 'not_yet_covered', data: [], banners: [], meta: {} };
  const view = postureView({ devices: null, tools: blind, classes: null, findings: null });
  assert.match(view.tables[0].emptyText, /^Not yet covered\./);
  assert.equal(view.tables[2].emptyText, 'No finding was raised in this window.');
});

test('Usage switches between tools, data classes and unsanctioned use, and Devices narrows to what needs attention', async () => {
  const dashboard = dashboardFor();
  assert.equal((await dashboard.load('tools', { filters: { view: 'classes' } })).view.id, 'classes');
  assert.equal((await dashboard.load('tools', { filters: { view: 'unsanctioned' } })).view.id, 'unsanctioned');
  assert.equal((await dashboard.load('tools', {})).view.id, 'tools');
  const all = (await dashboard.load('devices', {})).view;
  const attention = (await dashboard.load('devices', { filters: { status: 'attention' } })).view;
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
  assert.deepEqual(view.tables[0].columns.map((c) => c.label), ['Device', 'User', 'Directory name', 'Status', 'Collectors', 'Last seen', 'Agent version', 'Mode', 'OS', 'Management', '']);
  assert.equal(view.tables[0].rows.length, 4, 'one row per device');
  assert.deepEqual(view.filters.map((f) => f.label), ['Status', 'OS', 'Management']);
  const html = renderScreen(view, SHELL);
  assert.match(html, /Never checked in/);
  assert.match(html, /Quiet since 2026-09-29/);
  assert.match(html, /days ago/);
  assert.match(html, /href="explore\.html#events\?device=/);
  // The device is named by hostname, the UUID stays on hover, and the user, version and mode are shown.
  assert.match(html, /FIN-LAPTOP-07/);
  assert.match(html, /title="9f1c0b6e-0000-4000-8000-000000000001"/);
  assert.match(html, /alice@contoso\.example/);
  assert.match(html, /Alice Smith/, 'the directory display name is shown beside the account name');
  assert.match(html, /1\.4\.2/);
  assert.match(html, /m3/);
  assert.match(html, /2 healthy/, 'a device sums its collectors by state');
  assert.match(html, /1 degraded · 1 healthy/, 'the worst state leads');
  assert.ok(!/Spool|watermark/i.test(html), 'pipeline internals are not on this screen');
  const windowsOnly = (await dashboard.load('devices', { filters: { device_os: 'windows' } })).view;
  assert.ok(windowsOnly.tables[0].rows.every(({ row }) => row.device_os === 'windows'));
});

// The fleet card and "Need attention" must describe the same population. When the read returns
// fleet-wide counts by status (meta.extras.device_status), both cards use it; the loaded page is
// never the denominator.
test('the two Devices cards agree when the read returns fleet-wide counts by status', async () => {
  const state = readState(envelope('ok', {
    data: [
      { device: 'd1', liveness: 'reporting', collectors_reporting: 1, collectors_healthy: 1, collectors_degraded: 0, collectors_absent: 0, collectors_tampered: 0, last_seen_at: '2026-10-04T11:00:00Z' },
      { device: 'd2', liveness: 'stale', collectors_reporting: 0, collectors_healthy: 0, collectors_degraded: 0, collectors_absent: 0, collectors_tampered: 0, last_seen_at: '2026-09-29T02:11:00Z' },
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
    rows: [{ row: { tool: hostile, case: "'><script>alert(2)</script>" }, vocab: {} }],
    emptyText: 'none',
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
    rows: [{ row: { collector_state: null }, vocab: {} }],
    emptyText: 'none',
  });
  assert.match(html, /v-vocab-unknown">unknown</);
});

test('the acceptance run: every scenario renders every screen without throwing', async () => {
  const rendered = [];
  for (const scenario of SCENARIO_NAMES) {
    const dashboard = dashboardFor(scenario);
    for (const screen of SCREENS) {
      const { view, shell, admin } = await dashboard.load(screen.id, { filters: { submission_id: '11111111-2222-4333-8444-555555555551', subject: 'u_1' } });
      // Settings → Deployment is not a query screen: it reads the admin API through its own
      // controller (test/deployment.test.mjs).
      if (admin) continue;
      const html = renderScreen(view, shell);
      assert.ok(typeof html === 'string' && html.length > 0, `${scenario}/${screen.id} rendered`);
      rendered.push(`${scenario}/${screen.id}`);
    }
  }
  const queryScreens = SCREENS.filter((s) => s.kind !== 'admin').length;
  assert.equal(rendered.length, SCENARIO_NAMES.length * queryScreens, 'every scenario × every query screen');
  assert.ok(rendered.length >= 70, `expected a broad matrix, got ${rendered.length}`);
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
    data: [{ team: '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001', team_name: 'Engineering', submissions: 10, users: 9 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period', extras: { team_coverage: [{ users_all: 4210, users_in_teams: 2680, teams: 4 }] } },
  }));
  const teamHtml = renderScreen(teamsView(teams, { manageHref: '#directory' }), SHELL);
  assert.ok(/1,530 are in none/.test(teamHtml), 'people in no team are said, not dropped');
  assert.ok(/Engineering/.test(teamHtml));
  assert.ok(teamHtml.includes('href="#directory"'), 'an admin reaches team management from the page');
  const none = readState(envelope('empty', {
    data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period', extras: { team_coverage: [{ users_all: 12, users_in_teams: 0, teams: 0 }] } },
  }));
  assert.ok(/No team exists yet/.test(renderScreen(teamsView(none), SHELL)));
});

test('the event detail screen reads the record as query-api answers it: one row per observation', async () => {
  const state = readState({ ...STATE_ENVELOPES.stale, data: RECORD_ROWS, meta: { source: 'ingest.submission', content_state: 'uploaded' } });
  const view = eventView(state);
  assert.equal(view.tiles.find((t) => t.label === 'Content state').value.text, 'uploaded');
  assert.equal(view.tiles.find((t) => t.label === 'Mode').value.text, 'm2', 'the mode is the record\'s collection_mode');
  assert.equal(view.tiles.find((t) => t.label === 'Routes').value.text, '2');
  const [metadata, routes] = view.tables;
  const fields = metadata.rows.map(({ row }) => row.field);
  assert.ok(fields.includes('user_ref') && fields.includes('policy_action') && fields.includes('tool_fingerprint'));
  for (const column of ['observation_event_id', 'observation_source', 'direction', 'policy_decision']) assert.ok(!fields.includes(column), `${column} describes an observation, not the submission`);
  assert.ok(fields.includes('observation_count'), 'the route count is a fact about the submission');
  assert.equal(routes.rows.length, 2, 'both observation rows are routes, the first included');
  assert.deepEqual(routes.rows.map(({ row }) => row.observation_source), ['ext.page_context', 'proxy.tls']);

  const dashboard = dashboardFor();
  const loaded = (await dashboard.load('event', { filters: { submission_id: RECORD_ROWS[0].submission_id } })).view;
  assert.equal(loaded.tables[1].rows.length, 2);

  const none = eventView(readState({ ...STATE_ENVELOPES.stale, data: [{ ...RECORD_ROWS[0], observation_event_id: null, observation_source: null }], meta: { source: 'ingest.submission' } }));
  assert.equal(none.tables[1].rows.length, 0, 'a submission with no observation has no routes');
});
