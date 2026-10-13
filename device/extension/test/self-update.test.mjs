/**
 * test/self-update.test.mjs — the extension reads its own update manifest and asks the browser for
 * a newer version as soon as it is listed, and installs a downloaded update by reloading after the
 * queue is handed over.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { HEALTH_ALARM, UPDATE_CHECK_ALARM } from '../background/service-worker.js';
import { createHarness, settle } from '../test-support/harness.mjs';
import { listedVersion, versionNewer } from '../src/self-update.js';
import { TYPE } from '../src/messages.js';

const UPDATE_URL = 'https://console.example.invalid/v1/extension/updates.xml';

/** An update manifest as control-api writes it. */
function gupdate(version, id = 'fake-extension-id') {
  return `<?xml version="1.0" encoding="UTF-8"?>\n<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0"><app appid="${id}"><updatecheck codebase="https://console.example.invalid/v1/extension/shadow-ai-capture.crx" version="${version}" hash_sha256="00" size="1"></updatecheck></app></gupdate>`;
}

/** A policy-installed extension at `version` whose update server lists `listed`. */
function installed({ version = '1.0.27', listed = version, agentVersion = null, status = 'update_available', ...opts } = {}) {
  const h = createHarness({ version, core: { agentVersion }, ...opts });
  h.fake.state.updateUrl = UPDATE_URL;
  h.fake.state.updateManifest = gupdate(listed);
  h.fake.state.updateCheckStatus = status;
  return h;
}

function fireAlarm(h, name) {
  for (const fn of h.fake.state.alarmHandlers) fn({ name });
}

test('versionNewer compares dotted release versions and ignores anything else', () => {
  assert.equal(versionNewer('1.0.28', '1.0.27'), true);
  assert.equal(versionNewer('1.0.100', '1.0.99'), true, 'numeric, not lexical');
  assert.equal(versionNewer('1.1', '1.0.9'), true);
  assert.equal(versionNewer('1.0.27', '1.0.27'), false);
  assert.equal(versionNewer('1.0.26', '1.0.27'), false, 'never toward an older version');
  assert.equal(versionNewer('dev', '1.0.27'), false, 'a development agent names no release');
  assert.equal(versionNewer('1.0.28', 'dev'), false);
  assert.equal(versionNewer(undefined, '1.0.27'), false);
});

test('listedVersion reads the version an update manifest lists for this extension', () => {
  assert.equal(listedVersion(gupdate('1.0.66'), 'fake-extension-id'), '1.0.66');
  assert.equal(listedVersion(gupdate('1.0.66', 'another-extension'), 'fake-extension-id'), null, 'another extension\'s entry');
  const two = `<gupdate><app appid='other'><updatecheck version='9.9'/></app><app appid='fake-extension-id'><updatecheck status='noupdate'/></app></gupdate>`;
  assert.equal(listedVersion(two, 'fake-extension-id'), null, 'an entry with no version');
  assert.equal(listedVersion('<html>bad gateway</html>', 'fake-extension-id'), null);
});

test('a newer version on the update server is asked for at once, with no agent involved', async () => {
  const h = installed({ listed: '1.0.28' });
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.fetches[0].url, UPDATE_URL);
  assert.equal(h.fake.state.fetches[0].init.credentials, 'omit', 'the read carries no credentials');
  assert.equal(h.fake.state.updateChecks, 1);
  assert.equal(h.app.selfUpdate.lastStatus, 'update_available');
});

test('the update manifest is read on the alarm, and the browser is asked only when it lists a newer version', async () => {
  const h = installed();
  await h.app.start();
  await settle();
  assert.ok(h.fake.state.alarms.some((a) => a.name === UPDATE_CHECK_ALARM), 'the read is periodic');
  const reads = h.fake.state.fetches.length;
  fireAlarm(h, UPDATE_CHECK_ALARM);
  await settle();
  assert.equal(h.fake.state.fetches.length, reads + 1);
  assert.equal(h.fake.state.updateChecks, 0, 'nothing newer: the browser is not asked, so it never throttles');

  h.fake.state.updateManifest = gupdate('1.0.28');
  fireAlarm(h, UPDATE_CHECK_ALARM);
  await settle();
  assert.equal(h.fake.state.updateChecks, 1, 'a deploy is asked for at the next read');
});

test('until the browser has the update, each read asks again', async () => {
  const h = installed({ listed: '1.0.28', status: 'no_update' });
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.updateChecks, 1);
  fireAlarm(h, UPDATE_CHECK_ALARM);
  await settle();
  assert.equal(h.fake.state.updateChecks, 2, 'one failed check does not leave the extension waiting on the browser');
});

test('an unreachable update server asks the browser nothing', async () => {
  const h = installed({ listed: '1.0.28' });
  h.fake.state.failFetch = true;
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.updateChecks, 0);
  assert.equal(h.app.selfUpdate.lastStatus, 'unreachable');
});

test('a build loaded unpacked has no update_url and reads nothing', async () => {
  const h = createHarness({ version: '1.0.27', core: { agentVersion: '1.0.28' } });
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.fetches.length, 0);
  assert.equal(h.fake.state.updateChecks, 0);
});

test('an agent newer than the extension starts a read at once; the manifest decides', async () => {
  const h = installed({ agentVersion: '1.0.27' });
  await h.app.start();
  await settle();
  const reads = h.fake.state.fetches.length;

  await h.app.selfUpdate.consider('1.0.28');
  assert.equal(h.fake.state.fetches.length, reads + 1, 'the newer agent started a read');
  assert.equal(h.fake.state.updateChecks, 0, 'the manifest lists nothing newer yet, so the browser is not asked');

  await h.app.selfUpdate.consider('1.0.27');
  await h.app.selfUpdate.consider(null);
  assert.equal(h.fake.state.fetches.length, reads + 1, 'an agent at the same or no release starts nothing');
});

test('when capture-core restarts as a newer release, the update is asked for as the channel comes back', async () => {
  const h = installed({ agentVersion: '1.0.27', connectCooldownMs: 0 });
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.updateChecks, 0);

  h.fake.state.updateManifest = gupdate('1.0.28');
  h.core.setAgentVersion('1.0.28');
  h.fake.state.nativePorts.at(-1).__die();
  await settle();
  // An idle browser has nothing queued, so the health report is the only message on the new port.
  fireAlarm(h, HEALTH_ALARM);
  await settle();
  assert.equal(h.fake.state.updateChecks, 1, 'asked on reconnect, not at the next alarm');
  assert.equal(h.app.selfUpdate.lastStatus, 'update_available');
});

test('a downloaded update hands the queue to capture-core and then reloads the extension', async () => {
  const h = installed({ listed: '1.0.28' });
  await h.app.start();
  await settle();
  h.app.queue.enqueue(TYPE.OBSERVATION, { held: true });
  h.core.received.length = 0;

  assert.equal(h.fake.state.updateAvailableHandlers.length, 1, 'the worker listens for a downloaded update');
  h.fake.state.updateAvailableHandlers[0]({ version: '1.0.28' });
  await settle();

  assert.equal(h.fake.state.reloads, 1, 'the update is installed now, not when the worker next stops');
  assert.equal(h.app.queue.stats().depth, 0, 'nothing held in memory is lost to the reload');
  assert.ok(h.core.received.some((m) => m.type === TYPE.OBSERVATION), 'the queued observation reached capture-core first');

  const checks = h.fake.state.updateChecks;
  fireAlarm(h, UPDATE_CHECK_ALARM);
  await settle();
  assert.equal(h.fake.state.updateChecks, checks, 'a reload under way is not asked for again');
});

test('a reload goes ahead when capture-core cannot be reached', async () => {
  const h = createHarness({ version: '1.0.27', failConnect: true });
  await h.app.start();
  await settle();
  h.fake.state.updateAvailableHandlers[0]({ version: '1.0.28' });
  await settle();
  assert.equal(h.fake.state.reloads, 1);
});
