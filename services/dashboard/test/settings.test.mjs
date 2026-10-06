// settings.test.mjs — Settings → Settings, driven without a browser.
//
// The controller (src/settings.js) runs against a fake admin API built on the real createAdminApi,
// so the requests asserted are the ones a browser would send. The renderer
// (src/settings-render.js) is asserted for each state. The one confirmation the page has — a mode
// increase that starts capturing content — is asserted not to send until confirmed.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, normaliseSettings } from '../src/transport.js';
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
    ...overrides,
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
});
