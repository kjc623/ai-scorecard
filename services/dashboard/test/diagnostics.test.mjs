// diagnostics.test.mjs — a device's diagnostics on its page, driven without a browser.
//
// The controller (src/diagnostics.js) runs against a fake admin API built on the real
// createAdminApi, so the requests asserted are the ones a browser would send, and on a fake clock,
// so a request waiting for its device is read again when the test says. The renderer
// (src/diagnostics-render.js) is asserted for each state; boot() shows who is offered the section.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdminApi, createQueryApi, normaliseDiagnostics } from '../src/transport.js';
import { WATCH_EVERY, createDiagnostics } from '../src/diagnostics.js';
import { renderDeviceDiagnostics } from '../src/diagnostics-render.js';
import { boot } from '../src/app.js';
import { fakeDocument } from './helpers.mjs';
import { fixtureTransport } from './fixtures.mjs';

const DEVICE = '9f1c0b6e-0000-4000-8000-000000000002';
const BASE = `/admin/v1/devices/${DEVICE}/diagnostics`;

const waiting = { request_id: 'r-2', state: 'waiting', requested_at: '2026-10-14T12:00:00Z', requested_by: 'it@corp.test', received_at: null, agent_version: null, size_bytes: null, expires_at: '2026-10-21T12:00:00Z' };
const received = { request_id: 'r-1', state: 'received', requested_at: '2026-10-13T09:00:00Z', requested_by: 'it@corp.test', received_at: '2026-10-13T09:12:00Z', agent_version: '1.0.42', size_bytes: 482113, expires_at: '2026-10-27T09:12:00Z' };

/** A clock that runs only when told: `fire()` runs the next timer and waits for what it started. */
function fakeTimers() {
  const due = new Map();
  let next = 0;
  return {
    setTimeout(fn, ms) { next += 1; due.set(next, { fn, ms }); return next; },
    clearTimeout(id) { due.delete(id); },
    get due() { return [...due.values()].map((t) => t.ms); },
    async fire() {
      const [id, { fn }] = due.entries().next().value;
      due.delete(id);
      await fn();
    },
  };
}

function fakeAdmin(answers) {
  const requests = [];
  const transport = {
    async request(spec) {
      requests.push(spec);
      const answer = answers[`${spec.method} ${spec.path}`];
      if (!answer) return { status: 404, body: { error: { code: 'not_found', message: `no answer for ${spec.method} ${spec.path}` } } };
      return answer(spec);
    },
  };
  return { admin: createAdminApi({ transport }), requests };
}

test('a device with no requests offers Collect logs and says nothing was requested', async () => {
  const { admin, requests } = fakeAdmin({ [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: [] } }) });
  const c = createDiagnostics({ admin });
  await c.load(DEVICE);
  const html = renderDeviceDiagnostics(c.state);
  assert.match(html, /<h3>Diagnostics<\/h3>/);
  assert.match(html, /data-diag="request"(?![^>]*disabled)/);
  assert.match(html, /No diagnostics have been requested from this device/);
  assert.match(html, /recorded in the audit trail/);
  assert.equal(requests[0].path, BASE);
});

test('requesting makes one POST, then the request waits in its own row and no second one is offered', async () => {
  let list = [];
  const { admin, requests } = fakeAdmin({
    [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: list } }),
    [`POST ${BASE}`]: () => { list = [waiting]; return { status: 201, body: waiting }; },
  });
  const seen = [];
  const c = createDiagnostics({ admin, timers: fakeTimers(), onChange: (s) => seen.push(renderDeviceDiagnostics(s)) });
  await c.load(DEVICE);
  seen.length = 0;
  await c.act({ diag: 'request' });
  assert.deepEqual(requests.map((r) => `${r.method} ${r.path}`), [`GET ${BASE}`, `POST ${BASE}`, `GET ${BASE}`]);
  assert.ok(seen.every((html) => /Waiting for the device to send its log bundle/.test(html) || /Asking the device…/.test(html)),
    'the button is busy until the request is listed, so it never comes back before the row');
  const html = renderDeviceDiagnostics(c.state);
  assert.match(html, /Waiting for the device to send its log bundle<\/span> <span class="dp-sub">Due at its next check-in · lapses <span title="2026-10-21 12:00Z">Oct 21 12:00<\/span>/);
  assert.match(html, /data-diag="request"[^>]*disabled/, 'a request already waiting is not asked for again');
  assert.doesNotMatch(html, /Requested\.|dp-done|Asking the device/, 'the row says the request waits; no message stays behind');
  // On a quiet device the row says what it is waiting on.
  assert.match(renderDeviceDiagnostics(c.state, { quietSince: '2026-10-12T08:00:00Z' }),
    /The device has not checked in since <span title="2026-10-12 08:00Z">Oct 12 08:00<\/span> · lapses/);
});

test('a waiting request is read again until its bundle arrives, repaints only when it changed, and stops when the page is left', async () => {
  let list = [waiting];
  const { admin, requests } = fakeAdmin({ [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: list } }) });
  const timers = fakeTimers();
  let changes = 0;
  const c = createDiagnostics({ admin, timers, onChange: () => { changes += 1; } });
  await c.load(DEVICE, { quiet: true });
  assert.deepEqual(timers.due, [WATCH_EVERY]);

  const before = changes;
  await timers.fire();
  assert.equal(requests.length, 2);
  assert.equal(changes, before, 'an answer that did not change is not repainted');
  assert.deepEqual(timers.due, [WATCH_EVERY], 'still waiting, so read again later');

  list = [{ ...waiting, state: 'received', received_at: '2026-10-14T12:20:00Z', agent_version: '1.0.77', size_bytes: 482113 }];
  await timers.fire();
  assert.equal(changes, before + 1);
  assert.match(renderDeviceDiagnostics(c.state), /Received<\/span> <span class="dp-sub"><span title="2026-10-14 12:20Z">Oct 14 12:20<\/span> · agent 1\.0\.77<\/span>/);
  assert.match(renderDeviceDiagnostics(c.state), /data-diag="download" data-id="r-2"/);
  assert.deepEqual(timers.due, [], 'nothing waits, so nothing is read again');

  list = [waiting];
  await c.load(DEVICE);
  assert.deepEqual(timers.due, [WATCH_EVERY]);
  c.leave();
  assert.deepEqual(timers.due, [], 'a page left is not read again');

  // An outage while watching keeps the row and says the page was not refreshed.
  const flaky = fakeAdmin({ [`GET ${BASE}`]: () => (list === null ? { status: 503, body: { error: { code: 'unavailable', message: 'try again' } } } : { status: 200, body: { diagnostics: list } }) });
  const w = createDiagnostics({ admin: flaky.admin, timers });
  await w.load(DEVICE);
  list = null;
  await timers.fire();
  const html = renderDeviceDiagnostics(w.state);
  assert.match(html, /Waiting for the device to send its log bundle/);
  assert.match(html, /Not refreshed\./);
});

test('a received bundle downloads under the name the server gives, from its own address', async () => {
  const saved = [];
  const { admin, requests } = fakeAdmin({
    [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: [received] } }),
    [`GET ${BASE}/r-1/bundle`]: () => ({ status: 200, file: { data: 'PK', filename: 'diagnostics-LAPTOP-7-20261013-0912.zip' } }),
  });
  const c = createDiagnostics({ admin, save: (data, filename) => saved.push({ data, filename }) });
  await c.load(DEVICE);
  let html = renderDeviceDiagnostics(c.state);
  assert.match(html, /Received<\/span> <span class="dp-sub"><span title="2026-10-13 09:12Z">Oct 13 09:12<\/span> · agent 1\.0\.42<\/span>/);
  assert.match(html, /470\.8 KB|471 KB/);
  assert.match(html, /data-diag="download" data-id="r-1"/);
  await c.act({ diag: 'download', id: 'r-1' });
  assert.deepEqual(saved, [{ data: 'PK', filename: 'diagnostics-LAPTOP-7-20261013-0912.zip' }]);
  assert.equal(requests.at(-1).expect, 'file');
  html = renderDeviceDiagnostics(c.state);
  assert.match(html, /data-diag="download" data-id="r-1"(?![^>]*disabled)/, 'saved, and ready to download again');
});

test('refusals: a revoked device says why, an analyst is told only an admin can, an outage offers a retry', async () => {
  const revoked = fakeAdmin({
    [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: [] } }),
    [`POST ${BASE}`]: () => ({ status: 409, body: { error: { code: 'revoked_device', message: 'the device is revoked and will not check in again' } } }),
  });
  const c = createDiagnostics({ admin: revoked.admin });
  await c.load(DEVICE);
  await c.request();
  assert.match(renderDeviceDiagnostics(c.state), /Not requested\.<\/strong> the device is revoked/);

  const forbidden = createDiagnostics({ admin: fakeAdmin({ [`GET ${BASE}`]: () => ({ status: 403, body: { error: { code: 'forbidden' } } }) }).admin });
  await forbidden.load(DEVICE);
  assert.match(renderDeviceDiagnostics(forbidden.state), /Only an admin can request/);
  assert.doesNotMatch(renderDeviceDiagnostics(forbidden.state), /data-diag="retry"/);

  const down = createDiagnostics({ admin: fakeAdmin({ [`GET ${BASE}`]: () => { throw new Error('refused'); } }).admin });
  await down.load(DEVICE);
  assert.match(renderDeviceDiagnostics(down.state), /data-diag="retry"/);
});

test('API strings are escaped and malformed rows are dropped', () => {
  const rows = normaliseDiagnostics([{ ...received, requested_by: '<script>x</script>' }, { state: 'received' }, null]);
  assert.equal(rows.length, 1);
  const html = renderDeviceDiagnostics({ device: DEVICE, status: 'ready', data: rows, problem: null, refreshProblem: null,
    request: { pending: false, problem: null }, download: { pending: null, problem: null } });
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
});

function session(roles, pages) {
  return { actor: `${roles[0]}@lab.test`, tenant: '5a3c0de0-7e57-4a11-9000-0000000d3a01', roles, role: roles[0], pages };
}

test('an admin sees Diagnostics on a device\'s page; a viewer does not, and the admin API is not asked', async () => {
  const forAdmin = fakeAdmin({
    'GET /admin/v1/deployment': () => ({ status: 200, body: {} }),
    [`GET ${BASE}`]: () => ({ status: 200, body: { diagnostics: [received] } }),
  });
  const doc = fakeDocument(`#devices?device=${DEVICE}`);
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin: forAdmin.admin, session: session(['admin'], ['posture', 'tools', 'teams', 'rules', 'labels', 'sanction', 'settings', 'devices', 'directory', 'audit']) });
  const page = doc.html('app');
  assert.match(page, /<h3>Collectors<\/h3>/);
  assert.match(page, /<h3>Diagnostics<\/h3>/);
  assert.ok(page.indexOf('<h3>Diagnostics</h3>') > page.indexOf('<h3>Collectors</h3>'), 'below the collectors');
  assert.match(page, /data-diag="download" data-id="r-1"/);

  const forViewer = fakeAdmin({});
  const viewerDoc = fakeDocument(`#devices?device=${DEVICE}`);
  await boot({ document: viewerDoc, api: createQueryApi({ transport: fixtureTransport() }), admin: forViewer.admin, session: session(['viewer'], ['posture', 'tools', 'teams', 'rules', 'sanction', 'devices']) });
  assert.doesNotMatch(viewerDoc.html('app'), /Diagnostics/);
  assert.equal(forViewer.requests.length, 0);
});
