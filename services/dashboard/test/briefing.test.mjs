// briefing.test.mjs — Insights' Briefing: three figures against the previous 30 days, then the
// findings that crossed a fixed threshold, each with the one action it calls for. A check whose read
// did not come back is named, so its silence is not read as clear.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { briefingView } from '../src/views.js';
import { createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { DEMAND_ROWS, DISCOVERY_ROWS, INVENTORY_SCAN, LABEL_ROWS, PEOPLE_ROW, PROMPT_ROWS, RULE_MATCH_ROWS, RULE_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const stateOf = (source, data, meta = {}, coverage = COMPLETE) => readState(envelope('ok', { data, freshness: FRESH, coverage, meta: { source, ...meta } }));
const usageOf = (data = TOOL_ROWS, people = PEOPLE_ROW) => stateOf('mart.v_tool_usage', data, { applied_bucket: 'day', ...(people ? { extras: { people: [people] } } : {}) });
const rulesOf = (data = RULE_ROWS) => stateOf('ops.enforcement_rule', data);
const matchesOf = (data = RULE_MATCH_ROWS) => stateOf('mart.agg_rule_period', data, { applied_bucket: 'week' });
const labelsOf = (data = LABEL_ROWS) => stateOf('mart.agg_class_period', data, { applied_bucket: 'month' });
const demandOf = (rows = DEMAND_ROWS) => stateOf('mart.agg_team_period', [], { applied_bucket: 'month', extras: { demand: rows, team_coverage: [{ users_all: 4210, users_in_teams: 2680, teams: 4 }] } });
const foundOf = (data = DISCOVERY_ROWS) => stateOf('mart.agg_discovery_period', data, { applied_bucket: 'month', extras: { inventory_scan: [INVENTORY_SCAN] } }, { ...COMPLETE, devices_reporting: 4180 });
const promptsOf = (data = PROMPT_ROWS) => stateOf('mart.v_tool_usage', data, { applied_bucket: 'month' });

/** The 30 days before: fewer people, more of the prompts sanctioned, and more of them sensitive. */
const PREVIOUS_ROWS = Object.freeze([
  { bucket: '2026-08-20T00:00:00Z', tool: 'tls_f412811be7ac6539', tool_name: 'ChatGPT', sanctioned_state: 'sanctioned', submissions: 4700, sensitive: 170 },
  { bucket: '2026-08-20T00:00:00Z', tool: 'tls_b6681b043244c43f', tool_name: 'Claude Code', sanctioned_state: 'unsanctioned', submissions: 300, sensitive: 10 },
]);

const viewOf = (input = {}) => briefingView({
  usage: usageOf(), rules: rulesOf(), ruleMatches: matchesOf(), labels: labelsOf(), demand: demandOf(), found: foundOf(), prompts: promptsOf(), ...input,
});
const tileOf = (view, label) => view.tiles.find((t) => t.label === label);
const findings = (view) => view.blocks.find((b) => b.kind === 'findings');
const items = (view) => findings(view).items.map((f) => [f.topicText, f.text, f.evidence, f.action.label, f.action.href]);

test('three figures, each against the previous 30 days and opening the tab that holds the rest of it', () => {
  const view = viewOf({ previous: usageOf(PREVIOUS_ROWS, { ...PEOPLE_ROW, people: 1800 }) });
  assert.deepEqual(view.tiles.map((t) => [t.label, t.value.text, t.note, t.change, t.href]), [
    ['Users of AI', '2,050', 'of 4,210 active users in the directory', { direction: 'up', text: 'Up from 1,800' }, '#insights?view=adoption'],
    ['Sanctioned share', '83%', 'of 4,836 prompts', { direction: 'down', text: 'Down from 94%' }, '#insights?view=licensing'],
    ['Sensitive per 1,000 prompts', '33.3', '161 of 4,836 prompts', { direction: 'down', text: 'Down from 36.0' }, '#insights?view=risk'],
  ]);
  assert.equal(view.subtitle, 'Briefing: the last 30 days against the 30 before, and what needs a decision.');
  assert.ok(!view.notes.includes('The previous 30 days could not be read, so the figures show no change.'));

  const alone = viewOf();
  assert.deepEqual(alone.tiles.map((t) => t.change), [undefined, undefined, undefined], 'no previous period, no change');
  assert.ok(alone.notes.includes('The previous 30 days could not be read, so the figures show no change.'));
});

test('a figure that cannot be read is absent, never zero, and an undecided fleet points at the decision', () => {
  const uncounted = tileOf(viewOf({ usage: usageOf(TOOL_ROWS, null) }), 'Users of AI');
  assert.equal(uncounted.value.kind, 'absent', 'a read that did not count people has no adopters figure');

  const quiet = viewOf({ usage: usageOf(TOOL_ROWS.map((row) => ({ ...row, submissions: 0, sensitive: 0 }))) });
  assert.deepEqual([tileOf(quiet, 'Sanctioned share').value.kind, tileOf(quiet, 'Sanctioned share').note], ['absent', 'No prompts in the last 30 days.']);

  const undecided = tileOf(viewOf({ usage: usageOf(TOOL_ROWS.map((row) => ({ ...row, sanctioned_state: null }))) }), 'Sanctioned share');
  assert.deepEqual([undecided.value.kind, undecided.note, undecided.href], ['absent', 'No tool in use has a sanction decision yet.', '#sanction'],
    'with no decision, every prompt is unsanctioned by definition; a 0% would say nothing');
});

test('risk findings come first, largest first, then licensing; each names its figures and one action', () => {
  const view = viewOf();
  assert.deepEqual(items(view), [
    ['Risk', '400 prompts with source code went outside sanctioned tools. No rule covers source code.', 'Claude Code 400', 'Add a rule for source code', '#rules'],
    ['Risk', 'The warning pii.warn stopped 0 of 78 prompts. 78 went ahead.', 'Labels: customer_pii', 'Edit pii.warn', '#rules'],
    ['Risk', '42 prompts with credentials went outside sanctioned tools. No rule acted on them.', 'chat.example.ai 22 · Claude Code 20', 'Review the rules', '#rules'],
    ['Licensing', 'chat.example.ai has 14 users and no sanction decision.', 'Teams: Finance 9', 'Decide on chat.example.ai', '#sanction'],
  ]);
  const keys = findings(view).items.map((f) => f.key);
  assert.ok(!keys.includes('uncovered:health'), 'four prompts with health information are under the threshold');
  assert.ok(!keys.includes('uncovered:customer_pii'), 'a rule acted on every customer data prompt');
  assert.ok(!keys.includes('warning:claude-code.warn'), 'a warning on tool hooks cannot be answered, so it cannot be heeded');
  assert.ok(!keys.some((k) => k.startsWith('undecided:app:claude_code')), 'an unsanctioned tool has a decision');
  assert.equal(findings(view).summary, undefined, 'every check ran and every finding is shown');
});

test('each finding carries the previous 30 days where that period was read', () => {
  const view = viewOf({
    previousLabels: labelsOf(LABEL_ROWS.filter((row) => row.class !== 'source_code')),
    previousMatches: matchesOf([{ bucket: '2026-08-24T00:00:00Z', rule: 'pii.warn', matches: 40, blocked: 25, warned: 15, logged: 0 }]),
    previousDemand: demandOf(DEMAND_ROWS.filter((row) => row.tool !== 'host:chat.example.ai')),
  });
  assert.deepEqual(findings(view).items.map((f) => f.evidence), [
    'Claude Code 400 · Previous 30 days: 0',
    'Labels: customer_pii · Previous 30 days: 25 of 40 stopped',
    'chat.example.ai 22 · Claude Code 20 · Previous 30 days: 42',
    'Teams: Finance 9 · Previous 30 days: 0 users',
  ]);
  const unwarned = viewOf({ previousMatches: matchesOf([]) });
  assert.equal(findings(unwarned).items[1].evidence, 'Labels: customer_pii · Previous 30 days: none warned');
});

test('an application found on enough devices with no prompt seen is a coverage finding, its use unknown', () => {
  const desktop = { ...DISCOVERY_ROWS[0], devices: 30 };
  const ollama = { ...DISCOVERY_ROWS[2], devices: 40 };
  const view = viewOf({ found: foundOf([desktop, ollama, ...DISCOVERY_ROWS.slice(3)]) });
  const coverage = findings(view).items.filter((f) => f.topic === 'coverage');
  assert.deepEqual(coverage.map((f) => [f.topicText, f.text, f.evidence, f.action.label, f.action.href]), [
    ['Coverage', 'ChatGPT (desktop) is on 30 devices, but no prompt from it was seen.', 'Found as Desktop app · Its use is unknown, not zero', 'See what was found', '#insights?view=risk'],
  ], 'Ollama sent prompts, so its use is known');
  assert.equal(findings(view).items.at(-1).topic, 'coverage', 'coverage comes after risk and licensing');

  const unread = viewOf({ found: foundOf([desktop]), prompts: null });
  assert.ok(!findings(unread).items.some((f) => f.topic === 'coverage'), 'without the prompts, silence cannot be told from use');
  assert.equal(findings(unread).summary, 'Not checked, because a read did not come back: applications on devices.');
});

test('a threshold is a fixed count: just under it is not a finding, at it is', () => {
  const rule = (id, routes) => ({ position: 1, rule: id, action: 'warn', match: { labels: ['customer_pii'], ...(routes ? { routes } : {}) }, label_names: null, tool_names: null });
  const match = (id, blocked, warned) => ({ bucket: '2026-09-28T00:00:00Z', rule: id, matches: blocked + warned, blocked, warned, logged: 0 });
  const view = viewOf({
    rules: rulesOf([rule('nine.warn'), rule('third.warn'), rule('under.warn'), rule('page.warn', ['ext.page_context', 'tool.hook'])]),
    ruleMatches: matchesOf([match('nine.warn', 0, 9), match('third.warn', 4, 8), match('under.warn', 3, 9), match('page.warn', 0, 10)]),
    labels: labelsOf([
      { bucket: '2026-09-01T00:00:00Z', class: 'health', tool: 'host:chat.example.ai', tool_name: 'chat.example.ai', sanctioned_state: null, submissions: 9, flagged: 0 },
      { bucket: '2026-09-01T00:00:00Z', class: 'source_code', tool: 'app:claude_code', tool_name: 'Claude Code', sanctioned_state: 'unsanctioned', submissions: 10, flagged: 0 },
    ]),
    demand: demandOf([]),
  });
  assert.deepEqual(findings(view).items.map((f) => f.key), ['uncovered:source_code', 'warning:page.warn', 'warning:under.warn'],
    'nine warned is under ten; four of twelve is a third, not fewer; a rule that reaches the extension can be answered');
});

test('at most six findings show, and the summary says how many there were', () => {
  const classes = ['payment_card', 'government_id', 'credential', 'customer_pii', 'source_code', 'legal_commercial', 'health'];
  const labels = labelsOf(classes.map((code, i) => ({ bucket: '2026-09-01T00:00:00Z', class: code, tool: 'app:claude_code', tool_name: 'Claude Code', sanctioned_state: 'unsanctioned', submissions: 100 + i, flagged: 0 })));
  const view = viewOf({ rules: rulesOf([]), labels });
  const block = findings(view);
  assert.equal(block.items.length, 6);
  assert.equal(block.summary, 'The 6 largest of 8 findings are shown.');
  assert.deepEqual(block.items.map((f) => f.key), ['uncovered:health', 'uncovered:legal_commercial', 'uncovered:source_code', 'uncovered:customer_pii', 'uncovered:credential', 'uncovered:government_id'],
    'the licensing finding is the one left out: risk first, largest first');
});

test('a read that did not come back is named rather than read as clear', () => {
  const refused = readState(envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'Too broad' } }));
  const view = viewOf({ labels: refused, ruleMatches: null });
  assert.equal(findings(view).summary, 'Not checked, because a read did not come back: labels no rule covers and warnings.');
  assert.deepEqual(findings(view).items.map((f) => f.topic), ['licensing']);

  const none = viewOf({ rules: null, labels: null, demand: null, found: null });
  assert.deepEqual(findings(none).items, []);
  assert.equal(findings(none).emptyText, 'Nothing could be checked: none of the reads came back.');

  const clear = viewOf({ labels: labelsOf(LABEL_ROWS.map((row) => ({ ...row, flagged: row.submissions }))), ruleMatches: matchesOf([]), demand: demandOf([]) });
  assert.deepEqual([findings(clear).items, findings(clear).summary, findings(clear).emptyText], [[], undefined, 'Nothing crossed a threshold in the last 30 days.']);
});

test('the notes give every threshold, so a reader can check a finding against its table', () => {
  const { notes } = viewOf();
  for (const note of [
    'A label is a finding when 10 or more prompts holding it went outside sanctioned tools and no rule acted on them.',
    'A warning is a finding when it was shown on 10 or more prompts and stopped fewer than a third of them. A warning limited to routes that cannot ask the person is not one.',
    'A tool with no sanction decision is a finding when 10 or more users sent it a prompt.',
    'An application is a finding when it was found on 10 or more devices and no prompt from it was seen.',
    'At most 6 findings show: risk first, then licensing, then coverage, each largest first.',
  ]) assert.ok(notes.includes(note), note);
});

test('Insights opens on the Briefing, reading both periods one after another, and renders each finding with its action', async () => {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = { send: (body) => { sent.push(body); return fixtures.send(body); } };
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { filters: {} });
  const [current, previous] = [['2026-09-01T12:00:00.000Z', '2026-10-01T12:00:00.000Z'], ['2026-08-02T12:00:00.000Z', '2026-09-01T12:00:00.000Z']];
  assert.deepEqual(sent.map((b) => [b.template ?? b.source, ...(b.params?.window ?? b.window ? [(b.params?.window ?? b.window).from, (b.params?.window ?? b.window).to] : [])]), [
    ['q1_tools_ranked', ...previous],
    ['q1_tools_ranked', ...current],
    ['ops.enforcement_rule'],
    ['mart.agg_rule_period', ...current],
    ['mart.agg_rule_period', ...previous],
    ['mart.agg_class_period', ...current],
    ['mart.agg_class_period', ...previous],
    ['mart.agg_team_period', ...current],
    ['mart.agg_team_period', ...previous],
    ['mart.agg_discovery_period', ...current],
    ['mart.v_tool_usage', ...current],
  ]);
  assert.equal(view.subtitle, 'Briefing: the last 30 days against the 30 before, and what needs a decision.');
  const html = renderScreen(view, {});
  assert.match(html, /<ol class="findings"><li class="finding"><span class="v-vocab v-vocab-finding_risk" title="finding_risk">Risk<\/span><p class="finding-text">400 prompts with source code went outside sanctioned tools\. No rule covers source code\.<\/p>/);
  assert.match(html, /<a class="finding-action" href="#sanction">Decide on chat\.example\.ai<\/a>/);
  assert.doesNotMatch(html, /undefined|NaN/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');
});
