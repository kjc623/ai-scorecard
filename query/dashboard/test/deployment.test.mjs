// deployment.test.mjs — Settings → Deployment, driven without a browser.
//
// The controller (src/deployment.js) runs against a fake admin API built on the real
// createAdminApi, so the requests asserted are the ones a browser would send. The renderer
// (src/deployment-render.js) is asserted for each state the page can be in: loading, refused,
// empty and populated. The navigation is asserted through boot() with a signed-in session, for
// what each role is offered.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, filenameFromDisposition, normaliseDeployment } from '../src/transport.js';
import { createDeployment, deploymentKeyState, verificationAvailable } from '../src/deployment.js';
import { renderDeployment } from '../src/deployment-render.js';
import { boot } from '../src/app.js';
import { fakeDocument } from './helpers.mjs';
import { fixtureTransport } from './fixtures.mjs';

const queryApi = () => createQueryApi({ transport: fixtureTransport() });

const NOW = new Date('2026-10-05T12:00:00Z');

function populated(overrides = {}) {
  return {
    connection: { provider: 'entra', status: 'active', entra_tenant_id: '3f9a1c52-6d0e-4b7a-9c11-5e2d8a7b4c90' },
    device_verification: 'none',
    deployment_keys: [
      { key_id: 'k-1', label: 'Laptops', created_at: '2026-09-02T09:14:00Z', created_by: 'it@corp.test', expires_at: null, revoked_at: null, enrolment_count: 312, last_used_at: '2026-10-01T08:51:00Z' },
      { key_id: 'k-2', label: 'Pilot', created_at: '2026-08-11T15:02:00Z', created_by: 'it@corp.test', expires_at: null, revoked_at: '2026-09-02T09:20:00Z', enrolment_count: 41, last_used_at: null },
      { key_id: 'k-3', label: 'Kiosks', created_at: '2026-09-20T11:30:00Z', created_by: 'desk@corp.test', expires_at: null, revoked_at: null, enrolment_count: 0, last_used_at: null },
    ],
    scim: { tokens: [{ token_id: 't-1', label: 'Entra provisioning', created_at: '2026-09-02T09:40:00Z', revoked_at: null }], base_url: 'https://dash.corp.test/scim/v2', users: 412, groups: 18, last_provisioned_at: '2026-10-01T09:00:00Z' },
    devices: { enrolled: 353, last_enrolled_at: '2026-10-01T08:51:00Z' },
    release: { version: '1.4.0', product_code: '{6F1C2B9E-3A4D-4E57-9B21-0C8D7E6F5A41}' },
    ...overrides,
  };
}

/**
 * A fake admin API behind the real client. `answers` maps "METHOD path" to a function returning
 * the transport's answer; every request is recorded.
 */
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

const GET = 'GET /admin/v1/deployment';

async function loaded(answers, options = {}) {
  const { admin, requests } = fakeAdmin(answers);
  const saved = [];
  const copied = [];
  const changes = [];
  const controller = createDeployment({
    admin,
    onChange: (s) => changes.push(s.status),
    save: (data, filename) => saved.push({ data, filename }),
    copy: options.copy ?? (async (text) => { copied.push(text); }),
  });
  await controller.load();
  const html = () => renderDeployment(controller.state, { now: NOW });
  return { controller, requests, saved, copied, changes, html };
}

// ---------------------------------------------------------------------------------------------
// The four states
// ---------------------------------------------------------------------------------------------

test('loading: the page shows its frame and placeholders, and says it is loading', async () => {
  let release;
  const { admin } = fakeAdmin({ [GET]: () => new Promise((r) => { release = r; }) });
  const controller = createDeployment({ admin });
  const pending = controller.load();
  assert.equal(controller.state.status, 'loading');
  const html = renderDeployment(controller.state, { now: NOW });
  assert.match(html, /<h2>Deployment<\/h2>/);
  assert.match(html, /aria-busy="true"/);
  assert.match(html, /dp-skel/);
  assert.match(html, /Loading the deployment settings/);
  release({ status: 200, body: populated() });
  await pending;
  assert.equal(controller.state.status, 'ready');
});

test('refused: a 403 says only an admin can open it; an unreachable API offers a retry', async () => {
  const forbidden = await loaded({ [GET]: () => ({ status: 403, body: { error: 'forbidden', message: 'admin only' } }) });
  assert.equal(forbidden.controller.state.status, 'refused');
  assert.match(forbidden.html(), /Only an admin can open the deployment settings/);
  assert.doesNotMatch(forbidden.html(), /data-dep="retry"/, 'retrying a role refusal changes nothing');

  const down = await loaded({ [GET]: () => { throw new Error('connection refused'); } });
  assert.match(down.html(), /The admin API could not be reached/);
  assert.match(down.html(), /data-dep="retry"/);
  assert.match(down.html(), /transport_unavailable/);

  const ended = await loaded({ [GET]: () => ({ status: 401, body: { error: 'session_ended' } }) });
  assert.match(ended.html(), /Your session has ended/);
  assert.match(ended.html(), /href="\/signin"/);
});

test('empty: nothing linked, no keys, no tokens, and "not reported" is never a zero', async () => {
  const { html } = await loaded({
    [GET]: () => ({ status: 200, body: { connection: null, device_verification: 'none', deployment_keys: [], scim: { tokens: [], base_url: null }, devices: {}, release: null } }),
  });
  const page = html();
  assert.match(page, /No identity provider is linked yet/);
  assert.match(page, /No deployment keys yet\. Downloading a package creates one\./);
  assert.match(page, /No SCIM tokens/);
  assert.match(page, /not reported by the server/, 'a count the server did not send is said to be missing');
  assert.doesNotMatch(page, /<span class="v-number">0<\/span><\/span><span class="tile-note">not reported/, 'and is not drawn as 0');
  assert.match(page, /Intune verification needs an active Microsoft Entra ID connection/);
  assert.match(page, /\{product code\}/, 'the install steps say the product code is a placeholder');
});

test('populated: connection, counts with their denominators, keys, SCIM and the setup steps', async () => {
  const { html } = await loaded({ [GET]: () => ({ status: 200, body: populated() }) });
  const page = html();
  assert.match(page, /Microsoft Entra ID/);
  assert.match(page, /3f9a1c52-6d0e-4b7a-9c11-5e2d8a7b4c90/);
  assert.match(page, /v-vocab-active">active</);
  assert.match(page, />353</, 'devices enrolled');
  assert.match(page, />2 of 3</, 'active keys with their denominator');
  assert.match(page, /1 revoked/);
  assert.match(page, />Laptops</);
  assert.match(page, />312</);
  assert.match(page, /v-vocab-revoked">revoked</);
  assert.match(page, />0</, 'a key with no enrolments shows a real zero');
  assert.match(page, /Download Intune package \(\.intunewin\)/);
  assert.match(page, /Download package \(\.zip\)/);
  assert.match(page, /msiexec \/i ShadowAICapture\.msi \/qn/);
  assert.match(page, /msiexec \/x \{6F1C2B9E-3A4D-4E57-9B21-0C8D7E6F5A41\} \/qn/);
  assert.match(page, /Windows app \(Win32\)/);
  assert.match(page, /rule type <b>MSI<\/b>, product code <code>\{6F1C2B9E-3A4D-4E57-9B21-0C8D7E6F5A41\}<\/code>.*<code>1\.4\.0<\/code>/);
  assert.match(page, /https:\/\/dash\.corp\.test\/scim\/v2/);
  assert.match(page, /map <code>objectId<\/code> to <code>externalId<\/code>/);
  assert.match(page, />412</);
  assert.match(page, /1 of 1 active/, 'tokens with their denominator');
  assert.doesNotMatch(page, /\d%/, 'no fleet percentage');
});

test('every string from the API is escaped', async () => {
  const body = populated();
  body.deployment_keys[0].label = '<img src=x onerror=alert(1)>';
  body.connection = { provider: 'oidc', status: 'pending', issuer: 'https://idp.test/"><script>' };
  const { html } = await loaded({ [GET]: () => ({ status: 200, body }) });
  assert.doesNotMatch(html(), /<img src=x/);
  assert.doesNotMatch(html(), /"><script>/);
  assert.match(html(), /&lt;img src=x onerror=alert\(1\)&gt;/);
});

// ---------------------------------------------------------------------------------------------
// Device verification
// ---------------------------------------------------------------------------------------------

test('Intune verification is offered only with an active Entra connection, and says why when it is not', () => {
  assert.equal(verificationAvailable(populated()), true);
  assert.equal(verificationAvailable(populated({ connection: { provider: 'entra', status: 'pending' } })), false);
  assert.equal(verificationAvailable(populated({ connection: { provider: 'oidc', status: 'active', issuer: 'https://okta.test' } })), false);
  assert.equal(verificationAvailable(populated({ connection: null })), false);
});

test('turning Intune on sends one PUT and re-reads the page; it is disabled for an OIDC tenant', async () => {
  let verification = 'none';
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ device_verification: verification }) }),
    'PUT /admin/v1/deployment/verification': (spec) => { verification = spec.body.device_verification; return { status: 204 }; },
  });
  await controller.act({ dep: 'verification', value: 'intune' });
  const put = requests.find((r) => r.method === 'PUT');
  assert.deepEqual(put.body, { device_verification: 'intune' });
  assert.equal(controller.state.data.device_verification, 'intune', 're-read after the change');
  assert.match(html(), /data-value="intune" aria-pressed="true"/);
  assert.match(html(), /only if your Intune manages it with a matching serial number/);

  const oidc = await loaded({ [GET]: () => ({ status: 200, body: populated({ connection: { provider: 'oidc', status: 'active', issuer: 'https://okta.test' } }) }) });
  assert.match(oidc.html(), /data-value="intune" aria-pressed="false" disabled/);
  await oidc.controller.act({ dep: 'verification', value: 'intune' });
  assert.equal(oidc.requests.filter((r) => r.method === 'PUT').length, 0, 'a disabled choice sends nothing');
});

test('a refused verification change is said beside the control, and nothing is changed', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'PUT /admin/v1/deployment/verification': () => ({ status: 409, body: { error: 'no_active_entra_connection', message: 'Intune needs an active Entra connection.' } }),
  });
  await controller.setVerification('intune');
  assert.match(html(), /Not changed\./);
  assert.match(html(), /no_active_entra_connection/);
  assert.equal(controller.state.data.device_verification, 'none');
});

// ---------------------------------------------------------------------------------------------
// The package download
// ---------------------------------------------------------------------------------------------

test('a download posts the format and label, saves the file, and names the key the server minted', async () => {
  const body = populated();
  const { controller, requests, saved, html } = await loaded({
    [GET]: () => ({ status: 200, body }),
    'POST /admin/v1/deployment/package': () => ({ status: 200, file: { data: 'bytes', filename: 'ShadowAICapture-corp.intunewin', keyId: 'k-9', keyLabel: 'Floor 3' } }),
  });
  controller.setDraft('packageLabel', 'Floor 3');
  await controller.act({ dep: 'download', format: 'intunewin' });
  assert.deepEqual(requests.find((r) => r.method === 'POST').body, { format: 'intunewin', label: 'Floor 3' });
  assert.equal(requests.find((r) => r.method === 'POST').expect, 'file');
  assert.deepEqual(saved, [{ data: 'bytes', filename: 'ShadowAICapture-corp.intunewin' }]);
  assert.match(html(), /Saved <strong>ShadowAICapture-corp\.intunewin<\/strong>\. It carries the deployment key <strong>“Floor 3”<\/strong>\./);
  assert.equal(controller.state.drafts.packageLabel, '', 'the label field is cleared for the next one');
});

test('without the server naming it, the minted key is the one key that was not there before', async () => {
  let keys = populated().deployment_keys;
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ deployment_keys: keys }) }),
    'POST /admin/v1/deployment/package': () => {
      keys = [{ key_id: 'k-new', label: 'zip-2026-10-05', created_at: '2026-10-05T11:59:00Z', created_by: 'it@corp.test', expires_at: null, revoked_at: null, enrolment_count: 0, last_used_at: null }, ...keys];
      return { status: 200, file: { data: 'b', filename: null, keyId: null, keyLabel: null } };
    },
  });
  await controller.download('zip');
  assert.equal(controller.state.download.last.filename, 'ShadowAICapture.zip', 'the fallback name when the server sends none');
  assert.equal(controller.state.download.last.keyLabel, 'zip-2026-10-05');
  assert.match(html(), /“zip-2026-10-05”/);
  assert.match(html(), />3 of 4</, 'the new key is counted');
});

test('a refused download saves nothing and says why', async () => {
  const { controller, saved, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'POST /admin/v1/deployment/package': () => ({ status: 503, body: { error: 'no_release', message: 'No agent release is installed on the server.' } }),
  });
  await controller.download('intunewin');
  assert.equal(saved.length, 0);
  assert.match(html(), /No package was saved\./);
  assert.match(html(), /No agent release is installed on the server\./);
});

test('the file name from Content-Disposition is reduced to a plain name', () => {
  assert.equal(filenameFromDisposition('attachment; filename="ShadowAICapture-x.intunewin"'), 'ShadowAICapture-x.intunewin');
  assert.equal(filenameFromDisposition("attachment; filename*=UTF-8''Shadow%20AI.zip"), 'Shadow AI.zip');
  assert.equal(filenameFromDisposition('attachment; filename="..\\..\\Windows\\evil.exe"'), 'evil.exe');
  assert.equal(filenameFromDisposition('attachment'), null);
  assert.equal(filenameFromDisposition(null), null);
});

// ---------------------------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------------------------

test('revoking a key asks first, then posts to that key and re-reads', async () => {
  let keys = populated().deployment_keys;
  const { controller, requests, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated({ deployment_keys: keys }) }),
    'POST /admin/v1/deployment/keys/k-1/revoke': () => {
      keys = keys.map((k) => (k.key_id === 'k-1' ? { ...k, revoked_at: '2026-10-05T12:00:00Z' } : k));
      return { status: 204 };
    },
  });
  await controller.act({ dep: 'revoke-key', id: 'k-1' });
  assert.equal(requests.filter((r) => r.method === 'POST').length, 0, 'the first click only asks');
  assert.match(html(), /Stop new enrolments with this key\?/);
  await controller.act({ dep: 'confirm-revoke-key', id: 'k-1' });
  assert.equal(requests.filter((r) => r.method === 'POST').at(-1).path, '/admin/v1/deployment/keys/k-1/revoke');
  assert.match(html(), />1 of 3</);
});

test('a key is active, expired or revoked, and the three are not merged', () => {
  assert.equal(deploymentKeyState({ revoked_at: null, expires_at: null }, NOW), 'active');
  assert.equal(deploymentKeyState({ revoked_at: null, expires_at: '2026-10-01T00:00:00Z' }, NOW), 'expired');
  assert.equal(deploymentKeyState({ revoked_at: '2026-09-01T00:00:00Z', expires_at: '2026-10-01T00:00:00Z' }, NOW), 'revoked');
});

// ---------------------------------------------------------------------------------------------
// The SCIM token, shown once
// ---------------------------------------------------------------------------------------------

test('a new SCIM token is shown once, with a warning and a copy button, and never again', async () => {
  const SECRET = 'sacscim_5a3c0de0-7e57-4a11-9000-0000000d3a01_s3cr3tTail';
  const { controller, requests, copied, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'POST /admin/v1/scim/tokens': (spec) => ({ status: 201, body: { token_id: 't-9', token: SECRET, base_url: 'https://dash.corp.test/scim/v2' } }),
  });
  await controller.act({ dep: 'create-token' });
  assert.equal(requests.filter((r) => r.method === 'POST').length, 0, 'a token needs a label');
  assert.match(html(), /Give the token a label/);

  controller.setDraft('scimLabel', 'Entra provisioning');
  await controller.act({ dep: 'create-token' });
  assert.deepEqual(requests.find((r) => r.method === 'POST').body, { label: 'Entra provisioning' });
  const shown = html();
  assert.equal(shown.split(SECRET).length - 1, 1, 'the token appears exactly once on the page');
  assert.match(shown, /Copy this token now\. It will not be shown again\./);
  assert.match(shown, /data-dep="copy" data-what="token"/);
  assert.equal(requests.filter((r) => r.method === 'GET').length, 2, 'the page was re-read after the create');
  assert.ok(!JSON.stringify(controller.state.data).includes(SECRET), 'the re-read carries no token');

  await controller.act({ dep: 'copy', what: 'token' });
  assert.deepEqual(copied, [SECRET]);
  assert.match(html(), /Copied/);

  await controller.act({ dep: 'dismiss-token' });
  assert.doesNotMatch(html(), new RegExp(SECRET), 'dismissed: gone');
  await controller.load();
  assert.doesNotMatch(html(), new RegExp(SECRET), 'and no read brings it back');
});

test('leaving the page takes the one-time token with it, and a refused copy says to select the text', async () => {
  const { controller, html } = await loaded({
    [GET]: () => ({ status: 200, body: populated() }),
    'POST /admin/v1/scim/tokens': () => ({ status: 201, body: { token_id: 't-9', token: 'sacscim_once', base_url: null } }),
  }, { copy: async () => { throw new Error('denied'); } });
  controller.setDraft('scimLabel', 'x');
  await controller.createScimToken();
  await controller.copyValue('token');
  assert.match(html(), /did not allow copying: select the text and copy it/);
  controller.leave();
  assert.doesNotMatch(html(), /sacscim_once/);
});

test('an absent count is null after normalising, never 0', () => {
  const data = normaliseDeployment({ scim: {}, devices: {} });
  assert.equal(data.scim.users, null);
  assert.equal(data.devices.enrolled, null);
  assert.equal(data.connection, null);
  assert.equal(data.release, null);
  assert.equal(data.device_verification, 'none');
});

// ---------------------------------------------------------------------------------------------
// Navigation by role, through boot()
// ---------------------------------------------------------------------------------------------

function session(roles, pages) {
  return { actor: `${roles[0]}@lab.test`, tenant: '5a3c0de0-7e57-4a11-9000-0000000d3a01', roles, role: roles[0], pages };
}

test('a viewer is shown no Search, no Users, no Audit trail and no Settings; reaching Deployment directly is refused in the page', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#posture');
  await boot({ document: doc, api: queryApi(), admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'devices']) });
  const nav = doc.html('nav');
  assert.doesNotMatch(nav, />Search</);
  assert.doesNotMatch(nav, />Users</);
  assert.doesNotMatch(nav, />Audit trail</);
  assert.doesNotMatch(nav, />Deployment</);
  assert.doesNotMatch(nav, />Settings</);
  assert.match(nav, />Devices</);
  assert.match(doc.html('who'), /viewer@lab\.test/);
  assert.match(doc.html('who'), /action="\/signout"/);
  await doc.navigate('#deployment');
  assert.match(doc.html('app'), /Your role cannot use this page/);
  assert.equal(requests.length, 0, 'the admin API was not asked');
});

test('an admin is shown Settings → Deployment and the page renders over the admin API', async () => {
  const { admin, requests } = fakeAdmin({ [GET]: () => ({ status: 200, body: populated() }) });
  const doc = fakeDocument('#deployment');
  await boot({ document: doc, api: queryApi(), admin, session: session(['admin'], ['posture', 'tools', 'teams', 'audit', 'deployment']) });
  const nav = doc.html('nav');
  assert.match(nav, />Settings</);
  assert.match(nav, />Deployment</);
  assert.doesNotMatch(nav, />Search</, 'an admin does not search prompt text');
  assert.match(doc.html('app'), /Deployment keys/);
  assert.match(doc.html('app'), /Laptops/);
  assert.equal(requests[0].path, '/admin/v1/deployment');
});
