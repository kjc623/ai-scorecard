// settings.test.mjs — Settings → Settings, driven without a browser.
//
// The controller (src/settings.js) runs against a fake admin API built on the real createAdminApi,
// so the requests asserted are the ones a browser would send. The renderer
// (src/settings-render.js) is asserted for each state. The one confirmation the page has — a mode
// increase that starts capturing content — is asserted not to send until confirmed.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, normaliseSettings, normaliseRules } from '../src/transport.js';
import { createSettings, modeIncreaseNeedsConfirmation, searchTierAllowed, effectiveMode } from '../src/settings.js';
import { renderSettings } from '../src/settings-render.js';
import { boot } from '../src/app.js';
import { fakeDocument } from './helpers.mjs';
import { fixtureTransport } from './fixtures.mjs';

const queryApi = () => createQueryApi({ transport: fixtureTransport() });

function populated(overrides = {}) {
  return {
    ceiling_mode: 'm3',
    collection_mode: 'm3',
    scope_overrides: { tls_b6681b043244c43f: 'm1' },
    event_retention_days: 120,
    content_retention_days: null,
    retention_defaults: { event_days: 90, content_days: 30 },
    content_search: 'attachment_names',
    tools: [
      { tool_fingerprint: 'tls_b6681b043244c43f', display_name: 'Claude Code', sanctioned_state: 'unsanctioned' },
      { tool_fingerprint: 'tls_f32477ff734d70d1', display_name: 'OpenAI API', sanctioned_state: 'unknown' },
    ],
    devices: [{ device_id: 'd-1', hostname: 'LAPTOP-1', collection_mode: 'm2', last_seen_at: '2026-10-01T09:00:00Z' }],
    endpoint: endpointDefaults(),
    tls_inspection: false,
    data_classes: ['credential', 'customer_pii', 'government_id', 'health', 'legal_commercial', 'payment_card', 'source_code'],
    app_categories: ['chat_assistant', 'coding_agent', 'ide', 'ide_assistant', 'inference_api', 'local_runtime'],
    ...overrides,
  };
}

/** The endpoint settings control-api serves a tenant that never changed them. */
function endpointDefaults() {
  return {
    inventory: true, processes: true, flows: true, otel: true, hooks: true, hooks_managed_only: false,
    tools: {
      claude_code: { otel: true, hooks: true },
      codex: { otel: true, hooks: false },
      copilot: { otel: true, hooks: false },
      cursor: { otel: false, hooks: true },
    },
  };
}

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

const GET = 'GET /admin/v1/settings';

async function loaded(answers) {
  const { admin, requests } = fakeAdmin(answers);
  const changes = [];
  const controller = createSettings({ admin, onChange: (s) => changes.push(s.status) });
  await controller.load();
  const html = () => renderSettings(controller.state, { eyebrow: 'Settings' });
  return { controller, requests, changes, html };
}

// ---------------------------------------------------------------------------------------------
// The states
// ---------------------------------------------------------------------------------------------

test('loading: the page shows its frame and placeholders, and says it is loading', async () => {
  let release;
  const { admin } = fakeAdmin({ [GET]: () => new Promise((r) => { release = r; }) });
  const controller = createSettings({ admin });
  const pending = controller.load();
  assert.equal(controller.state.status, 'loading');
  const html = renderSettings(controller.state, { eyebrow: 'Settings' });
  assert.match(html, /<h2>Settings<\/h2>/);
  assert.match(html, /aria-busy="true"/);
  assert.match(html, /dp-skel/);
  release({ status: 200, body: populated() });
  await pending;
  assert.equal(controller.state.status, 'ready');
});

test('refused: a 403 says only an admin can open it', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 403, body: { error: 'forbidden', message: 'admin only' } }) });
  assert.match(html(), /Only an admin can open the settings/);
  const down = await loaded({ [GET]: () => { throw new Error('connection refused'); } });
  assert.match(down.html(), /The admin API could not be reached/);
});

test('populated: requested mode, the applied device mode, retention and sanction are all shown', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 200, body: populated() }) });
  const page = html();
  assert.match(page, /Collection mode/);
  assert.match(page, /M3 · prompt/);
  assert.match(page, /Claude Code/);
  assert.match(page, /LAPTOP-1/);
  assert.match(page, /Applied mode/, 'the devices table shows the mode each device actually applied');
  assert.match(page, /<span class="v-vocab v-vocab-m">m2<\/span>/, 'the device\'s applied mode (m2) is shown, not only the requested mode (m3)');
  assert.match(page, /Full text/, 'the three search tiers are offered');
  assert.match(page, /unsanctioned/);
});

// ---------------------------------------------------------------------------------------------
// Collection mode, with the confirmation step
// ---------------------------------------------------------------------------------------------

test('a mode increase that starts capturing content asks for confirmation before it is sent', async () => {
  let mode = 'm0';
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ collection_mode: mode, ceiling_mode: 'm3' }) }),
    'PUT /admin/v1/settings/collection-mode': (spec) => { mode = spec.body.collection_mode; return { status: 204 }; },
  });
  // M0 -> M2 reads content: it must not send until confirmed.
  await controller.chooseMode('m2');
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 0, 'no write before confirmation');
  assert.match(html(), /This starts capturing content/);
  await controller.act({ action: 'mode-confirm' });
  const put = requests.find((r) => r.method === 'PUT');
  assert.deepEqual(put.body, { collection_mode: 'm2' });
  // A decrease back needs no confirmation.
  await controller.chooseMode('m1');
  assert.equal(requests.filter((r) => r.method === 'PUT').at(-1).body.collection_mode, 'm1');
});

test('a refused collection mode is said beside the control, and nothing is changed', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ collection_mode: 'm1', ceiling_mode: 'm1' }) }),
    'PUT /admin/v1/settings/collection-mode': () => ({ status: 409, body: { error: 'collection_exceeds_ceiling', message: 'The requested mode is above the ceiling.' } }),
  });
  await controller.chooseMode('m2');
  assert.match(html(), /Not changed\./);
  assert.match(html(), /above the ceiling/);
  assert.match(html(), /collection_exceeds_ceiling/);
});

// ---------------------------------------------------------------------------------------------
// Refused combinations and search tier
// ---------------------------------------------------------------------------------------------

test('a search tier the ceiling cannot back is refused with a clear message', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ ceiling_mode: 'm1', content_search: 'disabled' }) }),
    'PUT /admin/v1/settings/content-search': () => ({ status: 409, body: { error: 'search_tier_requires_mode', message: 'This search tier needs a higher ceiling.' } }),
  });
  await controller.setContentSearch('full_text');
  assert.match(html(), /Not changed\./);
  assert.match(html(), /needs a higher ceiling/);
});

test('the confirmation and tier-allowed predicates', () => {
  assert.equal(modeIncreaseNeedsConfirmation('m0', 'm1'), true, 'M0 -> M1 reads content');
  assert.equal(modeIncreaseNeedsConfirmation('m0', 'm3'), true);
  assert.equal(modeIncreaseNeedsConfirmation('m2', 'm3'), true, 'M2 -> M3 holds the prompt');
  assert.equal(modeIncreaseNeedsConfirmation('m1', 'm2'), false, 'an excerpt is not the start of content capture');
  assert.equal(modeIncreaseNeedsConfirmation('m3', 'm1'), false, 'a decrease needs no confirmation');
  assert.equal(searchTierAllowed('full_text', 'm3'), true);
  assert.equal(searchTierAllowed('full_text', 'm2'), false);
  assert.equal(searchTierAllowed('attachment_names', 'm1'), true);
  assert.equal(searchTierAllowed('attachment_names', 'm0'), false);
  assert.equal(effectiveMode({ collection_mode: '', ceiling_mode: 'm3' }), 'm3');
  assert.equal(effectiveMode({ collection_mode: 'm1', ceiling_mode: 'm3' }), 'm1');
});

// ---------------------------------------------------------------------------------------------
// Retention and sanction
// ---------------------------------------------------------------------------------------------

test('retention drafts validate before the write, and the write carries the days', async () => {
  let retention = null;
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'PUT /admin/v1/settings/retention': (spec) => { retention = spec.body; return { status: 204 }; },
  });
  controller.setRetentionDraft('event', '');
  await controller.act({ action: 'save-retention', appliesTo: 'event' });
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 0, 'an empty draft sends nothing');
  assert.match(html(), /whole number of days/);
  controller.setRetentionDraft('event', '180');
  await controller.act({ action: 'save-retention', appliesTo: 'event' });
  assert.deepEqual(retention, { applies_to: 'event', ttl_days: 180 });
});

test('a tool sanction writes the decision and re-reads', async () => {
  let state = populated();
  const { controller, requests } = await loaded({
    [GET]: () => ({ status: 200, body: state }),
    'PUT /admin/v1/settings/tools/tls_b6681b043244c43f/sanction': (spec) => {
      state = { ...state, tools: state.tools.map((t) => (t.tool_fingerprint === 'tls_b6681b043244c43f' ? { ...t, sanctioned_state: spec.body.sanctioned_state } : t)) };
      return { status: 204 };
    },
  });
  await controller.setToolSanction('tls_b6681b043244c43f', 'sanctioned');
  const put = requests.find((r) => r.method === 'PUT');
  assert.equal(put.path, '/admin/v1/settings/tools/tls_b6681b043244c43f/sanction');
  assert.deepEqual(put.body, { sanctioned_state: 'sanctioned' });
  assert.equal(controller.state.data.tools[0].sanctioned_state, 'sanctioned');
});

// ---------------------------------------------------------------------------------------------
// Endpoint collectors
// ---------------------------------------------------------------------------------------------

/** The Endpoint collectors card of a rendered page. */
function endpointCard(page) {
  const start = page.indexOf('<h3>Endpoint collectors</h3>');
  assert.ok(start >= 0, 'the page has an Endpoint collectors card');
  return page.slice(start, page.indexOf('</section>', start));
}

/** The pressed button of one switch, by its data attributes. */
function pressed(card, attrs) {
  const m = card.match(new RegExp(`aria-pressed="true"${attrs} data-value="(on|off)"`));
  assert.ok(m, `a pressed button for ${attrs}`);
  return m[1];
}

test('endpoint collectors: the card shows the defaults, and a collector a tool lacks as unavailable', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 200, body: populated() }) });
  const card = endpointCard(html());
  for (const label of ['Inventory', 'Processes', 'Network flows', 'OpenTelemetry', 'Hooks', 'Only managed hooks', 'Claude Code', 'Codex', 'Copilot', 'Cursor']) {
    assert.match(card, new RegExp(`>${label}<`), `${label} is shown`);
  }
  for (const c of ['inventory', 'processes', 'flows', 'otel', 'hooks']) {
    assert.equal(pressed(card, ` data-action="endpoint" data-collector="${c}"`), 'on', `${c} is on by default`);
  }
  assert.equal(pressed(card, ' data-action="endpoint" data-collector="hooks_managed_only"'), 'off');
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="claude_code" data-collector="hooks"'), 'on');
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="cursor" data-collector="hooks"'), 'on');
  // Cursor has no OTel; Codex and Copilot have no hooks: three cells, none of them a control.
  assert.equal((card.match(/<span class="v-absent">unavailable<\/span>/g) ?? []).length, 3);
  assert.doesNotMatch(card, /data-tool="cursor" data-collector="otel"/);
  assert.doesNotMatch(card, /data-tool="codex" data-collector="hooks"/);
  assert.doesNotMatch(card, /data-tool="copilot" data-collector="hooks"/);
});

test('endpoint collectors: switching one sends every switch, and the page re-reads', async () => {
  let body = populated();
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body }),
    'PUT /admin/v1/settings/endpoint': (spec) => { body = { ...body, endpoint: { ...body.endpoint, ...spec.body } }; return { status: 204 }; },
  });
  await controller.act({ action: 'endpoint', collector: 'inventory', value: 'off' });
  const puts = requests.filter((r) => r.method === 'PUT');
  assert.equal(puts.length, 1);
  assert.equal(puts[0].path, '/admin/v1/settings/endpoint');
  assert.deepEqual(puts[0].body, { inventory: false, processes: true, flows: true, otel: true, hooks: true, hooks_managed_only: false });
  assert.equal(requests.at(-1).method, 'GET', 'the page re-reads after the write');
  assert.equal(pressed(endpointCard(html()), ' data-action="endpoint" data-collector="inventory"'), 'off');
  // The switch already in that position sends nothing; a value that is not on or off sends nothing.
  await controller.act({ action: 'endpoint', collector: 'inventory', value: 'off' });
  await controller.act({ action: 'endpoint', collector: 'flows', value: 'maybe' });
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 1);
});

test('endpoint collectors: a tool switch sends both of its collectors to its own path', async () => {
  let body = populated();
  const { controller, requests } = await loaded({
    [GET]: () => ({ status: 200, body }),
    'PUT /admin/v1/settings/endpoint/tools/cursor': (spec) => {
      body = { ...body, endpoint: { ...body.endpoint, tools: { ...body.endpoint.tools, cursor: spec.body } } };
      return { status: 204 };
    },
  });
  await controller.act({ action: 'endpoint-tool', tool: 'cursor', collector: 'hooks', value: 'off' });
  const put = requests.find((r) => r.method === 'PUT');
  assert.equal(put.path, '/admin/v1/settings/endpoint/tools/cursor');
  assert.deepEqual(put.body, { otel: false, hooks: false });
  assert.equal(controller.state.data.endpoint.tools.cursor.hooks, false);
  // A collector the tool does not have is never sent.
  await controller.act({ action: 'endpoint-tool', tool: 'cursor', collector: 'otel', value: 'on' });
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 1);
});

test('endpoint collectors: a refused write is said in the card, and settings the server did not send are not reported', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'PUT /admin/v1/settings/endpoint': () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'every switch is required' } } }),
  });
  await controller.act({ action: 'endpoint', collector: 'hooks', value: 'off' });
  const card = endpointCard(html());
  assert.match(card, /Not changed\./);
  assert.match(card, /every switch is required/);

  const { endpoint: _drop, ...without } = populated();
  const missing = await loaded({ [GET]: () => ({ status: 200, body: without }) });
  assert.match(endpointCard(missing.html()), /not reported/);
  await missing.controller.act({ action: 'endpoint', collector: 'hooks', value: 'off' });
  assert.equal(missing.requests.filter((r) => r.method === 'PUT').length, 0, 'nothing is written over settings that were not read');
});

// ---------------------------------------------------------------------------------------------
// TLS inspection
// ---------------------------------------------------------------------------------------------

/** The TLS inspection card of a rendered page. */
function tlsCard(page) {
  const start = page.indexOf('<h3>TLS inspection</h3>');
  assert.ok(start >= 0, 'the page has a TLS inspection card');
  return page.slice(start, page.indexOf('</section>', start));
}

test('TLS inspection: the switch shows off by default, with what turning it on installs', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 200, body: populated() }) });
  const card = tlsCard(html());
  assert.equal(pressed(card, ' data-action="tls-inspection"'), 'off');
  assert.match(card, /aria-label="TLS inspection"/);
  assert.match(card, /installs each device's own root certificate in its trust store/);
  assert.match(card, /local proxy/);
});

test('TLS inspection: switching it on sends the setting, and the page re-reads', async () => {
  let body = populated();
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body }),
    'PUT /admin/v1/settings/tls-inspection': (spec) => { body = { ...body, tls_inspection: spec.body.enabled }; return { status: 204 }; },
  });
  await controller.act({ action: 'tls-inspection', value: 'on' });
  const puts = requests.filter((r) => r.method === 'PUT');
  assert.equal(puts.length, 1);
  assert.equal(puts[0].path, '/admin/v1/settings/tls-inspection');
  assert.deepEqual(puts[0].body, { enabled: true });
  assert.equal(requests.at(-1).method, 'GET', 'the page re-reads after the write');
  assert.equal(controller.state.data.tls_inspection, true);
  assert.equal(pressed(tlsCard(html()), ' data-action="tls-inspection"'), 'on');
  // The switch already in that position sends nothing; a value that is not on or off sends nothing.
  await controller.act({ action: 'tls-inspection', value: 'on' });
  await controller.act({ action: 'tls-inspection', value: 'maybe' });
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 1);
  await controller.act({ action: 'tls-inspection', value: 'off' });
  assert.deepEqual(requests.filter((r) => r.method === 'PUT').at(-1).body, { enabled: false });
  assert.equal(controller.state.data.tls_inspection, false);
});

test('TLS inspection: a refused write is said in the card, and a setting the server did not send is not reported', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'PUT /admin/v1/settings/tls-inspection': () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'enabled is required' } } }),
  });
  await controller.act({ action: 'tls-inspection', value: 'on' });
  const card = tlsCard(html());
  assert.match(card, /Not changed\./);
  assert.match(card, /enabled is required/);
  assert.equal(controller.state.data.tls_inspection, false);

  const { tls_inspection: _drop, ...without } = populated();
  const missing = await loaded({ [GET]: () => ({ status: 200, body: without }) });
  assert.match(tlsCard(missing.html()), /not reported/);
  assert.doesNotMatch(tlsCard(missing.html()), /data-action="tls-inspection"/);
  await missing.controller.act({ action: 'tls-inspection', value: 'on' });
  assert.equal(missing.requests.filter((r) => r.method === 'PUT').length, 0, 'nothing is written over a setting that was not read');
});

// ---------------------------------------------------------------------------------------------
// Enforcement rules
// ---------------------------------------------------------------------------------------------

const RULES = 'GET /admin/v1/settings/rules';
const PUT_RULES = 'PUT /admin/v1/settings/rules';

/** Two rules in control-api's spelling: every match list present, no link as absent. */
function servedRules() {
  return {
    rules: [
      {
        rule_id: 'block_credentials', action: 'block',
        match: { labels: ['credential'], tools: ['tls_b6681b043244c43f'], categories: ['coding_agent'], sanction: ['unsanctioned'], routes: ['tool.hook'] },
        message: 'Remove the credential and try again.', link: 'https://intranet.example/ai',
      },
      { rule_id: 'allow_rest', action: 'allow', match: { labels: [], tools: [], categories: [], sanction: [], routes: [] }, message: '' },
    ],
  };
}

/** The enforcement rules card of a rendered page. */
function rulesCard(page) {
  const start = page.indexOf('<h3>Enforcement rules</h3>');
  assert.ok(start >= 0, 'the page has an enforcement rules card');
  return page.slice(start, page.indexOf('</section>', start));
}

/** A page whose rules route serves `rules` and whose PUT stores what it is sent. */
async function withRules(rules = servedRules(), put = null) {
  let served = rules;
  const answers = {
    [GET]: () => ({ status: 200, body: populated() }),
    [RULES]: () => ({ status: 200, body: served }),
    [PUT_RULES]: put ?? ((spec) => { served = { rules: spec.body.rules }; return { status: 204 }; }),
  };
  return loaded(answers);
}

const puts = (requests) => requests.filter((r) => r.method === 'PUT');

test('enforcement rules: the card shows the ordered list, each condition by its name', async () => {
  const { html, requests } = await withRules();
  assert.ok(requests.some((r) => r.method === 'GET' && r.path === '/admin/v1/settings/rules'), 'the page reads the rules');
  const card = rulesCard(html());
  const rows = card.slice(card.indexOf('<tbody>'), card.indexOf('</tbody>')).split('<tr>').slice(1);
  assert.equal(rows.length, 2);
  assert.match(rows[0], /<td>1<\/td>/);
  assert.match(rows[0], /v-vocab-block/);
  assert.match(rows[0], />credential</);
  assert.match(rows[0], />Claude Code</, 'a tool is shown by its name');
  assert.match(rows[0], />Coding agent</, 'a category is shown by its name');
  assert.match(rows[0], />unsanctioned</);
  assert.match(rows[0], />Tool hooks</, 'a route is shown by its display name');
  assert.match(rows[0], /Remove the credential and try again\./);
  assert.match(rows[0], /https:\/\/intranet\.example\/ai/);
  assert.match(rows[1], /v-vocab-allow/);
  assert.match(rows[1], /Any submission/);
  assert.match(rows[1], /<span class="v-absent">none<\/span>/, 'an allow with no message says none');
  // The first rule cannot move up, the last cannot move down.
  assert.match(rows[0], /data-action="rule-up" data-index="0" disabled/);
  assert.doesNotMatch(rows[0], /data-action="rule-down" data-index="0" disabled/);
  assert.match(rows[1], /data-action="rule-down" data-index="1" disabled/);
  assert.doesNotMatch(card, /data-action="rules-save"/, 'nothing to save before a change');

  const none = await withRules({ rules: [] });
  assert.match(rulesCard(none.html()), /No rules\. Every submission is logged\./);
});

test('enforcement rules: the editor offers the data classes, the tools, the catalog categories, sanction and the routes', async () => {
  const { controller, html } = await withRules();
  await controller.act({ action: 'rule-edit', index: '0' });
  const card = rulesCard(html());
  const option = (field, value) => new RegExp(`aria-pressed="(true|false)" data-action="rule-match" data-field="${field}" data-value="${value.replace('.', '\\.')}">`);
  for (const c of ['credential', 'customer_pii', 'government_id', 'health', 'legal_commercial', 'payment_card', 'source_code']) assert.match(card, option('labels', c));
  for (const t of ['tls_b6681b043244c43f', 'tls_f32477ff734d70d1']) assert.match(card, option('tools', t));
  for (const c of ['chat_assistant', 'coding_agent', 'ide_assistant', 'ide', 'local_runtime', 'inference_api']) assert.match(card, option('categories', c));
  assert.doesNotMatch(card, option('categories', 'ai_feature'), 'a category no catalog app falls in is not offered');
  assert.ok(card.indexOf('data-value="chat_assistant"') < card.indexOf('data-value="local_runtime"'), 'categories are offered in the display order');
  assert.match(card, />Local model runtime</, 'categories are offered by their display names');
  for (const s of ['sanctioned', 'unsanctioned']) assert.match(card, option('sanction', s));
  for (const r of ['tool.hook', 'ext.page_context', 'tool.otel', 'cli.shim', 'proxy.loopback', 'ext.web_request', 'proxy.tls', 'ext.dom', 'proc.detect', 'inv.scan', 'net.flow']) assert.match(card, option('routes', r));
  assert.match(card, />Browser extension \(page\)</, 'routes are offered by their display names');
  // The rule's own choices are pressed; the others are not.
  assert.equal(card.match(option('labels', 'credential'))[1], 'true');
  assert.equal(card.match(option('labels', 'health'))[1], 'false');
  assert.equal(card.match(option('routes', 'tool.hook'))[1], 'true');
  assert.match(card, /data-rule-draft="message" value="Remove the credential and try again\."/);
  // While a rule is open the list cannot be reordered or saved.
  assert.match(card, /data-action="rule-down" data-index="0" disabled/);
});

test('enforcement rules: categories the server did not report are not reported, an empty catalog has none known yet', async () => {
  for (const [categories, text] of [[undefined, 'not reported'], [[], 'none known yet']]) {
    const { controller, html } = await loaded({
      [GET]: () => ({ status: 200, body: populated({ app_categories: categories }) }),
      [RULES]: () => ({ status: 200, body: { rules: [] } }),
    });
    await controller.act({ action: 'rule-add' });
    const card = rulesCard(html());
    const picker = card.slice(card.indexOf('>Categories</span>'), card.indexOf('>Sanction</span>'));
    assert.match(picker, new RegExp(`<span class="v-absent">${text}</span>`));
    assert.doesNotMatch(picker, /data-field="categories"/);
  }
});

test('enforcement rules: add, edit, move and delete change a draft; saving sends the whole list, then re-reads', async () => {
  const { controller, requests, html } = await withRules();

  // Add a warn rule for an unsanctioned coding agent.
  await controller.act({ action: 'rule-add' });
  controller.setRuleDraft('rule_id', 'warn_unsanctioned_agents');
  controller.setRuleDraft('message', 'Use the approved coding agent.');
  controller.setRuleDraft('link', 'https://intranet.example/agents');
  await controller.act({ action: 'rule-action', value: 'warn' });
  await controller.act({ action: 'rule-match', field: 'categories', value: 'coding_agent' });
  await controller.act({ action: 'rule-match', field: 'sanction', value: 'unsanctioned' });
  await controller.act({ action: 'rule-match', field: 'routes', value: 'proxy.tls' });
  await controller.act({ action: 'rule-match', field: 'routes', value: 'proxy.tls' }); // toggled off again
  await controller.act({ action: 'rule-apply' });
  assert.equal(controller.state.rules.editor, null, 'a valid rule closes the editor');
  assert.equal(puts(requests).length, 0, 'nothing is sent until the list is saved');
  assert.match(rulesCard(html()), /Unsaved changes/);

  // Edit the first rule's message and drop its tool; move the new rule to the top; delete the allow.
  await controller.act({ action: 'rule-edit', index: '0' });
  controller.setRuleDraft('message', 'Take the credential out first.');
  await controller.act({ action: 'rule-match', field: 'tools', value: 'tls_b6681b043244c43f' });
  await controller.act({ action: 'rule-apply' });
  await controller.act({ action: 'rule-up', index: '2' });
  await controller.act({ action: 'rule-up', index: '1' });
  await controller.act({ action: 'rule-delete', index: '2' });
  const shown = rulesCard(html());
  assert.ok(shown.indexOf('Use the approved coding agent.') < shown.indexOf('Take the credential out first.'), 'the moved rule is shown first');
  assert.doesNotMatch(shown, /Any submission/, 'the deleted rule is gone');

  await controller.act({ action: 'rules-save' });
  const sent = puts(requests);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].path, '/admin/v1/settings/rules');
  assert.deepEqual(sent[0].body, {
    rules: [
      {
        rule_id: 'warn_unsanctioned_agents', action: 'warn',
        match: { labels: [], tools: [], categories: ['coding_agent'], sanction: ['unsanctioned'], routes: [] },
        message: 'Use the approved coding agent.', link: 'https://intranet.example/agents',
      },
      {
        rule_id: 'block_credentials', action: 'block',
        match: { labels: ['credential'], tools: [], categories: ['coding_agent'], sanction: ['unsanctioned'], routes: ['tool.hook'] },
        message: 'Take the credential out first.', link: 'https://intranet.example/ai',
      },
    ],
  });
  assert.equal(requests.at(-1).path, '/admin/v1/settings/rules', 'the page re-reads the rules after the write');
  assert.equal(controller.state.rules.draft, null, 'the saved list is the one read back');
  assert.deepEqual(controller.state.data.rules.map((r) => r.rule_id), ['warn_unsanctioned_agents', 'block_credentials']);
  assert.doesNotMatch(rulesCard(html()), /Unsaved changes/);

  // Discarding a draft goes back to the list read, and sends nothing.
  await controller.act({ action: 'rule-delete', index: '0' });
  await controller.act({ action: 'rules-discard' });
  assert.equal(controller.state.rules.draft, null);
  assert.equal(puts(requests).length, 1);
});

test('enforcement rules: a rule the server would refuse is not kept, and says why', async () => {
  const { controller, html } = await withRules();
  const attempt = async (fields, action = 'block') => {
    await controller.act({ action: 'rule-add' });
    for (const [k, v] of Object.entries(fields)) controller.setRuleDraft(k, v);
    await controller.act({ action: 'rule-action', value: action });
    await controller.act({ action: 'rule-apply' });
    const problem = controller.state.rules.editor?.problem ?? null;
    await controller.act({ action: 'rule-cancel' });
    return problem;
  };
  assert.match((await attempt({ rule_id: 'needs_message', message: '' })).message, /needs a message/);
  assert.match((await attempt({ rule_id: 'needs_message', message: '  ' }, 'warn')).message, /needs a message/);
  assert.match((await attempt({ rule_id: 'Upper', message: 'x' })).message, /lower-case letter/);
  assert.match((await attempt({ rule_id: '', message: 'x' })).message, /lower-case letter/);
  assert.match((await attempt({ rule_id: 'allow_rest', message: 'x' })).message, /already has the id allow_rest/);
  assert.match((await attempt({ rule_id: 'long', message: 'é'.repeat(281) })).message, /281 characters; at most 280/);
  assert.match((await attempt({ rule_id: 'plain_http', message: 'x', link: 'http://intranet.example' })).message, /https:\/\//);
  assert.equal(controller.state.rules.draft, null, 'no refused rule reached the list');

  // An allow needs no message; 280 characters are kept.
  assert.equal(await attempt({ rule_id: 'quiet_allow', message: '' }, 'allow'), null);
  assert.equal(await attempt({ rule_id: 'longest', message: 'é'.repeat(280) }, 'warn'), null);
  assert.deepEqual(controller.state.rules.draft.map((r) => r.rule_id), ['block_credentials', 'allow_rest', 'quiet_allow', 'longest']);

  // The problem is shown in the open editor.
  await controller.act({ action: 'rule-add' });
  await controller.act({ action: 'rule-apply' });
  assert.match(rulesCard(html()), /Not kept\./);
});

test('enforcement rules: a refused save keeps the draft and says why; rules that were not read cannot be saved over', async () => {
  const { controller, html } = await withRules(servedRules(),
    () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'a rule names a label that is not a data class' } } }));
  await controller.act({ action: 'rule-delete', index: '1' });
  await controller.act({ action: 'rules-save' });
  const card = rulesCard(html());
  assert.match(card, /Not changed\./);
  assert.match(card, /a rule names a label that is not a data class/);
  assert.equal(controller.state.rules.draft.length, 1, 'the draft is kept to correct and save again');

  const unread = await loaded({ [GET]: () => ({ status: 200, body: populated() }), [RULES]: () => ({ status: 503, body: { error: 'unavailable' } }) });
  assert.equal(unread.controller.state.data.rules, null, 'rules that were not read are null, never an empty list');
  assert.match(rulesCard(unread.html()), /not reported/);
  assert.doesNotMatch(rulesCard(unread.html()), /data-action="rule-add"/);
  await unread.controller.act({ action: 'rule-add' });
  await unread.controller.act({ action: 'rules-save' });
  assert.equal(puts(unread.requests).length, 0, 'nothing is written over rules that were not read');
  assert.match(rulesCard(unread.html()), /were not read/);
});

test('enforcement rules: a message, a rule id and a tool name from the API are escaped', async () => {
  const rules = servedRules();
  rules.rules[0].message = '<img src=x onerror=alert(1)>';
  rules.rules[0].match.tools = ['<b>tool</b>'];
  const { controller, html } = await withRules(rules);
  await controller.act({ action: 'rule-edit', index: '0' });
  const card = rulesCard(html());
  assert.doesNotMatch(card, /<img src=x/);
  assert.doesNotMatch(card, /<b>tool<\/b>/);
  assert.match(card, /&lt;img src=x onerror=alert\(1\)&gt;/);
});

test('every string from the API is escaped', async () => {
  const body = populated();
  body.tools[0].display_name = '<img src=x onerror=alert(1)>';
  const { html } = await loaded({ [GET]: () => ({ status: 200, body }) });
  assert.doesNotMatch(html(), /<img src=x/);
  assert.match(html(), /&lt;img src=x onerror=alert\(1\)&gt;/);
});

// ---------------------------------------------------------------------------------------------
// Navigation by role, through boot()
// ---------------------------------------------------------------------------------------------

function session(roles, pages) {
  return { actor: `${roles[0]}@lab.test`, tenant: '5a3c0de0-7e57-4a11-9000-0000000d3a01', roles, role: roles[0], pages };
}

test('a viewer is shown no Settings; reaching it directly is refused in the page', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#posture');
  await boot({ document: doc, api: queryApi(), admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'devices']) });
  assert.doesNotMatch(doc.html('nav'), />Settings</);
  await doc.navigate('#settings');
  assert.match(doc.html('app'), /Your role cannot use this page/);
  assert.equal(requests.length, 0, 'the admin API was not asked');
});

test('an admin is shown Settings → Settings and the page renders over the admin API', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#settings');
  await boot({ document: doc, api: queryApi(), admin, session: session(['admin'], ['posture', 'tools', 'teams', 'audit', 'settings', 'deployment']) });
  const nav = doc.html('nav');
  assert.match(nav, />Settings</);
  assert.match(nav, />Deployment</);
  assert.match(doc.html('app'), /Collection mode/);
  assert.match(doc.html('app'), /Tool sanction/);
  assert.equal(requests[0].path, '/admin/v1/settings');
});

test('an absent count is null after normalising, never 0', () => {
  const data = normaliseSettings({ tools: [], devices: [], scope_overrides: {}, retention_defaults: {} });
  assert.equal(data.event_retention_days, null);
  assert.equal(data.content_retention_days, null);
  assert.equal(data.collection_mode, null);
  assert.equal(data.content_search, 'disabled');
  assert.deepEqual(data.scope_overrides, {});
  assert.equal(data.endpoint, null, 'endpoint settings the server did not send are null, never all off');
  assert.equal(data.tls_inspection, null, 'a TLS inspection setting the server did not send is null, never off');
  assert.equal(data.data_classes, null, 'data classes the server did not send are null, never none');
  assert.equal(data.app_categories, null, 'app categories the server did not send are null, never none');
  const rules = normaliseRules([{ rule_id: 'r', action: 'warn', match: { labels: ['credential', 7], routes: 'tool.hook' } }, null]);
  assert.deepEqual(JSON.parse(JSON.stringify(rules)), [{ rule_id: 'r', action: 'warn', match: { labels: ['credential'], tools: [], categories: [], sanction: [], routes: [] }, message: '', link: '' }]);
  const partial = normaliseSettings({ endpoint: { inventory: false, tools: { cursor: { hooks: true } } } });
  assert.equal(partial.endpoint.inventory, false);
  assert.equal(partial.endpoint.flows, null);
  assert.equal(partial.endpoint.tools.cursor.otel, null);
});
