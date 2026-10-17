// risk.test.mjs — Insights' Risk tab: the sensitive prompts per 1,000 against the previous 30 days;
// the labels on prompts sent outside sanctioned tools that no rule acted on; what each rule did when
// it fired; and every AI application discovery found, whether or not it sent prompts.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen, renderTile } from '../src/render.js';
import { riskView } from '../src/views.js';
import { createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { DISCOVERY_ROWS, INVENTORY_SCAN, LABEL_ROWS, PROMPT_ROWS, RULE_MATCH_ROWS, RULE_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const stateOf = (source, data, meta = {}, coverage = COMPLETE) => readState(envelope('ok', { data, freshness: FRESH, coverage, meta: { source, ...meta } }));
const usageOf = (data = TOOL_ROWS) => stateOf('mart.v_tool_usage', data, { applied_bucket: 'day' });
const foundOf = (data = DISCOVERY_ROWS, meta = {}) => stateOf('mart.agg_discovery_period', data, {
  applied_bucket: 'month', measure_semantics: { devices: 'distinct_lower_bound' }, extras: { inventory_scan: [INVENTORY_SCAN] }, ...meta,
}, { ...COMPLETE, devices_reporting: 4180 });
const promptsOf = (data = PROMPT_ROWS, meta = {}) => stateOf('mart.v_tool_usage', data, { applied_bucket: 'month', ...meta });
const labelsOf = (data = LABEL_ROWS) => stateOf('mart.agg_class_period', data, { applied_bucket: 'month' });
const rulesOf = (data = RULE_ROWS) => stateOf('ops.enforcement_rule', data);
const matchesOf = (data = RULE_MATCH_ROWS) => stateOf('mart.agg_rule_period', data, { applied_bucket: 'week' });
const refused = () => readState(envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'Too broad' } }));

/** The prompts sent in the 30 days before: fewer, and more of them sensitive. */
const PREVIOUS_ROWS = Object.freeze([
  { bucket: '2026-08-20T00:00:00Z', tool: 'tls_f412811be7ac6539', tool_name: 'ChatGPT', sanctioned_state: 'sanctioned', submissions: 4700, sensitive: 170 },
]);

const viewOf = (input = {}, options = {}) => riskView({
  usage: usageOf(), rules: rulesOf(), ruleMatches: matchesOf(), labels: labelsOf(), found: foundOf(), prompts: promptsOf(), ...input,
}, options);
const tileOf = (view, label) => view.tiles.find((t) => t.label === label);
const block = (view, title) => view.blocks.find((b) => b.title === title);
const found = (view) => block(view, 'Found on devices').rows.map(({ row }) => [row.tool_cell.primary, row.found_as, row.devices, row.first_seen_day, row.last_seen_day, row.prompts]);
const gaps = (view) => block(view, 'Policy gaps').rows.map(({ row }) => [row.label, row.prompts, row.rules, row.uncovered, row.tools, row.change]);

test('the sensitive rate is per 1,000 prompts, with the counts it is made of and how it moved', () => {
  const view = viewOf({ previous: usageOf(PREVIOUS_ROWS) });
  const rate = tileOf(view, 'Sensitive per 1,000 prompts');
  assert.deepEqual([rate.value.text, rate.note, rate.change], ['33.3', '161 of 4,836 prompts', { direction: 'down', text: '−2.9 (8%) on the 30 days before' }]);
  assert.match(rate.info, /^Prompts that matched a rule naming one of their labels, per 1,000 prompts/);
  assert.equal(rate.href, undefined, 'production does not offer Activity, so the rate opens nothing');
  assert.match(renderTile(rate), /<span class="tile-note">161 of 4,836 prompts<\/span><span class="tile-change tile-change-down">−2\.9 \(8%\) on the 30 days before<\/span>/);

  const preprod = tileOf(viewOf({}, { activity: true }), 'Sensitive per 1,000 prompts');
  assert.equal(preprod.href, '#activity?preset=d30', 'Activity holds the records, over the same 30 days');
  assert.equal(preprod.change, undefined, 'no previous period, no change');

  const quiet = tileOf(viewOf({ usage: usageOf(TOOL_ROWS.map((row) => ({ ...row, submissions: 0, sensitive: 0 }))) }), 'Sensitive per 1,000 prompts');
  assert.deepEqual([quiet.value.kind, quiet.note], ['absent', 'No prompts in the last 30 days.'], 'a rate out of no prompts is no rate, not zero');
  const tiny = tileOf(viewOf({ usage: usageOf([{ bucket: '2026-09-30T00:00:00Z', tool: 'a', submissions: 40_000, sensitive: 1 }]) }), 'Sensitive per 1,000 prompts');
  assert.equal(tiny.value.text, '<0.1', 'a rate too small to round to a tenth is not shown as none');
});

test('Policy gaps lists each label found outside sanctioned tools, the rules that name it, and the prompts no rule acted on', () => {
  const previous = labelsOf(LABEL_ROWS.map((row) => (row.class === 'credential' && row.tool === 'app:claude_code' ? { ...row, flagged: 130 } : row)).filter((row) => row.class !== 'health'));
  const view = viewOf({ previousLabels: previous });
  assert.deepEqual(gaps(view), [
    ['Source code', 400, 'No rule', 400, 'Claude Code 400', 'No change'],
    ['Credentials', 162, 'secrets.block, credential.allow', 42, 'chat.example.ai 22 · Claude Code 20', '+10 (31%) on the 30 days before'],
    ['Health information', 4, 'No rule', 4, 'chat.example.ai 4', '+4 on the 30 days before'],
    ['Customer personal data', 50, 'pii.warn', 0, null, 'No change'],
  ], 'a sanctioned tool is not outside, so ChatGPT\'s credentials are not counted');
  const table = block(view, 'Policy gaps');
  assert.deepEqual(table.columns.map((c) => c.label), ['Label', 'Prompts', 'Rules', 'No rule acted', 'Where', 'Change']);
  assert.equal(table.summary, '3 of the 4 labels found outside sanctioned tools had prompts no rule acted on.');
  assert.deepEqual([table.href, table.moreLabel], ['#rules', 'Rules']);
  const tile = tileOf(view, 'Labels with no rule');
  assert.deepEqual([tile.value.text, tile.note, tile.jump], ['3', 'of 4 labels found outside sanctioned tools', 'Policy gaps']);

  const covered = viewOf({ labels: labelsOf(LABEL_ROWS.map((row) => ({ ...row, flagged: row.submissions }))) });
  assert.equal(block(covered, 'Policy gaps').summary, 'A rule acted on every labelled prompt sent outside sanctioned tools.');
  assert.equal(gaps(covered)[0][5], null, 'without the previous period no change is said');
});

test('without the rules a label\'s rules are not known, and without the labels the gaps say why', () => {
  const unread = viewOf({ rules: refused() });
  assert.deepEqual(gaps(unread).map((row) => row[2]), [null, null, null, null], 'not "No rule": the rules were not read');
  const failed = viewOf({ labels: refused() });
  assert.equal(block(failed, 'Policy gaps').emptyText, 'The labels could not be read: Too broad.');
  assert.deepEqual([tileOf(failed, 'Labels with no rule').value.kind, tileOf(failed, 'Labels with no rule').note], ['absent', 'The labels could not be read.']);
  const none = viewOf({ labels: labelsOf(LABEL_ROWS.filter((row) => row.sanctioned_state === 'sanctioned')) });
  assert.deepEqual([tileOf(none, 'Labels with no rule').value.text, tileOf(none, 'Labels with no rule').note], ['0', 'No labelled prompt went outside sanctioned tools.']);
  assert.equal(block(none, 'Policy gaps').emptyText, 'No labelled prompt went outside sanctioned tools in the last 30 days.');
});

test('Rules at work says what each rule did when it fired, in the order a device applies them', () => {
  const view = viewOf();
  const table = block(view, 'Rules at work');
  assert.deepEqual(table.columns.map((c) => c.label), ['Rule', 'Action', 'Prompts', 'What happened']);
  assert.deepEqual(table.rows.map(({ row }) => [row.rule.primary, row.action_text, row.matches, row.outcome]), [
    ['secrets.block', 'Block', 13, '13 blocked'],
    ['pii.warn', 'Warn', 80, '0 stopped, 78 went ahead, 2 where no warning could be shown'],
    ['credential.allow', 'Allow', 66, '66 allowed'],
    ['claude-code.warn', 'Warn', 310, '0 stopped, 40 went ahead, 270 where no warning could be shown'],
    ['runtime.block', 'Block', 0, 'Did not fire'],
  ]);
  assert.equal(table.summary, '4 of 5 rules matched a prompt in the last 30 days; 1 matched none.');

  const unread = block(viewOf({ ruleMatches: refused() }), 'Rules at work');
  assert.ok(unread.rows.every(({ row }) => row.matches === null && row.outcome === null));
  assert.equal(unread.summary, 'What each rule matched could not be read for the last 30 days.');
  assert.equal(block(viewOf({ rules: refused() }), 'Rules at work').emptyText, 'The rules could not be read.');
});

test('Warnings heeded counts what the browser\'s warnings stopped; a warning on routes that cannot ask is left out', () => {
  const tile = tileOf(viewOf(), 'Warnings heeded');
  assert.deepEqual([tile.value.text, tile.note, tile.jump], ['0%', '0 of 78 warned prompts stopped', 'Rules at work'], 'claude-code.warn is a hook\'s, which records no answer');
  const answered = RULE_MATCH_ROWS.map((row) => (row.rule === 'pii.warn' ? { ...row, blocked: 21, warned: 61 } : row));
  const heeded = tileOf(viewOf({ ruleMatches: matchesOf(answered) }), 'Warnings heeded');
  assert.deepEqual([heeded.value.text, heeded.note], ['26%', '21 of 82 warned prompts stopped']);
  const noWarn = tileOf(viewOf({ rules: rulesOf(RULE_ROWS.filter((rule) => rule.action !== 'warn')) }), 'Warnings heeded');
  assert.deepEqual([noWarn.value.kind, noWarn.note, noWarn.href], ['absent', 'No rule warns in the browser.', '#rules']);
  const unshown = tileOf(viewOf({ ruleMatches: matchesOf([]) }), 'Warnings heeded');
  assert.deepEqual([unshown.value.kind, unshown.note], ['absent', 'No warning was shown in the last 30 days.']);
});

test('each tool found is one row, local models and inference endpoints first, then by the devices it was on', () => {
  const view = viewOf();
  assert.deepEqual(found(view), [
    ['Ollama', 'Local model, Desktop app', 5, '2026-09-12', '2026-09-30', 30],
    ['Unrecognised tool', 'Inference endpoint', 1, '2026-09-29', '2026-09-30', 0],
    ['Claude Code', 'CLI', 12, '2026-08-02', '2026-09-30', 812],
    ['ChatGPT (desktop)', 'Desktop app', 7, '2026-08-02', '2026-09-30', 0],
  ]);
  const table = block(view, 'Found on devices');
  const [ollama, process, claude, chatgpt] = table.rows.map(({ row }) => row);
  assert.equal(ollama.tool_cell.secondary, 'llama3, mistral', 'the models it holds');
  assert.deepEqual(ollama.tool_cell.chip, { key: 'unknown', text: 'No decision yet' });
  assert.equal(process.tool_cell.secondary, 'api.example.test · exe:0123456789abcdef', 'the host it reached, and the fingerprint that tells it apart');
  assert.equal(claude.href, '#tools?tool=app%3Aclaude_code&preset=d30', 'opens its Tools row over the same 30 days');
  assert.equal(chatgpt.href, null, 'nothing to open for a tool that sent no prompt');
  assert.deepEqual(table.columns.map((c) => c.label), ['Application', 'Found as', 'Devices of 4,180', 'First seen', 'Last seen', 'Prompts']);
  assert.equal(table.summary, '4 applications were found on devices in this window; 1 holds local models, 1 reached an inference endpoint and 2 sent no prompt.');
  const silent = tileOf(view, 'Installed but silent');
  assert.deepEqual([silent.value.text, silent.note, silent.jump], ['2', 'of 4 tools found', 'Found on devices']);
  assert.match(silent.info, /Their use is unknown, not zero\./);
});

test('an application found in two ways is one row, with what each found', () => {
  const running = { ...DISCOVERY_ROWS[1], discovery_type: 'app_running', devices: 15, first_seen: '2026-07-20T00:00:00Z' };
  const view = viewOf({ found: foundOf([...DISCOVERY_ROWS, running]) });
  assert.deepEqual(found(view).filter((c) => c[0] === 'Claude Code'), [['Claude Code', 'CLI, Seen running', 15, '2026-07-20', '2026-09-30', 812]]);
});

test('the notes say how devices are counted and where the inventory scan stands', () => {
  const view = viewOf();
  assert.match(view.notes.join(' '), /on its busiest day in the window/, 'a month is read over days');
  assert.ok(view.notes.includes('200 enrolled devices have no inventory scan in the last 24 hours, of 4,620, so what they hold is not known.'));
  assert.ok(view.notes.includes('A warning stops a prompt only when the person cancels it, which the browser extension asks. On other routes the prompt goes ahead with the warning recorded.'));
  const hours = viewOf({ found: foundOf(DISCOVERY_ROWS, { applied_bucket: 'hour' }) });
  assert.match(hours.notes.join(' '), /on its busiest hour in the window/);
});

test('without the prompts the column is empty and the tile says why, and a cut-short read leaves the missing ones unknown', () => {
  const none = viewOf({ prompts: null });
  assert.deepEqual(found(none).map((c) => c[5]), [null, null, null, null]);
  assert.deepEqual([tileOf(none, 'Installed but silent').value.kind, tileOf(none, 'Installed but silent').note], ['absent', 'The prompts could not be read.']);
  assert.match(none.notes.join(' '), /The prompts each application sent could not be read/);
  const cut = viewOf({ prompts: promptsOf(PROMPT_ROWS, { truncated: true }) });
  assert.deepEqual(found(cut).map((c) => c[5]), [30, null, 812, null], 'a tool the read left out is not known to be silent');
  assert.equal(tileOf(cut, 'Installed but silent').value.text, '0');
});

test('a window with nothing found says so, and a discovery read that failed is a dash', () => {
  const view = viewOf({ found: foundOf([], { extras: {} }), prompts: promptsOf([]) });
  assert.equal(block(view, 'Found on devices').emptyText, 'Discovery found no AI application on a device in the last 30 days.');
  assert.equal(block(view, 'Found on devices').summary, undefined);
  const failed = viewOf({ found: null });
  assert.equal(block(failed, 'Found on devices').emptyText, 'What discovery found could not be read.');
  assert.deepEqual([tileOf(failed, 'Installed but silent').value.kind, tileOf(failed, 'Installed but silent').note], ['absent', 'What discovery found could not be read.']);
});

test('the Risk tab reads the last 30 days and the 30 before, the labels by tool and sanction, the rules and what was found', async () => {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const dashboard = createDashboard({ api: createQueryApi({ transport: { send: (body) => { sent.push(body); return fixtures.send(body); } } }), now: NOW });
  const { view } = await dashboard.load('insights', { filters: { view: 'risk' } });
  assert.match(view.subtitle, /^Risk: /);
  assert.deepEqual(sent.filter((b) => b.template === 'q1_tools_ranked').map((b) => b.params.window.from), ['2026-08-02T12:00:00.000Z', '2026-09-01T12:00:00.000Z']);
  const labels = sent.filter((b) => b.source === 'mart.agg_class_period');
  assert.deepEqual(labels.map((b) => [b.bucket, b.dimensions, b.measures, b.window.from]), [
    ['month', ['class', 'tool', 'sanctioned_state'], ['submissions', 'flagged'], '2026-09-01T12:00:00.000Z'],
    ['month', ['class', 'tool', 'sanctioned_state'], ['submissions', 'flagged'], '2026-08-02T12:00:00.000Z'],
  ]);
  for (const source of ['ops.enforcement_rule', 'mart.agg_rule_period', 'mart.agg_discovery_period']) assert.ok(sent.some((b) => b.source === source), source);
  assert.deepEqual(sent.find((b) => b.source === 'mart.v_tool_usage').dimensions, ['tool']);
  assert.deepEqual(view.tiles.map((t) => t.label), ['Sensitive per 1,000 prompts', 'Labels with no rule', 'Warnings heeded', 'Installed but silent']);
  assert.deepEqual(view.blocks.map((b) => b.title), ['Policy gaps', 'Rules at work', 'Found on devices']);
  assert.equal(view.series[0].title, 'Sensitive prompts per day');
  const html = renderScreen(view, {});
  assert.match(html, /<h3>Policy gaps<\/h3>/);
  assert.match(html, /<td><span class="v-text">Source code<\/span><\/td>/);
  assert.match(html, /<tr class="row-link" tabindex="0" data-href="#tools\?tool=app%3Aollama&amp;preset=d30">/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');

  const failing = createDashboard({ api: createQueryApi({ transport: { send: (body) => (body.source === 'mart.agg_class_period' ? refused() : fixtures.send(body)) } }), now: NOW });
  const without = (await failing.load('insights', { filters: { view: 'risk' } })).view;
  assert.equal(tileOf(without, 'Labels with no rule').value.kind, 'absent', 'a refused labels read leaves the page standing');
});
