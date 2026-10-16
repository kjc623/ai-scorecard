// demand.test.mjs — Insights' Demand tab: the tools not sanctioned that people use, by team, and
// the seats each would need. People are counted, never named.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { demandView } from '../src/views.js';
import { boot, createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { DEMAND_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope, fakeDocument } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const stateOf = (rows = DEMAND_ROWS, { teams = 4, extras = null } = {}) => readState(envelope('ok', {
  data: [],
  freshness: { ...FRESH, aggregate: 'mart.agg_team_period' },
  coverage: COMPLETE,
  meta: { source: 'mart.agg_team_period', applied_bucket: 'month', extras: extras ?? { demand: rows, team_coverage: [{ users_all: 4210, users_in_teams: 2680, teams }] } },
}));

const block = (view, title) => view.blocks.find((b) => b.title.startsWith(title));
const seats = (view) => block(view, 'Seats to license').rows.map(({ row }) => [row.tool_cell.primary, row.tool_cell.chip.text, row.people, row.no_team, row.teams]);
const byTeam = (view) => block(view, 'Tools not sanctioned, by team').rows.map(({ row }) => [
  row.team_cell.primary, ...Object.keys(row).filter((k) => k.startsWith('tool_')).map((k) => `${row[k].count}/${row[k].of}`),
]);

test('each tool not sanctioned is a row of seats: its people, those in no team, and the teams they are in', () => {
  const view = demandView(stateOf(), { preset: 'd30' });
  assert.deepEqual(seats(view), [
    ['Claude Code', 'Unsanctioned', 230, 30, 'Engineering 180, Finance 25, Legal 1'],
    ['chat.example.ai', 'No decision yet', 14, 5, 'Finance 9'],
    ['Unrecognised tool', 'No decision yet', 2, 0, 'Engineering 2'],
  ]);
  const table = block(view, 'Seats to license');
  assert.equal(table.rows[0].row.href, '#tools?tool=app%3Aclaude_code&preset=d30', 'opens its Tools row');
  assert.equal(table.rows[2].row.tool_cell.secondary, 'shadow_llm_gateway', 'an unrecognised tool is told apart by its fingerprint');
  assert.equal(table.summary, '3 tools not sanctioned were used in this window; Claude Code had the most users, 230 in 3 teams.');
});

test('teams run down the side and the tools across the top, each cell a part of the team, both by people', () => {
  const view = demandView(stateOf(), { preset: 'd30' });
  const table = block(view, 'Tools not sanctioned, by team');
  assert.deepEqual(table.columns.map((c) => c.label), ['Team', 'Claude Code', 'chat.example.ai', 'shadow_llm_gateway']);
  assert.deepEqual(byTeam(view), [
    ['Engineering', '180/700', '0/700', '2/700'],
    ['Finance', '25/110', '9/110', '0/110'],
    ['Legal', '1/12', '0/12', '0/12'],
  ]);
  assert.equal(table.rows[0].row.href, '#teams?team=7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001&preset=d30');
  assert.deepEqual(view.tiles.map((t) => [t.label, t.value.text, t.note, t.jump ?? t.href]), [
    ['Tools not sanctioned', '3', 'used in this window', 'Seats to license'],
    ['Teams using them', '3', 'of 4 teams', 'Tools not sanctioned, by team'],
    ['Most seats', '230', 'users of Claude Code', '#tools?tool=app%3Aclaude_code&preset=d30'],
  ], 'each tile opens the table that holds its figure, and the most seats opens the tool');
  assert.match(view.tiles[0].info, /^Tools marked unsanctioned or with no decision yet/);
});

test('past eight tools the table by team keeps the eight with the most people and says so', () => {
  const rows = Array.from({ length: 10 }, (_, i) => [
    { tool: `host:t${i}.example`, tool_name: `t${i}.example`, sanctioned_state: 'unknown', team_id: null, team_name: null, team_size: null, people: 20 - i, people_in_teams: 1 },
    { tool: `host:t${i}.example`, tool_name: `t${i}.example`, sanctioned_state: 'unknown', team_id: 'team-a', team_name: 'Ops', team_size: 30, people: 1, people_in_teams: null },
  ]).flat();
  const view = demandView(stateOf(rows));
  assert.equal(block(view, 'Tools not sanctioned, by team').columns.length, 9);
  assert.equal(block(view, 'Seats to license').rows.length, 10);
  assert.match(view.notes.join(' '), /The 8 tools with the most users are shown by team; Seats to license lists all 10\./);
});

test('with no team yet the table by team says so and opens Teams, and the seats still stand', () => {
  const rows = DEMAND_ROWS.filter((r) => r.team_id === null);
  const view = demandView(stateOf(rows, { teams: 0 }));
  const table = block(view, 'Tools not sanctioned, by team');
  assert.equal(table.emptyText, 'No team exists yet, so demand cannot be shown per team. Teams are added on the Teams page.');
  assert.deepEqual([table.href, table.moreLabel], ['#teams', 'Teams']);
  assert.deepEqual([view.tiles[1].value.kind, view.tiles[1].note, view.tiles[1].href], ['absent', 'No team exists yet.', '#teams']);
  assert.equal(block(view, 'Seats to license').rows.length, 3);
});

test('a window with nothing not sanctioned says so, and a read without the demand is a dash, not a zero', () => {
  const none = demandView(stateOf([]));
  assert.equal(block(none, 'Seats to license').emptyText, 'No tool that is not sanctioned was used in this window.');
  assert.equal(block(none, 'Tools not sanctioned, by team').emptyText, 'No team member used a tool that is not sanctioned in this window.');
  assert.deepEqual(none.tiles.map((t) => t.value.text), ['0', '0', '—']);

  const unread = demandView(stateOf([], { extras: {} }));
  assert.deepEqual(unread.tiles.map((t) => t.value.kind), ['absent', 'absent', 'absent']);
  assert.equal(block(unread, 'Seats to license').emptyText, 'The demand could not be read for this window.');
});

test('the Demand tab reads teams crossed with tools and renders each cell as a part of its team', async () => {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = { send: (body) => { sent.push(body); return fixtures.send(body); } };
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { preset: 'd30', filters: { view: 'demand' } });
  const read = sent.find((b) => b.source === 'mart.agg_team_period');
  assert.deepEqual([read.bucket, read.dimensions, read.measures, read.limit], ['month', ['team', 'tool'], ['submissions'], 1999]);
  assert.equal(view.source, 'mart.agg_team_period');
  const html = renderScreen(view, {});
  assert.match(html, /<h3>Seats to license<\/h3>/);
  assert.match(html, /<span class="cell-bar" style="--w:26" aria-hidden="true"><\/span><span class="v-number">180<\/span><span class="cell-of"> of 700<\/span>/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');
});

test('Insights switches to Demand beside its window', async () => {
  const doc = fakeDocument('#insights?view=demand&preset=d30');
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin: null, session: null });
  const html = doc.html('app');
  assert.match(html, /<a class="page-tab" href="#insights\?preset=d30&amp;view=demand" aria-current="page">Demand<\/a>/);
  assert.match(html, /Most seats/);
});
