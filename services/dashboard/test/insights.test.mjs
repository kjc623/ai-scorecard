// insights.test.mjs — Insights' Exposure tab and the Overview tiles that open it and Search: the
// sensitive prompts out of every prompt, what happened to them, where they went and which labels,
// each compared with the period before; and the attachments, with the prompts they were counted on.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderFunnel, renderNav, renderScreen, renderSeries } from '../src/render.js';
import { insightsView, postureView } from '../src/views.js';
import { createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { shellNavItems } from '../src/shell.js';
import { RULE_MATCH_ROWS, RULE_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, PARTIAL, envelope } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');
const DAY = '2026-09-30T00:00:00Z';

/** The tool usage of the seven days before the window: fewer prompts, more of them sensitive. */
const PREVIOUS_ROWS = Object.freeze([
  { bucket: '2026-09-23T00:00:00Z', tool: 'tls_b6681b043244c43f', tool_name: 'Claude Code', sanctioned_state: 'unsanctioned', submissions: 700, users: 200, sensitive: 20, sensitive_blocked: 5, sensitive_warned: 5, sensitive_logged: 10 },
  { bucket: '2026-09-23T00:00:00Z', tool: 'tls_f412811be7ac6539', tool_name: 'ChatGPT', sanctioned_state: 'sanctioned', submissions: 4000, users: 1800, sensitive: 150, sensitive_blocked: 10, sensitive_warned: 80, sensitive_logged: 60 },
]);

const usageOf = (data) => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' } }));

/** The realistic answers, with the q1 read of the period before the window answered from PREVIOUS_ROWS. */
function dashboardWith({ previous = 'answer' } = {}) {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = {
    send(body) {
      sent.push(body);
      if (body.template === 'q1_tools_ranked' && body.params.window.to === '2026-09-24T12:00:00.000Z') {
        if (previous === 'refused') return envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'Too broad.' } });
        return envelope('ok', { data: PREVIOUS_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' } });
      }
      return fixtures.send(body);
    },
  };
  return { sent, dashboard: createDashboard({ api: createQueryApi({ transport }), now: NOW }) };
}

test('Overview counts prompts, then the attachments and the sensitive prompts among them, each opening where the rest is', async () => {
  const { dashboard } = dashboardWith();
  const { view } = await dashboard.load('posture', {});
  assert.deepEqual(view.tiles.map((t) => t.label), ['Prompts', 'Attachments', 'Sensitive prompts', 'Users', 'Devices reporting']);

  const attachments = view.tiles[1];
  assert.equal(attachments.value.text, '240');
  assert.equal(attachments.note, 'Counted on 3,803 of 4,836 prompts: metadata-only collection (M0) does not describe attachments.');
  assert.equal(attachments.href, 'explore.html#events?attachments=yes', 'Search opens on the prompts that carried an attachment, in its own default window');
  assert.equal(attachments.moreLabel, 'Search');

  const sensitive = view.tiles[2];
  assert.equal(sensitive.value.text, '161');
  assert.equal(sensitive.note, 'of 4,836 prompts matched a rule on their labels');
  assert.deepEqual(sensitive.split.map((p) => [p.key, p.count]), [['allowed', 68], ['blocked', 15], ['warned', 78]]);
  assert.equal(sensitive.href, '#insights');

  const html = renderScreen(view, {});
  assert.match(html, /<a class="tile card tile-link" href="explore.html#events\?attachments=yes">/);
  assert.match(html, /<a class="tile card tile-link" href="#insights">/, 'the split draws its parts without links, so the whole tile opens Insights');
});

test('the attachments link keeps the window where Search offers it, and takes Search\'s widest where it does not', async () => {
  const { dashboard } = dashboardWith();
  const day = (await dashboard.load('posture', { preset: 'h24' })).view.tiles[1];
  assert.equal(day.href, 'explore.html#events?window=h24&attachments=yes');
  assert.equal(day.moreLabel, 'Search');
  const quarter = (await dashboard.load('posture', { preset: 'd90' })).view.tiles[1];
  assert.equal(quarter.href, 'explore.html#events?window=d30&attachments=yes');
  assert.equal(quarter.moreLabel, 'Search, last 30 days', 'the link says it lists a shorter window than the tile counts');
});

test('an attachment count is unknown, not zero, when no prompt was sent in a mode that describes attachments', () => {
  const blind = usageOf([{ bucket: DAY, tool: 'a', submissions: 5, sensitive: 0, attachments: 0, content_prompts: 0 }]);
  const tile = postureView({ devices: null, tools: blind }).tiles[1];
  assert.equal(tile.value.kind, 'absent');
  assert.match(tile.note, /^Not collected: every prompt in this window was sent at metadata-only collection \(M0\)/);
  assert.equal(tile.href, undefined, 'there is nothing to open');

  const counted = postureView({ devices: null, tools: usageOf([{ bucket: DAY, tool: 'a', submissions: 5, attachments: 0, content_prompts: 5 }]) }).tiles[1];
  assert.deepEqual([counted.value.kind, counted.value.text, counted.note], ['number', '0', 'Files attached to prompts in this window.'], 'every prompt could describe one and none did: a zero');

  const older = postureView({ devices: null, tools: usageOf([{ bucket: DAY, tool: 'a', submissions: 5 }]) }).tiles;
  assert.equal(older[1].value.kind, 'absent', 'a read that carries no attachment figure has none to show');
  assert.equal(older[2].value.kind, 'absent');
  assert.equal(older[2].split, undefined);
});

test('Insights reads the window, the period of the same length before it, and the class mix', async () => {
  const { dashboard, sent } = dashboardWith();
  const { view } = await dashboard.load('insights', {});
  const usage = sent.filter((b) => b.template === 'q1_tools_ranked').map((b) => b.params.window);
  assert.deepEqual(usage, [
    { from: '2026-09-17T12:00:00.000Z', to: '2026-09-24T12:00:00.000Z' },
    { from: '2026-09-24T12:00:00.000Z', to: '2026-10-01T12:00:00.000Z' },
  ]);
  assert.ok(sent.some((b) => b.template === 'q4_class_mix'));
  assert.equal(view.id, 'insights');
  assert.equal(view.title, 'Insights');
});

test('the funnel narrows prompts to sensitive prompts and splits them by outcome, each against the previous period', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', {});
  const funnel = view.lead[0];
  assert.equal(funnel.kind, 'funnel');
  const said = (stage) => [stage.label, stage.value.text, stage.detail, stage.change?.text ?? null];
  assert.deepEqual(funnel.stages.map(said), [
    ['Prompts', '4,836', null, '+3% on the previous 7 days'],
    ['Sensitive prompts', '161', '3% of prompts', '−5% on the previous 7 days'],
  ]);
  assert.deepEqual(funnel.outcomes.map(said), [
    ['Allowed', '68', '42% of sensitive prompts', '−3% on the previous 7 days'],
    ['Blocked', '15', '9% of sensitive prompts', 'Same as the previous 7 days'],
    ['Warned', '78', '48% of sensitive prompts', '−8% on the previous 7 days'],
  ]);
  const html = renderFunnel(funnel);
  assert.match(html, /<span class="funnel-label">Sensitive prompts<\/span><span class="funnel-value"><span class="v-number">161<\/span><\/span><span class="funnel-detail">3% of prompts<\/span>/);
  assert.match(html, /<span class="funnel-change funnel-down">−5% on the previous 7 days<\/span>/);
  assert.match(html, /<span class="dist-seg v-vocab-allowed" style="flex-grow:68"><\/span>/);
});

test('sensitive prompts are ranked by tool, with what was allowed to unsanctioned tools said first', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', { preset: 'd30' });
  const ranking = view.blocks[0];
  assert.equal(ranking.title, 'Where sensitive prompts went');
  assert.equal(ranking.summary, '20 sensitive prompts were allowed to unsanctioned tools, and 1 to tools with no decision yet.');
  assert.deepEqual(ranking.items.map((i) => [i.label, i.value.value, i.chip.key]), [
    ['ChatGPT', 120, 'sanctioned'], ['Claude Code', 40, 'unsanctioned'], ['Unrecognised tool', 1, 'unknown'],
  ], 'a tool with no sensitive prompt is not listed');
  assert.deepEqual(ranking.items[0].meta.map((f) => [f.label, f.value.value]), [['allowed', 47], ['blocked', 3], ['warned', 70]]);
  assert.equal(ranking.items[0].href, '#tools?tool=tls_f412811be7ac6539&preset=d30');
  assert.match(renderScreen(view, {}), /<p class="block-summary">20 sensitive prompts were allowed to unsanctioned tools, and 1 to tools with no decision yet\.<\/p>/);
});

test('each tool\'s change is against its own previous period, and a tool new to it says so', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', {});
  assert.deepEqual(view.blocks[0].items.map((i) => i.sublabel), [
    '−20% on the previous 7 days',
    '+100% on the previous 7 days',
    'shadow_llm_gateway · None in the previous 7 days',
  ]);
});

test('what was exposed lists the labels a rule matched, by sensitive prompts and outcome', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', {});
  const table = view.blocks[1];
  assert.equal(table.kind, 'table');
  assert.equal(table.title, 'What was exposed');
  assert.deepEqual(table.columns.map((c) => c.label), ['Label', 'Severity', 'Sensitive prompts', 'Allowed', 'Blocked', 'Warned']);
  assert.deepEqual(table.rows.map(({ row }) => [row.class, row.severity, row.flagged, row.flagged_logged, row.flagged_blocked, row.flagged_warned]), [
    ['customer_pii', 'high', 90, 30, 10, 50],
    ['credential', 'critical', 60, 35, 5, 20],
  ], 'a label no rule matched is not listed');
  assert.ok(view.notes.includes('24 events in this window could not be classified, so they carry no label and none is counted as sensitive.'));
});

const listOf = (data, source) => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source } }));
const rulesOf = (data = RULE_ROWS) => listOf(data, 'ops.enforcement_rule');
const matchesOf = (data = RULE_MATCH_ROWS, meta = {}) => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_rule_period', applied_bucket: 'week', ...meta } }));
const refusedRead = () => readState(envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'The window is too broad for this read' } }));
const rulesBlock = (view) => view.blocks.find((block) => block.title === 'Rules');
const ruleRow = ({ row }) => [row.rule.primary, row.rule.secondary, row.action, row.action_text, row.matches, row.logged, row.blocked, row.warned];

test('Rules reads the rules as they stand and the prompts each matched in the window, a week at a time', async () => {
  const { dashboard, sent } = dashboardWith();
  await dashboard.load('insights', {});
  assert.deepEqual(sent.find((b) => b.source === 'ops.enforcement_rule'), { query_version: '1', source: 'ops.enforcement_rule', filters: [], limit: 100 });
  const matches = sent.find((b) => b.source === 'mart.agg_rule_period');
  assert.deepEqual([matches.bucket, matches.dimensions, matches.window], ['week', ['rule'], { from: '2026-09-24T12:00:00.000Z', to: '2026-10-01T12:00:00.000Z' }]);

  sent.length = 0;
  await dashboard.load('insights', { preset: 'h24' });
  assert.equal(sent.find((b) => b.source === 'mart.agg_rule_period').bucket, 'hour');
});

test('Rules lists every rule in the order a device applies it, what it matches, and the prompts it matched by outcome', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', {});
  const table = rulesBlock(view);
  assert.equal(table.kind, 'table');
  assert.deepEqual(table.columns.map((c) => c.label), ['Rule', 'Action', 'Matched', 'Allowed', 'Blocked', 'Warned']);
  assert.deepEqual(table.rows.map(ruleRow), [
    ['secrets.block', 'Rule 1 · Labels: credential or Project Falcon; Sanction: Unsanctioned', 'blocked', 'Block', 13, 0, 13, 0],
    ['pii.warn', 'Rule 2 · Labels: customer_pii', 'warned', 'Warn', 80, 2, 0, 78],
    ['credential.allow', 'Rule 3 · Labels: credential', 'allowed', 'Allow', 66, 66, 0, 0],
    ['claude-code.warn', 'Rule 4 · Tools: Claude Code; Routes: Tool hooks', 'warned', 'Warn', 310, 270, 0, 40],
    ['runtime.block', 'Rule 5 · Categories: Local model runtime', 'blocked', 'Block', 0, 0, 0, 0],
    ['legacy.block', 'No longer a rule', 'removed', 'Removed', 2, 0, 2, 0],
  ], 'a rule that matched nothing is listed with zeros, and a removed rule after the rest');
  assert.equal(table.summary, '4 of 5 rules matched a prompt in this window; 1 matched none.');

  const html = renderScreen(view, {});
  assert.match(html, /<p class="block-summary">4 of 5 rules matched a prompt in this window; 1 matched none\.<\/p>/);
  assert.match(html, /<span class="cell-stack"><span class="cell-primary v-mono">secrets\.block<\/span><span class="cell-secondary">Rule 1 · Labels: credential or Project Falcon; Sanction: Unsanctioned<\/span><\/span>/);
  assert.match(html, /<span class="v-vocab v-vocab-blocked" title="blocked">Block<\/span>/);
  assert.match(html, /<span class="v-vocab v-vocab-removed" title="removed">Removed<\/span>/);
});

test('the Rules summary says when every rule matched, or none did', () => {
  const usage = usageOf(TOOL_ROWS);
  const twoRules = rulesOf(RULE_ROWS.slice(0, 2));
  const every = insightsView({ usage, rules: twoRules, ruleMatches: matchesOf() });
  assert.equal(rulesBlock(every).summary, 'Every rule matched at least one prompt in this window.');
  const none = insightsView({ usage, rules: twoRules, ruleMatches: matchesOf([]) });
  assert.equal(rulesBlock(none).summary, 'No rule matched a prompt in this window.');
  assert.deepEqual(rulesBlock(none).rows.map(({ row }) => row.matches), [0, 0]);
});

test('a rule with no condition matches any prompt, and says so', () => {
  const anyPrompt = rulesOf([{ position: 1, rule: 'everything.warn', action: 'warn', match: {}, label_names: null, tool_names: null }]);
  const view = insightsView({ usage: usageOf(TOOL_ROWS), rules: anyPrompt, ruleMatches: matchesOf([]) });
  assert.equal(rulesBlock(view).rows[0].row.rule.secondary, 'Rule 1 · Any prompt');
});

test('without the matches the rules are still listed, with their counts absent and the reason given', () => {
  const view = insightsView({ usage: usageOf(TOOL_ROWS), rules: rulesOf(), ruleMatches: refusedRead() });
  const table = rulesBlock(view);
  assert.equal(table.rows.length, 5);
  assert.ok(table.rows.every(({ row }) => row.matches === null && row.blocked === null));
  assert.equal(table.summary, 'The prompts each rule matched could not be read for this window.');
});

test('without the rules the matches are listed by rule id, and no rule is called removed', () => {
  const table = rulesBlock(insightsView({ usage: usageOf(TOOL_ROWS), rules: refusedRead(), ruleMatches: matchesOf() }));
  assert.deepEqual(table.rows.map(ruleRow).slice(0, 2), [
    ['claude-code.warn', null, 'unknown', 'Not read', 310, 270, 0, 40],
    ['pii.warn', null, 'unknown', 'Not read', 80, 2, 0, 78],
  ], 'the busiest rule first');
  assert.equal(table.summary, undefined);
  assert.equal(rulesBlock(insightsView({ usage: usageOf(TOOL_ROWS), rules: refusedRead(), ruleMatches: matchesOf([]) })).emptyText, 'The rules could not be read: The window is too broad for this read.');
});

test('a tenant with no rule is told that every prompt is logged, and where rules are set', () => {
  const table = rulesBlock(insightsView({ usage: usageOf(TOOL_ROWS), rules: rulesOf([]), ruleMatches: matchesOf([]) }));
  assert.deepEqual(table.rows, []);
  assert.equal(table.emptyText, 'No enforcement rule is set, so every prompt is logged. Rules are set in Settings.');
  assert.equal(table.summary, undefined);
});

test('a matches read cut short says Rules undercounts', () => {
  const view = insightsView({ usage: usageOf(TOOL_ROWS), rules: rulesOf(), ruleMatches: matchesOf(RULE_MATCH_ROWS, { truncated: true }) });
  assert.ok(view.notes.includes('Not every rule\'s matches could be read for this window, so Rules undercounts; a shorter window reads them all.'));
  const whole = insightsView({ usage: usageOf(TOOL_ROWS), rules: rulesOf(), ruleMatches: matchesOf() });
  assert.ok(!whole.notes.some((note) => /undercounts/.test(note)));
});

test('the trend stacks each bucket\'s sensitive prompts by outcome', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', {});
  assert.equal(view.series.length, 1);
  const series = view.series[0];
  assert.equal(series.title, 'Sensitive prompts per day');
  assert.deepEqual(series.points.map((p) => [p.value.value, p.parts.map((part) => part.value)]), [[161, [68, 15, 78]]]);
  const html = renderSeries(series);
  assert.match(html, /data-tip="2026-09-30 · 161 \(Allowed 68, Blocked 15, Warned 78\)"/);
  assert.match(html, /<span class="bar-stack" style="--h:100"><span class="bar-seg v-vocab-allowed" style="flex-grow:68"><\/span>/);
  assert.match(html, /<ul class="dist-legend series-legend">/);
});

test('without the previous period no change is shown, and the page says why', async () => {
  const { view } = await dashboardWith({ previous: 'refused' }).dashboard.load('insights', {});
  assert.ok(view.lead[0].stages.every((stage) => stage.change === null));
  assert.ok(view.blocks[0].items.every((item) => !/previous/.test(item.sublabel ?? '')));
  assert.ok(view.notes.includes('The previous 7 days could not be read, so no change is shown.'));
});

test('a window with prompts and no sensitive one says how a prompt becomes sensitive', () => {
  const quiet = usageOf(TOOL_ROWS.map((row) => ({ ...row, sensitive: 0, sensitive_blocked: 0, sensitive_warned: 0, sensitive_logged: 0 })));
  const view = insightsView({ usage: quiet });
  assert.match(view.notes[0], /Nothing counts as sensitive until an enforcement rule names a label; rules are set in Settings\./);
  assert.deepEqual(view.blocks[0].items, []);
  assert.equal(view.blocks[0].summary, null);
  assert.equal(view.blocks[0].emptyText, 'No sensitive prompt in this window.');
  assert.deepEqual(view.series, [], 'a trend of zeros is not drawn');
  assert.equal(view.blocks[1].emptyText, 'The labels could not be read for this window.');
});

test('a window with nothing recorded shows the funnel\'s empty line, not a row of dashes', () => {
  const view = insightsView({ usage: usageOf([]) });
  assert.equal(view.lead[0].emptyText, 'No prompts in this window.');
  assert.match(renderFunnel(view.lead[0]), /<p class="block-empty">No prompts in this window\.<\/p>/);
});

test('Insights sits under Overview in the navigation, with Search after it', () => {
  const items = shellNavItems();
  assert.deepEqual(items.slice(0, 4).map((i) => i.id), ['posture', 'insights', 'explore', 'tools']);
  assert.match(renderNav(items, 'insights'), /<a href="#insights" class="active" aria-current="page"><svg class="nav-icon"[^>]*>.*<\/svg><span>Insights<\/span><\/a>/);
});
