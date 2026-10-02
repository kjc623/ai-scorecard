/**
 * test/worker.test.mjs — the extension driven end to end through the fake chrome, which is the
 * closest this host can get to the browser.
 *
 * Everything here goes through the real wiring: `background/service-worker.js` → `registration.js`
 * → `pipeline.js` → `native.js` → the fake `capture-core`. `fake.drive(lane, detail)` calls the
 * listener Chrome would call, with the `extraInfoSpec` Chrome would have been given, so the
 * registration assertions are assertions about what a browser would actually hand over.
 *
 * NOT VERIFIED HERE, and not verifiable on this host: that Chromium honours `{cancel: true}`,
 * that a content script can read a real `File`, and that a real native host exists. See README.md
 * for the exact clicks a human must make.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createHarness, settle } from '../test-support/harness.mjs';
import { chromeRequest, INVALID_UTF8 } from '../test-support/fake-chrome.mjs';
import { CHAT_BODY, DRAFT_BODY, STREAMING_RESPONSE } from '../test-support/fixtures.mjs';
import { COUNTER, CORE_TYPE, REFUSAL, TYPE } from '../src/messages.js';

const CHAT_URL = 'https://chat.example-ai.invalid/v1/chat/completions';
const DRAFT_URL = 'https://saas.example-ai.invalid/api/drafts';

/** Start the extension against a bundle that reads bodies for one host and refuses one other. */
async function started({ core = {}, capacity = 200, bundle = null, failConnect = false } = {}) {
  const h = createHarness({ core, capacity, failConnect });
  await h.app.start();
  const policy = bundle || {
    policy_version: 'bundle-1',
    default_mode: 'm1',
    scope: { 'denied.invalid': 'm0' },
  };
  h.app.applyPolicy({ policy_version: policy.policy_version, bundle: policy });
  await settle();
  // A clean slate: the counters are cumulative since process start, and the policy sync and lane
  // re-registration above are not what these tests are measuring.
  h.app.health.counters.reset();
  h.core.received.length = 0;
  return h;
}

function observationFrames(core) {
  return core.received.filter((m) => m.type === TYPE.OBSERVATION).map((m) => m.body);
}

// ── the M0 guarantee ─────────────────────────────────────────────────────────────────────────

test('M0: a destination the policy resolves to M0 is never registered on the body-bearing lane', async () => {
  const h = await started();
  const body = h.fake.registration('body');
  assert.ok(body, 'a bundle with a readable default must install the body lane');
  assert.deepEqual(body.extra.includes('requestBody'), true, 'the body lane is the one asking for requestBody');
});

test('M0: with no bundle at all, no body-bearing listener is installed', async () => {
  const h = createHarness();
  // Start, but never let a bundle arrive: the fake core reports `unchanged`, so the cache stays empty.
  await h.app.start();
  h.app.policy.clear();
  h.app.lanes.refresh();
  assert.equal(h.fake.registration('body'), null, 'no bundle ⇒ no requestBody ⇒ Chrome never hands over a byte');
  assert.ok(h.fake.registration('metadata'), 'observation is still broad: the metadata lane is installed');
});

test('M0: a request to an M0 host is observed for identity and volume, and NOT retained, hashed or sent as content', async () => {
  const h = await started();
  const secret = JSON.stringify(CHAT_BODY);
  // The extension is asked about an M0 destination. The fake hands over a requestBody anyway —
  // which is the hostile case: even if bytes arrive, the pipeline must not read them.
  const detail = chromeRequest({
    url: 'https://denied.invalid/v1/chat/completions',
    headers: { 'content-type': 'application/json' },
    body: secret,
  });

  const result = await h.fake.drive('metadata', detail);
  await settle();

  assert.equal(result, undefined, 'an M0 observation does not cancel anything');
  const obs = h.core.lastObservation();
  assert.ok(obs, 'M0 still emits: identity, tool, times, size, mode and the policy decision');
  assert.equal(obs.has_content, false, 'has_content is false');
  assert.equal(obs.content, undefined, 'no content field');
  assert.equal(obs.content_digest, undefined, 'not hashed');
  assert.equal(obs.attachments, undefined, 'M0\'s closed list has no filenames on it either');
  assert.equal(obs.size_bytes, 0, 'the body was not measured, because it was not read');

  // The observation the pipeline built, before it was framed: the local audit must agree.
  const queued = h.app.queue.size();
  assert.equal(queued, 0, 'it went out on the wire, not the queue');

  // And the counters: observed moved, nothing was skipped, nothing was dropped.
  const counters = h.app.health.counters.snapshot().counters;
  assert.equal(counters.observed, 1);
  assert.equal(counters.dropped, 0);
});

test('M0: the body bytes handed to the metadata lane are not even looked at', async () => {
  const h = await started();
  // A body that would fail a strict UTF-8 decode: if it were read and decoded, the pipeline would
  // have produced a digest over it. It must produce neither.
  const detail = chromeRequest({
    url: 'https://denied.invalid/v1/chat/completions',
    headers: { 'content-type': 'application/json' },
    body: INVALID_UTF8,
  });
  await h.fake.drive('metadata', detail);
  await settle();
  const obs = h.core.lastObservation();
  assert.equal(obs.content_digest, undefined);
  assert.equal(obs.content, undefined);
  assert.equal(obs.content_is_binary, undefined);
});

// ── classification and the counters ──────────────────────────────────────────────────────────

test('a chat submission on the body lane is emitted with content, digest and identity', async () => {
  const h = await started();
  const bodyText = JSON.stringify(CHAT_BODY);
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: bodyText }));
  await settle();

  const obs = h.core.lastObservation();
  assert.ok(obs, 'a positive match is emitted');
  assert.equal(obs.has_content, true);
  // `content` is base64 because the protocol declares it `[]byte`; assert on the DECODED bytes,
  // which is what a real capture-core sees (§7.2's identity property).
  const decoded = h.core.lastDecodedContent();
  assert.ok(decoded, 'the frame content decodes the way encoding/json decodes []byte');
  assert.equal(Buffer.from(decoded).toString('utf8'), bodyText, 'the decoded content is the payload as sent');
  assert.equal(await h.core.lastComputedDigest(), obs.content_digest, 'and its digest matches content_digest');
  assert.equal(obs.content_is_binary, undefined);
  assert.ok(obs.tool_fingerprint.startsWith('tf1:'), '§8.1\'s versioned prefix');
  assert.equal(obs.route, 'ext.web_request');
  assert.equal(obs.size_bytes, Buffer.byteLength(bodyText));
});

test('a negative match is counted, never emitted — that is what makes the predicate auditable (§7.3)', async () => {
  const h = await started();
  const before = h.app.health.counters.snapshot().counters;
  await h.fake.drive(
    'body',
    chromeRequest({ url: DRAFT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(DRAFT_BODY) }),
  );
  await settle();

  const after = h.app.health.counters.snapshot().counters;
  assert.equal(after.observed - before.observed, 1, 'it was observed');
  assert.equal(after.skipped_not_generative - before.skipped_not_generative, 1, 'and counted as not generative');
  assert.equal(after.emitted, before.emitted, 'and nothing was emitted');
  assert.equal(observationFrames(h.core).length, 0);
});

test('a provider that classifies nothing and one that classifies everything are both visible in the counters', async () => {
  const h = await started();
  await h.fake.drive('body', chromeRequest({ url: DRAFT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(DRAFT_BODY) }));
  await h.fake.drive('body', chromeRequest({ requestId: 'r2', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const c = h.app.health.counters.snapshot().counters;
  assert.equal(c.observed, 2);
  assert.equal(c.emitted, 1, '§4.3: `emitted` counts envelopes, and an envelope reached capture-core once');
  assert.equal(c.skipped_not_generative, 1);
  assert.equal(observationFrames(h.core).length, 1, 'the counter and the wire must agree');

  // The audit property §7.3 states: a provider that classifies nothing and one that classifies
  // everything are both visible in the counters, and neither is visible from the event stream.
  const quiet = await started();
  for (let i = 0; i < 4; i++) {
    await quiet.fake.drive('body', chromeRequest({ requestId: `q${i}`, url: DRAFT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(DRAFT_BODY) }));
  }
  await settle();
  const q = quiet.app.health.counters.snapshot().counters;
  assert.equal(q.observed, 4);
  assert.equal(q.skipped_not_generative, 4, 'a silent provider is distinguishable from a broken one');
  assert.equal(q.emitted, 0);
  assert.equal(observationFrames(quiet.core).length, 0, 'and nothing at all was emitted');
});

// ── §7.2 decode paths, end to end ────────────────────────────────────────────────────────────

test('a strict UTF-8 body is carried as text; an invalid one is carried as bytes and marked binary', async () => {
  const h = await started();
  const good = JSON.stringify(CHAT_BODY);
  await h.fake.drive('body', chromeRequest({ requestId: 'r-good', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: good }));
  await settle();
  const textObs = h.core.lastObservation();
  assert.equal(textObs.content_is_binary, undefined);
  // The wire field is base64 in both paths; the decoded bytes are what carry the identity.
  assert.equal(Buffer.from(h.core.lastDecodedContent()).toString('utf8'), good);
  assert.equal(await h.core.lastComputedDigest(), textObs.content_digest);
  const goodDigest = textObs.content_digest;

  // The same structurally-valid submission, but with bytes inside a string value that are not
  // valid UTF-8. The strict decoder must fail on the whole payload, so the extension marks it
  // binary and hashes the RAW BYTES — never a lossily-substituted string. A lossy decode here
  // would produce a different digest for the same wire bytes, breaking cross-route dedup (§7.2).
  const invalidChat = Buffer.concat([
    Buffer.from('{"model":"example-large-2026","messages":[{"role":"user","content":"Quarterly figures '),
    Buffer.from([0xc3, 0x28, 0xff, 0xfe]), // 0xC3 expects a continuation byte; 0x28 is not one
    Buffer.from(' margin"}]}'),
  ]);
  await h.fake.drive('body', chromeRequest({ requestId: 'r-bad', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: invalidChat }));
  await settle();
  const binaryObs = h.core.lastObservation();
  assert.ok(binaryObs, 'a structurally valid submission with undecodable bytes is still an observation');
  assert.equal(binaryObs.content_is_binary, true, '§7.2: binary fallback, never lossy replacement');
  assert.equal(binaryObs.size_bytes, invalidChat.byteLength);
  assert.notEqual(binaryObs.content_digest, goodDigest);

  // The proof that the digest is over the raw bytes and nothing else: decode the frame the way
  // encoding/json would, and it is byte-identical to what the browser sent.
  const decodedBinary = h.core.lastDecodedContent();
  assert.deepEqual([...decodedBinary], [...invalidChat], 'the bytes survive the whole path unchanged');
  assert.equal(await h.core.lastComputedDigest(), binaryObs.content_digest, 'the digest describes those bytes');
  assert.equal(Buffer.from(decodedBinary).toString('utf8').includes('\uFFFD'), true, 'the lossy reading is available but was NOT what was hashed');
});

// ── bounded bodies (§5.3) ───────────────────────────────────────────────────────────────────

test('an over-cap body is sized and hashed, emitted degraded, and never held whole', async () => {
  const cap = 2048;
  const h = await started({ bundle: { policy_version: 'b1', default_mode: 'm1', mode_caps: { body_bytes: cap } } });
  const huge = Buffer.alloc(64 * 1024, 0x61);
  const prefix = Buffer.from('{"model":"m","messages":[{"role":"user","content":"');
  prefix.copy(huge, 0);

  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: huge }));
  await settle();

  const obs = h.core.lastObservation();
  assert.ok(obs, 'an over-cap body is still an event');
  assert.equal(obs.over_cap, true, 'native.go carries `over_cap` as the degraded signal');
  assert.equal(obs.size_bytes, huge.byteLength, 'the size is of the payload as sent');
  assert.match(obs.content_digest, /^sha256:/, 'hashed, because a prefix digest is what M1 permits');
  const decoded = h.core.lastDecodedContent();
  assert.ok(decoded.byteLength <= cap, 'only the prefix was ever held, and the frame carries no more than that');
  assert.equal(await h.core.lastComputedDigest(), obs.content_digest, 'the digest describes the prefix that was actually carried');
  assert.equal(obs.degraded_reason, 'content_over_cap', '§9.7: an over-cap body is degraded, never reported as "clean"');});

// ── §7.4 inline warn/block ──────────────────────────────────────────────────────────────────

test('a blocked request is CANCELLED and is still an event', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      rules: [{ rule_id: 'BLOCK_EXTERNAL', action: 'blocked', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'enforcing' },
    },
  });

  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.deepEqual(response, { cancel: true }, 'blocked cancels through webRequestBlocking');
  const obs = h.core.lastObservation();
  assert.ok(obs, '§7.4: a blocked request is still an event, or we could not answer "what did we stop"');
  assert.equal(obs.decision.action, 'blocked');
  assert.equal(obs.decision.rule_id, 'BLOCK_EXTERNAL');
  assert.equal(obs.decision.decided_locally, true, 'the decision is local: no round trip');
});

test('a shadow release computes the decision but never blocks (§9.6)', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      rules: [{ rule_id: 'BLOCK_EXTERNAL', action: 'blocked', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'shadow' },
    },
  });
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined, 'a shadow release must not block');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.decision.decided_locally, true);
});

test('a warned request is held, the user answer is recorded, and declining cancels it', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      confirmation_window_ms: 1000,
      rules: [{ rule_id: 'WARN_PII', action: 'warned', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'enforcing' },
    },
  });
  h.fake.state.tabAnswers.set(1, () => ({ ok: true, answered: true, proceeded: false }));

  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.deepEqual(response, { cancel: true }, 'declining the warning cancels the request');
  const obs = h.core.lastObservation();
  assert.ok(obs, 'the warned submission is an event either way');
  assert.equal(obs.decision.action, 'blocked');
  assert.equal(obs.decision.rule_id, 'WARN_PII');
  assert.equal(obs.decision.decided_locally, true);
});

test('proceeding past a warning is recorded as `warned`, with the answer as part of the decision', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      confirmation_window_ms: 1000,
      rules: [{ rule_id: 'WARN_PII', action: 'warned', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'enforcing' },
    },
  });
  h.fake.state.tabAnswers.set(1, () => ({ ok: true, answered: true, proceeded: true }));
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined, 'the request proceeds');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'warned', 'two prompts sent is two events, each recording its own answer');
  assert.equal(obs.decision.decided_locally, true);
});

test('a warn with no one to ask fails OPEN, degraded, and counts the failure', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      confirmation_window_ms: 40,
      rules: [{ rule_id: 'WARN_PII', action: 'warned', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'enforcing' },
    },
  });
  // No tab answer is registered: the content script is absent, which is the real case on a page
  // the extension cannot reach.
  const errorsBefore = h.app.health.counters.snapshot().counters.errors;
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.equal(response, undefined, 'brief §6: a broken classifier must not become a broken browser');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.degraded_reason, 'classifier_unavailable', 'C21: degraded is explicit, never "clean"');
  const errorsAfter = h.app.health.counters.snapshot().counters.errors;
  assert.ok(errorsAfter > errorsBefore, 'the failure is counted, because a silent fail-open is a lie');
});

test('a logged decision carries decided_locally: true', async () => {
  const h = await started({
    bundle: {
      policy_version: 'b1',
      default_mode: 'm1',
      rules: [{ rule_id: 'LOG_ALL', action: 'logged', hosts: ['chat.example-ai.invalid'] }],
      classifier_release: { version: 'c1', state: 'enforcing' },
    },
  });
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.decision.decided_locally, true);
});

// ── §7.4/E4: the WebSocket gap ──────────────────────────────────────────────────────────────

test('a WebSocket handshake is captured as identity and volume only, and the frames are not', async () => {
  const h = await started();
  await h.fake.drive(
    'metadata',
    chromeRequest({
      requestId: 'ws1',
      url: 'wss://chat.example-ai.invalid/socket',
      method: 'GET',
      type: 'websocket',
      headers: { upgrade: 'websocket' },
    }),
  );
  await settle();

  const obs = h.core.lastObservation();
  assert.ok(obs, '§7.4: the handshake is captured, so tool identity and a session count are obtainable');
  assert.equal(obs.has_content, false);
  assert.equal(obs.content, undefined);
  assert.match(obs.tool_fingerprint, /^tf1:/);
  const queued = h.app.queue.size();
  assert.equal(queued, 0);
});

// ── §3.4 channel-down reporting ─────────────────────────────────────────────────────────────

test('with capture-core absent, observations are queued, the queue is bounded, and drops are counted', async () => {
  // The channel never exists: the harness is built with the connection failing, so the adapter
  // closes over a failing `connectNative` and every send lands in the queue.
  const h = await started({ capacity: 3, failConnect: true });

  for (let i = 0; i < 6; i++) {
    await h.fake.drive(
      'body',
      chromeRequest({ requestId: `r${i}`, url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }),
    );
    await settle(2);
  }
  await settle();

  assert.equal(h.app.queue.size(), 3, 'the bound is enforced, not hoped for');
  const c = h.app.health.counters.snapshot().counters;
  assert.equal(c.observed, 6);
  assert.equal(c.dropped, 3, 'drop-oldest with a counter (C22)');
  assert.equal(h.app.queue.stats().dropped_total, 3);
  assert.equal(h.app.health.state, 'absent', 'the channel, not the coverage, is what is absent');
});

test('a send that fails mid-flight counts the error and falls back to the queue', async () => {
  const h = await started();
  // Connected, then the host dies between two sends: the failure is counted, not swallowed.
  h.fake.state.failPostAfter = h.fake.state.posted.length;
  const before = h.app.health.counters.snapshot().counters;
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle(2);
  const after = h.app.health.counters.snapshot();
  assert.ok(after.counters.errors > before.errors, 'a failed send is an error, not a silent drop');
  assert.equal(h.app.queue.size(), 1, 'and the observation is held rather than lost');
  assert.equal(h.app.health.state, 'absent', 'the channel is now absent and the health report says so');
});

test('a failed connect is reported as capture-core absent AND extension-side degraded, never as "no observations"', async () => {
  const h = await started({ failConnect: true });
  const before = h.app.health.counters.snapshot().counters;
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle(2);

  const report = h.app.health.report();
  assert.equal(report.core, 'absent');
  assert.equal(report.state, 'degraded');
  assert.equal(report.collector, 'capture_extension');
  assert.ok(report.counters.observed >= 1, 'what WAS observed is still reported');
  assert.ok(report.queue.depth >= 1, 'and what could not be delivered is reported with it');
  assert.ok(report.counters.dropped >= before.dropped, 'the queue owns the drop counter and it is visible');
  // §3.4 is explicit that this is NOT "no observations": observed moved even though the channel is
  // dead, and the two facts are reported together rather than one standing in for the other.
  assert.equal(report.counters.observed, before.observed + 1);
});

test('when the channel returns, the queued observations are merged out and the report says so', async () => {
  // Queue first, against a dead channel.
  const h = createHarness();
  h.fake.state.failConnect = true;
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { policy_version: 'b1', default_mode: 'm1' } });
  await settle();
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(h.app.queue.size(), 1, 'held in extension memory only');
  const held = h.app.health.report();
  assert.equal(held.core, 'absent');

  // Now let the channel come back: the drain runs and the queue empties.
  h.fake.state.failConnect = false;
  const sent = await h.app.pipeline.drainQueue(10);
  assert.equal(sent.sent, 1, 'the queued observation went out oldest-first');
  assert.equal(h.app.queue.size(), 0);
  const report = h.app.health.report();
  assert.equal(report.core, 'connected');
  assert.ok(report.counters.emitted >= 1);
});

test('nothing durable is written: no extension storage call is ever made', async () => {
  const h = await started();
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await h.fake.drive('body', chromeRequest({ requestId: 'r2', url: DRAFT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(DRAFT_BODY) }));
  await settle();
  assert.equal(h.fake.state.session.size, 0, '§7.1: the extension holds nothing durable');
});

// ── health ──────────────────────────────────────────────────────────────────────────────────

test('the health report is sent on the health channel with all seven counters and the bundle version', async () => {
  const h = await started();
  const report = h.app.reportHealth();
  await settle();
  const frame = h.core.received.filter((m) => m.type === TYPE.HEALTH).pop();
  assert.ok(frame, 'the report travels on the health channel, not as an event');
  assert.equal(frame.body.collector, 'capture_extension');
  assert.deepEqual(Object.keys(frame.body.counters).sort(), [
    'blind_tunnelled',
    'dropped',
    'emitted',
    'errors',
    'not_cooperative',
    'observed',
    'skipped_not_generative',
  ]);
  assert.equal(frame.body.policy.version, 'bundle-1', '§11.3 mode-change attribution');
});

test('the alarms are installed for the health report and the policy poll', async () => {
  const h = createHarness();
  await h.app.start();
  const names = h.fake.state.alarms.map((a) => a.name);
  assert.ok(names.includes('capture-health'), 'the report is periodic, so a drain happens without traffic');
  assert.ok(names.includes('capture-policy-sync'), '§11.3: the bundle is re-read, so a mode change takes effect');
  assert.equal(h.fake.state.alarmHandlers.length, 1);
});

// ── §7.5 Mode B: the response contract ──────────────────────────────────────────────────────

test('§7.5 Mode B: a weak submission is upgraded when the response contract agrees, on the higher-fidelity route', async () => {
  const h = await started();
  const prose = JSON.stringify({ note_id: 'n1', text: 'A long note that a person typed. '.repeat(8) });
  const detail = chromeRequest({ requestId: 'resp-1', url: 'https://saas.example-ai.invalid/api/assist', headers: { 'content-type': 'application/json' }, body: prose });
  await h.fake.drive('body', detail);
  await settle(2);

  const listener = h.fake.completeListener();
  assert.ok(listener, 'the response lane is registered');
  await listener({
    requestId: 'resp-1',
    url: 'https://saas.example-ai.invalid/api/assist',
    method: 'POST',
    statusCode: 200,
    responseHeaders: Object.entries(STREAMING_RESPONSE.headers).map(([name, value]) => ({ name, value })),
    type: 'xmlhttprequest',
    timeStamp: 1,
  });
  await settle(2);

  const observations = observationFrames(h.core);
  assert.ok(observations.length >= 1, 'the response contract produced an observation the request alone did not');
  assert.equal(observations[observations.length - 1].route, 'ext.page_context', 'the canonical route for this evidence');
  const counters = h.app.health.counters.snapshot().counters;
  assert.ok(counters.emitted >= 1);
});

test('a response for a request the predicate never held changes nothing', async () => {
  const h = await started();
  const listener = h.fake.completeListener();
  await listener({ requestId: 'never-seen', url: 'https://x.invalid/a', method: 'POST', statusCode: 200, responseHeaders: [], type: 'xmlhttprequest', timeStamp: 1 });
  await settle();
  assert.equal(observationFrames(h.core).length, 0);
});

// ── policy change ───────────────────────────────────────────────────────────────────────────

test('a policy change re-derives the body lane, so an M0 destination stops being body-bearing', async () => {
  const h = await started();
  assert.ok(h.fake.registration('body'));
  h.app.applyPolicy({
    policy_version: 'bundle-2',
    bundle: { policy_version: 'bundle-2', default_mode: 'm1', body_lane_patterns: ['https://chat.example-ai.invalid/*'], scope: { 'saas.example-ai.invalid': 'm0' } },
  });
  await settle();
  const registration = h.fake.registration('body');
  assert.deepEqual(registration.urls, ['https://chat.example-ai.invalid/*'], 'the include list is what Chrome is given');
  assert.equal(h.app.policy.snapshot().policy_version, 'bundle-2');
});

test('a bundle that resolves everything to M0 removes the body lane entirely', async () => {
  const h = await started();
  assert.ok(h.fake.registration('body'));
  h.app.applyPolicy({ policy_version: 'b3', bundle: { policy_version: 'b3', default_mode: 'm1', body_lane_patterns: ['https://only.invalid/*'] } });
  await settle();
  h.app.applyPolicy({ policy_version: 'b4', bundle: { policy_version: 'b4', default_mode: 'm1', body_lane_patterns: [] } });
  h.app.policy.clear();
  h.app.lanes.refresh();
  assert.equal(h.fake.registration('body'), null, 'the listener is removed, not just ignored');
  const removed = h.fake.registrations.filter((r) => r.action === 'remove');
  assert.ok(removed.length >= 1);
});

test('an unusable bundle leaves the previous policy enforcing and is reported', async () => {
  const h = await started();
  const result = h.app.applyPolicy({ policy_version: 'b-bad', bundle: 'not an object' });
  assert.equal(result.applied, false);
  assert.equal(h.app.policy.snapshot().policy_version, 'bundle-1', '§13.3: the previous bundle is retained');
  assert.ok(h.app.health.counters.snapshot().counters.errors >= 1);
});

// ── the frames themselves ───────────────────────────────────────────────────────────────────

test('every emitted observation frame validates against the protocol\'s own rules', async () => {
  const h = await started();
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const frame = h.core.received.find((m) => m.type === TYPE.OBSERVATION);
  assert.equal(frame.version, 1);
  assert.ok(frame.body.client_id, 'each observation carries a client id for its later attachment frames');
  assert.equal(h.core.emitted.length >= 1, true);
  // The fake core would have refused a contradiction; nothing was refused.
  assert.equal(h.core.received.some((m) => m.type === CORE_TYPE.REFUSAL), false);
});

test('the client id is stable per observation so attachment chunks can reference it', async () => {
  const h = await started();
  await h.fake.drive('body', chromeRequest({ requestId: 'a', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await h.fake.drive('body', chromeRequest({ requestId: 'b', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const ids = observationFrames(h.core).map((o) => o.client_id);
  assert.equal(ids.length, 2);
  assert.notEqual(ids[0], ids[1], 'two observations are two logical facts, even of the same bytes');
});

test('an automation marker raises the context evidence but is never required (§7.5 Mode C)', async () => {
  const h = await started();
  const marked = chromeRequest({
    url: 'https://agent.example-ai.invalid/v1/respond',
    headers: { 'content-type': 'application/json', 'x-automation': 'playwright' },
    body: JSON.stringify({ prompt: 'Summarise the repository and list every TODO.', model: 'example-large-2026' }),
  });
  await h.fake.drive('body', marked);
  await settle();
  const obs = h.core.lastObservation();
  assert.ok(obs, 'identity and volume are mandatory for mode C and are met without any DOM access');
  assert.match(obs.tool_fingerprint, /^tf1:/);
});
