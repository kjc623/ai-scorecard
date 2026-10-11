// ranking.test.mjs — the folded, ranked and shared screens: one figure per tool or team over the
// window, each opening its own screen, with the honesty rules kept through the fold.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderRanking, renderScreen, renderShare, renderSparkline, renderTable } from '../src/render.js';
import { devicesView, foldCells, rankTools, sanctionShare, teamView, teamsView, toolsView } from '../src/views.js';
import { createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { DEVICE_ROWS, TEAM_MEMBER_ROWS, TEAM_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, PARTIAL, envelope } from './helpers.mjs';

const SHELL = { coverage: PARTIAL, freshness: FRESH };
const NOW = () => new Date('2026-10-01T12:00:00Z');

function dashboardFor(scenario = 'realistic', extra = {}) {
  return createDashboard({ api: createQueryApi({ transport: fixtureTransport(scenario) }), now: NOW, ...extra });
}

test('folding sums an additive measure over the buckets, keeps a distinct count at its largest cell, and leaves an absent measure absent', () => {
  const state = readState(envelope('ok', {
    data: [
      { bucket: '2026-09-29T00:00:00Z', tool: 'a', submissions: 10, users: 4, bytes_total: 100 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'a', submissions: 2, users: 6, bytes_total: 50 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'b', submissions: 0, users: 0 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'c' },
    ],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const groups = foldCells(state, 'tool');
  const a = groups.find((g) => g.key === 'a');
  assert.deepEqual(a.measures, { submissions: 12, bytes_total: 150, users: 6 }, 'summed, and the distinct count is the largest cell, not 10');
  assert.deepEqual(a.trend.map((p) => [p.bucket.slice(0, 10), p.value]), [['2026-09-29', 10], ['2026-09-30', 2]]);
  assert.deepEqual(groups.find((g) => g.key === 'b').measures, { submissions: 0, users: 0 }, 'a zero folds to a zero');
  assert.deepEqual(groups.find((g) => g.key === 'c').measures, {}, 'no cell carried a measure, so none is invented');
});

test('the tool ranking folds the window per tool, ranks by submissions, says each share, and links each tool to its screen', () => {
  const state = readState(envelope('ok', {
    data: [
      ...TOOL_ROWS,
      { bucket: '2026-09-29T00:00:00Z', tool: 'tls_b6681b043244c43f', tool_name: 'Claude Code', sanctioned_state: 'unsanctioned', submissions: 188, users: 90, bytes_total: 1_000_000, blocked: 1 },
    ],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' },
  }));
  const ranking = rankTools(state);
  assert.equal(ranking.kind, 'ranking');
  assert.deepEqual(ranking.items.map((i) => i.key), ['tls_f412811be7ac6539', 'tls_b6681b043244c43f', 'shadow_llm_gateway', 'legacy_summariser'], 'largest first, with one line per tool');
  const claude = ranking.items[1];
  assert.equal(claude.value.value, 1000, 'two days of the same tool fold into one figure');
  assert.equal(claude.href, '#tools?tool=tls_b6681b043244c43f');
  assert.equal(claude.chip.key, 'unsanctioned');
  assert.equal(Math.round(claude.share * 1000), Math.round((1000 / 5024) * 1000));
  assert.deepEqual(claude.meta.map((m) => [m.label, m.value.kind === 'number' ? m.value.text ?? m.value.value : m.value.kind]), [['people', 214], ['blocked', 13], ['sent', '40 MB']], 'people is the largest cell, the rest are sums');
  assert.deepEqual(claude.trend.map((p) => p.value), [188, 812]);
  assert.equal(ranking.items[3].value.value, 0, 'a tool with no submissions ranks last with its zero, never dropped');
  const html = renderRanking(ranking);
  assert.match(html, /<a class="rank-link" href="#tools\?tool=tls_f412811be7ac6539">/);
  assert.match(html, /<span class="v-number">4,021<\/span><span class="rank-share">80%<\/span>/);
  assert.match(html, /class="rank-fill" style="--w:100"/);
  assert.match(html, /<svg class="spark"/, 'each tool carries its trend');
  assert.match(html, /v-vocab-unsanctioned" title="unsanctioned">Unsanctioned</);
});

test('a ranking escapes what the API said, and says absent for a tool whose cells carry no submissions', () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const state = readState(envelope('ok', {
    data: [{ tool: hostile, tool_name: hostile, users: 3 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const html = renderRanking(rankTools(state));
  assert.ok(!html.includes('<img'), 'the name is escaped');
  assert.match(html, /&lt;img/);
  assert.match(html, /rank-bar-absent/, 'no submissions measure: no bar is drawn');
  assert.match(html, /<span class="rank-value"><span class="v-absent"/, 'and the value is absent, not zero');
});

test('an empty ranking says why, and an unrecognised tool shows its fingerprint under its name', () => {
  const none = readState(envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  assert.match(renderRanking(rankTools(none, { emptyText: 'No tool was in use in this window.' })), /No tool was in use in this window\./);
  const odd = readState(envelope('ok', {
    data: [{ tool: 'tls_2a942648fee3bbd5', tool_name: 'Unrecognised tool', submissions: 3 }, { tool: 'legacy_fingerprint', submissions: 1 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const ranking = rankTools(odd);
  assert.equal(ranking.items[0].sublabel, 'tls_2a942648fee3bbd5', 'the fingerprint is the only thing that tells two unrecognised tools apart');
  assert.equal(ranking.items[1].label, 'legacy_fingerprint');
  assert.equal(ranking.items[1].mono, true, 'a bare fingerprint is shown as one');
});

test('the sanction share splits the window\'s submissions by the tool\'s present sanction, with unknown as its own part', () => {
  const state = readState(envelope('ok', { data: TOOL_ROWS, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  const share = sanctionShare(state);
  assert.equal(share.total, 4836);
  assert.deepEqual(share.parts.map((p) => [p.key, p.count]), [['sanctioned', 4021], ['unsanctioned', 812], ['unknown', 3]]);
  const html = renderShare(share);
  assert.match(html, /role="img" aria-label="Sanctioned tools 83%, Unsanctioned tools 17%, No decision yet &lt;1%"/);
  assert.match(html, /dist-seg v-vocab-unsanctioned/);
  assert.match(html, /<span class="dist-n">3<\/span><span class="dist-pct">&lt;1%<\/span>/, 'a small part is said, never rounded to nothing');
  const none = readState(envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  assert.match(renderShare(sanctionShare(none)), /No submissions in this window\./);
});

test('a sparkline draws every point to one scale and lists the values for a reader who cannot see it', () => {
  const html = renderSparkline([
    { bucket: '2026-09-29T00:00:00Z', value: 10 },
    { bucket: '2026-09-30T00:00:00Z', value: 0 },
    { bucket: '2026-10-01T00:00:00Z', value: 5 },
  ], { width: 100, height: 30 });
  assert.match(html, /aria-label="2026-09-29: 10, 2026-09-30: 0, 2026-10-01: 5"/);
  assert.match(html, /<path class="spark-line" d="M3\.5,3\.5 L50\.0,26\.5 L96\.5,15\.0"/, 'the largest value is at the top, zero on the floor, the rest between');
  assert.match(html, /<circle class="spark-end" cx="96\.5" cy="15\.0"/);
  assert.equal(renderSparkline([{ bucket: 'x', value: 1 }]), '', 'one point is no shape');
});

test('the Tools screen is tiles, the share and the ranking, with the chart below and no per-bucket table', async () => {
  const { view } = await dashboardFor().load('tools', {});
  assert.deepEqual(view.tiles.map((t) => t.label), ['Submissions', 'People (lower bound)', 'Blocked', 'Data sent']);
  assert.equal(view.tiles[1].value.text, '1,877', 'people is the largest cell, not the sum over tools');
  assert.deepEqual(view.blocks.map((b) => b.kind), ['share', 'ranking']);
  assert.deepEqual(view.tables, []);
  assert.equal(view.series[0].title, 'Submissions per day');
  const html = renderScreen(view, SHELL);
  assert.match(html, /Coverage is partial/);
  assert.ok(!/<table/.test(html), 'no table of buckets');
});

test('a tool\'s own screen folds its usage, lists who uses it in the API\'s order, and keeps its findings', async () => {
  const sent = [];
  const transport = fixtureTransport();
  const dashboard = createDashboard({ api: createQueryApi({ transport: { send: (body) => { sent.push(body); return transport.send(body); } } }), now: NOW });
  const { view } = await dashboard.load('tools', { filters: { tool: 'tls_b6681b043244c43f' } });
  assert.equal(view.id, 'tool');
  assert.equal(view.title, 'Claude Code');
  assert.equal(view.subtitle, 'Fingerprint tls_b6681b043244c43f');
  assert.deepEqual(view.badges, [{ key: 'unsanctioned', text: 'Unsanctioned' }]);
  assert.deepEqual(sent.filter((b) => b.template).map((b) => [b.template, b.params.tool, b.params.sanctioned_state]), [
    ['q1_tools_ranked', 'tls_b6681b043244c43f', undefined],
    ['q2_unsanctioned_users', 'tls_b6681b043244c43f', undefined],
    ['q5_findings', 'tls_b6681b043244c43f', undefined],
  ], 'every read is scoped to the tool, and the people read lists its people whatever its sanction');
  const lookup = sent.find((b) => b.source === 'mart.v_person');
  assert.deepEqual(lookup?.filters, [{ field: 'subject', op: 'in', value: ['u_9a02', 'u_4f21'] }], 'the roster\'s references are looked up by name, in one read');
  assert.deepEqual(view.actions.map((a) => a.href), ['explore.html#events?tool=tls_b6681b043244c43f', '#tools']);
  const people = view.tables.find((t) => t.title === 'People using it');
  assert.deepEqual(people.rows.map((r) => r.row.subject), ['u_9a02', 'u_4f21'], 'this tool\'s people, in the API order, not a volume ranking');
  const findings = view.tables.find((t) => t.title === 'Findings');
  assert.deepEqual(findings.rows.map((r) => r.row.rule), ['PAYMENT_CARD_PAN'], 'this tool\'s findings only');
  assert.equal(view.series[0].points[0].value.value, 812, 'the chart is this tool\'s series');
  const html = renderScreen(view, SHELL);
  assert.match(html, /<h2>Claude Code <span class="screen-badges">/);
  assert.match(html, /href="#person\?subject=u_9a02"/);
});

test('a role that may not see people gets the tool\'s totals and is told why the roster is missing', async () => {
  const transport = fixtureTransport();
  const viewer = createDashboard({
    api: createQueryApi({ transport: { send: (body) => (body.template === 'q2_unsanctioned_users'
      ? { api_version: '1', query_version: '1', result_state: 'unauthorised_role', error: { code: 'unauthorised_role', message: 'This role may not read people.' } }
      : transport.send(body)) } }),
    now: NOW,
  });
  const { view } = await viewer.load('tools', { filters: { tool: 'tls_b6681b043244c43f' } });
  assert.equal(view.tiles[0].value.text, '812');
  assert.ok(!view.tables.some((t) => t.title === 'People using it'));
  assert.ok(view.notes.some((n) => /totals only/.test(n)));
});

test('a tool with no cell in the window shows absent figures and reads nobody', async () => {
  const sent = [];
  const empty = createDashboard({
    api: createQueryApi({ transport: { send: (body) => { sent.push(body); return envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }); } } }),
    now: NOW,
  });
  const { view } = await empty.load('tools', { filters: { tool: 'tls_nobody' } });
  assert.equal(view.title, 'tls_nobody');
  assert.equal(view.tiles[0].value.kind, 'absent');
  assert.ok(!sent.some((b) => b.template === 'q2_unsanctioned_users'), 'no sanction to scope the people read to, so it is not made');
});

test('the Teams screen ranks the teams, lists the admin\'s quiet teams with a zero, and keeps the roster under its team', async () => {
  const admin = {
    async directory() {
      return { state: 'available', data: { teams: [
        { team_id: '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001', name: 'Engineering', source: 'group', group_name: 'eng-all', members: 640 },
        { team_id: '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0009', name: 'Marketing', source: 'console', members: 12 },
      ] } };
    },
  };
  const { view } = await dashboardFor('realistic', { admin }).load('teams', { mayManage: true });
  assert.deepEqual(view.tiles.map((t) => t.label), ['Submissions', 'People using AI', 'Teams']);
  assert.deepEqual(view.tiles[1].split.map((p) => [p.label, p.count]), [['In a team', 2680], ['In no team', 1530]]);
  assert.equal(view.tiles[2].href, '#directory');
  const ranking = view.blocks[0];
  assert.deepEqual(ranking.items.map((i) => [i.label, i.value.value]), [['Engineering', 2210], ['Finance', 812], ['Legal', 7], ['Marketing', 0]], 'by submissions, then the team with no usage');
  assert.equal(ranking.items[0].href, '#teams?team=7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001');
  assert.equal(ranking.items[0].sublabel, '640 members · Group eng-all');
  assert.match(ranking.items[3].sublabel, /No usage recorded in this window/);
  const roster = view.tables.find((t) => t.title === 'Submissions by person');
  assert.equal(roster.groupBy, 'team_name');
  const html = renderScreen(view, SHELL);
  assert.match(html, /<tr class="row-group"><th scope="rowgroup" colspan="2">Engineering<\/th><\/tr>/, 'the team heads its people rather than repeating on every line');
  assert.equal((html.match(/row-group/g) ?? []).length, 2, 'one heading per team');
});

test('without the admin list the Teams screen still ranks the teams that have usage', async () => {
  const { view } = await dashboardFor().load('teams', {});
  assert.deepEqual(view.blocks[0].items.map((i) => i.label), ['Engineering', 'Finance', 'Legal']);
  assert.equal(view.tiles[2].value.text, '3', 'the team count comes from the coverage read');
});

test('a team\'s own screen reads that team only, and lists its people by name', async () => {
  const sent = [];
  const transport = fixtureTransport();
  const dashboard = createDashboard({ api: createQueryApi({ transport: { send: (body) => { sent.push(body); return transport.send(body); } } }), now: NOW });
  const { view } = await dashboard.load('teams', { filters: { team: '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001' }, mayManage: true });
  assert.equal(view.id, 'team');
  assert.equal(view.title, 'Engineering');
  assert.equal(sent[0].params.team, '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001');
  assert.deepEqual(sent[1].filters, [{ field: 'team', op: 'in', value: ['7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001'] }], 'the roster read names the one team');
  assert.deepEqual(view.actions.map((a) => a.label), ['All teams', 'Manage teams']);
  assert.equal(view.tiles[0].value.text, '2,210', 'this team\'s figure, not every team\'s');
  assert.equal(view.tiles[2].value.text, '3', 'this team\'s people');
  const roster = view.tables[0];
  assert.deepEqual(roster.columns.map((c) => c.label), ['User', 'Submissions']);
  assert.deepEqual(roster.rows.map((r) => r.row.name), ['Ada Lovelace', 'Grace Hopper', 'u_1b77'], 'by name');
  assert.equal(roster.groupBy, undefined);
});

test('a team known only to the roster is still named, and a team with no row shows absent figures', () => {
  const totals = readState(envelope('ok', { data: TEAM_ROWS, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period' } }));
  const members = readState(envelope('ok', { data: TEAM_MEMBER_ROWS.filter((r) => r.team_name === 'Finance'), freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_team_member_period' } }));
  const view = teamView(totals, { team: '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0003', members });
  assert.equal(view.title, 'Finance');
  assert.equal(view.tiles[0].value.text, '812');
  const quiet = teamView(readState(envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period' } })), { team: 'x', members });
  assert.equal(quiet.title, 'Finance', 'named by the roster when the totals carry no row');
  assert.equal(quiet.tiles[0].value.kind, 'absent');
});

test('teams the totals do not know are not invented when the read could not say', () => {
  const blind = readState(envelope('not_yet_covered', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period' } }));
  const view = teamsView(blind, { teams: [{ team_id: 't1', name: 'Quiet', source: 'console', members: 3 }] });
  assert.deepEqual(view.blocks[0].items, [], 'no zero is claimed for a window the system cannot answer for');
  assert.match(view.blocks[0].emptyText, /^Not yet covered\./);
});

test('a device row is three cells: the device and what it runs, the person on it, and its state with when it was last seen', () => {
  const state = readState(envelope('ok', { data: DEVICE_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }));
  const view = devicesView(state, { now: NOW() });
  const [fin, mac, silent, ops] = view.tables[0].rows.map((r) => r.row);
  assert.deepEqual(fin.device_cell, { primary: 'FIN-LAPTOP-07', mono: false, title: '9f1c0b6e-0000-4000-8000-000000000001', secondary: 'Windows 11 · Managed · Agent 1.4.2 · M3 · prompt' });
  assert.deepEqual(fin.user_cell, { primary: 'Alice Smith', secondary: 'alice@contoso.example', title: 'u_4f21' });
  assert.deepEqual(fin.status_cell, { chip: { key: 'reporting', text: 'Reporting' }, secondary: 'Last seen 3 min ago · Collectors 2 healthy' });
  assert.deepEqual(mac.status_cell, { chip: { key: 'degraded', text: 'Degraded' }, secondary: 'Last seen 2 days ago · Collectors 1 degraded · 1 healthy' }.chip ? { chip: { key: mac.status, text: mac.status_text }, secondary: 'Last seen 2 days ago · Collectors 1 degraded · 1 healthy' } : null);
  assert.equal(silent.device_cell.primary, '9f1c0b6e…', 'a device with no hostname is named by the start of its id');
  assert.equal(silent.device_cell.mono, true);
  assert.equal(silent.user_cell.primary, null, 'nobody has used it');
  assert.deepEqual(silent.status_cell, { chip: { key: 'never_reported', text: 'Never checked in' }, secondary: null });
  assert.equal(ops.status_cell.chip.key, 'revoked');
  assert.equal(view.tables[0].rowHref, 'activity');
  const html = renderScreen(view, SHELL);
  assert.match(html, /<tr class="row-link" tabindex="0" data-href="explore\.html#events\?device=9f1c0b6e-0000-4000-8000-000000000001">/, 'the row opens the device\'s activity');
  assert.match(html, /<span class="cell-stack" title="9f1c0b6e-0000-4000-8000-000000000001"><span class="cell-primary">FIN-LAPTOP-07<\/span><span class="cell-secondary">Windows 11 · Managed · Agent 1\.4\.2 · M3 · prompt<\/span><\/span>/);
  assert.match(html, /<span class="cell-stack"><span class="v-vocab v-vocab-never_reported" title="never_reported">Never checked in<\/span><\/span>/, 'a state chip can be the first line');
  assert.match(html, /<span class="cell-stack"><span class="v-absent">—<\/span><\/span>/, 'no user is an absence, not a blank');
});

test('the attention tile says why devices need attention, each reason opening the devices it counts, and the status filter takes a reason', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('devices', {});
  assert.deepEqual(view.tiles[1].split.map((p) => [p.key, p.count, p.href]), [
    ['stale', 1, '#devices?status=stale'], ['never_reported', 1, '#devices?status=never_reported'], ['revoked', 1, '#devices?status=revoked'],
  ]);
  assert.equal(view.tiles[1].moreLabel, 'Show them');
  const stale = (await dashboard.load('devices', { filters: { status: 'stale' } })).view;
  assert.deepEqual(stale.tables[0].rows.map((r) => r.row.hostname), ['MAC-DESIGN-2']);
  assert.equal(stale.filters[0].current, 'stale');
  assert.ok(stale.filters[0].items.some((i) => i.id === 'stale' && i.label === 'Quiet'), 'the chosen reason is on the filter row');
  const html = renderScreen(view, SHELL);
  assert.match(html, /<a class="dist-link" href="#devices\?status=stale">Quiet<span class="dist-n">1<\/span><\/a>/);
  assert.match(html, /<a class="tile-more" href="#devices\?status=attention">Show them<\/a>/);
  assert.ok(!/<a class="tile card tile-link"[^>]*href="#devices\?status=attention"/.test(html), 'a tile holding links is not itself a link');
});

test('a tile with a trend draws it beside the figure, and the screen names where it reads from in its notes', async () => {
  const { view } = await dashboardFor().load('tools', {});
  assert.equal(view.tiles[0].trend, undefined, 'one bucket is no trend');
  const html = renderScreen(view, SHELL);
  assert.match(html, /Reads from Tool usage \(view\)\./);
  assert.ok(!/Source <code>/.test(html), 'the source is a note, not a line in the header');
  const two = readState(envelope('ok', {
    data: [{ bucket: '2026-09-29T00:00:00Z', tool: 'a', submissions: 10 }, { bucket: '2026-09-30T00:00:00Z', tool: 'a', submissions: 4 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' },
  }));
  const tools = toolsView(two);
  assert.deepEqual(tools.tiles[0].trend.map((p) => p.value), [10, 4]);
  assert.match(renderScreen(tools, SHELL), /<span class="tile-figure"><span class="tile-value"><span class="v-number">14<\/span><\/span><span class="tile-spark"><svg class="spark"/);
});

test('partial coverage is one quiet line under the title; every other banner keeps its box', () => {
  const view = {
    id: 'x', title: 'X', question: null, source: null, sourceLabel: null,
    banners: [
      { level: 'warning', about: 'coverage', title: 'Coverage is partial', text: '4,180 of 4,620 enrolled devices reporting · gaps in 2 categories. Every figure here is a floor, not a total.' },
      { level: 'warning', title: 'Stale aggregate', text: 'These numbers are old.' },
      { level: 'hatched', about: 'coverage', title: 'Not yet covered', text: 'Coverage not yet measured.' },
    ],
    tiles: [], tables: [], series: [], notes: [],
  };
  const html = renderScreen(view, SHELL);
  assert.match(html, /<p class="data-status" role="status"><span class="data-status-item"><strong>Coverage is partial<\/strong> 4,180 of 4,620 enrolled devices reporting/);
  assert.match(html, /<div class="banner banner-warning" role="status"><strong>Stale aggregate<\/strong>/);
  assert.match(html, /<div class="banner banner-hatched" role="status"><strong>Not yet covered<\/strong>/);
  assert.equal((html.match(/Coverage is partial/g) ?? []).length, 1, 'said once');
});

test('a grouped table drops the grouping column from its cells and heads each run; a row link follows the named column', () => {
  const html = renderTable({
    title: 'T',
    columns: [{ key: 'team', label: 'Team' }, { key: 'name', label: 'Person' }, { key: 'n', label: 'N', kind: 'measure' }],
    groupBy: 'team',
    rowHref: 'where',
    rows: [
      { row: { team: 'A', name: 'x', n: 1, where: 'explore.html#events?subject=x' } },
      { row: { team: 'A', name: 'y', n: 2 } },
      { row: { team: null, name: 'z', n: 3, where: '#person?subject=z' } },
    ],
    emptyText: 'none',
  });
  assert.match(html, /<thead><tr><th scope="col">Person<\/th><th scope="col" class="num">N<\/th><\/tr><\/thead>/);
  assert.match(html, /<tr class="row-group"><th scope="rowgroup" colspan="2">A<\/th><\/tr><tr class="row-link" tabindex="0" data-href="explore\.html#events\?subject=x">/);
  assert.match(html, /<tr class="row-group"><th scope="rowgroup" colspan="2"><span class="v-absent">—<\/span><\/th><\/tr>/, 'a missing group is an absence');
  assert.equal((html.match(/row-group/g) ?? []).length, 2);
  assert.equal((html.match(/row-link/g) ?? []).length, 2, 'a row without the column is not a link');
});

test('two or more unrecognised tools are one line in the ranking, and are listed by fingerprint beneath it', () => {
  const state = readState(envelope('ok', {
    data: [
      { bucket: '2026-09-30T00:00:00Z', tool: 'tls_aaaa', tool_name: 'Unrecognised tool', sanctioned_state: 'unknown', submissions: 5, users: 2, bytes_total: 100 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'tls_bbbb', tool_name: 'Unrecognised tool', sanctioned_state: 'unknown', submissions: 3, users: 1, bytes_total: 50 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'tls_cccc', tool_name: 'Unrecognised tool', sanctioned_state: 'unknown', submissions: 1, users: 1 },
      { bucket: '2026-09-30T00:00:00Z', tool: 'tls_f412811be7ac6539', tool_name: 'ChatGPT', sanctioned_state: 'sanctioned', submissions: 4, users: 3 },
    ],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const ranking = rankTools(state);
  assert.deepEqual(ranking.items.map((i) => [i.label, i.value.value, i.href]), [['Unrecognised tools', 9, null], ['ChatGPT', 4, '#tools?tool=tls_f412811be7ac6539']], 'the three fold into one line with no screen of its own');
  assert.equal(ranking.items[0].sublabel, '3 fingerprints the catalogue does not name');
  assert.deepEqual(ranking.items[0].chip, { key: 'unknown', text: 'No decision yet' });
  assert.deepEqual(ranking.items[0].meta[0], { label: 'people', value: { kind: 'number', value: 2 } }, 'people is the largest cell, a floor, not 2+1+1');
  const view = toolsView(state, { preset: 'd30' });
  const listed = view.tables.find((t) => t.title === 'Unrecognised tools');
  assert.deepEqual(listed.rows.map((r) => [r.row.fingerprint_cell.primary, r.row.submissions, r.row.href]), [['tls_aaaa', 5, '#tools?tool=tls_aaaa&preset=d30'], ['tls_bbbb', 3, '#tools?tool=tls_bbbb&preset=d30'], ['tls_cccc', 1, '#tools?tool=tls_cccc&preset=d30']]);
  assert.equal(listed.rowHref, 'href');
  assert.equal(view.blocks[1].items[1].href, '#tools?tool=tls_f412811be7ac6539&preset=d30', 'a drill-down keeps the window it was read in');
  const one = readState(envelope('ok', { data: [state.data[0], state.data[3]], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  assert.equal(toolsView(one).tables.length, 0, 'one unrecognised tool is its own line, so there is nothing to list beneath');
  assert.equal(rankTools(one).items[0].sublabel, 'tls_aaaa');
});

test('a window with no cell draws no chart; the empty ranking says why', () => {
  const none = readState(envelope('empty', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  assert.deepEqual(toolsView(none).series, []);
  assert.deepEqual(teamsView(none).series, []);
  assert.match(toolsView(none).blocks[1].emptyText, /No tool was in use/);
});

test('a quiet team is listed when the read answered, on partial coverage too; a tool with no decision is unknown', async () => {
  const teams = [{ team_id: 't1', name: 'Engineering', source: 'console', members: 1 }];
  const degraded = readState(envelope('coverage_degraded', { data: [], freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.agg_team_period', extras: { team_coverage: [{ users_all: 1, users_in_teams: 0, teams: 1 }] } } }));
  const view = teamsView(degraded, { teams });
  assert.deepEqual(view.blocks[0].items.map((i) => [i.label, i.value.value]), [['Engineering', 0]], 'the team exists, so it is listed, and the coverage line says every figure is a floor');
  assert.match(view.blocks[0].items[0].sublabel, /^1 member · Chosen in the console · No usage recorded in this window$/);
  assert.ok(view.notes.some((n) => n === '1 person used AI in this window; 0 are in at least one team and 1 is in none.'));
  assert.equal(view.banners.filter((b) => b.about === 'coverage').length, 1, 'partial coverage is said once, not as two banners');
  const own = teamView(degraded, { team: 't1', teams, preset: 'd90' });
  assert.equal(own.title, 'Engineering', 'named from the admin list when it has no cell in the window');
  assert.match(own.subtitle, /^1 member · Chosen in the console\./);
  assert.deepEqual(own.series, []);
  assert.equal(own.actions[0].href, '#teams?preset=d90');
});

test('a tool\'s screen marks a tool with no recorded decision as unknown, names its people, and keeps its window', async () => {
  const sent = [];
  const transport = fixtureTransport();
  const api = createQueryApi({ transport: { send: (body) => {
    sent.push(body);
    if (body.source === 'mart.v_person') {
      return { api_version: '1', query_version: '1', result_state: 'ok', data: [{ subject: 'u_9a02', name: 'Bob Jones', directory_name: 'Bob Jones', subject_name: 'bob@contoso.example' }, { subject: 'u_4f21', name: 'u_4f21' }], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_person' } };
    }
    return transport.send(body);
  } } });
  const dashboard = createDashboard({ api, now: NOW });
  const { view } = await dashboard.load('tools', { preset: 'd30', filters: { tool: 'tls_b6681b043244c43f' } });
  const roster = view.tables.find((t) => t.title === 'People using it');
  assert.deepEqual(roster.columns.map((c) => c.label), ['User', 'Submissions', 'Data sent']);
  assert.deepEqual(roster.rows.map((r) => [r.row.subject, r.row.name]), [['u_9a02', 'Bob Jones'], ['u_4f21', null]], 'a name the directory knows; a reference it does not stays a reference');
  const html = renderScreen(view, SHELL);
  assert.match(html, /<a class="cell-link" href="#person\?subject=u_9a02" title="User reference u_9a02">Bob Jones<\/a>/);
  assert.match(html, /<a class="cell-link" href="#person\?subject=u_4f21" title="User reference u_4f21">u_4f21<\/a>/);
  assert.equal(view.actions[1].href, '#tools?preset=d30');
  // No decision recorded: the people read is still made, by tool alone, and the badge says so.
  const undecided = readState(envelope('ok', { data: [{ bucket: '2026-09-30T00:00:00Z', tool: 'tls_zzzz', tool_name: 'Unrecognised tool', submissions: 2, users: 1 }], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' } }));
  const quiet = createDashboard({ api: createQueryApi({ transport: { send: (body) => { sent.push(body); return body.template === 'q1_tools_ranked' ? { ...undecided, result_state: 'ok', api_version: '1', query_version: '1', data: undecided.data, freshness: FRESH, coverage: COMPLETE, meta: undecided.meta } : transport.send(body); } } }), now: NOW });
  const unknown = (await quiet.load('tools', { filters: { tool: 'tls_zzzz' } })).view;
  assert.deepEqual(unknown.badges, [{ key: 'unknown', text: 'No decision yet' }]);
  const read = sent.find((b) => b.template === 'q2_unsanctioned_users' && b.params.tool === 'tls_zzzz');
  assert.ok(read, 'the people read is made for a tool with cells');
  assert.equal(read.params.sanctioned_state, undefined, 'a fingerprint outside the catalogue has no state to scope by');
  const unsanctionedList = (await quiet.load('tools', { filters: { view: 'unsanctioned' } })).view;
  assert.equal(unsanctionedList.id, 'unsanctioned');
  assert.equal(sent.filter((b) => b.template === 'q2_unsanctioned_users' && !b.params.tool).pop()?.params.sanctioned_state, 'unsanctioned', 'the list without a tool is still the unsanctioned question');
});

test('the Overview tiles open the screens they summarise, and the findings table names the user', async () => {
  const { view } = await dashboardFor().load('posture', { preset: 'd30' });
  assert.deepEqual(view.tiles.map((t) => [t.label, t.href ?? null]), [
    ['Submissions', '#tools?preset=d30'], ['People (lower bound)', '#person'], ['Open findings', 'explore.html#findings?review_state=open'], ['Devices reporting', '#devices'],
  ]);
  assert.equal(view.blocks[1].href, '#tools?preset=d30');
  assert.ok(view.blocks[1].items.every((i) => i.href === null || i.href.endsWith('&preset=d30')));
  assert.equal(view.blocks[2].href, '#tools?view=classes&preset=d30');
  const findings = view.tables.find((t) => t.title === 'Findings');
  assert.equal(findings.href, 'explore.html#findings');
  assert.deepEqual(findings.columns.map((c) => c.label), ['Detected', 'Severity', 'Rule', 'User', 'Tool', 'Review']);
});

test('a share of one part is a sentence, and a split draws and links only the parts that count something', () => {
  const all = readState(envelope('ok', {
    data: [{ bucket: '2026-09-30T00:00:00Z', tool: 'tls_f412811be7ac6539', tool_name: 'ChatGPT', sanctioned_state: 'sanctioned', submissions: 40, users: 3 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const html = renderShare(sanctionShare(all));
  assert.match(html, /<p class="share-one">All <span class="v-number">40<\/span> submissions went to sanctioned tools\.<\/p>/);
  assert.ok(!/dist-bar/.test(html), 'one part is the whole, so there is no bar to read');
  const undecided = readState(envelope('ok', {
    data: [{ bucket: '2026-09-30T00:00:00Z', tool: 'tls_zzzz', tool_name: 'Unrecognised tool', sanctioned_state: 'unknown', submissions: 4 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage' },
  }));
  const share = sanctionShare(undecided);
  assert.equal(share.href, '#settings');
  assert.equal(share.moreLabel, 'Decide which tools are sanctioned');
  assert.match(share.note, /No tool has a sanction decision yet/);
  const teams = readState(envelope('ok', {
    data: [{ bucket: '2026-09-30T00:00:00Z', team: 't1', team_name: 'Engineering', submissions: 9, users: 5 }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_team_period', extras: { team_coverage: [{ users_all: 5, users_in_teams: 5, teams: 1 }] } },
  }));
  const page = renderScreen(teamsView(teams), SHELL);
  assert.match(page, /<div class="dist-bar tile-split" aria-hidden="true"><span class="dist-seg v-vocab-in_team" style="flex-grow:5"><\/span><\/div>/, 'the empty part draws no segment');
  assert.match(page, /<span class="dist-dot v-vocab-no_team" aria-hidden="true"><\/span>In no team<span class="dist-n">0<\/span><\/li>/, 'and stays in the legend as a zero, not a link');
});

test('the Devices filters are offered only where they can change the list', () => {
  const uniform = readState(envelope('ok', {
    data: [{ ...DEVICE_ROWS[0] }, { ...DEVICE_ROWS[0], device: '9f1c0b6e-0000-4000-8000-00000000000a', hostname: 'FIN-LAPTOP-08' }],
    freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_device_liveness' },
  }));
  assert.deepEqual(devicesView(uniform, { now: NOW() }).filters, [], 'two reporting Windows managed devices: nothing to filter by');
  assert.deepEqual(devicesView(uniform, { now: NOW(), filters: { device_os: 'macos' } }).filters.map((f) => f.label), ['OS'], 'a filter already chosen stays, so it can be cleared');
  const mixed = readState(envelope('ok', { data: DEVICE_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }));
  assert.deepEqual(devicesView(mixed, { now: NOW() }).filters.map((f) => f.label), ['Status', 'OS', 'Management']);
  assert.deepEqual(devicesView(mixed, { now: NOW() }).tables[0].columns.map((c) => c.label), ['Device', 'Status', 'User', '']);
});
