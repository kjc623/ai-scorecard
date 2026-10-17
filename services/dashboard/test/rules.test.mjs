// rules.test.mjs — Policy → Rules and Policy → Labels: the rulebase and the custom labels its rules
// name, driven without a browser.
//
// The controller (src/rules.js) runs against a fake admin API built on the real createAdminApi, so
// the requests asserted are the ones a browser would send, and a fake query API whose documents
// query-api's own validator accepts. The renderer (src/rules-render.js) is asserted for each state,
// and boot() for where the page sits and how its controls reach the controller.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, normaliseRules, normaliseRuleUsers, normaliseSettings } from '../src/transport.js';
import {
  MAX_RULE_USERS, changeCount, createRules, customLabelCode, movedRules, parseTerms, pickerItems, ruleProblem, shadowedBy, userOf,
} from '../src/rules.js';
import { renderRuleDialog, renderRules, ruleSentence } from '../src/rules-render.js';
import { createSettings } from '../src/settings.js';
import { renderSettings } from '../src/settings-render.js';
import { boot } from '../src/app.js';
import { tabsFor } from '../src/shell.js';
import { pagesForRoles } from '../server/session.mjs';
import { validate } from '../../query-api/src/validate.js';
import { fakeDocument } from './helpers.mjs';
import { fixtureTransport } from './fixtures.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');
const ADA = `u_${'a'.repeat(32)}`;
const BOB = `u_${'b'.repeat(32)}`;
const FINANCE = '00000000-0000-4000-8000-000000000901';
const GONE = '00000000-0000-4000-8000-000000000999';
const OPS = '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0002';

function falcon() {
  return {
    label: 'custom_project_falcon', display_name: 'Project Falcon',
    words: ['falcon roadmap'], customer_names: ['Acme Corp'], file_extensions: ['dwg'], file_names: ['falcon*.xlsx'],
  };
}

/** What the rules page reads from Settings: the catalogue's tools, the labels and categories a rule can name. */
function settingsBody(overrides = {}) {
  return {
    ceiling_mode: 'm3', collection_mode: 'm3', scope_overrides: {}, retention_defaults: { event_days: 90, content_days: 30 },
    content_search: 'disabled', devices: [], tls_inspection: false, restart_notice: 'dashboard', kill_switches: [],
    // One catalogue tool can come back on more than one row; the page lists it once.
    tools: [
      { tool_key: 'chatgpt_web', display_name: 'ChatGPT (web)', sanctioned_state: 'unsanctioned', fingerprints: ['tls_f412811be7ac6539'] },
      { tool_key: 'chatgpt_web', display_name: 'ChatGPT (web)', sanctioned_state: 'unsanctioned', fingerprints: ['host:chatgpt.com'] },
      { tool_key: 'claude_code', display_name: 'Claude Code', sanctioned_state: 'sanctioned', fingerprints: ['app:claude_code'] },
    ],
    data_classes: ['credential', 'customer_pii', 'source_code'],
    app_categories: ['chat_assistant', 'coding_agent', 'local_runtime'],
    custom_labels: [falcon()],
    ...overrides,
  };
}

/** Three rules in control-api's spelling, and the names of what their users hold. */
function servedRules() {
  return {
    rules: [
      {
        rule_id: 'allow_finance_falcon', action: 'allow',
        match: { labels: ['custom_project_falcon'], tools: [], categories: [], sanction: [], routes: [] },
        message: '', users: [`group:${FINANCE}`, `user:${ADA}`], enabled: true,
      },
      {
        rule_id: 'block_credentials', action: 'block',
        match: { labels: ['credential'], tools: ['chatgpt_web'], categories: ['chat_assistant'], sanction: ['unsanctioned'], routes: ['tool.hook'] },
        message: 'Remove the credential and try again.', link: 'https://intranet.example/ai', users: [], enabled: true,
      },
      {
        rule_id: 'warn_pii', action: 'warn',
        match: { labels: ['customer_pii'], tools: [], categories: [], sanction: [], routes: [] },
        message: 'Check for customer data.', users: [], enabled: false,
      },
    ],
    users: [
      { value: `group:${FINANCE}`, name: 'Finance', members: 12, found: true },
      { value: `user:${ADA}`, name: 'Ada Lovelace', members: 1, found: true },
    ],
  };
}

const GET_SETTINGS = 'GET /admin/v1/settings';
const GET_RULES = 'GET /admin/v1/settings/rules';
const PUT_RULES = 'PUT /admin/v1/settings/rules';
const PUT_LABELS = 'PUT /admin/v1/settings/custom-labels';

/** What the picker reads from the directory and the teams. */
const DIRECTORY = Object.freeze({
  'GET /admin/v1/directory/groups': () => ({ status: 200, body: { provisioned: [{ id: FINANCE, object_id: 'f1', name: 'Finance', members: 12 }], directory: [] } }),
  'GET /admin/v1/directory/values?kind=department': () => ({ status: 200, body: { values: [{ value: 'Engineering', people: 7 }] } }),
  'GET /admin/v1/directory/values?kind=org_unit': () => ({ status: 200, body: { values: [{ value: 'OU=Research,DC=contoso,DC=com', people: 3 }] } }),
  'GET /admin/v1/teams': () => ({ status: 200, body: { teams: [{ team_id: OPS, name: 'Ops', includes: [], people: 2, members: 2 }] } }),
});

function fakeAdmin(answers) {
  const requests = [];
  const transport = {
    async request(spec) {
      requests.push(spec);
      const key = `${spec.method} ${spec.path}`;
      const answer = answers[key];
      if (!answer) return { status: 404, body: { error: 'not_found', message: `no answer for ${key}` } };
      return answer(spec);
    },
  };
  return { admin: createAdminApi({ transport }), requests };
}

/** Each rule's prompts by week, as mart.agg_rule_period answers: a rule can have a row per week. */
const HITS = [
  { bucket: '2026-09-28T00:00:00Z', rule: 'block_credentials', matches: 9, blocked: 9, warned: 0, logged: 0 },
  { bucket: '2026-09-21T00:00:00Z', rule: 'block_credentials', matches: 4, blocked: 4, warned: 0, logged: 0 },
  { bucket: '2026-09-28T00:00:00Z', rule: 'allow_finance_falcon', matches: 2, blocked: 0, warned: 0, logged: 2 },
];

const PEOPLE = [
  { subject: ADA, name: 'Ada Lovelace', department: 'Finance' },
  { subject: BOB, name: 'Bob Jones', department: 'Sales' },
];

/** The query API: the rule hits and the people list, answering a name prefix as query-api would. */
function fakeQuery({ hits = HITS, people = PEOPLE, refuse = false } = {}) {
  const sent = [];
  return {
    sent,
    async run(body) {
      sent.push(body);
      if (refuse) return { result_state: 'unauthorised_role', error: { code: 'unauthorised_role', message: 'not for this role' } };
      if (body.source === 'mart.agg_rule_period') return { result_state: 'ok', data: hits, meta: { source: body.source } };
      let data = people;
      for (const f of body.filters ?? []) if (f.field === 'name_key') data = data.filter((r) => r.name.toLowerCase().startsWith(f.value));
      return { result_state: 'ok', data, page: { returned: data.length, next_cursor: null } };
    },
  };
}

/** A page whose rules route serves `rules`, whose PUTs store what they are sent, loaded. */
async function loaded({ rules = servedRules(), settings = settingsBody(), answers = {}, query = fakeQuery() } = {}) {
  let served = rules;
  let labels = settings?.custom_labels;
  const { admin, requests } = fakeAdmin({
    [GET_SETTINGS]: () => (settings ? { status: 200, body: { ...settings, custom_labels: labels } } : { status: 503, body: { error: 'unavailable' } }),
    [GET_RULES]: () => (served ? { status: 200, body: served } : { status: 503, body: { error: 'unavailable' } }),
    [PUT_RULES]: (spec) => { served = { rules: spec.body.rules, users: served?.users ?? [] }; return { status: 204 }; },
    [PUT_LABELS]: (spec) => { labels = spec.body.custom_labels; return { status: 204 }; },
    ...DIRECTORY,
    ...answers,
  });
  const page = createRules({ admin, query, now: NOW });
  await page.load();
  // The hits are read after the list is shown; wait for them.
  for (let i = 0; i < 50 && page.state.hits.pending; i++) await new Promise((resolve) => setTimeout(resolve, 1));
  const html = (options = {}) => renderRules(page.state, options);
  const panel = () => renderRuleDialog(page.state);
  return { page, requests, query, html, panel };
}

const puts = (requests) => requests.filter((r) => r.method === 'PUT');

/** The rows of the rules table, the default last. */
function rows(page) {
  const body = page.slice(page.indexOf('<tbody>'), page.indexOf('</tbody>'));
  return body.split('<tr').slice(1);
}

// ---------------------------------------------------------------------------------------------
// The states
// ---------------------------------------------------------------------------------------------

test('loading: the page shows its frame and placeholders, and says it is loading', async () => {
  let release;
  const { admin } = fakeAdmin({ [GET_SETTINGS]: () => new Promise((r) => { release = r; }), [GET_RULES]: () => ({ status: 200, body: servedRules() }) });
  const page = createRules({ admin, query: fakeQuery(), now: NOW });
  const pending = page.load();
  assert.equal(page.state.status, 'loading');
  const html = renderRules(page.state);
  assert.match(html, /<h2>Policy<\/h2>/);
  assert.match(html, /aria-busy="true"/);
  assert.match(html, /dp-skel/);
  release({ status: 200, body: settingsBody() });
  await pending;
  assert.equal(page.state.status, 'ready');
});

test('refused: a 403 says only an admin can open it; an unreachable API says so', async () => {
  const forbidden = () => ({ status: 403, body: { error: 'forbidden', message: 'admin only' } });
  const { html } = await loaded({ answers: { [GET_SETTINGS]: forbidden, [GET_RULES]: forbidden } });
  assert.match(html(), /Only an admin can open the rules/);
  const down = () => { throw new Error('connection refused'); };
  const unreachable = await loaded({ answers: { [GET_SETTINGS]: down, [GET_RULES]: down } });
  assert.match(unreachable.html(), /The admin API could not be reached/);
  assert.match(unreachable.html(), /data-rl="retry"/);
});

// ---------------------------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------------------------

test('the list reads like a rulebase: numbered rows, each criterion by name, Any where a rule checks nothing, the default last', async () => {
  const { html, requests } = await loaded();
  assert.ok(requests.some((r) => r.method === 'GET' && r.path === '/admin/v1/settings/rules'), 'the page reads the rules');
  const page = html();
  assert.match(page, /<th scope="col">#<\/th><th scope="col">On<\/th><th scope="col">Name<\/th><th scope="col">Users and groups<\/th><th scope="col">Tool<\/th><th scope="col">Content<\/th><th scope="col">Action<\/th><th scope="col" class="num" title="The prompts each rule decided in the last 30 days">Prompts<\/th>/);
  const [first, second, third, fallback, ...rest] = rows(page);
  assert.equal(rest.length, 0);

  assert.match(first, /<td class="rl-num">1<\/td>/);
  assert.match(first, /role="switch" aria-checked="true" data-rl="toggle" data-index="0"/);
  assert.match(first, /<button type="button" class="rl-name rl-open" data-rl="edit" data-index="0" aria-label="Edit rule 1, allow_finance_falcon" title="Edit rule 1">allow_finance_falcon<\/button>/, 'the name opens the rule');
  assert.match(first, /<button type="button" class="rl-menu-item" data-rl="edit" data-index="0">Edit<\/button>/, 'and so does the row menu');
  assert.match(first, />Finance<span class="rl-kind">Group<\/span><\/span>/, 'a group by its name and kind');
  assert.match(first, /title="Finance · Group · 12 people"/, 'with its name whole and how many people it reaches');
  assert.match(first, />Ada Lovelace<\/span>/, 'a person by name, with no kind');
  assert.match(first, />Project Falcon</, 'a custom label by its name');
  assert.match(first, /<span class="rl-action rl-action-allow">Allow<\/span>/);

  assert.match(second, /<td class="rl-num">2<\/td>/);
  assert.match(second, /<td><span class="rl-any">Any<\/span><\/td>/, 'no users is Any');
  assert.equal((second.match(/ChatGPT \(web\)/g) ?? []).length, 1, 'a tool is shown once, by its catalogue name');
  assert.match(second, />Chat assistant<span class="rl-kind">Category<\/span>/);
  assert.match(second, />Unsanctioned</);
  assert.match(second, />Credentials</, 'a label by its name');
  assert.match(second, /Seen by Tool hooks/, 'a route shows under the name');
  assert.match(second, /Remove the credential and try again\./);
  assert.match(second, /rl-action-block">Block</);
  assert.match(second, /<span class="v-number">13<\/span>/, 'hits are summed across the weeks of the window');

  assert.match(third, /^ class="rl-row rl-off"/, 'a rule switched off is dimmed but keeps its place');
  assert.match(third, /<span class="rl-mark rl-mark-off">Off<\/span>/);
  assert.match(third, /aria-checked="false"/);
  assert.match(third, /<span class="v-number">0<\/span>/, 'a rule with no hits counts 0');

  assert.match(fallback, /^ class="rl-default"/);
  assert.match(fallback, /<span class="rl-name">Default<\/span><span class="dp-sub">Every prompt no rule above matches<\/span>/);
  assert.equal((fallback.match(/rl-any">Any/g) ?? []).length, 3);
  assert.match(fallback, /rl-action-allow">Allow<\/span><span class="dp-sub">and logged<\/span>/);

  // The first rule cannot move up, the last cannot move down; nothing to save before a change.
  assert.match(first, /data-rl="up" data-index="0" aria-label="Move rule 1 up" title="Move rule 1 up" disabled/);
  assert.doesNotMatch(first, /data-rl="down" data-index="0"[^>]* disabled/);
  assert.match(third, /data-rl="down" data-index="2"[^>]* disabled/);
  assert.match(third, /data-rl="bottom" data-index="2" disabled>Move to bottom/);
  assert.doesNotMatch(page, /data-rl="save"/);
  assert.match(page, /Prompts is how many each rule decided in the last 30 days\./);
});

test('the hits are read from the query API over the last 30 days, in a document query-api accepts', async () => {
  const { query, page } = await loaded();
  const [read] = query.sent;
  assert.equal(read.source, 'mart.agg_rule_period');
  assert.deepEqual(read.dimensions, ['rule']);
  assert.equal(read.window.to, '2026-10-01T12:00:00.000Z');
  assert.equal(read.window.from, '2026-09-01T12:00:00.000Z');
  assert.doesNotThrow(() => validate(read), JSON.stringify(read));
  assert.deepEqual({ ...page.state.hits.counts }, { block_credentials: 13, allow_finance_falcon: 2 });

  const refused = await loaded({ query: fakeQuery({ refuse: true }) });
  assert.equal(refused.page.state.hits.counts, null);
  assert.match(rows(refused.html())[0], /<span class="v-absent">not reported<\/span>/, 'hits that were not read are not reported, never 0');
  assert.match(refused.html(), /The prompts each rule decided could not be read: not for this role/);
});

test('an empty list says every prompt goes to the default', async () => {
  const { html } = await loaded({ rules: { rules: [], users: [] } });
  const [empty, fallback] = rows(html());
  assert.match(empty, /No rules yet\. Every prompt goes to the default below\./);
  assert.match(fallback, /rl-default/);
});

test('a group since removed is marked not found; a rule whose people all reach nobody says devices are not sent it', async () => {
  const rules = servedRules();
  rules.rules[0].users = [`group:${GONE}`, 'department:Nowhere'];
  rules.users = [{ value: `group:${GONE}`, name: '', members: 0, found: false }, { value: 'department:Nowhere', name: 'Nowhere', members: 0, found: true }];
  const { html } = await loaded({ rules });
  const [first] = rows(html());
  assert.match(first, /<span class="rl-tag rl-tag-gone" title="No longer exists, so it reaches nobody">Group not found<\/span>/);
  assert.match(first, />Nowhere<span class="rl-kind">Department<\/span>/);
  assert.match(first, /Reaches nobody now, so devices are not sent it\./);
  const named = servedRules();
  named.rules[0].users = [`team:${OPS}`];
  named.users = [{ value: `team:${OPS}`, name: 'Ops', members: 0, found: false }];
  const [team] = rows((await loaded({ rules: named })).html());
  assert.match(team, /<span class="rl-tag rl-tag-gone" title="Ops · No longer exists, so it reaches nobody">Ops<span class="rl-kind">Team · not found<\/span><\/span>/, 'a name still known is kept');
});

test('a rule an enabled rule above always catches is marked: it never decides a prompt', async () => {
  const rules = servedRules();
  rules.rules[2] = { ...rules.rules[1], rule_id: 'warn_chatgpt_credentials', action: 'warn', message: 'Careful.', enabled: true, link: undefined };
  const { html, page } = await loaded({ rules });
  assert.match(rows(html())[2], /Never decides a prompt: rule 2 above matches every prompt this one would\./);
  assert.equal(shadowedBy(page.state.data.rules, 2), 1);
  assert.equal(shadowedBy(page.state.data.rules, 1), null, 'a rule for some people does not shadow one for everyone');
  const off = page.state.data.rules.map((r, i) => (i === 1 ? { ...r, enabled: false } : r));
  assert.equal(shadowedBy(off, 2), null, 'a rule switched off shadows nothing');
});

// ---------------------------------------------------------------------------------------------
// Changing the list: a draft until saved
// ---------------------------------------------------------------------------------------------

test('switching off, moving, cloning and deleting change a draft that is marked and counted; Save sends the whole list', async () => {
  const { page, html, requests } = await loaded();
  await page.act({ rl: 'toggle', index: '0' });
  await page.act({ rl: 'top', index: '2' });
  await page.act({ rl: 'clone', index: '2' });
  assert.equal(puts(requests).length, 0, 'nothing is sent until the list is saved');
  assert.deepEqual(page.state.draft.map((r) => [r.rule_id, r.enabled]), [
    ['warn_pii', false], ['allow_finance_falcon', false], ['block_credentials', true], ['block_credentials_copy', false],
  ]);
  const shown = html();
  assert.match(shown, /<strong>3 unsaved changes\.<\/strong> Devices keep the saved rules until you save\./, 'one switched off, one moved, one added');
  const [moved, toggled, pushed, copy] = rows(shown);
  assert.match(moved, /^ class="rl-row rl-off rl-changed"/);
  assert.match(moved, /<span class="rl-mark">Moved<\/span>/);
  assert.match(toggled, /<span class="rl-mark">Edited<\/span>/);
  assert.doesNotMatch(toggled, /Moved/, 'a rule pushed down by one moved above it has not moved');
  assert.doesNotMatch(pushed, /rl-mark|rl-changed/);
  assert.match(copy, /<span class="rl-mark rl-mark-new">New<\/span>/, 'a clone is new, and off until switched on');
  assert.match(copy, /<span class="v-absent" title="Not saved yet">—<\/span>/);
  await page.act({ rl: 'clone', index: '3' });
  assert.equal(page.state.draft[4].rule_id, 'block_credentials_copy_2');
  await page.act({ rl: 'delete', index: '4' });
  await page.act({ rl: 'delete', index: '3' });

  await page.act({ rl: 'save' });
  const [sent] = puts(requests);
  assert.equal(sent.path, '/admin/v1/settings/rules');
  assert.deepEqual(sent.body.rules.map((r) => ({ id: r.rule_id, enabled: r.enabled, users: r.users })), [
    { id: 'warn_pii', enabled: false, users: [] },
    { id: 'allow_finance_falcon', enabled: false, users: [`group:${FINANCE}`, `user:${ADA}`] },
    { id: 'block_credentials', enabled: true, users: [] },
  ]);
  assert.deepEqual(sent.body.rules[2], {
    rule_id: 'block_credentials', action: 'block',
    match: { labels: ['credential'], tools: ['chatgpt_web'], categories: ['chat_assistant'], sanction: ['unsanctioned'], routes: ['tool.hook'] },
    message: 'Remove the credential and try again.', users: [], enabled: true, link: 'https://intranet.example/ai',
  });
  assert.equal(requests.at(-1).path, '/admin/v1/settings/rules', 'the page re-reads the rules after the write');
  assert.equal(page.state.draft, null, 'the saved list is the one read back');
  assert.doesNotMatch(html(), /unsaved change/);

  // Discarding goes back to the list read, and sends nothing.
  await page.act({ rl: 'delete', index: '0' });
  assert.match(html(), /1 unsaved change\./);
  await page.act({ rl: 'discard' });
  assert.equal(page.state.draft, null);
  assert.equal(puts(requests).length, 1);
  // A draft moved back to where it was has nothing to save.
  await page.act({ rl: 'down', index: '0' });
  await page.act({ rl: 'up', index: '1' });
  assert.equal(changeCount(page.state.draft, page.state.data.rules), 0);
  assert.doesNotMatch(html(), /data-rl="save"/);
});

test('a move counts the rules that moved, not every rule whose number changed', () => {
  const list = (...ids) => ids.map((rule_id) => ({ rule_id, action: 'allow', message: '', link: '', users: [], enabled: true, match: { labels: [], tools: [], categories: [], sanction: [], routes: [] } }));
  const saved = list('a', 'b', 'c', 'd', 'e');
  const moved = (...ids) => [...movedRules(list(...ids), saved)];
  assert.deepEqual(moved('e', 'a', 'b', 'c', 'd'), ['e'], 'the last to the top is one move');
  assert.deepEqual(moved('b', 'c', 'd', 'e', 'a'), ['a'], 'the first to the bottom is one move');
  assert.deepEqual(moved('a', 'd', 'b', 'c', 'e'), ['d']);
  assert.deepEqual(moved('e', 'd', 'c', 'b', 'a').length, 4, 'reversing five rules moves four');
  assert.deepEqual(moved('a', 'c', 'e'), [], 'deleting rules moves none');
  assert.deepEqual(moved('a', 'b', 'x', 'c', 'd', 'e'), [], 'adding one moves none');
  assert.equal(changeCount(list('e', 'a', 'b', 'c', 'd'), saved), 1);
  assert.equal(changeCount(list('e', 'b', 'c', 'd'), saved), 2, 'one moved, one deleted');
  assert.equal(changeCount(saved, saved), 0);
});

test('a refused save keeps the draft and says why; rules that were not read cannot be changed or saved over', async () => {
  const refused = await loaded({ answers: { [PUT_RULES]: () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'a rule names a label that is not a data class' } } }) } });
  await refused.page.act({ rl: 'delete', index: '1' });
  await refused.page.act({ rl: 'save' });
  assert.match(refused.html(), /Not saved\.<\/strong> a rule names a label that is not a data class/);
  assert.equal(refused.page.state.draft.length, 2, 'the draft is kept to correct and save again');

  const unread = await loaded({ rules: null });
  assert.equal(unread.page.state.data.rules, null, 'rules that were not read are null, never an empty list');
  assert.match(unread.html(), /The server did not send the rules/);
  assert.doesNotMatch(unread.html(), /data-rl="add"/);
  await unread.page.act({ rl: 'add' });
  await unread.page.act({ rl: 'save' });
  assert.equal(puts(unread.requests).length, 0, 'nothing is written over rules that were not read');
  assert.match(unread.html(), /were not read/);
});

test('the filter narrows the rows by name, person, group, tool or label, and turns reordering off', async () => {
  const { page, html } = await loaded();
  for (const [text, ids] of [['finance', ['allow_finance_falcon']], ['ada', ['allow_finance_falcon']], ['chatgpt', ['block_credentials']], ['falcon', ['allow_finance_falcon']], ['warn', ['warn_pii']]]) {
    page.setQuery(text);
    await page.act({ rl: 'filter' });
    const shown = rows(html()).filter((r) => r.startsWith(' class="rl-row'));
    assert.deepEqual(shown.map((r) => r.match(/class="rl-name rl-open"[^>]*>([^<]+)</)[1]), ids, text);
  }
  const filtered = html();
  assert.match(filtered, /1 of 3 rules · reordering is off while filtered/);
  assert.match(filtered, /data-rl="down" data-index="2"[^>]* disabled/);
  assert.doesNotMatch(filtered, /rl-default/, 'the default is not a match for a filter');
  assert.match(filtered, /value="warn"/, 'the filter keeps what was typed');
  page.setQuery('nothing like it');
  await page.act({ rl: 'filter' });
  assert.match(html(), /No rule matches “nothing like it”\./);
  // Typing alone filters nothing until it is applied.
  page.setQuery('');
  assert.match(html(), /No rule matches/);
  await page.act({ rl: 'filter' });
  assert.equal(rows(html()).length, 4);
});

// ---------------------------------------------------------------------------------------------
// The panel
// ---------------------------------------------------------------------------------------------

test('the panel offers every tool once by its catalogue name, the categories the catalogue uses, the labels, sanction and routes', async () => {
  const { page, panel, html } = await loaded();
  await page.act({ rl: 'edit', index: '1' });
  const open = panel();
  const chip = (field, value) => new RegExp(`aria-pressed="(true|false)" data-rl="match" data-field="${field}" data-value="${value.replace('.', '\\.')}">`);
  assert.equal((open.match(/data-field="tools" data-value="chatgpt_web"/g) ?? []).length, 1, 'a catalogue tool is offered once');
  assert.equal(open.match(chip('tools', 'chatgpt_web'))[1], 'true');
  assert.equal(open.match(chip('tools', 'claude_code'))[1], 'false');
  for (const c of ['credential', 'customer_pii', 'source_code', 'custom_project_falcon']) assert.match(open, chip('labels', c));
  assert.match(open, /data-field="labels" data-value="custom_project_falcon">Project Falcon</);
  for (const c of ['chat_assistant', 'coding_agent', 'local_runtime']) assert.match(open, chip('categories', c));
  assert.doesNotMatch(open, chip('categories', 'ai_feature'), 'a category no catalogue app falls in is not offered');
  assert.match(open, />Local model runtime</);
  for (const s of ['sanctioned', 'unsanctioned']) assert.match(open, chip('sanction', s));
  assert.match(open, /<details class="rl-advanced" open><summary>Advanced: collection routes<\/summary>/, 'routes are folded away unless the rule sets one');
  for (const r of ['tool.hook', 'proxy.tls', 'net.flow']) assert.match(open, chip('routes', r));
  assert.match(open, /data-rl-draft="message" value="Remove the credential and try again\."/);
  assert.match(open, /<p class="dialog-lead rl-sentence">Block prompts holding Credentials, to ChatGPT \(web\), to a tool in Chat assistant, to an unsanctioned tool, seen by Tool hooks\.<\/p>/);
  assert.match(open, /the person sees “Request blocked” with your message, the link and Dismiss/);
  assert.doesNotMatch(open, /id="rl-at"/, 'an existing rule keeps its place; the list moves it');
  // While a rule is open the list cannot be changed or saved.
  assert.match(html(), /data-rl="down" data-index="0"[^>]* disabled/);
  assert.match(html(), /data-rl="add" disabled/);
});

test('a match list the server did not send is not reported; an empty one has none known yet', async () => {
  for (const [categories, text] of [[undefined, 'not reported'], [[], 'none known yet']]) {
    const { page, panel } = await loaded({ settings: settingsBody({ app_categories: categories }) });
    await page.act({ rl: 'add' });
    const open = panel();
    const field = open.slice(open.indexOf('>Categories'), open.indexOf('>Sanction'));
    assert.match(field, new RegExp(`<span class="v-absent">${text}</span>`));
    assert.doesNotMatch(field, /data-field="categories"/);
  }
});

test('adding a rule: it is checked, kept where it was put, and read back as a sentence', async () => {
  const { page, panel } = await loaded();
  const attempt = async (fields, action = 'block') => {
    await page.act({ rl: 'add' });
    for (const [k, v] of Object.entries(fields)) page.setDraft(k, v);
    await page.act({ rl: 'action', value: action });
    await page.act({ rl: 'apply' });
    const problem = page.state.panel?.problem ?? null;
    if (page.state.panel) await page.act({ rl: 'cancel' });
    return problem;
  };
  assert.match((await attempt({ rule_id: 'needs_message', message: '' })).message, /needs a message/);
  assert.match((await attempt({ rule_id: 'needs_message', message: '  ' }, 'warn')).message, /needs a message/);
  assert.match((await attempt({ rule_id: 'Upper', message: 'x' })).message, /The name must start with a lower-case letter/);
  assert.match((await attempt({ rule_id: 'warn_pii', message: 'x' })).message, /Another rule is already called warn_pii/);
  assert.match((await attempt({ rule_id: 'long', message: 'é'.repeat(281) })).message, /281 characters; at most 280/);
  assert.match((await attempt({ rule_id: 'plain_http', message: 'x', link: 'http://intranet.example' })).message, /https:\/\//);
  assert.match((await attempt({ rule_id: 'no_link', message: 'Use Copilot.' }, 'redirect')).message, /redirect rule needs a link/);
  assert.equal(page.state.draft, null, 'no refused rule reached the list');
  const tooMany = { rule_id: 'r', action: 'allow', message: '', link: '', users: Array.from({ length: MAX_RULE_USERS + 1 }, (_, i) => `department:d${i}`), enabled: true, match: {} };
  assert.match(ruleProblem(tooMany).message, /at most 200/);

  // A new rule goes to the bottom, just above the default, unless it is put elsewhere.
  await page.act({ rl: 'add' });
  assert.match(panel(), /<option value="bottom" selected>At the bottom, above the default<\/option><option value="0">At the top, above rule 1 \(allow_finance_falcon\)<\/option><option value="1">Above rule 2 \(block_credentials\)<\/option>/);
  page.setDraft('rule_id', 'warn_agents');
  page.setDraft('message', 'Use the approved coding agent.');
  await page.act({ rl: 'action', value: 'warn' });
  await page.act({ rl: 'match', field: 'categories', value: 'coding_agent' });
  await page.act({ rl: 'match', field: 'sanction', value: 'unsanctioned' });
  await page.act({ rl: 'match', field: 'routes', value: 'proxy.tls' });
  await page.act({ rl: 'match', field: 'routes', value: 'proxy.tls' });
  await page.act({ rl: 'at', value: '1' });
  assert.match(panel(), /<option value="1" selected>Above rule 2/);
  assert.match(panel(), /Warn before sending prompts to a tool in Coding agent, to an unsanctioned tool\./);
  await page.act({ rl: 'apply' });
  assert.equal(page.state.panel, null, 'a valid rule closes the panel');
  assert.deepEqual(page.state.draft.map((r) => r.rule_id), ['allow_finance_falcon', 'warn_agents', 'block_credentials', 'warn_pii']);
  await page.act({ rl: 'add' });
  page.setDraft('rule_id', 'allow_everything');
  await page.act({ rl: 'action', value: 'allow' });
  await page.act({ rl: 'at', value: 'top' });
  assert.match(panel(), /Allow every prompt\./);
  await page.act({ rl: 'enabled', value: 'off' });
  assert.match(panel(), /Allow every prompt\. Off: devices are not sent it\./);
  await page.act({ rl: 'apply' });
  assert.deepEqual(page.state.draft.slice(0, 2).map((r) => [r.rule_id, r.enabled]), [['allow_everything', false], ['allow_finance_falcon', true]]);
});

test('a redirect rule needs its link, names what it does, and is sent with it', async () => {
  const { page, panel } = await loaded();
  await page.act({ rl: 'add' });
  page.setDraft('rule_id', 'chatgpt_to_copilot');
  page.setDraft('message', 'Use Copilot for work.');
  page.setDraft('link', 'https://copilot.example/');
  await page.act({ rl: 'action', value: 'redirect' });
  await page.act({ rl: 'match', field: 'categories', value: 'chat_assistant' });
  assert.match(panel(), /aria-pressed="true">Redirect</);
  assert.match(panel(), /Redirect visits and prompts to a tool in Chat assistant\./);
  assert.match(panel(), /sees the Sundial page with your message and a button to your link/);
  assert.match(panel(), />Link<\/label>/);
  await page.act({ rl: 'apply' });
  assert.equal(page.state.panel, null, 'a redirect with its link closes the panel');
  const added = page.state.draft.find((r) => r.rule_id === 'chatgpt_to_copilot');
  assert.deepEqual([added.action, added.link], ['redirect', 'https://copilot.example/']);
});

test('people, groups, departments, units and teams are picked from one list, and the rule names each by kind', async () => {
  const { page, panel, query, requests, html } = await loaded();
  await page.act({ rl: 'edit', index: '1' });
  assert.match(panel(), /<span class="rl-any">Any<\/span><span class="dp-note">Everyone, including a prompt the device cannot name a person for\.<\/span>/);
  await page.act({ rl: 'pick-open' });
  const people = query.sent.find((b) => b.source === 'mart.v_person');
  assert.doesNotThrow(() => validate(people), JSON.stringify(people));
  for (const path of ['/admin/v1/directory/groups', '/admin/v1/directory/values?kind=department', '/admin/v1/directory/values?kind=org_unit', '/admin/v1/teams']) {
    assert.ok(requests.some((r) => r.path === path), path);
  }
  assert.deepEqual(pickerItems(page.state.panel.picker).map((i) => i.name), ['Finance', 'Engineering', 'Research', 'Ops', 'Ada Lovelace', 'Bob Jones'],
    'groups, departments, units, teams, then people');
  let open = panel();
  assert.deepEqual([...open.matchAll(/data-rl="pick-filter" data-value="([a-z_]+)"/g)].map((m) => m[1]), ['all', 'user', 'group', 'department', 'org_unit', 'team']);
  assert.match(open, /data-rl-pick="group:00000000-0000-4000-8000-000000000901"><span class="pick-name">Finance<\/span><span class="pick-type">Group<\/span><span class="pick-detail">12 members<\/span>/);
  assert.match(open, /<span class="pick-name">Research<\/span><span class="pick-type">Unit<\/span><span class="pick-detail">3 people · OU=Research,DC=contoso,DC=com<\/span>/);
  assert.match(open, /<span class="pick-name">Ops<\/span><span class="pick-type">Team<\/span><span class="pick-detail">2 members<\/span>/);
  assert.match(open, /<span class="pick-name">Bob Jones<\/span><span class="pick-type">Person<\/span><span class="pick-detail">Sales<\/span>/);

  await page.act({ rl: 'user', value: `team:${OPS}` });
  await page.act({ rl: 'user', value: `user:${BOB}` });
  await page.act({ rl: 'user', value: 'department:Engineering' });
  await page.act({ rl: 'user', value: 'department:Engineering' });
  open = panel();
  assert.match(open, /data-rl-pick="team:7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0002" checked>/);
  assert.match(open, /Block prompts from Ops or Bob Jones, holding Credentials/);

  // The search narrows the people by the start of their name and the rest by any part of it.
  page.setDraft('query', 'bo');
  await page.act({ rl: 'pick-search' });
  assert.deepEqual(pickerItems(page.state.panel.picker).map((i) => i.name), ['Bob Jones']);
  await page.act({ rl: 'pick-filter', value: 'team' });
  assert.deepEqual(pickerItems(page.state.panel.picker).map((i) => i.name), []);
  assert.match(panel(), /Nothing matches “bo”\./);
  await page.act({ rl: 'pick-close' });
  open = panel();
  assert.match(open, /<ul class="rl-chosen-list"><li><span class="rl-tag" title="Ops · Team · 2 people">Ops<span class="rl-kind">Team<\/span><\/span><button type="button" class="rl-remove" data-rl="user" data-value="team:7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0002" aria-label="Take out Ops">×<\/button><\/li>/);
  assert.match(open, /data-rl="users-any">Any</);

  await page.act({ rl: 'apply' });
  assert.deepEqual(page.state.draft[1].users, [`team:${OPS}`, `user:${BOB}`]);
  const [, row] = rows(html());
  assert.match(row, />Ops<span class="rl-kind">Team<\/span>/, 'what the panel learnt names the row');
  assert.match(row, />Bob Jones<\/span>/);

  await page.act({ rl: 'edit', index: '1' });
  await page.act({ rl: 'users-any' });
  await page.act({ rl: 'apply' });
  assert.deepEqual(page.state.draft[1].users, []);
  // An answer that arrives after the picker closed is dropped.
  await page.act({ rl: 'edit', index: '0' });
  const opening = page.act({ rl: 'pick-open' });
  await page.act({ rl: 'cancel' });
  await opening;
  assert.equal(page.state.panel, null);
});

test('a name a rule shows comes from the rules read, else what the panel listed, else the value itself', () => {
  const names = normaliseRuleUsers([
    { value: `group:${FINANCE}`, name: 'Finance', members: 12, found: true },
    { value: `group:${GONE}`, name: '', members: 0, found: false },
    { value: 'bad' }, null, { value: `user:${ADA}`, name: 7 },
  ]);
  assert.deepEqual(Object.keys(names), [`group:${FINANCE}`, `group:${GONE}`, 'bad', `user:${ADA}`]);
  assert.deepEqual({ ...userOf(`group:${FINANCE}`, names) }, { value: `group:${FINANCE}`, kind: 'group', name: 'Finance', members: 12, found: true });
  assert.equal(userOf(`group:${GONE}`, names).found, false);
  assert.equal(userOf(`user:${ADA}`, names).name, ADA, 'a person with no name is shown by their reference');
  assert.equal(userOf('org_unit:OU=Research,DC=contoso,DC=com').name, 'Research', 'a unit by the last part of its name');
  assert.equal(userOf('department:Sales').name, 'Sales');
  const rules = normaliseRules([{ rule_id: 'r', action: 'warn', match: { labels: ['credential', 7], routes: 'tool.hook' }, users: ['user:x', 3, ''], enabled: false }, null, { rule_id: 's' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(rules)), [
    { rule_id: 'r', action: 'warn', match: { labels: ['credential'], tools: [], categories: [], sanction: [], routes: [] }, message: '', link: '', users: ['user:x'], enabled: false },
    { rule_id: 's', action: '', match: { labels: [], tools: [], categories: [], sanction: [], routes: [] }, message: '', link: '', users: [], enabled: true },
  ]);
});

test('ruleSentence reads a rule back with every criterion it sets', () => {
  const data = normaliseSettings(settingsBody());
  const rule = {
    rule_id: 'r', action: 'block', message: 'x', link: '', enabled: true, users: [`group:${FINANCE}`, 'department:Sales'],
    match: { labels: ['credential', 'custom_project_falcon'], tools: ['chatgpt_web', 'claude_code'], categories: [], sanction: ['sanctioned', 'unsanctioned'], routes: ['proxy.tls'] },
  };
  assert.equal(ruleSentence(rule, data, { [`group:${FINANCE}`]: { name: 'Finance' } }),
    'Block prompts from Finance or Sales, holding Credentials or Project Falcon, to ChatGPT (web) or Claude Code, to a sanctioned or unsanctioned tool, seen by TLS proxy.');
});

test('every string from an API is escaped: a message, a name, a tool and a person', async () => {
  const rules = servedRules();
  rules.rules[1].message = '<img src=x onerror=alert(1)>';
  rules.rules[1].match.tools = ['<b>tool</b>'];
  rules.users[0].name = '<script>alert(1)</script>';
  const { page, html, panel } = await loaded({ rules, query: fakeQuery({ people: [{ subject: BOB, name: '<i>Bob</i>', department: '<u>d</u>' }] }) });
  await page.act({ rl: 'edit', index: '1' });
  await page.act({ rl: 'pick-open' });
  for (const shown of [html(), panel()]) {
    assert.doesNotMatch(shown, /<img src=x|<b>tool<\/b>|<script>|<i>Bob<\/i>|<u>d<\/u>/);
  }
  assert.match(html(), /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html(), /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(panel(), /&lt;i&gt;Bob&lt;\/i&gt;/);
});

// ---------------------------------------------------------------------------------------------
// Custom labels: the Labels tab
// ---------------------------------------------------------------------------------------------

/** The custom labels card of a rendered page. */
function labelsCard(page) {
  const start = page.indexOf('<h3>Custom labels</h3>');
  assert.ok(start >= 0, 'the page has a custom labels card');
  return page.slice(start, page.indexOf('</section>', start));
}

test('the Labels tab lists each label with its terms and the rules that name it', async () => {
  const { html } = await loaded();
  const page = html({ tab: 'labels', head: { tabs: tabsFor('labels') } });
  assert.match(page, /<a class="page-tab" href="#rules">Rules<\/a><a class="page-tab" href="#labels" aria-current="page">Labels<\/a>/);
  assert.doesNotMatch(page, /<h3>Rules<\/h3>/, 'the rules are on the other tab');
  const card = labelsCard(page);
  assert.match(card, /Project Falcon/);
  assert.match(card, /custom_project_falcon/);
  for (const term of ['falcon roadmap', 'Acme Corp', 'dwg', 'falcon\\*\\.xlsx']) assert.match(card, new RegExp(term));
  assert.match(card, /<span class="v-text" title="allow_finance_falcon">Rule 1<\/span>/, 'a label shows the rules that name it');
  assert.match(card, /data-rl="label-edit" data-label="custom_project_falcon"/);
  assert.match(card, /data-rl="label-add"/);

  const none = await loaded({ settings: settingsBody({ custom_labels: [] }) });
  assert.match(labelsCard(none.html({ tab: 'labels' })), /No custom labels\./);
  const unread = await loaded({ settings: settingsBody({ custom_labels: undefined }) });
  assert.equal(unread.page.state.data.custom_labels, null, 'labels the server did not send are null, never none');
  assert.match(unread.html({ tab: 'labels' }), /The server did not send the custom labels\./);
  assert.doesNotMatch(unread.html({ tab: 'labels' }), /data-rl="label-add"/);
});

test('adding a label sends the whole list with the new label under a code from its name, then re-reads', async () => {
  const { page, requests, html } = await loaded();
  await page.act({ rl: 'label-add' });
  assert.match(labelsCard(html({ tab: 'labels' })), /data-rl-label-draft="display_name"/);
  page.setLabelDraft('display_name', 'Café Unicode');
  page.setLabelDraft('words', 'blue moon\n\nBlue Moon\n  orbit ');
  page.setLabelDraft('file_extensions', '.PDF, tar.gz pdf');
  await page.act({ rl: 'label-save' });
  const sent = puts(requests);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].path, '/admin/v1/settings/custom-labels');
  assert.deepEqual(sent[0].body, {
    custom_labels: [
      falcon(),
      { label: 'custom_cafe_unicode', display_name: 'Café Unicode', words: ['blue moon', 'orbit'], customer_names: [], file_extensions: ['pdf', 'tar.gz'], file_names: [] },
    ],
  });
  assert.ok(requests.slice(-2).some((r) => r.path === '/admin/v1/settings'), 'the page re-reads after the write');
  assert.equal(page.state.labels.editor, null);
  assert.deepEqual(page.state.data.custom_labels.map((l) => l.display_name), ['Project Falcon', 'Café Unicode']);
});

test('editing a label keeps its code, so a rename keeps what rules name', async () => {
  const { page, requests } = await loaded();
  await page.act({ rl: 'label-edit', label: 'custom_project_falcon' });
  assert.equal(page.state.labels.editor.file_extensions, 'dwg');
  page.setLabelDraft('display_name', 'Falcon');
  page.setLabelDraft('customer_names', 'Acme Corp\nGlobex');
  await page.act({ rl: 'label-save' });
  const [sent] = puts(requests);
  assert.deepEqual(sent.body.custom_labels, [{ ...falcon(), display_name: 'Falcon', customer_names: ['Acme Corp', 'Globex'] }]);
});

test('a label the server would refuse is not sent, and says why', async () => {
  const { page, requests, html } = await loaded();
  await page.act({ rl: 'label-add' });
  page.setLabelDraft('display_name', 'project falcon');
  page.setLabelDraft('words', 'x');
  await page.act({ rl: 'label-save' });
  assert.match(page.state.labels.editor.problem.message, /already called/);
  page.setLabelDraft('display_name', 'Other');
  page.setLabelDraft('words', '');
  await page.act({ rl: 'label-save' });
  assert.match(page.state.labels.editor.problem.message, /at least one/);
  page.setLabelDraft('file_extensions', 'pd f!');
  await page.act({ rl: 'label-save' });
  assert.match(page.state.labels.editor.problem.message, /not a file type/);
  assert.equal(puts(requests).length, 0);
  assert.match(labelsCard(html({ tab: 'labels' })), /Not saved\./);
});

test('deleting a label sends the list without it; one a saved or an unsaved rule names is refused with the rule', async () => {
  const used = await loaded();
  await used.page.act({ rl: 'label-delete', label: 'custom_project_falcon' });
  assert.equal(puts(used.requests).length, 0);
  assert.match(labelsCard(used.html({ tab: 'labels' })), /Rule allow_finance_falcon names Project Falcon; take the label out of the rule first\./);

  const rules = servedRules();
  rules.rules[0].match.labels = [];
  const drafted = await loaded({ rules });
  await drafted.page.act({ rl: 'edit', index: '2' });
  await drafted.page.act({ rl: 'match', field: 'labels', value: 'custom_project_falcon' });
  await drafted.page.act({ rl: 'apply' });
  await drafted.page.act({ rl: 'label-delete', label: 'custom_project_falcon' });
  assert.equal(puts(drafted.requests).length, 0, 'an unsaved rule naming the label holds it too');
  assert.match(labelsCard(drafted.html({ tab: 'labels' })), /Rule warn_pii names Project Falcon/);
  assert.match(drafted.html({ tab: 'labels' }), /1 unsaved change\./, 'the unsaved list is said on both tabs');

  const free = await loaded({ rules });
  await free.page.act({ rl: 'label-delete', label: 'custom_project_falcon' });
  assert.deepEqual(puts(free.requests)[0].body, { custom_labels: [] });
  assert.deepEqual(free.page.state.data.custom_labels, []);

  const refused = await loaded({ rules, answers: { [PUT_LABELS]: () => ({ status: 409, body: { error: 'custom_label_in_use', message: 'an enforcement rule names the label' } }) } });
  await refused.page.act({ rl: 'label-delete', label: 'custom_project_falcon' });
  assert.match(labelsCard(refused.html({ tab: 'labels' })), /an enforcement rule names the label/);
  assert.equal(refused.page.state.data.custom_labels.length, 1);
});

test('a label\'s name and terms from the API are escaped', async () => {
  const { page, html } = await loaded({ settings: settingsBody({ custom_labels: [{ ...falcon(), display_name: '<img src=x onerror=alert(1)>', words: ['<b>w</b>'] }] }) });
  await page.act({ rl: 'label-edit', label: 'custom_project_falcon' });
  const card = labelsCard(html({ tab: 'labels' }));
  assert.doesNotMatch(card, /<img src=x|<b>w<\/b>/);
  assert.match(card, /&lt;img src=x onerror=alert\(1\)&gt;/);
});

test('the typed lists of a label and the code a name gets', () => {
  assert.deepEqual(parseTerms('file_extensions', '.PDF, dwg\ntar.gz  .pdf'), ['pdf', 'dwg', 'tar.gz']);
  assert.deepEqual(parseTerms('words', ' a b \n\nA B\nc'), ['a b', 'c']);
  assert.equal(customLabelCode('Project Falcon!'), 'custom_project_falcon');
  assert.equal(customLabelCode('Project Falcon', ['custom_project_falcon']), 'custom_project_falcon_2');
  assert.equal(customLabelCode('***'), 'custom_label');
  assert.equal(customLabelCode('x'.repeat(90)).length, 63);
  const data = normaliseSettings({ custom_labels: [{ label: 'custom_a', display_name: 'A', words: ['w', 3] }, null] });
  assert.deepEqual(JSON.parse(JSON.stringify(data.custom_labels)), [{ label: 'custom_a', display_name: 'A', words: ['w'], customer_names: [], file_extensions: [], file_names: [] }]);
});

// ---------------------------------------------------------------------------------------------
// Where the page sits, through boot()
// ---------------------------------------------------------------------------------------------

function session(roles, pages) {
  return { actor: `${roles[0]}@lab.test`, tenant: '5a3c0de0-7e57-4a11-9000-0000000d3a01', roles, role: roles[0], pages };
}

const ADMIN_PAGES = ['posture', 'tools', 'teams', 'rules', 'labels', 'sanction', 'activity', 'insights', 'settings', 'devices', 'directory', 'audit'];

/** A booted page with a way to fire the events a browser would. */
async function booted(hash, { rules = servedRules(), query = fakeQuery() } = {}) {
  let served = rules;
  const { admin, requests } = fakeAdmin({
    [GET_SETTINGS]: () => ({ status: 200, body: settingsBody() }),
    [GET_RULES]: () => ({ status: 200, body: served }),
    [PUT_RULES]: (spec) => { served = { rules: spec.body.rules, users: served.users }; return { status: 204 }; },
    ...DIRECTORY,
  });
  const doc = fakeDocument(hash);
  const listeners = [];
  const add = doc.addEventListener;
  doc.addEventListener = (type, fn) => { listeners.push({ type, fn }); add(type, fn); };
  const app = await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin, session: session(['admin'], ADMIN_PAGES) });
  const fire = (type, event) => Promise.all(listeners.filter((l) => l.type === type).map((l) => l.fn(event)));
  const settle = async () => {
    for (let i = 0; i < 100 && (app.rules()?.state.pending || app.rules()?.state.hits.pending); i++) await new Promise((resolve) => setTimeout(resolve, 2));
  };
  await settle();
  return { doc, app, requests, fire, settle, query };
}

/** A click on a control, as the browser delivers it: the handler finds it with closest(). */
const click = (dataset, tagName = 'BUTTON') => ({
  target: { closest: (sel) => (sel === '[data-rl]' ? { tagName, disabled: false, dataset } : null) },
  preventDefault() {},
});

test('everyone reads Policy → Rules; only a role that may change settings edits the rules or opens Labels', () => {
  for (const environment of ['prod', 'preprod']) {
    for (const role of ['viewer', 'analyst', 'content_reader', 'admin']) assert.ok(pagesForRoles([role], { environment }).includes('rules'), `${role} in ${environment}`);
    assert.ok(pagesForRoles(['admin'], { environment }).includes('labels'), environment);
    for (const role of ['viewer', 'analyst', 'content_reader']) assert.equal(pagesForRoles([role], { environment }).includes('labels'), false, role);
  }
  assert.deepEqual(tabsFor('rules').items.map((t) => [t.label, t.href]), [['Rules', '#rules'], ['Labels', '#labels'], ['Tools', '#sanction'], ['Activity', '#activity']]);
});

test('an admin opens Policy from the navigation; Rules renders over the admin API, and Labels is a tab of its own address', async () => {
  const { doc, requests } = await booted('#rules');
  assert.match(doc.html('nav'), /<a href="#rules" class="active" aria-current="page"><svg class="nav-icon"[^>]*>.*?<\/svg><span>Policy<\/span><\/a>/);
  assert.match(doc.html('app'), /<h2>Policy<\/h2>/);
  assert.match(doc.html('app'), /<a class="page-tab" href="#rules" aria-current="page">Rules<\/a><a class="page-tab" href="#labels">Labels<\/a>/);
  assert.match(doc.html('app'), /allow_finance_falcon/);
  assert.deepEqual(requests.map((r) => r.path).sort(), ['/admin/v1/settings', '/admin/v1/settings/rules']);
  await doc.navigate('#labels');
  assert.match(doc.html('app'), /<h3>Custom labels<\/h3>/);
  assert.equal(requests.length, 2, 'switching tabs reads nothing again');
  await doc.navigate('#rules?view=labels');
  assert.equal(doc.location.hash, '#labels', 'the old Labels address lands on its tab');
});

test('a viewer reads the rules over the query API, with no control to change them and no Labels tab', async () => {
  const { admin, requests } = fakeAdmin({});
  const doc = fakeDocument('#posture');
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'rules', 'sanction', 'devices']) });
  assert.match(doc.html('nav'), /<a href="#rules"><svg[^>]*>.*?<\/svg><span>Policy<\/span><\/a>/);
  await doc.navigate('#rules');
  const page = doc.html('app');
  assert.match(page, /<h2>Policy<\/h2>/);
  assert.match(page, /<a class="page-tab" href="#rules" aria-current="page">Rules<\/a><a class="page-tab" href="#sanction">Tools<\/a>/);
  assert.doesNotMatch(page, /href="#labels"/);
  assert.match(page, /secrets\.block/);
  assert.doesNotMatch(page, /data-rl=/, 'nothing to edit');
  await doc.navigate('#labels');
  assert.match(doc.html('app'), /This page is not offered to you here\./);
  assert.equal(requests.length, 0, 'the admin API was not asked');
});

test('on the page, clicks, ticks, typing and Enter reach the rules', async () => {
  const { doc, app, fire, settle, requests } = await booted('#rules');
  const rules = app.rules();
  await fire('click', click({ rl: 'toggle', index: '2' }));
  assert.equal(rules.state.draft[2].enabled, true);
  assert.match(doc.html('app'), /1 unsaved change\./);

  await fire('click', click({ rl: 'add' }));
  assert.ok(rules.state.panel, 'Add rule opens the panel');
  assert.match(doc.html('rule-dialog'), /<h3 id="rule-dialog-title">Add a rule<\/h3>/);
  await fire('input', { target: { dataset: { rlDraft: 'rule_id' }, value: 'block_source' } });
  await fire('input', { target: { dataset: { rlDraft: 'message' }, value: 'No source code, please.' } });
  await fire('change', { target: { dataset: {}, value: '0', closest: (sel) => (sel === 'select[data-rl]' ? { dataset: { rl: 'at' }, value: '0' } : null) } });
  await fire('click', click({ rl: 'match', field: 'labels', value: 'source_code' }));
  await fire('click', click({ rl: 'pick-open' }));
  await settle();
  const box = { dataset: { rlPick: `user:${ADA}` }, checked: true };
  await fire('change', { target: box });
  assert.deepEqual(rules.state.panel.rule.users, [`user:${ADA}`]);
  await fire('input', { target: { dataset: { rlDraft: 'query' }, value: 'zz' } });
  await fire('submit', { target: { dataset: { rlForm: 'pick-search' } }, preventDefault() {} });
  await settle();
  assert.equal(rules.state.panel.picker.term, 'zz');
  await fire('click', click({ rl: 'apply' }));
  assert.equal(rules.state.panel, null);
  assert.equal(doc.html('rule-dialog'), '', 'the panel empties when it closes');
  assert.deepEqual(rules.state.draft.map((r) => r.rule_id), ['block_source', 'allow_finance_falcon', 'block_credentials', 'warn_pii']);
  assert.deepEqual(rules.state.draft[0].users, [`user:${ADA}`]);

  await fire('input', { target: { dataset: { rlQuery: '' }, value: 'source' } });
  await fire('submit', { target: { dataset: { rlForm: 'filter' } }, preventDefault() {} });
  assert.match(doc.html('app'), /1 of 4 rules/);
  await fire('input', { target: { dataset: { rlQuery: '' }, value: '' } });
  assert.equal(rules.state.filter, '', 'emptying the filter lists every rule again');

  // Leaving the page closes an open panel; the unsaved list stays.
  await fire('click', click({ rl: 'edit', index: '0' }));
  assert.ok(rules.state.panel);
  await doc.navigate('#posture');
  assert.equal(rules.state.panel, null);
  assert.equal(rules.state.draft.length, 4);
  await doc.navigate('#rules');
  await settle();
  assert.match(doc.html('app'), /2 unsaved changes\./);

  await fire('click', click({ rl: 'save' }));
  await settle();
  const put = requests.find((r) => r.method === 'PUT');
  assert.equal(put.path, '/admin/v1/settings/rules');
  assert.deepEqual(put.body.rules[0], {
    rule_id: 'block_source', action: 'block', match: { labels: ['source_code'], tools: [], categories: [], sanction: [], routes: [] },
    message: 'No source code, please.', users: [`user:${ADA}`], enabled: true,
  });
});

test('Settings no longer reads or shows the rules or the custom labels', async () => {
  const { admin, requests } = fakeAdmin({ [GET_SETTINGS]: () => ({ status: 200, body: settingsBody() }) });
  const settings = createSettings({ admin });
  await settings.load();
  assert.deepEqual(requests.map((r) => r.path), ['/admin/v1/settings']);
  const page = renderSettings(settings.state);
  assert.doesNotMatch(page, /Enforcement rules|Custom labels|data-action="rule-|data-action="label-/);
  assert.match(page, /Kill switch/, 'the kill switches stay in Settings');
});
