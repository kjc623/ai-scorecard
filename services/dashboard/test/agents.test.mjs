// agents.test.mjs — the coding agents on Insights' Adoption tab: what coding agents send, and to
// which models. People are counted, never named.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { adoptionView } from '../src/views.js';
import { createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { AGENT_ROWS, PEOPLE_ROW, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const stateOf = (rows = AGENT_ROWS, meta = {}) => readState(envelope('ok', {
  data: rows,
  freshness: { ...FRESH, aggregate: 'mart.agg_agent_period' },
  coverage: COMPLETE,
  meta: { source: 'mart.agg_agent_period', applied_bucket: 'month', measure_semantics: { users: 'distinct_lower_bound', tool_users: 'distinct_lower_bound' }, ...meta },
}));
const usage = readState(envelope('ok', { data: TOOL_ROWS, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', extras: { people: [PEOPLE_ROW] } } }));
const viewOf = (agents, previousAgents = null) => adoptionView({ usage, agents, previousAgents });

const block = (view, title) => view.blocks.find((b) => b.title === title);
const byTool = (view) => block(view, 'Coding agents').rows.map(({ row }) => [row.tool_cell.primary, row.tool_cell.chip.text, row.people, row.actions, row.input_tokens, row.output_tokens, row.change]);
const byModel = (view) => block(view, 'Models called').rows.map(({ row }) => [row.model, row.tools, row.input_tokens, row.people]);

test('each coding agent is one row, its models folded, by the tokens it sent', () => {
  const view = viewOf(stateOf());
  assert.deepEqual(byTool(view), [
    ['Claude Code', 'Sanctioned', 41, 9800, 64_000_000, 2_500_000, null],
    ['Codex CLI', 'No decision yet', 9, 1500, 8_000_000, 500_000, null],
    ['GitHub Copilot', 'Sanctioned', 22, 0, 7_000_000, 390_000, null],
    ['GitHub Copilot CLI', 'Unsanctioned', 2, 0, 200_000, 10_000, null],
  ]);
  assert.deepEqual(block(view, 'Coding agents').columns.map((c) => c.label), ['Tool', 'Users', 'Agent actions', 'Input tokens', 'Output tokens', 'Change']);
});

test('each model is one row with the tools that called it; a request that named no model is kept apart', () => {
  const view = viewOf(stateOf());
  assert.deepEqual(byModel(view), [
    ['claude-sonnet-4', 'Claude Code, GitHub Copilot', 63_000_000, 38],
    ['gpt-5-codex', 'Codex CLI', 8_000_000, 9],
    ['gpt-4.1', 'GitHub Copilot', 5_000_000, 20],
    ['claude-haiku-4', 'Claude Code', 3_000_000, 12],
    ['Not reported by the agent', 'GitHub Copilot CLI', 200_000, 2],
  ]);
  assert.equal(block(view, 'Models called').summary, '4 models received code and context from coding agents in this window; claude-sonnet-4 received the most.');
});

test('each tool moves against the period before, and a tool new to the window says so', () => {
  const earlier = AGENT_ROWS.filter((row) => row.tool === 'app:claude_code').map((row) => ({ ...row, input_tokens: row.input_tokens / 2 }));
  const view = viewOf(stateOf(), stateOf(earlier));
  assert.deepEqual(byTool(view).map((row) => [row[0], row[6]]), [
    ['Claude Code', '+100%'],
    ['Codex CLI', 'New'],
    ['GitHub Copilot', 'New'],
    ['GitHub Copilot CLI', 'New'],
  ]);
});

test('the notes name the agents that report and the one that cannot, so a missing row is not read as no use', () => {
  const view = viewOf(stateOf());
  assert.ok(view.notes.includes('Claude Code, Codex and Copilot report coding agent figures while their OpenTelemetry switch is on in Settings; Cursor has no telemetry the device can collect, so its prompts are on Tools but not its tokens. A tool with no row reported nothing in this window.'));
  assert.ok(view.notes.includes('A coding agent\'s users is the most users on any one day in the window: a floor for the window, as on Tools.'));
});

test('a window with no agent activity says so, and a read that failed says that instead', () => {
  const none = viewOf(stateOf([]));
  assert.equal(block(none, 'Coding agents').emptyText, 'No coding agent reported in this window.');
  assert.equal(block(none, 'Models called').emptyText, 'No coding agent called a model in this window.');
  const failed = viewOf(null);
  assert.deepEqual(block(failed, 'Coding agents').rows, []);
  assert.equal(block(failed, 'Coding agents').emptyText, 'What coding agents reported could not be read.');
  assert.equal(block(failed, 'Models called').emptyText, 'What coding agents reported could not be read.');
});

test('Adoption reads the coding agents per tool and model over both periods, and names nobody', async () => {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = { send: (body) => { sent.push(body); return fixtures.send(body); } };
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { filters: { view: 'adoption' } });
  const reads = sent.filter((b) => b.source === 'mart.agg_agent_period');
  assert.deepEqual(reads.map((b) => [b.bucket, b.dimensions, b.window.from, b.window.to]), [
    ['month', ['tool', 'model'], '2026-08-02T12:00:00.000Z', '2026-09-01T12:00:00.000Z'],
    ['month', ['tool', 'model'], '2026-09-01T12:00:00.000Z', '2026-10-01T12:00:00.000Z'],
  ]);
  assert.equal(byTool(view)[0][6], '0%');
  const html = renderScreen(view, {});
  assert.match(html, /<h3>Coding agents<\/h3>/);
  assert.match(html, /<h3>Models called<\/h3>/);
  assert.match(html, /<span class="v-number">64,000,000<\/span>/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');
});
