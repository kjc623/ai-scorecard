// settings.test.mjs — Settings → Collection, and the sanction decisions an admin makes on Policy →
// Tools, driven without a browser.
//
// The controller (src/settings.js) runs against a fake admin API built on the real createAdminApi,
// so the requests asserted are the ones a browser would send. The renderers
// (src/settings-render.js) are asserted for each state. The one confirmation the page has — a mode
// increase that starts capturing content — is asserted not to send until confirmed.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, normaliseSettings } from '../src/transport.js';
import { createSettings, modeIncreaseNeedsConfirmation, searchTierAllowed, effectiveMode } from '../src/settings.js';
import { renderSanctionDecisions, renderSettings } from '../src/settings-render.js';
import { boot } from '../src/app.js';
import { fakeDocument } from './helpers.mjs';
import { fixtureTransport } from './fixtures.mjs';

const queryApi = () => createQueryApi({ transport: fixtureTransport() });

function populated(overrides = {}) {
  return {
    ceiling_mode: 'm3',
    collection_mode: 'm3',
    scope_overrides: { claude_code: 'm1' },
    event_retention_days: 120,
    content_retention_days: null,
    retention_defaults: { event_days: 90, content_days: 30 },
    content_search: 'attachment_names',
    tools: [
      { tool_key: 'claude_code', display_name: 'Claude Code', sanctioned_state: 'unsanctioned', fingerprints: ['app:claude_code', 'tls_b6681b043244c43f'] },
      { tool_key: 'openai_api', display_name: 'OpenAI API', sanctioned_state: 'unknown', fingerprints: ['app:openai_api', 'tls_f32477ff734d70d1'] },
    ],
    endpoint: endpointDefaults(),
    tls_inspection: false,
    kill_switches: [],
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
      codex: { otel: true, hooks: true },
      copilot: { otel: true, hooks: false },
      cursor: { otel: false, hooks: true },
      ollama: { loopback: false },
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
  const html = () => renderSettings(controller.state) + renderSanctionDecisions(controller.state);
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
  const html = renderSettings(controller.state);
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

test('populated: Collection shows the requested mode and retention; the sanction decisions list every tool', async () => {
  const { controller } = await loaded({ [GET]: () => ({ status: 200, body: populated() }) });
  const page = renderSettings(controller.state);
  assert.match(page, /Collection mode/);
  assert.match(page, /M3 · prompt/);
  assert.match(page, /Claude Code/);
  assert.match(page, /Store and search prompts/, 'prompt storage is one switch');
  assert.doesNotMatch(page, /Attachment names|Full text|bytes|MB/, 'no search tiers and no byte allowance are offered');
  assert.doesNotMatch(page, /Sanction decisions/, 'sanction is decided on Policy → Tools');
  const decisions = renderSanctionDecisions(controller.state);
  assert.match(decisions, /<h3>Sanction decisions<\/h3>/);
  assert.match(decisions, /aria-label="Sanction for Claude Code"/);
  assert.match(decisions, /unsanctioned/);
  assert.match(decisions, /A tool with no decision is <em>unknown<\/em>, never unsanctioned\./);
});

test('the prompt switch turns storage and search on as one setting, and says what off means', async () => {
  const sent = [];
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ content_search: 'disabled' }) }),
    'PUT /admin/v1/settings/content-search': (spec) => { sent.push(spec.body); return { status: 204 }; },
  });
  assert.match(html(), /aria-pressed="true" data-action="search" data-value="disabled">Off</);
  assert.match(html(), /Prompts stay on the devices, so the Search page finds none\./);
  await controller.act({ action: 'search', value: 'full_text' });
  assert.deepEqual(sent, [{ content_search: 'full_text' }]);
});

test('below an M3 ceiling the prompt switch cannot be turned on', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 200, body: populated({ ceiling_mode: 'm2', content_search: 'disabled' }) }) });
  assert.match(html(), /data-action="search" data-value="full_text" disabled>On</);
  assert.match(html(), /Needs an M3 ceiling/);
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
    'PUT /admin/v1/settings/tools/claude_code/sanction': (spec) => {
      state = { ...state, tools: state.tools.map((t) => (t.tool_key === 'claude_code' ? { ...t, sanctioned_state: spec.body.sanctioned_state } : t)) };
      return { status: 204 };
    },
  });
  await controller.setToolSanction('claude_code', 'sanctioned');
  const put = requests.find((r) => r.method === 'PUT');
  assert.equal(put.path, '/admin/v1/settings/tools/claude_code/sanction');
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
  for (const label of ['Inventory', 'Processes', 'Network flows', 'OpenTelemetry', 'Hooks', 'Only managed hooks', 'Claude Code', 'Codex', 'Copilot', 'Cursor', 'Ollama', 'Local model capture']) {
    assert.match(card, new RegExp(`>${label}<`), `${label} is shown`);
  }
  for (const c of ['inventory', 'processes', 'flows', 'otel', 'hooks']) {
    assert.equal(pressed(card, ` data-action="endpoint" data-collector="${c}"`), 'on', `${c} is on by default`);
  }
  assert.equal(pressed(card, ' data-action="endpoint" data-collector="hooks_managed_only"'), 'off');
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="claude_code" data-collector="hooks"'), 'on');
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="cursor" data-collector="hooks"'), 'on');
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="codex" data-collector="hooks"'), 'on');
  // Cursor has no OTel; Copilot has no hooks; only Ollama has local model capture, and it has
  // nothing else: eight cells, none of them a control.
  assert.equal((card.match(/<span class="v-absent">unavailable<\/span>/g) ?? []).length, 8);
  assert.doesNotMatch(card, /data-tool="cursor" data-collector="otel"/);
  assert.doesNotMatch(card, /data-tool="copilot" data-collector="hooks"/);
  assert.doesNotMatch(card, /data-tool="claude_code" data-collector="loopback"/);
  assert.doesNotMatch(card, /data-tool="ollama" data-collector="(otel|hooks)"/);
});

test('endpoint collectors: Ollama has a Local model capture switch, off by default, that says it moves Ollama', async () => {
  let body = populated();
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body }),
    'PUT /admin/v1/settings/endpoint/tools/ollama': (spec) => {
      body = { ...body, endpoint: { ...body.endpoint, tools: { ...body.endpoint.tools, ollama: spec.body } } };
      return { status: 204 };
    },
  });
  const card = endpointCard(html());
  assert.match(card, /<span class="v-text">Ollama<\/span>/);
  assert.equal(pressed(card, ' data-action="endpoint-tool" data-tool="ollama" data-collector="loopback"'), 'off');
  assert.match(card, /the device moves Ollama to another port/);

  await controller.act({ action: 'endpoint-tool', tool: 'ollama', collector: 'loopback', value: 'on' });
  const puts = requests.filter((r) => r.method === 'PUT');
  assert.equal(puts.length, 1);
  assert.equal(puts[0].path, '/admin/v1/settings/endpoint/tools/ollama');
  assert.deepEqual(puts[0].body, { loopback: true });
  assert.equal(requests.at(-1).method, 'GET', 'the page re-reads after the write');
  assert.equal(pressed(endpointCard(html()), ' data-action="endpoint-tool" data-tool="ollama" data-collector="loopback"'), 'on');
  // Ollama has no native collectors, so neither is ever sent for it.
  await controller.act({ action: 'endpoint-tool', tool: 'ollama', collector: 'otel', value: 'on' });
  await controller.act({ action: 'endpoint-tool', tool: 'claude_code', collector: 'loopback', value: 'on' });
  assert.equal(requests.filter((r) => r.method === 'PUT').length, 1);
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
// Kill switch
// ---------------------------------------------------------------------------------------------

/** The kill switch card of a rendered page. */
function killCard(page) {
  const start = page.indexOf('<h3>Kill switch</h3>');
  assert.ok(start >= 0, 'the page has a kill switch card');
  return page.slice(start, page.indexOf('</section>', start));
}

/** A fake admin API whose kill switches change as control-api's would. */
async function withKillSwitches(extra = {}) {
  let switches = [];
  const loadedPage = await loaded({
    [GET]: () => ({ status: 200, body: populated({ kill_switches: switches }) }),
    'PUT /admin/v1/settings/kill-switch/proxy.tls': (spec) => {
      switches = spec.body.on
        ? [{ route: 'proxy.tls', reason_code: spec.body.reason_code, effective_at: '2026-10-05T10:00:00Z', set_by: 'admin@contoso.example' }]
        : [];
      return { status: 204 };
    },
    ...extra,
  });
  return loadedPage;
}

test('kill switch: a control per interception route, off by default, with a reason code field', async () => {
  const { html } = await withKillSwitches();
  const card = killCard(html());
  for (const [route, label] of [['proxy.tls', 'TLS proxy'], ['proxy.loopback', 'Local model broker']]) {
    assert.match(card, new RegExp(`>${label}<`));
    assert.match(card, new RegExp(`data-kill-switch-reason="${route.replace('.', '\\.')}"`));
    assert.match(card, new RegExp(`data-action="kill-switch" data-route="${route.replace('.', '\\.')}" data-value="on"`));
  }
  assert.doesNotMatch(card, /data-value="off"/, 'nothing to clear while no switch is tripped');
  assert.match(card, /stops decryption and enforcement/);
  assert.match(card, /reason code is required/i);
});

test('kill switch: tripping needs a reason code, sends it, and the card shows the switch tripped; clearing sends off', async () => {
  const { controller, requests, html } = await withKillSwitches();
  const puts = () => requests.filter((r) => r.method === 'PUT');

  // No reason, or a malformed one, sends nothing and says why.
  await controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'on' });
  controller.setKillSwitchReason('proxy.tls', 'App breakage!');
  await controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'on' });
  assert.equal(puts().length, 0, 'no write without a valid reason code');
  assert.match(killCard(html()), /Not changed\./);
  assert.match(killCard(html()), /invalid_reason_code/);

  controller.setKillSwitchReason('proxy.tls', ' app_breakage ');
  await controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'on' });
  assert.equal(puts().length, 1);
  assert.equal(puts()[0].path, '/admin/v1/settings/kill-switch/proxy.tls');
  assert.deepEqual(puts()[0].body, { on: true, reason_code: 'app_breakage' });
  assert.equal(requests.at(-1).method, 'GET', 'the page re-reads after the write');
  const card = killCard(html());
  assert.match(card, /tripped/);
  assert.match(card, /<code>app_breakage<\/code> by admin@contoso\.example/);
  assert.match(card, /data-action="kill-switch" data-route="proxy\.tls" data-value="off"/);
  assert.doesNotMatch(card, /Not changed\./);

  // Clearing sends off without a reason; a switch that is not tripped is not cleared.
  await controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'off' });
  assert.deepEqual(puts().at(-1).body, { on: false });
  assert.doesNotMatch(killCard(html()), /tripped/);
  await controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'off' });
  await controller.act({ action: 'kill-switch', route: 'cli.shim', value: 'on' });
  assert.equal(puts().length, 2);
});

test('kill switch: a refused write is said in the card, and switches the server did not send are not reported', async () => {
  const { controller, html } = await withKillSwitches({
    'PUT /admin/v1/settings/kill-switch/proxy.loopback': () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'reason_code is required to trip a kill switch' } } }),
  });
  controller.setKillSwitchReason('proxy.loopback', 'local_model_breakage');
  await controller.act({ action: 'kill-switch', route: 'proxy.loopback', value: 'on' });
  assert.match(killCard(html()), /reason_code is required to trip a kill switch/);

  const { kill_switches: _drop, ...without } = populated();
  const missing = await loaded({ [GET]: () => ({ status: 200, body: without }) });
  assert.match(killCard(missing.html()), /not reported/);
  assert.doesNotMatch(killCard(missing.html()), /data-action="kill-switch"/);
  missing.controller.setKillSwitchReason('proxy.tls', 'app_breakage');
  await missing.controller.act({ action: 'kill-switch', route: 'proxy.tls', value: 'on' });
  assert.equal(missing.requests.filter((r) => r.method === 'PUT').length, 0, 'nothing is written over switches that were not read');
  assert.equal(normaliseSettings({}).kill_switches, null, 'kill switches the server did not send are null, never none tripped');
});

test('kill switch: typing a reason and pressing Trip on the page sends the switch', async () => {
  let switches = [];
  const { admin, requests } = fakeAdmin({
    [GET]: () => ({ status: 200, body: populated({ kill_switches: switches }) }),
    'PUT /admin/v1/settings/kill-switch/proxy.tls': (spec) => {
      switches = [{ route: 'proxy.tls', reason_code: spec.body.reason_code, effective_at: '2026-10-05T10:00:00Z', set_by: 'admin@contoso.example' }];
      return { status: 204 };
    },
  });
  const doc = fakeDocument('#settings');
  const listeners = [];
  const add = doc.addEventListener;
  doc.addEventListener = (type, fn) => { listeners.push({ type, fn }); add(type, fn); };
  const app = await boot({ document: doc, api: queryApi(), admin, session: session(['admin'], ['posture', 'tools', 'teams', 'audit', 'settings', 'deployment']) });
  const fire = (type, event) => Promise.all(listeners.filter((l) => l.type === type).map((l) => l.fn(event)));
  await fire('input', { target: { dataset: { killSwitchReason: 'proxy.tls' }, value: 'app_breakage' } });
  const button = { tagName: 'BUTTON', disabled: false, dataset: { action: 'kill-switch', route: 'proxy.tls', value: 'on' } };
  await fire('click', { target: { closest: (sel) => (sel === '[data-action]' ? button : null) }, preventDefault() {} });
  // The click handler does not return the write; wait for the controller to finish it.
  for (let i = 0; i < 100 && (app.settings().state.killSwitch.pending || !requests.some((r) => r.method === 'PUT')); i++) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  const put = requests.find((r) => r.method === 'PUT');
  assert.ok(put, 'the click sent a write');
  assert.equal(put.path, '/admin/v1/settings/kill-switch/proxy.tls');
  assert.deepEqual(put.body, { on: true, reason_code: 'app_breakage' });
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

test('a viewer\'s Settings is Devices alone; Collection is refused in the page and asks nothing', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#posture');
  await boot({ document: doc, api: queryApi(), admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'rules', 'sanction', 'devices']) });
  assert.match(doc.html('nav'), /<a href="#devices"><svg[^>]*>.*?<\/svg><span>Settings<\/span><\/a>/);
  await doc.navigate('#settings');
  assert.match(doc.html('app'), /This page is not offered to you here/);
  await doc.navigate('#sanction');
  assert.doesNotMatch(doc.html('app'), /Sanction decisions/, 'a viewer reads the decisions and changes none');
  assert.equal(requests.length, 0, 'the admin API was not asked');
});

test('an admin opens Settings on Collection over the admin API, and decides sanction on Policy → Tools', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#settings');
  await boot({ document: doc, api: queryApi(), admin, session: session(['admin'], ['posture', 'tools', 'teams', 'rules', 'labels', 'sanction', 'settings', 'devices', 'directory', 'audit']) });
  assert.match(doc.html('nav'), /<a href="#settings" class="active" aria-current="page"><svg[^>]*>.*?<\/svg><span>Settings<\/span><\/a>/);
  const page = doc.html('app');
  assert.match(page, /<h2>Settings<\/h2>/);
  assert.match(page, /<nav class="page-tabs" aria-label="Settings"><a class="page-tab" href="#settings" aria-current="page">Collection<\/a><a class="page-tab" href="#devices">Devices<\/a><a class="page-tab" href="#directory">Identity Providers<\/a><a class="page-tab" href="#audit">Audit trail<\/a><\/nav>/);
  assert.match(page, /Collection mode/);
  assert.equal(requests[0].path, '/admin/v1/settings');

  await doc.navigate('#sanction');
  const policy = doc.html('app');
  assert.match(policy, /<h2>Policy<\/h2>/);
  assert.match(policy, /<h3>Sanction decisions<\/h3>/);
  assert.ok(policy.indexOf('Sanction decisions') < policy.indexOf('Tools by decision'), 'the decisions sit above the tools in use');
});

test('an absent count is null after normalising, never 0', () => {
  const data = normaliseSettings({ tools: [], scope_overrides: {}, retention_defaults: {} });
  assert.equal(data.event_retention_days, null);
  assert.equal(data.content_retention_days, null);
  assert.equal(data.collection_mode, null);
  assert.equal(data.content_search, 'disabled');
  assert.deepEqual(data.scope_overrides, {});
  assert.equal(data.endpoint, null, 'endpoint settings the server did not send are null, never all off');
  assert.equal(data.tls_inspection, null, 'a TLS inspection setting the server did not send is null, never off');
  assert.equal(data.data_classes, null, 'data classes the server did not send are null, never none');
  assert.equal(data.app_categories, null, 'app categories the server did not send are null, never none');
  assert.equal(data.custom_labels, null, 'custom labels the server did not send are null, never none');
  const partial = normaliseSettings({ endpoint: { inventory: false, tools: { cursor: { hooks: true } } } });
  assert.equal(partial.endpoint.inventory, false);
  assert.equal(partial.endpoint.flows, null);
  assert.equal(partial.endpoint.tools.cursor.otel, null);
});
