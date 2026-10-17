// integrations.test.mjs — Settings → Integrations: the controller over a fake admin transport, the
// requests it makes for each action, what the page says for each provider's state, and where the
// page sits in the navigation.

import test from 'node:test';
import assert from 'node:assert/strict';
import { DELIVERY_ERRORS, INTAKE_SETUP, PULL_ERRORS, createIntegrations } from '../src/integrations.js';
import { renderIntegrations } from '../src/integrations-render.js';
import { createAdminApi, createQueryApi } from '../src/transport.js';
import { escapeHtml } from '../src/render.js';
import { boot } from '../src/app.js';
import { pagesForRoles } from '../server/session.mjs';
import { fixtureTransport } from './fixtures.mjs';
import { fakeDocument } from './helpers.mjs';

const KEY = 'sk-compliance-7f3a9c0e1b2d4f6a';
const CONNECTION = '0b3c2f5e-1d4a-4c8e-9a6b-7e2f1c0d0001';
const PROVIDERS = '/admin/v1/providers';
const CHOICES = Object.freeze([15, 30, 60, 360, 1440]);

const CHATGPT = Object.freeze({
  provider: 'chatgpt_enterprise', display_name: 'ChatGPT Enterprise', vendor: 'openai',
  credential: 'api_key', credential_label: 'Compliance API key', account_label: 'Workspace ID',
  source_url: 'https://cookbook.openai.com/examples/chatgpt/compliance_api/logs_platform',
  tools: [{ app: 'chatgpt_web', name: 'ChatGPT (web)' }], available: true, connection: null,
});
const CLAUDE = Object.freeze({
  provider: 'claude_enterprise', display_name: 'Claude Enterprise', vendor: 'anthropic',
  credential: 'api_key', credential_label: 'Compliance API key', account_label: 'Organization ID',
  source_url: 'https://platform.claude.com/docs/en/manage-claude/compliance-api',
  tools: [{ app: 'claude_web', name: 'Claude (web)' }, { app: 'claude_desktop', name: 'Claude Desktop' }, { app: 'claude_code', name: 'Claude Code' }],
  available: true, connection: null,
});
const COPILOT = Object.freeze({
  provider: 'microsoft_365_copilot', display_name: 'Microsoft 365 Copilot', vendor: 'microsoft',
  credential: 'entra_app', credential_label: '', account_label: '',
  source_url: 'https://learn.microsoft.com/en-us/microsoft-365/copilot/extensibility/api/ai-services/interaction-export/aiinteractionhistory-getallenterpriseinteractions',
  tools: [{ app: 'm365_copilot', name: 'Microsoft 365 Copilot' }], available: true, connection: null,
});

const BEDROCK = Object.freeze({
  provider: 'amazon_bedrock', display_name: 'Amazon Bedrock', vendor: 'amazon',
  credential: 'intake', credential_label: '', account_label: '', intake: 'firehose',
  source_url: 'https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html',
  tools: [{ app: 'aws_bedrock', name: 'Amazon Bedrock' }], available: true, connection: null,
});
const TOKEN = 'sacintake_Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXhx';
const ENDPOINT = `https://app.contoso.com/intake/v1/amazon_bedrock/${CONNECTION}`;

/** A connection as control-api returns it: active, its last pull finished and succeeded. */
function connection(patch = {}) {
  return {
    connection_id: CONNECTION, account: 'ws-4821', status: 'active',
    created_by: 'admin@contoso.com', created_at: '2026-10-01T10:00:00Z', updated_by: 'admin@contoso.com', updated_at: '2026-10-01T10:00:00Z',
    last_started_at: '2026-10-01T11:00:00Z', last_completed_at: '2026-10-01T11:02:00Z', last_status: 'ok', last_error: '',
    records_read: 1520, records_new: 1490, pull_every_minutes: 60, next_pull_at: '2026-10-01T12:00:00Z',
    ...patch,
  };
}

/** The admin API over a transport that answers from memory, as control-api would, and records every request. */
function fakeAdmin({ providers = [CHATGPT, CLAUDE, COPILOT], entra = true, answers = {} } = {}) {
  const requests = [];
  const held = providers.map((p) => ({ ...p }));
  const transport = {
    async request(spec) {
      requests.push(spec);
      const route = `${spec.method} ${spec.path}`;
      if (answers[route]) return answers[route](spec);
      if (route === `GET ${PROVIDERS}`) return { status: 200, body: { entra_connected: entra, pull_every_choices: CHOICES, providers: held } };
      const provider = held.find((p) => p.provider === decodeURIComponent(spec.path.split('/')[4] ?? ''));
      if (!provider) return { status: 404, body: { error: { code: 'not_found', message: 'no such provider' } } };
      if (spec.method === 'PUT' && spec.path.endsWith('/schedule')) {
        const every = spec.body.pull_every_minutes;
        const next = new Date(Date.parse(provider.connection.last_started_at) + every * 60_000).toISOString().replace('.000', '');
        provider.connection = { ...provider.connection, pull_every_minutes: every, next_pull_at: next, updated_at: '2026-10-01T11:30:00Z' };
        return { status: 204, body: null };
      }
      if (spec.method === 'PUT') {
        provider.connection = connection({ account: spec.body.account ?? '', last_started_at: null, last_completed_at: null, last_status: '', records_read: 0, records_new: 0, next_pull_at: null });
        return { status: 200, body: { connection_id: CONNECTION, queued: true } };
      }
      if (spec.method === 'DELETE') {
        provider.connection = { ...provider.connection, status: 'disabled', updated_at: '2026-10-02T09:00:00Z' };
        return { status: 204, body: null };
      }
      return { status: 202, body: { queued: true } };
    },
  };
  return { admin: createAdminApi({ transport }), requests };
}

async function loaded(options) {
  const { admin, requests } = fakeAdmin(options);
  const page = createIntegrations({ admin, copy: options?.copy });
  await page.load();
  return { page, requests, html: (now) => renderIntegrations(page.state, now ? { now } : {}) };
}

/** One provider's card, by its name. */
function card(html, name) {
  const start = html.indexOf(`<h3>${escapeHtml(name)}</h3>`);
  assert.ok(start >= 0, `${name} has a card`);
  const end = html.indexOf('</section>', start);
  return html.slice(start, end);
}

const writes = (requests) => requests.filter((r) => r.method !== 'GET').map((r) => [r.method, r.path, r.body]);

// ---------------------------------------------------------------------------------------------
// What the page shows
// ---------------------------------------------------------------------------------------------

test('with no provider listed the page says so and offers nothing to connect', async () => {
  const { html } = await loaded({ providers: [] });
  assert.match(html(), /<h2>Integrations<\/h2>/);
  assert.match(html(), /No provider is listed yet/);
  assert.doesNotMatch(html(), /data-int="(open|connect|run|disconnect)"/);
});

test('every provider the API lists has a card drawn from its row alone, a new one included', async () => {
  const GEMINI = {
    provider: 'gemini_enterprise', display_name: 'Gemini Enterprise', vendor: 'google', credential: 'api_key',
    credential_label: 'Service account key', account_label: '', source_url: 'https://cloud.google.com/gemini/docs/audit',
    tools: [{ app: 'gemini_web', name: 'Gemini (web)' }], available: true, connection: null,
  };
  const { page, html } = await loaded({ providers: [CHATGPT, CLAUDE, COPILOT, GEMINI] });
  for (const p of [CHATGPT, CLAUDE, COPILOT, GEMINI]) {
    const one = card(html(), p.display_name);
    assert.match(one, new RegExp(`<span class="table-note">${p.vendor}<span class="v-vocab v-vocab-unknown">not connected</span></span>`));
    for (const t of p.tools) assert.ok(one.includes(escapeHtml(t.name)), `${p.display_name} covers ${t.name}`);
    assert.ok(one.includes(`href="${escapeHtml(p.source_url)}" target="_blank" rel="noopener noreferrer"`), `${p.display_name} links its documentation`);
  }
  assert.match(card(html(), 'Claude Enterprise'), /Claude \(web\), Claude Desktop, Claude Code/);
  assert.match(card(html(), 'ChatGPT Enterprise'), />How to issue a key</);
  assert.match(card(html(), 'Microsoft 365 Copilot'), />How it is read</);

  await page.act({ int: 'open', provider: 'gemini_enterprise' });
  const form = card(html(), 'Gemini Enterprise');
  assert.match(form, /<label class="dp-label" for="int-key-gemini_enterprise">Service account key<\/label>/);
  assert.doesNotMatch(form, /name="account"/, 'a provider whose key names its account asks for no account');
});

test('a connected provider shows its account, its last pull and the records read, and offers Pull now, Replace key and Disconnect', async () => {
  const { html } = await loaded({ providers: [{ ...CHATGPT, connection: connection() }] });
  const one = card(html(), 'ChatGPT Enterprise');
  assert.match(one, /<span class="v-vocab v-vocab-active">connected<\/span>/);
  assert.match(one, /<dt>Workspace ID<\/dt><dd><code>ws-4821<\/code><\/dd>/);
  assert.match(one, /<dt>Connected<\/dt><dd><span class="v-time"[^>]*>2026-10-01 10:00Z<\/span> <span class="dp-sub">by admin@contoso\.com<\/span>/);
  assert.match(one, /<dt>Last pull<\/dt><dd><span class="v-time"[^>]*>2026-10-01 11:02Z<\/span> <span class="v-vocab v-vocab-ok">ok<\/span>/);
  assert.match(one, /<dt>Records read<\/dt><dd><span class="v-number">1,520<\/span> <span class="dp-sub">1,490 of them new to Sundial<\/span>/);
  assert.doesNotMatch(one, /Last changed/, 'a connection never changed since it was made says nothing more');
  assert.doesNotMatch(one, /The last pull failed/);
  assert.deepEqual([...one.matchAll(/<button[^>]*data-int="([^"]+)"[^>]*>([^<]*)</g)].map((m) => [m[1], m[2]]), [['run', 'Pull now'], ['open', 'Replace key'], ['disconnect', 'Disconnect']]);
});

test('a pulled connection shows how often it is pulled, as a choice of the schedules offered, and when the next pull is due', async () => {
  const { html } = await loaded({ providers: [{ ...CHATGPT, connection: connection() }, { ...COPILOT, connection: connection({ account: '', pull_every_minutes: 360, next_pull_at: '2026-10-01T17:00:00Z' }) }] });
  const at = new Date('2026-10-01T11:30:00Z');
  const chatgpt = card(html(at), 'ChatGPT Enterprise');
  const select = chatgpt.match(/<dt>Pulled<\/dt><dd>(<select[^>]*>)(.*?)<\/select><\/dd>/);
  assert.ok(select, 'the card offers its schedule');
  assert.equal(select[1], '<select class="dp-input dp-select" data-int="schedule" data-provider="chatgpt_enterprise" aria-label="How often Sundial pulls ChatGPT Enterprise">');
  assert.deepEqual([...select[2].matchAll(/<option value="(\d+)"( selected)?>([^<]*)<\/option>/g)].map((m) => [Number(m[1]), Boolean(m[2]), m[3]]), [
    [15, false, 'Every 15 minutes'], [30, false, 'Every 30 minutes'], [60, true, 'Every hour'], [360, false, 'Every 6 hours'], [1440, false, 'Every day'],
  ]);
  assert.match(chatgpt, /<dt>Next pull<\/dt><dd><span class="v-time"[^>]*>2026-10-01 12:00Z<\/span><\/dd>/);
  const copilot = card(html(at), 'Microsoft 365 Copilot');
  assert.match(copilot, /<option value="360" selected>Every 6 hours<\/option>/);
  assert.match(copilot, /<dt>Next pull<\/dt><dd><span class="v-time"[^>]*>2026-10-01 17:00Z<\/span><\/dd>/);
  // Past its time, a pull is due; one never run is due at once.
  assert.match(card(html(new Date('2026-10-01T12:01:00Z')), 'ChatGPT Enterprise'), /<dt>Next pull<\/dt><dd><span class="v-time"[^>]*>2026-10-01 12:00Z<\/span> <span class="dp-sub">due<\/span><\/dd>/);
  const fresh = await loaded({ providers: [{ ...CHATGPT, connection: connection({ last_started_at: null, last_completed_at: null, next_pull_at: null }) }] });
  assert.match(card(fresh.html(at), 'ChatGPT Enterprise'), /<dt>Next pull<\/dt><dd>due now<\/dd>/);
  // A schedule the server did not send, or one it does not offer, is not reported.
  const unsent = await loaded({ providers: [{ ...CHATGPT, connection: connection({ pull_every_minutes: undefined }) }] });
  assert.match(card(unsent.html(at), 'ChatGPT Enterprise'), /<dt>Pulled<\/dt><dd><span class="v-absent">not reported<\/span><\/dd>/);
  const odd = await loaded({ providers: [{ ...CHATGPT, connection: connection({ pull_every_minutes: 45 }) }] });
  assert.match(card(odd.html(at), 'ChatGPT Enterprise'), /<dt>Pulled<\/dt><dd><span class="v-absent">not reported<\/span><\/dd>/);
});

test('choosing a schedule sends it once, says what it does, and reads the page again; the same schedule or one not offered sends nothing', async () => {
  const { page, requests, html } = await loaded({ providers: [{ ...CHATGPT, connection: connection() }] });
  await page.act({ int: 'schedule', provider: 'chatgpt_enterprise', value: '60' });
  await page.act({ int: 'schedule', provider: 'chatgpt_enterprise', value: '45' });
  await page.act({ int: 'schedule', provider: 'chatgpt_enterprise', value: 'often' });
  assert.deepEqual(writes(requests), [], 'nothing changed, so nothing is sent');

  const pending = page.act({ int: 'schedule', provider: 'chatgpt_enterprise', value: '360' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /data-int="schedule"[^>]* disabled>.*<span class="dp-sub" role="status">Saving…<\/span>/);
  await pending;
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/chatgpt_enterprise/schedule`, { pull_every_minutes: 360 }]]);
  assert.equal(requests.at(-1).method, 'GET', 'the page is read again');
  const one = card(html(new Date('2026-10-01T11:30:00Z')), 'ChatGPT Enterprise');
  assert.match(one, /<p class="dp-done" role="status">Saved\. Sundial pulls ChatGPT Enterprise every 6 hours, counted from the last pull\.<\/p>/);
  assert.match(one, /<option value="360" selected>Every 6 hours<\/option>/);
  assert.match(one, /<dt>Next pull<\/dt><dd><span class="v-time"[^>]*>2026-10-01 17:00Z<\/span><\/dd>/);
});

test('a refused schedule is shown with its code, and an intake or a disconnected provider has no schedule', async () => {
  const { page, requests, html } = await loaded({
    providers: [{ ...CLAUDE, connection: connection() }, { ...BEDROCK, connection: connection({ account: '', intake_url: ENDPOINT }) }, { ...CHATGPT, connection: connection({ status: 'disabled' }) }],
    answers: { [`PUT ${PROVIDERS}/claude_enterprise/schedule`]: () => ({ status: 409, body: { error: { code: 'invalid_request', message: 'connect Claude Enterprise first' } } }) },
  });
  await page.act({ int: 'schedule', provider: 'claude_enterprise', value: '15' });
  const claude = card(html(), 'Claude Enterprise');
  assert.match(claude, /<p class="dp-problem" role="alert"><strong>Not done\.<\/strong> connect Claude Enterprise first <code>invalid_request<\/code><\/p>/);
  assert.match(claude, /<option value="60" selected>/, 'the schedule shown is still the one held');
  for (const name of ['Amazon Bedrock', 'ChatGPT Enterprise']) {
    assert.doesNotMatch(card(html(), name), /Pulled|Next pull|data-int="schedule"/, `${name} has no schedule`);
  }
  await page.act({ int: 'schedule', provider: 'amazon_bedrock', value: '15' });
  await page.act({ int: 'schedule', provider: 'chatgpt_enterprise', value: '15' });
  assert.equal(writes(requests).length, 1, 'only the pulled, connected provider was sent a schedule');
});

test('a pull still running says so, and a connection whose counts were not sent says "not reported", never 0', async () => {
  const { html } = await loaded({ providers: [{ ...CHATGPT, connection: connection({ last_started_at: '2026-10-01T12:00:00Z', records_read: undefined, records_new: undefined }) }] });
  const one = card(html(), 'ChatGPT Enterprise');
  assert.match(one, /2026-10-01 12:00Z<\/span> <span class="dp-sub">started, still running<\/span>/);
  assert.match(one, /<dt>Records read<\/dt><dd><span class="v-absent">not reported<\/span>/);
});

test('a failed pull names its cause in words for every code, with the code beside it', async () => {
  assert.deepEqual(Object.keys(PULL_ERRORS).sort(), ['consent_missing', 'credential_refused', 'entra_not_connected', 'internal', 'not_available', 'provider_unavailable', 'rate_limited']);
  assert.equal(new Set(Object.values(PULL_ERRORS)).size, 7, 'each code has words of its own');
  for (const [code, words] of Object.entries(PULL_ERRORS)) {
    const { html } = await loaded({ providers: [{ ...CHATGPT, connection: connection({ last_status: 'failed', last_error: code }) }] });
    const one = card(html(), 'ChatGPT Enterprise');
    assert.ok(one.includes(`<p class="dp-problem" role="alert"><strong>The last pull failed.</strong> ${escapeHtml(words)} <code>${code}</code></p>`), code);
    assert.match(one, /<span class="v-vocab v-vocab-failed">failed<\/span>/);
  }
  assert.match(PULL_ERRORS.consent_missing, /AiEnterpriseInteraction\.Read\.All/);
  assert.match(PULL_ERRORS.credential_refused, /Replace the key/);
  const { html } = await loaded({ providers: [{ ...CHATGPT, connection: connection({ last_status: 'failed', last_error: 'something_new' }) }] });
  assert.match(card(html(), 'ChatGPT Enterprise'), /The provider could not be read\. <code>something_new<\/code>/, 'a code the page does not know is still shown');
});

test('a provider read with the tenant\'s Entra consent is one button, which waits for an active Entra connection', async () => {
  const without = await loaded({ providers: [COPILOT], entra: false });
  const waiting = card(without.html(), 'Microsoft 365 Copilot');
  assert.match(waiting, /<button type="button" class="btn btn-primary" data-int="connect" data-provider="microsoft_365_copilot" disabled>Connect<\/button>/);
  assert.match(waiting, /needs an active Microsoft Entra ID connection for your organisation, and there is none/);
  assert.match(waiting, /<a href="#directory">Identity Providers<\/a>/);
  assert.doesNotMatch(waiting, /type="password"/);
  await without.page.act({ int: 'connect', provider: 'microsoft_365_copilot' });
  await without.page.act({ int: 'open', provider: 'microsoft_365_copilot' });
  assert.deepEqual(writes(without.requests), [], 'nothing is sent without the Entra connection');

  const { page, requests, html } = await loaded({ providers: [COPILOT] });
  const ready = card(html(), 'Microsoft 365 Copilot');
  assert.match(ready, /<button type="button" class="btn btn-primary" data-int="connect" data-provider="microsoft_365_copilot">Connect<\/button>/);
  assert.match(ready, /there is no key to paste/);
  await page.act({ int: 'connect', provider: 'microsoft_365_copilot' });
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/microsoft_365_copilot`, {}]], 'no key and no account');
  const connected = card(html(), 'Microsoft 365 Copilot');
  assert.match(connected, /Connected\. The first pull has started: it reads the last 7 days\./);
  assert.match(connected, /<dt>Last pull<\/dt><dd><span class="v-absent">not run yet<\/span>/);
  assert.match(connected, /<dt>Next pull<\/dt><dd>due now<\/dd>/);
  assert.deepEqual([...connected.matchAll(/data-int="([^"]+)"/g)].map((m) => m[1]), ['schedule', 'run', 'disconnect'], 'no key to replace');
});

test('a provider this deployment cannot read is listed as not available and offers nothing', async () => {
  const { page, requests, html } = await loaded({ providers: [{ ...CLAUDE, available: false }] });
  const one = card(html(), 'Claude Enterprise');
  assert.match(one, /<span class="v-vocab v-vocab-unknown">not available<\/span>/);
  assert.match(one, /Not available on this deployment\./);
  assert.doesNotMatch(one, /<button|How to issue a key/);
  await page.act({ int: 'open', provider: 'claude_enterprise' });
  await page.act({ int: 'connect', provider: 'claude_enterprise' });
  assert.deepEqual(writes(requests), []);
});

test('a role that is not an admin is told the page is an admin\'s', async () => {
  const { html } = await loaded({ answers: { [`GET ${PROVIDERS}`]: () => ({ status: 403, body: { error: { code: 'forbidden', message: 'the admin role is required' } } }) } });
  assert.match(html(), /Only an admin can manage integrations/);
  assert.doesNotMatch(html(), /data-int="retry"/);
});

// ---------------------------------------------------------------------------------------------
// What each action sends
// ---------------------------------------------------------------------------------------------

test('connecting sends the account and the key once, in the PUT body, and the key never reaches the page', async () => {
  const { page, requests, html } = await loaded();
  await page.act({ int: 'open', provider: 'chatgpt_enterprise' });
  const form = card(html(), 'ChatGPT Enterprise');
  assert.match(form, /<form class="dp-form" data-int-form="chatgpt_enterprise" autocomplete="off">/);
  assert.match(form, /<label class="dp-label" for="int-account-chatgpt_enterprise">Workspace ID<\/label><input class="dp-input dp-mono" id="int-account-chatgpt_enterprise" name="account"[^>]* value="">/);
  assert.match(form, /<label class="dp-label" for="int-key-chatgpt_enterprise">Compliance API key<\/label><input class="dp-input dp-mono" id="int-key-chatgpt_enterprise" name="key" type="password"/);
  assert.match(form, /<button type="submit" class="btn btn-primary">Connect<\/button>/);

  page.setDraft('account', 'ws-4821');
  page.setDraft('key', KEY);
  assert.ok(!html().includes(KEY), 'the key field is never given a value');
  await page.act({ int: 'connect', provider: 'chatgpt_enterprise' });

  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/chatgpt_enterprise`, { account: 'ws-4821', key: KEY }]]);
  const carrying = requests.filter((r) => JSON.stringify(r).includes(KEY));
  assert.equal(carrying.length, 1, 'the key is sent once');
  assert.ok(!carrying[0].path.includes(KEY), 'in the body, never the address');
  assert.equal(requests.at(-1).path, PROVIDERS, 'the page was read again after connecting');
  assert.ok(!JSON.stringify(page.state).includes(KEY), 'and is not held after it was sent');
  const connected = card(html(), 'ChatGPT Enterprise');
  assert.match(connected, /<span class="v-vocab v-vocab-active">connected<\/span>/);
  assert.match(connected, /Connected\. The first pull has started: it reads the last 7 days\./);
  assert.doesNotMatch(connected, /<form/, 'the form closes');
});

test('a form left incomplete is not sent, and a refusal is shown with its code while the key is forgotten', async () => {
  const { page, requests, html } = await loaded({
    answers: { [`PUT ${PROVIDERS}/chatgpt_enterprise`]: () => ({ status: 400, body: { error: { code: 'invalid_request', message: 'the compliance api key is required, without spaces' } } }) },
  });
  await page.act({ int: 'open', provider: 'chatgpt_enterprise' });
  await page.act({ int: 'connect', provider: 'chatgpt_enterprise' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /Paste the Compliance API key\./);
  page.setDraft('key', KEY);
  await page.act({ int: 'connect', provider: 'chatgpt_enterprise' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /Enter the Workspace ID\./);
  assert.equal(page.state.drafts.key, '', 'a repaint empties the key field, and the key with it');
  assert.deepEqual(writes(requests), [], 'nothing was sent');

  page.setDraft('account', 'ws-4821');
  page.setDraft('key', 'bad key');
  await page.act({ int: 'connect', provider: 'chatgpt_enterprise' });
  assert.equal(writes(requests).length, 1);
  const refused = card(html(), 'ChatGPT Enterprise');
  assert.match(refused, /the compliance api key is required, without spaces <code>invalid_request<\/code>/);
  assert.match(refused, /data-int-form="chatgpt_enterprise"/, 'the form stays open to try again');
  assert.match(refused, /name="account"[^>]* value="ws-4821"/, 'with the account kept');
  assert.equal(page.state.drafts.key, '');
});

test('Replace key opens the form with the account already in it, and says the old key is forgotten', async () => {
  const { page, requests, html } = await loaded({ providers: [{ ...CHATGPT, connection: connection() }] });
  await page.act({ int: 'open', provider: 'chatgpt_enterprise' });
  const form = card(html(), 'ChatGPT Enterprise');
  assert.match(form, /name="account"[^>]* value="ws-4821"/);
  assert.match(form, /The new key replaces the one Sundial holds, which is forgotten\./);
  assert.match(form, /<button type="submit" class="btn btn-primary">Replace key<\/button>/);
  page.setDraft('key', KEY);
  await page.act({ int: 'connect', provider: 'chatgpt_enterprise' });
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/chatgpt_enterprise`, { account: 'ws-4821', key: KEY }]]);
  assert.match(card(html(), 'ChatGPT Enterprise'), /Key replaced\. A pull has started with it\./);
});

test('Pull now sends POST …/run and says whether a pull started; Disconnect asks first, then sends DELETE', async () => {
  let queued = true;
  const { page, requests, html } = await loaded({
    providers: [{ ...CHATGPT, connection: connection() }],
    answers: { [`POST ${PROVIDERS}/chatgpt_enterprise/run`]: () => ({ status: 202, body: { queued } }) },
  });
  await page.act({ int: 'run', provider: 'chatgpt_enterprise' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /<p class="dp-note" role="status">A pull has started\.<\/p>/);
  queued = false;
  await page.act({ int: 'run', provider: 'chatgpt_enterprise' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /A pull is already running\./);

  await page.act({ int: 'disconnect', provider: 'chatgpt_enterprise' });
  assert.match(card(html(), 'ChatGPT Enterprise'), /Stop reading ChatGPT Enterprise and forget its key\? <button[^>]*data-int="confirm-disconnect" data-provider="chatgpt_enterprise"/);
  await page.act({ int: 'cancel' });
  assert.doesNotMatch(card(html(), 'ChatGPT Enterprise'), /Stop reading/);
  assert.ok(!writes(requests).some(([method]) => method === 'DELETE'), 'asking is not disconnecting');

  await page.act({ int: 'disconnect', provider: 'chatgpt_enterprise' });
  await page.act({ int: 'confirm-disconnect', provider: 'chatgpt_enterprise' });
  assert.deepEqual(writes(requests), [
    ['POST', `${PROVIDERS}/chatgpt_enterprise/run`, undefined],
    ['POST', `${PROVIDERS}/chatgpt_enterprise/run`, undefined],
    ['DELETE', `${PROVIDERS}/chatgpt_enterprise`, undefined],
  ], 'only the connection itself carries a body');
  const gone = card(html(), 'ChatGPT Enterprise');
  assert.match(gone, /Disconnected\. Sundial no longer reads ChatGPT Enterprise and has forgotten its key\./);
  assert.match(gone, /<span class="v-vocab v-vocab-unknown">disconnected<\/span>/);
  assert.match(gone, /<dt>Disconnected<\/dt><dd><span class="v-time"[^>]*>2026-10-02 09:00Z<\/span>/);
  assert.match(gone, /data-int="open" data-provider="chatgpt_enterprise"[^>]*>Connect</, 'it can be connected again');
});

test('the admin API keeps only what the page can show of the provider list', async () => {
  const seen = [];
  const admin = createAdminApi({
    transport: {
      async request(spec) {
        seen.push(`${spec.method} ${spec.path}`);
        return {
          status: 200,
          body: {
            entra_connected: 'yes',
            providers: [
              { ...CHATGPT, source_url: 'javascript:alert(1)', connection: { connection_id: CONNECTION, status: 'active', last_status: '', last_error: '' } },
              { ...CLAUDE, credential: 'oauth' },
              { display_name: 'No id' },
            ],
          },
        };
      },
    },
  });
  const read = await admin.providers();
  assert.deepEqual(seen, [`GET ${PROVIDERS}`]);
  assert.equal(read.data.entra_connected, false, 'only true is connected');
  assert.deepEqual(read.data.providers.map((p) => p.provider), ['chatgpt_enterprise', 'claude_enterprise'], 'a provider with no id is dropped');
  const [chatgpt, claude] = read.data.providers;
  assert.equal(chatgpt.source_url, null, 'a documentation link that is not https is not a link');
  assert.equal(chatgpt.connection.last_status, null, 'an empty status is no status');
  assert.equal(chatgpt.connection.records_read, null, 'a count not sent stays unknown');
  assert.equal(chatgpt.connection.pull_every_minutes, null, 'a schedule not sent stays unknown');
  assert.equal(chatgpt.connection.next_pull_at, null);
  assert.deepEqual(read.data.pull_every_choices, [], 'no schedule is offered that the server did not list');
  const offered = (await createAdminApi({ transport: { request: async () => ({ status: 200, body: { pull_every_choices: [15, '30', 60.5, -5, 60], providers: [] } }) } }).providers()).data;
  assert.deepEqual(offered.pull_every_choices, [15, 60], 'only whole minutes are a schedule');
  assert.equal(claude.available, false, 'a credential this page cannot ask for is not available here');
  const copilot = (await createAdminApi({ transport: { request: async () => ({ status: 200, body: { providers: [COPILOT] } }) } }).providers()).data.providers[0];
  assert.equal(copilot.credential_label, null, 'an empty label is no label');
});

// ---------------------------------------------------------------------------------------------
// A provider sent to the intake
// ---------------------------------------------------------------------------------------------

/** The intake's PUT, as control-api answers it: the connection, its endpoint and a new token. */
function issuing(tokens = [TOKEN]) {
  let n = 0;
  return {
    [`PUT ${PROVIDERS}/amazon_bedrock`]: () => ({ status: 200, body: { connection_id: CONNECTION, queued: false, intake_url: ENDPOINT, intake_token: tokens[n++] } }),
  };
}

test('a provider sent to the intake says what to set up, and connecting asks for nothing and shows the token once', async () => {
  const copied = [];
  const { page, requests, html } = await loaded({ providers: [BEDROCK], answers: issuing(), copy: async (text) => { copied.push(text); } });
  const before = card(html(), 'Amazon Bedrock');
  assert.ok(before.includes(escapeHtml(INTAKE_SETUP.firehose)), 'the setup for its transport');
  assert.match(before, />How to turn on its log</);
  assert.match(before, /<button type="button" class="btn btn-primary" data-int="connect" data-provider="amazon_bedrock">Connect<\/button>/);
  assert.doesNotMatch(before, /type="password"|data-int="run"/);

  await page.act({ int: 'connect', provider: 'amazon_bedrock' });
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/amazon_bedrock`, {}]], 'no key and no account');
  const issued = card(html(), 'Amazon Bedrock');
  assert.match(issued, /Connected\. Sundial now receives what is sent with the token below\./);
  assert.match(issued, /Copy this token now\. It will not be shown again\./);
  assert.ok(issued.includes(`<input class="dp-input dp-mono" id="int-token-amazon_bedrock" readonly value="${TOKEN}">`));
  assert.ok(issued.includes(`<input class="dp-input dp-mono" id="int-url-amazon_bedrock" readonly value="${escapeHtml(ENDPOINT)}">`));

  await page.act({ int: 'copy', what: 'token' });
  await page.act({ int: 'copy', what: 'url' });
  assert.deepEqual(copied, [TOKEN, ENDPOINT]);
  assert.match(card(html(), 'Amazon Bedrock'), /<span class="dp-copied" role="status">Copied<\/span>/);

  await page.act({ int: 'cancel' });
  assert.ok(!html().includes(TOKEN) && !JSON.stringify(page.state).includes(TOKEN), 'once dismissed the token is gone');
});

test('a connected intake shows its endpoint and last delivery, offers no pull, and replaces its token only when asked twice', async () => {
  const providers = [{ ...BEDROCK, connection: connection({ account: '', intake_url: ENDPOINT, last_started_at: '2026-10-01T11:02:00Z' }) }];
  const { page, requests, html } = await loaded({ providers, answers: issuing(['sacintake_second']) });
  const one = card(html(), 'Amazon Bedrock');
  assert.ok(one.includes(`<dt>Endpoint</dt><dd><code class="dp-url">${escapeHtml(ENDPOINT)}</code></dd>`));
  assert.match(one, /<dt>Last delivery<\/dt><dd><span class="v-time"[^>]*>2026-10-01 11:02Z<\/span> <span class="v-vocab v-vocab-ok">ok<\/span>/);
  assert.doesNotMatch(one, /Last pull|still running/);
  assert.deepEqual([...one.matchAll(/<button[^>]*data-int="([^"]+)"[^>]*>([^<]*)</g)].map((m) => [m[1], m[2]]), [['replace', 'Replace token'], ['disconnect', 'Disconnect']]);

  await page.act({ int: 'run', provider: 'amazon_bedrock' });
  await page.act({ int: 'connect', provider: 'amazon_bedrock' });
  assert.deepEqual(writes(requests), [], 'an intake is never pulled, and its token is not replaced without asking');

  await page.act({ int: 'replace', provider: 'amazon_bedrock' });
  assert.match(card(html(), 'Amazon Bedrock'), /Issue a new token\? The current one stops working at once, so what Amazon Bedrock sends is refused until its sender has the new one\./);
  await page.act({ int: 'confirm-replace', provider: 'amazon_bedrock' });
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/amazon_bedrock`, {}]]);
  const replaced = card(html(), 'Amazon Bedrock');
  assert.match(replaced, /Token replaced\. The previous token no longer works\./);
  assert.match(replaced, /readonly value="sacintake_second"/);

  page.leave();
  assert.ok(!JSON.stringify(page.state).includes('sacintake_second'), 'leaving the page forgets the token');

  await page.act({ int: 'disconnect', provider: 'amazon_bedrock' });
  assert.match(card(html(), 'Amazon Bedrock'), /Stop receiving Amazon Bedrock and forget its token\?/);
  await page.act({ int: 'confirm-disconnect', provider: 'amazon_bedrock' });
  assert.match(card(html(), 'Amazon Bedrock'), /Disconnected\. Sundial no longer receives Amazon Bedrock and has forgotten its token\./);
});

test('a failed delivery names its cause, and a browser that will not copy says so', async () => {
  assert.deepEqual(Object.keys(DELIVERY_ERRORS).sort(), ['delivery_malformed', 'internal']);
  for (const [code, words] of Object.entries(DELIVERY_ERRORS)) {
    const { html } = await loaded({ providers: [{ ...BEDROCK, connection: connection({ intake_url: ENDPOINT, last_status: 'failed', last_error: code }) }] });
    assert.ok(card(html(), 'Amazon Bedrock').includes(`<strong>The last delivery failed.</strong> ${escapeHtml(words)} <code>${code}</code>`), code);
  }
  const { page, html } = await loaded({ providers: [BEDROCK], answers: issuing(), copy: async () => { throw new Error('denied'); } });
  await page.act({ int: 'connect', provider: 'amazon_bedrock' });
  await page.act({ int: 'copy', what: 'token' });
  assert.match(card(html(), 'Amazon Bedrock'), /This browser did not allow copying: select the text and copy it\./);
});

test('the admin API keeps an intake\'s transport and endpoint only when they are what the page knows', async () => {
  const admin = createAdminApi({
    transport: {
      async request() {
        return {
          status: 200,
          body: {
            providers: [
              { ...BEDROCK, connection: { connection_id: CONNECTION, status: 'active', intake_url: ENDPOINT } },
              { ...BEDROCK, provider: 'other', intake: 'smtp', connection: { connection_id: CONNECTION, status: 'active', intake_url: 'javascript:alert(1)' } },
              { ...CHATGPT, intake: 'firehose' },
            ],
          },
        };
      },
    },
  });
  const [bedrock, other, chatgpt] = (await admin.providers()).data.providers;
  assert.equal(bedrock.intake, 'firehose');
  assert.equal(bedrock.available, true);
  assert.equal(bedrock.connection.intake_url, ENDPOINT);
  assert.equal(other.intake, null, 'a transport the page does not know has no setup');
  assert.equal(other.connection.intake_url, null, 'an endpoint that is not http(s) is not shown');
  assert.equal(chatgpt.intake, null, 'only an intake provider has a transport');
});

// ---------------------------------------------------------------------------------------------
// Where the page sits
// ---------------------------------------------------------------------------------------------

const ADMIN_PAGES = ['posture', 'tools', 'teams', 'rules', 'labels', 'sanction', 'settings', 'devices', 'directory', 'integrations', 'audit'];

function session(roles, pages) {
  return { actor: `${roles[0]}@lab.test`, tenant: '5a3c0de0-7e57-4a11-9000-0000000d3a01', roles, pages };
}

test('Integrations is an admin\'s page, and no other role is offered it', () => {
  assert.ok(pagesForRoles(['admin']).includes('integrations'));
  for (const role of ['viewer', 'analyst', 'content_reader']) assert.equal(pagesForRoles([role]).includes('integrations'), false, role);
});

test('an admin opens Integrations under Settings, and its forms and buttons act through the page\'s own listeners', async () => {
  const { admin, requests } = fakeAdmin();
  const doc = fakeDocument('#integrations');
  const listeners = [];
  const add = doc.addEventListener;
  doc.addEventListener = (type, fn) => { listeners.push({ type, fn }); add(type, fn); };
  const app = await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin, session: session(['admin'], ADMIN_PAGES) });
  const fire = (type, event) => Promise.all(listeners.filter((l) => l.type === type).map((l) => l.fn(event)));

  const page = doc.html('app');
  assert.match(page, /<h2>Settings<\/h2>/);
  const tabs = [...page.matchAll(/<a class="page-tab" href="#([^"]+)"( aria-current="page")?>([^<]*)<\/a>/g)].map((m) => [m[3], Boolean(m[2])]);
  assert.deepEqual(tabs, [['Collection', false], ['Devices', false], ['Identity Providers', false], ['Integrations', true], ['Audit trail', false]]);
  assert.match(page, /<h3>ChatGPT Enterprise<\/h3>/);
  assert.deepEqual(requests.map((r) => `${r.method} ${r.path}`), [`GET ${PROVIDERS}`]);

  const button = (dataset) => ({ closest: (selector) => (selector === '[data-int]' ? { dataset, disabled: false } : null) });
  await fire('click', { target: button({ int: 'open', provider: 'chatgpt_enterprise' }), preventDefault() {} });
  assert.match(doc.html('app'), /data-int-form="chatgpt_enterprise"/);
  await fire('input', { target: { dataset: { intDraft: 'account' }, value: 'ws-4821' } });
  await fire('input', { target: { dataset: { intDraft: 'key' }, value: KEY } });
  assert.ok(!doc.html('app').includes(KEY));

  // Leaving the page forgets a key being typed.
  await doc.navigate('#posture');
  assert.equal(app.integrations().state.drafts.key, '');
  await doc.navigate('#integrations');
  assert.equal(requests.length, 2, 'coming back reads the page again');
  await fire('click', { target: button({ int: 'open', provider: 'chatgpt_enterprise' }), preventDefault() {} });
  await fire('input', { target: { dataset: { intDraft: 'key' }, value: KEY } });
  await fire('input', { target: { dataset: { intDraft: 'account' }, value: 'ws-4821' } });
  await fire('submit', { target: { dataset: { intForm: 'chatgpt_enterprise' } }, preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(writes(requests), [['PUT', `${PROVIDERS}/chatgpt_enterprise`, { account: 'ws-4821', key: KEY }]]);
  assert.match(doc.html('app'), /Connected\. The first pull has started/);

  // The schedule is a select: a click on it does nothing, the change it fires chooses.
  const select = { tagName: 'SELECT', dataset: { int: 'schedule', provider: 'chatgpt_enterprise' }, disabled: false, value: '15' };
  await fire('click', { target: { closest: (selector) => (selector === '[data-int]' ? select : null) }, preventDefault() {} });
  assert.equal(writes(requests).length, 1, 'opening the select sends nothing');
  await fire('change', { target: { dataset: {}, closest: (selector) => (selector === 'select[data-int]' ? select : null) } });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(writes(requests).at(-1), ['PUT', `${PROVIDERS}/chatgpt_enterprise/schedule`, { pull_every_minutes: 15 }]);
});

test('a viewer is not offered Integrations, and the admin API is not asked', async () => {
  const { admin, requests } = fakeAdmin();
  const doc = fakeDocument('#integrations');
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'rules', 'sanction', 'devices']) });
  assert.match(doc.html('app'), /This page is not offered to you here/);
  assert.equal(requests.length, 0);
});
