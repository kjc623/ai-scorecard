// agents.test.mjs — Insights' Coding agents tab: what coding agents send, and to which models.
// People are counted, never named.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { agentsView } from '../src/views.js';
import { boot, createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { AGENT_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope, fakeDocument } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const stateOf = (rows = AGENT_ROWS, meta = {}) => readState(envelope('ok', {
  data: rows,
  freshness: { ...FRESH, aggregate: 'mart.agg_agent_period' },
  coverage: COMPLETE,
  meta: { source: 'mart.agg_agent_period', applied_bucket: 'month', measure_semantics: { users: 'distinct_lower_bound', tool_users: 'distinct_lower_bound' }, ...meta },
}));

const block = (view, title) => view.blocks.find((b) => b.title === title);
const byTool = (view) => block(view, 'By tool').rows.map(({ row }) => [row.tool_cell.primary, row.tool_cell.chip.text, row.people, row.actions, row.input_tokens, row.output_tokens, row.change]);
const byModel = (view) => block(view, 'By model').rows.map(({ row }) => [row.model, row.tools, row.input_tokens, row.people]);

test('each coding agent is one row, its models folded, by the tokens it sent', () => {
  const view = agentsView({ agents: stateOf() }, { preset: 'd30' });
  assert.deepEqual(byTool(view), [
    ['Claude Code', 'Sanctioned', 41, 9800, 64_000_000, 2_500_000, null],
    ['Codex CLI', 'No decision yet', 9, 1500, 8_000_000, 500_000, null],
    ['GitHub Copilot', 'Sanctioned', 22, 0, 7_000_000, 390_000, null],
    ['GitHub Copilot CLI', 'Unsanctioned', 2, 0, 200_000, 10_000, null],
  ]);
  assert.match(view.notes.join(' '), /The previous 30 days could not be read, so no change is shown\./);
});

test('each model is one row with the tools that called it; a request that named no model is kept apart', () => {
  const view = agentsView({ agents: stateOf() }, { preset: 'd30' });
  assert.deepEqual(byModel(view), [
    ['claude-sonnet-4', 'Claude Code, GitHub Copilot', 63_000_000, 38],
    ['gpt-5-codex', 'Codex CLI', 8_000_000, 9],
    ['gpt-4.1', 'GitHub Copilot', 5_000_000, 20],
    ['claude-haiku-4', 'Claude Code', 3_000_000, 12],
    ['Not reported by the agent', 'GitHub Copilot CLI', 200_000, 2],
  ]);
  assert.equal(block(view, 'By model').summary, '4 models received code and context from coding agents in this window; claude-sonnet-4 received the most.');
  assert.deepEqual(view.tiles.map((t) => [t.label, t.value.text, t.note]), [
    ['Input tokens', '79,200,000', 'code and context sent to models in this window'],
    ['Agent actions', '11,300', 'tool calls the agents made in this window'],
    ['Models', '4', 'called by 4 coding agents in this window'],
  ]);
});

test('each tool and tile moves against the period before, and a tool new to the window says so', () => {
  const earlier = AGENT_ROWS.filter((row) => row.tool === 'app:claude_code').map((row) => ({ ...row, input_tokens: row.input_tokens / 2, actions: row.actions * 2 }));
  const view = agentsView({ agents: stateOf(), previous: stateOf(earlier) }, { preset: 'd30' });
  assert.deepEqual(byTool(view).map((row) => [row[0], row[6]]), [
    ['Claude Code', '+100% on the previous 30 days'],
    ['Codex CLI', 'None in the previous 30 days'],
    ['GitHub Copilot', 'None in the previous 30 days'],
    ['GitHub Copilot CLI', 'None in the previous 30 days'],
  ]);
  assert.deepEqual(view.tiles.slice(0, 2).map((t) => t.note), [
    'code and context sent to models in this window. +148% on the previous 30 days.',
    'tool calls the agents made in this window. −42% on the previous 30 days.',
  ]);
  assert.match(view.notes.join(' '), /Each change compares this window with the previous 30 days\./);
});

test('the notes name the agents that report and the one that cannot, so a missing row is not read as no use', () => {
  const view = agentsView({ agents: stateOf() });
  assert.ok(view.notes.includes('Claude Code, Codex and Copilot report here while their OpenTelemetry switch is on in Settings; Cursor has no telemetry the device can collect, so its prompts are on Tools but not its tokens. A tool with no row reported nothing in this window.'));
  assert.match(view.notes.join(' '), /People counts the most people on any one day in the window/);
});

test('a window with no agent activity says so, and a read that failed is a dash, not a zero', () => {
  const none = agentsView({ agents: stateOf([]) });
  assert.equal(block(none, 'By tool').emptyText, 'No coding agent reported in this window.');
  assert.equal(block(none, 'By model').emptyText, 'No coding agent called a model in this window.');
  assert.deepEqual(none.tiles.map((t) => [t.value.kind, t.note]), [
    ['absent', 'Nothing recorded in this window.'],
    ['absent', 'Nothing recorded in this window.'],
    ['absent', 'Nothing recorded in this window.'],
  ]);
});

test('the Coding agents tab reads per tool and model and names nobody', async () => {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = { send: (body) => { sent.push(body); return fixtures.send(body); } };
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { preset: 'd30', filters: { view: 'agents' } });
  const reads = sent.filter((b) => b.source === 'mart.agg_agent_period');
  assert.deepEqual(reads.map((b) => [b.bucket, b.dimensions, b.window.from, b.window.to]), [
    ['month', ['tool', 'model'], '2026-08-02T12:00:00.000Z', '2026-09-01T12:00:00.000Z'],
    ['month', ['tool', 'model'], '2026-09-01T12:00:00.000Z', '2026-10-01T12:00:00.000Z'],
  ], 'the previous period first, then the window');
  assert.equal(view.source, 'mart.agg_agent_period');
  assert.equal(byTool(view)[0][6], 'Same as the previous 30 days');
  const html = renderScreen(view, {});
  assert.match(html, /<h3>By tool<\/h3>/);
  assert.match(html, /<h3>By model<\/h3>/);
  assert.match(html, /<span class="v-number">64,000,000<\/span>/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');
});

test('Insights switches to Coding agents beside its window', async () => {
  const doc = fakeDocument('#insights?view=agents&preset=d30');
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin: null, session: null });
  const html = doc.html('app');
  assert.match(html, /<a class="seg-item" href="#insights\?preset=d30&amp;view=agents" aria-current="true">Coding agents<\/a>/);
  assert.match(html, /Agent actions/);
});
