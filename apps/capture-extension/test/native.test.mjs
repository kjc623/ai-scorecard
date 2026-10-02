/**
 * test/native.test.mjs — the channel to capture-core (§3.4) against the Lead's protocol.
 *
 * Four properties:
 *   - a failed connect is `native_unavailable` and never a silent no-op;
 *   - a request/response pair is correlated by `id`, and a timeout is `native_timeout`;
 *   - the 1 MiB ceiling is enforced on the *framed* message before posting;
 *   - a refusal is typed and its reason comes from native.go's closed set.
 *
 * The fake core here is `test-support/fake-core.mjs`, which mirrors
 * `ObservationMessage.Validate()` — so a drifting shape fails a test rather than a deployment.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createNativeClient, NATIVE_APP, policySyncRequest, modeQuery } from '../src/native.js';
import { CORE_TYPE, REFUSAL, TYPE, frame, framedByteLength, unframe, validateHealthReport, validateObservation } from '../src/messages.js';
import { createHarness, createFakeChrome, settle } from '../test-support/harness.mjs';
import { createChromeAdapter } from '../src/chrome-adapter.js';
import { fakeCrypto } from '../test-support/harness.mjs';

function clientOn(fake, core, opts = {}) {
  const scope = { chrome: { ...fake.chrome, crypto: fake.crypto }, crypto: fake.crypto, performance: globalThis.performance };
  const adapter = createChromeAdapter(scope);
  const fakeCore = core;
  const orig = fake.chrome.runtime.connectNative.bind(fake.chrome.runtime);
  fake.chrome.runtime.connectNative = (application) => {
    const port = orig(application);
    const realPost = port.postMessage.bind(port);
    port.postMessage = (m) => {
      realPost(m);
      const answer = fakeCore.handle(m);
      if (answer) queueMicrotask(() => port.__answer(answer));
    };
    return port;
  };
  const events = [];
  const client = createNativeClient({ adapter, onEvent: (e) => events.push(e), ...opts });
  return { client, adapter, events, fake };
}

test('a failed connect throws native_unavailable and does not pretend the channel exists', () => {
  const fake = createFakeChrome();
  fake.state.failConnect = true;
  const { client, events } = clientOn(fake, { handle: () => null });
  assert.throws(() => client.sendOneWay(TYPE.HEALTH, { a: 1 }), /native_unavailable|native host/i);
  assert.equal(client.isConnected(), false);
  assert.ok(events.some((e) => e.kind === 'connect_failed'));
});

test('the client reaches the fake core and gets an ack for a health report', async () => {
  const h = createHarness();
  const answer = await h.app.native.sendRequest(TYPE.HEALTH, h.app.reportHealth());
  assert.equal(answer.type, CORE_TYPE.HEALTH_SNAPSHOT);
  assert.ok(answer.id, 'the answer carries the correlation id it was asked with');
  const sent = h.core.received.find((m) => m.type === TYPE.HEALTH);
  assert.ok(sent, 'the frame reached the core');
  assert.equal(sent.version, 1, 'protocol.Version is carried per message');
  assert.equal(validateHealthReport(sent.body), null, 'the health body validates against the protocol shape');
});

test('a disconnect surfaces as native_unavailable and fails every in-flight request', async () => {
  const h = createHarness();
  h.app.native.connect();
  const port = h.fake.state.nativePorts[0];
  const pending = h.app.native.sendRequest(TYPE.MODE_QUERY, modeQuery({ tool_fingerprint: 'tf1:x', host: 'a.invalid' }));
  await settle(1);
  port.__die('Native host has exited.');
  await assert.rejects(pending, (e) => e.code === 'native_unavailable');
});

test('a timeout is native_timeout, not a hang', async () => {
  const h = createHarness();
  // Silence the fake core's answers for this port so the request stays pending.
  const recorded = [];
  const silent = h.fake.chrome.runtime.connectNative(NATIVE_APP);
  silent.onMessage.addListener((m) => recorded.push(m));
  const client = createNativeClient({ adapter: h.adapter, timeoutMs: 20 });
  await assert.rejects(client.sendRequest(TYPE.MODE_QUERY, modeQuery({ tool_fingerprint: 'tf1:x', host: 'a.invalid' })), (e) => e.code === 'native_timeout');
});

test('a refusal is typed: the reason comes from the closed set and is preserved', async () => {
  const h = createHarness({ core: { modeForbidsRead: true } });
  await assert.rejects(
    h.app.native.sendRequest(TYPE.ATTACHMENT_MANIFEST, {
      transfer_id: 't1',
      observation_id: 'o1',
      descriptor: { name: 'a.pdf', size_bytes: 10 },
    }),
    (e) => e.code === 'core_refused' && e.detail.reason === REFUSAL.MODE_FORBIDS_READ,
  );
});

test('the 1 MiB ceiling is enforced on the framed message before it is posted', () => {
  const fake = createFakeChrome();
  const { client } = clientOn(fake, { handle: () => ({ type: CORE_TYPE.ACK }) });
  const huge = 'x'.repeat(2 << 20);
  assert.throws(() => client.sendOneWay(TYPE.OBSERVATION, { content: huge }), (e) => e.code === 'core_refused' && /ceiling/.test(e.message));
  assert.equal(fake.state.posted.length, 0, 'nothing was posted, so Chromium never had to reject it');
});

test('framedByteLength measures the frame, not the body', () => {
  const body = { content: 'abc' };
  const naive = JSON.stringify(body).length;
  assert.ok(framedByteLength(TYPE.OBSERVATION, body) > naive, 'the envelope and the escaping both count');
});

test('the drain sends oldest-first and removes an entry only after the ack', async () => {
  const h = createHarness();
  h.app.native.connect();
  h.app.queue.enqueue(TYPE.OBSERVATION, { client_id: 'a', tool_fingerprint: 'tf1:a', route: 'ext.web_request', occurred_at: new Date().toISOString(), monotonic_offset_ms: 1, size_bytes: 1, has_content: true, content: 'hi' }, 1);
  h.app.queue.enqueue(TYPE.OBSERVATION, { client_id: 'b', tool_fingerprint: 'tf1:b', route: 'ext.web_request', occurred_at: new Date().toISOString(), monotonic_offset_ms: 2, size_bytes: 1, has_content: true, content: 'hi' }, 1);
  const result = await h.app.pipeline.drainQueue(10);
  assert.equal(result.sent, 2);
  assert.equal(h.app.queue.size(), 0, 'both entries were acked and removed');
  assert.deepEqual(
    h.core.observations().map((o) => o.client_id),
    ['a', 'b'],
    'oldest first',
  );
});

test('a transport failure during a drain leaves the queue intact so nothing is lost silently', async () => {
  const h = createHarness();
  h.app.native.connect();
  h.fake.state.failPostAfter = 1; // the next post fails
  h.app.queue.enqueue(TYPE.OBSERVATION, { client_id: 'a', tool_fingerprint: 'tf1:a', route: 'ext.web_request', occurred_at: new Date().toISOString(), monotonic_offset_ms: 1, size_bytes: 1, has_content: true, content: 'hi' }, 1);
  const result = await h.app.pipeline.drainQueue(10);
  assert.equal(result.sent, 0);
  assert.equal(h.app.queue.size(), 1, 'the entry is still there for the next drain');
  assert.ok(result.failures.length >= 1);
});

test('an unsolicited core message is reported, never silently consumed', () => {
  const fake = createFakeChrome();
  const { client, events } = clientOn(fake, { handle: () => null });
  client.connect();
  const port = fake.state.nativePorts[0];
  port.__answer({ type: CORE_TYPE.POLICY_BUNDLE, version: 1, body: { policy_version: 'v9' } });
  assert.ok(events.some((e) => e.kind === 'unsolicited' && e.type === CORE_TYPE.POLICY_BUNDLE));
});

test('a version mismatch is reported as such rather than parsed as a valid message', () => {
  const fake = createFakeChrome();
  const { client, events } = clientOn(fake, { handle: () => null });
  client.connect();
  const port = fake.state.nativePorts[0];
  port.__answer({ type: CORE_TYPE.ACK, version: 99, body: {} });
  assert.ok(events.some((e) => e.kind === 'version_mismatch' && e.version === 99));
});

test('unframe rejects anything that is not a NativeMessage', () => {
  assert.equal(unframe(null), null);
  assert.equal(unframe('a string'), null);
  assert.equal(unframe([1, 2]), null);
  assert.equal(unframe({ version: 1 }), null, 'a message with no type is not a message');
  const ok = unframe({ type: 'ack', version: 1, id: 'x', body: {} });
  assert.equal(ok.type, 'ack');
  assert.equal(ok.versionMismatch, false);
});

test('frame omits an absent id and always carries the version', () => {
  assert.deepEqual(frame(TYPE.HEALTH, { a: 1 }), { type: 'health', version: 1, body: { a: 1 } });
  assert.deepEqual(frame(TYPE.HEALTH, { a: 1 }, 'id-1'), { type: 'health', version: 1, id: 'id-1', body: { a: 1 } });
  assert.equal(frame(TYPE.HEALTH, { a: 1 }).id, undefined);
});

test('observation validation refuses the contradictions native.go refuses', () => {
  const good = {
    client_id: 'c1',
    route: 'ext.web_request',
    tool_fingerprint: 'tf1:x',
    occurred_at: '2026-10-02T00:00:00Z',
    monotonic_offset_ms: 1,
    size_bytes: 2,
    has_content: true,
    content: 'hi',
    decision: { rule_id: 'R1', action: 'logged', decided_locally: true },
  };
  assert.equal(validateObservation(good), null);

  assert.equal(validateObservation({ ...good, has_content: false }).reason, REFUSAL.MALFORMED, 'has_content false with bytes');
  assert.equal(validateObservation({ ...good, content: undefined }).reason, REFUSAL.MALFORMED, 'has_content true with no bytes');
  assert.equal(validateObservation({ ...good, tool_fingerprint: '' }).reason, REFUSAL.MALFORMED);
  assert.equal(validateObservation({ ...good, route: 'proxy.tls' }).reason, REFUSAL.MALFORMED, 'not an extension route');
  assert.equal(validateObservation({ ...good, decision: { rule_id: 'R', action: 'quarantine', decided_locally: true } }).reason, REFUSAL.MALFORMED);
  assert.equal(validateObservation({ ...good, attachments: [{ name: '', size_bytes: 1 }] }).reason, REFUSAL.MALFORMED);
  assert.equal(validateObservation({ ...good, degraded_reason: 'made_up' }).reason, REFUSAL.MALFORMED);
  assert.equal(validateObservation({ ...good, degraded_reason: 'budget_exhausted' }), null);
});

test('health-report validation refuses a counter outside the closed set', () => {
  const base = { device_id: 'd', collector: 'capture_extension', state: 'healthy', since: 'now', counters: { observed: 1 } };
  assert.equal(validateHealthReport(base), null);
  assert.equal(validateHealthReport({ ...base, counters: { invented_counter: 1 } }).reason, REFUSAL.MALFORMED);
  assert.equal(validateHealthReport({ ...base, collector: '' }).reason, REFUSAL.MALFORMED);
  assert.equal(validateHealthReport({ ...base, state: 'fine' }).reason, REFUSAL.MALFORMED);
});

test('the policy-sync and mode-query bodies match the protocol structs', () => {
  assert.deepEqual(policySyncRequest('v1'), { known_version: 'v1' });
  assert.deepEqual(policySyncRequest(), { known_version: '' });
  assert.deepEqual(modeQuery({ tool_fingerprint: 'tf1:x' }), { tool_fingerprint: 'tf1:x' });
  assert.deepEqual(modeQuery({ tool_fingerprint: 'tf1:x', host: 'a.invalid', media_type: 'application/json', size_bytes: 12 }), {
    tool_fingerprint: 'tf1:x',
    host: 'a.invalid',
    media_type: 'application/json',
    size_bytes: 12,
  });
});

test('a refusal whose reason is outside the closed set cannot be constructed', async () => {
  const { refusal } = await import('../src/messages.js');
  assert.throws(() => refusal('because_i_said_so', 'no'), /outside the closed set/);
  assert.deepEqual(refusal(REFUSAL.ATTACHMENT_TOO_LARGE, 'too big'), { reason: 'attachment_too_large', message: 'too big' });
});
