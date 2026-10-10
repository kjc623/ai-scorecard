/**
 * test/self-update.test.mjs — the extension asks the browser for its update when the agent it
 * talks to is a newer release, and installs a downloaded update by reloading after the queue is
 * handed over.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createHarness, settle } from '../test-support/harness.mjs';
import { versionNewer } from '../src/self-update.js';
import { TYPE } from '../src/messages.js';

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

test('an agent newer than the extension makes it ask the browser for an update, once per release', async () => {
  const h = createHarness({ version: '1.0.27', core: { agentVersion: '1.0.28' } });
  h.fake.state.updateCheckStatus = 'update_available';
  await h.app.start();
  await settle();
  assert.equal(h.fake.state.updateChecks, 1, 'the policy sync named a newer agent');
  assert.equal(h.app.selfUpdate.lastStatus, 'update_available');

  await h.app.requestPolicySync();
  await settle();
  assert.equal(h.fake.state.updateChecks, 1, 'the same release is not asked for again, so the browser does not throttle');
});

test('an agent at the same, an older or a development version asks for nothing', async () => {
  for (const agentVersion of ['1.0.27', '1.0.20', null]) {
    const h = createHarness({ version: '1.0.27', core: { agentVersion } });
    await h.app.start();
    await settle();
    assert.equal(h.fake.state.updateChecks, 0, `agent ${agentVersion}`);
  }
});

test('a downloaded update hands the queue to capture-core and then reloads the extension', async () => {
  const h = createHarness({ version: '1.0.27', core: { agentVersion: '1.0.28' } });
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
});

test('a reload goes ahead when capture-core cannot be reached', async () => {
  const h = createHarness({ version: '1.0.27', failConnect: true });
  await h.app.start();
  await settle();
  h.fake.state.updateAvailableHandlers[0]({ version: '1.0.28' });
  await settle();
  assert.equal(h.fake.state.reloads, 1);
});
