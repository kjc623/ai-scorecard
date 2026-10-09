/**
 * test/worker.test.mjs — the extension driven end to end through the fake chrome.
 *
 * Everything goes through the real wiring: `background/service-worker.js` → `registration.js` →
 * `pipeline.js` → `native.js` → the fake capture-core. `fake.drive(lane, detail)` calls the listener
 * Chrome would call, registered with the `extraInfoSpec` Chrome would have been given, so the
 * registration assertions are about what a browser would hand over. tools/in-browser-check.mjs
 * covers a real browser.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createHarness, settle, waitFor } from '../test-support/harness.mjs';
import { chromeRequest, INVALID_UTF8 } from '../test-support/fake-chrome.mjs';
import { CHAT_BODY, DRAFT_BODY, STREAMING_RESPONSE } from '../test-support/fixtures.mjs';
import { COUNTER, CORE_TYPE, REFUSAL, TYPE } from '../src/messages.js';
import { normaliseBody } from '../src/request-body.js';

/** A real delay, for the one test that has to let a backoff window elapse. */
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const CHAT_URL = 'https://chat.example-ai.invalid/v1/chat/completions';
const DRAFT_URL = 'https://saas.example-ai.invalid/api/drafts';

/** Start the extension against a bundle in force, by default one that reads bodies at m1. */
async function started({ core = {}, capacity = 200, bundle = null, failConnect = false } = {}) {
  const h = createHarness({ core, capacity, failConnect });
  await h.app.start();
  const policy = bundle || { version: 'bundle-1', tenant_default_mode: 'm1' };
  h.app.applyPolicy({ policy_version: policy.version, bundle: policy });
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

test('M0: a tenant at M0 has no body-bearing lane; one that reads content has it', async () => {
  const m0 = await started({ bundle: { version: 'b0', tenant_default_mode: 'm0' } });
  assert.equal(m0.fake.registration('body'), null, 'no destination may be read, so Chrome is never asked for a body');
  assert.ok(m0.fake.registration('metadata'), 'observation is still broad: the metadata lane is installed');

  const h = await started();
  const body = h.fake.registration('body');
  assert.ok(body, 'a bundle with a readable default must install the body lane');
  assert.deepEqual(body.extra.includes('requestBody'), true, 'the body lane is the one asking for requestBody');
});

test('a tool whose own mode is M0 is emitted without content, even on the body lane', async () => {
  const h = await started();
  const send = (requestId) =>
    h.fake.drive('body', chromeRequest({ requestId, url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await send('before');
  await settle();
  const tool = h.core.lastObservation().tool_fingerprint;
  assert.equal(h.core.lastObservation().has_content, true, 'at the tenant default the body is read');

  h.app.applyPolicy({ policy_version: 'bundle-2', bundle: { version: 'bundle-2', tenant_default_mode: 'm1', tool_modes: { [tool]: 'm0' } } });
  await send('after');
  await settle();
  const obs = h.core.lastObservation();
  assert.equal(obs.tool_fingerprint, tool, 'one tool keeps one fingerprint whatever its mode');
  assert.equal(obs.has_content, false);
  assert.equal(obs.content, undefined);
  assert.equal(obs.content_digest, undefined, 'not hashed either');
});

test('with population or device scopes in the bundle the mode is capture-core\'s answer to mode_query', async () => {
  for (const [answer, reads] of [
    ['m0', false],
    ['m2', true],
  ]) {
    const h = await started({
      core: { modeAnswer: { mode: answer, policy_version: 'b1', reason: 'population=' + answer } },
      bundle: { version: 'b1', tenant_default_mode: 'm2', population_modes: { finance: 'm0' } },
    });
    await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
    await settle();
    const query = h.core.received.find((m) => m.type === TYPE.MODE_QUERY);
    assert.ok(query, 'capture-core was asked');
    assert.match(query.body.tool_fingerprint, /^tf1:/, 'about this tool');
    assert.equal(h.core.lastObservation().has_content, reads, `answer ${answer}`);
  }
});

test('a mode_query nobody answers resolves to M0', async () => {
  const h = await started({ bundle: { version: 'b1', tenant_default_mode: 'm2', device_modes: { 'dev-1': 'm1' } } });
  h.core.handle = ((handle) => async (message) =>
    message.type === TYPE.MODE_QUERY ? { type: CORE_TYPE.REFUSAL, version: 1, id: message.id, body: { reason: REFUSAL.MALFORMED, message: 'no' } } : handle(message))(h.core.handle);
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const obs = h.core.lastObservation();
  assert.ok(obs, 'still an event');
  assert.equal(obs.has_content, false, 'no answer is not a licence to read');
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
  // which is what a real capture-core sees.
  const decoded = h.core.lastDecodedContent();
  assert.ok(decoded, 'the frame content decodes the way encoding/json decodes []byte');
  assert.equal(Buffer.from(decoded).toString('utf8'), bodyText, 'the decoded content is the payload as sent');
  assert.equal(await h.core.lastComputedDigest(), obs.content_digest, 'and its digest matches content_digest');
  assert.equal(obs.content_is_binary, undefined);
  assert.ok(obs.tool_fingerprint.startsWith('tf1:'), 'the versioned fingerprint prefix');
  assert.equal(obs.route, 'ext.web_request');
  assert.equal(obs.size_bytes, Buffer.byteLength(bodyText));
});

test('a negative match is counted, never emitted: that is what makes the predicate auditable', async () => {
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
  assert.equal(c.emitted, 1, '`emitted` counts observations capture-core accepted, once each');
  assert.equal(c.skipped_not_generative, 1);
  assert.equal(observationFrames(h.core).length, 1, 'the counter and the wire must agree');

  // A provider that classifies nothing and one that classifies everything are both visible in the
  // counters, though neither is visible from the event stream.
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

// ── decode paths, end to end ─────────────────────────────────────────────────────────────────

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
  // would produce a different digest for the same wire bytes, breaking cross-route dedup.
  const invalidChat = Buffer.concat([
    Buffer.from('{"model":"example-large-2026","messages":[{"role":"user","content":"Quarterly figures '),
    Buffer.from([0xc3, 0x28, 0xff, 0xfe]), // 0xC3 expects a continuation byte; 0x28 is not one
    Buffer.from(' margin"}]}'),
  ]);
  await h.fake.drive('body', chromeRequest({ requestId: 'r-bad', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: invalidChat }));
  await settle();
  const binaryObs = h.core.lastObservation();
  assert.ok(binaryObs, 'a structurally valid submission with undecodable bytes is still an observation');
  assert.equal(binaryObs.content_is_binary, true, 'binary fallback, never lossy replacement');
  assert.equal(binaryObs.size_bytes, invalidChat.byteLength);
  assert.notEqual(binaryObs.content_digest, goodDigest);

  // The proof that the digest is over the raw bytes and nothing else: decode the frame the way
  // encoding/json would, and it is byte-identical to what the browser sent.
  const decodedBinary = h.core.lastDecodedContent();
  assert.deepEqual([...decodedBinary], [...invalidChat], 'the bytes survive the whole path unchanged');
  assert.equal(await h.core.lastComputedDigest(), binaryObs.content_digest, 'the digest describes those bytes');
  assert.equal(Buffer.from(decodedBinary).toString('utf8').includes('\uFFFD'), true, 'the lossy reading is available but was NOT what was hashed');
});

// ── bounded bodies ──────────────────────────────────────────────────────────────────────────

test('an over-cap body is sized and hashed, emitted degraded, and never held whole', async () => {
  const cap = 2048;
  const h = await started();
  const huge = Buffer.alloc(64 * 1024, 0x61);
  const prefix = Buffer.from('{"model":"m","messages":[{"role":"user","content":"');
  prefix.copy(huge, 0);

  const detail = chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: huge });
  await h.app.pipeline.captureWithBody({ detail, body: normaliseBody(detail.requestBody, { capBytes: cap }) });
  await settle();

  const obs = h.core.lastObservation();
  assert.ok(obs, 'an over-cap body is still an event');
  assert.equal(obs.over_cap, true, 'native.go carries `over_cap` as the degraded signal');
  assert.equal(obs.size_bytes, huge.byteLength, 'the size is of the payload as sent');
  assert.match(obs.content_digest, /^sha256:/, 'hashed, because a prefix digest is what M1 permits');
  const decoded = h.core.lastDecodedContent();
  assert.ok(decoded.byteLength <= cap, 'only the prefix was ever held, and the frame carries no more than that');
  assert.equal(await h.core.lastComputedDigest(), obs.content_digest, 'the digest describes the prefix that was actually carried');
  assert.equal(obs.degraded_reason, 'content_over_cap', 'an over-cap body is degraded, never reported as "clean"');
});

// ── inline warn/block ───────────────────────────────────────────────────────────────────────

/** A bundle at m1 holding `rules`; nothing is sanctioned, so every browser tool is unsanctioned. */
function ruleBundle(rules) {
  return { version: 'b1', tenant_default_mode: 'm1', rules, sanctioned_tools: [] };
}

const BLOCK_BROWSER = {
  rule_id: 'block_unsanctioned_browser',
  action: 'block',
  match: { labels: [], tools: [], categories: [], sanction: ['unsanctioned'], routes: ['ext.web_request'] },
  message: 'This AI tool is not sanctioned.',
  link: 'https://intranet.example/ai',
};
const WARN_BROWSER = { ...BLOCK_BROWSER, rule_id: 'warn_unsanctioned_browser', action: 'warn', message: 'Check before you send company data.' };

test('a blocked request is CANCELLED, the overlay shows the rule\'s message and link, and it is still an event', async () => {
  const h = await started({ bundle: ruleBundle([BLOCK_BROWSER]) });
  h.fake.state.tabAnswers.set(1, () => ({ ok: true }));

  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.deepEqual(response, { cancel: true }, 'blocked cancels through webRequestBlocking');
  const notice = h.fake.state.messages.find((m) => m.message && m.message.type === 'capture_block');
  assert.ok(notice, 'the tab is told why');
  assert.equal(notice.message.spec.message, 'This AI tool is not sanctioned.');
  assert.equal(notice.message.spec.link, 'https://intranet.example/ai');
  assert.equal(notice.message.spec.rule_id, 'block_unsanctioned_browser');
  const obs = h.core.lastObservation();
  assert.ok(obs, 'a blocked request is still an event, or nothing could answer "what did we stop"');
  assert.equal(obs.decision.action, 'blocked');
  assert.equal(obs.decision.rule_id, 'block_unsanctioned_browser');
  assert.equal(obs.decision.decided_locally, true, 'the decision is local: no round trip');
});

test('a block nobody could be shown still cancels, and the missing notice is counted', async () => {
  const h = await started({ bundle: ruleBundle([BLOCK_BROWSER]) });
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.deepEqual(response, { cancel: true });
  assert.equal(h.app.health.counters.snapshot().errors_by_code.block_notice_unavailable, 1);
});

test('the bundle\'s rules match on what the extension knows: a label rule does not match without a classification', async () => {
  const h = await started({
    bundle: ruleBundle([{ ...BLOCK_BROWSER, rule_id: 'block_credentials', match: { labels: ['credential'], routes: ['ext.web_request'] } }]),
  });
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined);
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.decision.rule_id, 'policy.default');
});

test('a sanctioned tool is not matched by an unsanctioned rule', async () => {
  const h = await started();
  await h.fake.drive('body', chromeRequest({ requestId: 'probe', url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  const tool = h.core.lastObservation().tool_fingerprint;
  h.app.applyPolicy({ policy_version: 'b2', bundle: { ...ruleBundle([BLOCK_BROWSER]), version: 'b2', sanctioned_tools: [tool] } });
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined, 'the tenant sanctioned this tool');
  assert.equal(h.core.lastObservation().decision.rule_id, 'policy.default');
});

test('a warned request is held with the rule\'s message and link, the answer is recorded, and declining cancels it', async () => {
  const h = await started({ bundle: ruleBundle([WARN_BROWSER]) });
  let spec = null;
  h.fake.state.tabAnswers.set(1, (message) => {
    if (message.type === 'capture_warn') spec = message.spec;
    return { ok: true, answered: true, proceeded: false };
  });

  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.deepEqual(response, { cancel: true }, 'declining the warning cancels the request');
  assert.equal(spec.message, 'Check before you send company data.', 'the confirmation shows the rule\'s message');
  assert.equal(spec.link, 'https://intranet.example/ai', 'and its link');
  assert.equal(spec.timeout_ms, 20_000, 'and waits the extension\'s confirmation window');
  const obs = h.core.lastObservation();
  assert.ok(obs, 'the warned submission is an event either way');
  assert.equal(obs.decision.action, 'blocked');
  assert.equal(obs.decision.rule_id, 'warn_unsanctioned_browser');
  assert.equal(obs.decision.decided_locally, true);
});

test('proceeding past a warning is recorded as `warned`, with the answer as part of the decision', async () => {
  const h = await started({ bundle: ruleBundle([WARN_BROWSER]) });
  h.fake.state.tabAnswers.set(1, () => ({ ok: true, answered: true, proceeded: true }));
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined, 'the request proceeds');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'warned', 'two prompts sent is two events, each recording its own answer');
  assert.equal(obs.decision.decided_locally, true);
});

test('a warn with no one to ask fails OPEN, degraded, and counts the failure', async () => {
  const h = await started({ bundle: ruleBundle([WARN_BROWSER]) });
  // No tab answer is registered: the content script is absent, which is the real case on a page
  // the extension cannot reach.
  const errorsBefore = h.app.health.counters.snapshot().counters.errors;
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();

  assert.equal(response, undefined, 'a broken classifier must not become a broken browser');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.degraded_reason, 'classifier_unavailable', 'degraded is explicit, never "clean"');
  const errorsAfter = h.app.health.counters.snapshot().counters.errors;
  assert.ok(errorsAfter > errorsBefore, 'the failure is counted, because a silent fail-open is a lie');
});

test('an allow rule records logged under its own id, with decided_locally: true', async () => {
  const h = await started({ bundle: ruleBundle([{ ...BLOCK_BROWSER, rule_id: 'allow_browser', action: 'allow' }]) });
  const response = await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(response, undefined);
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.decision.rule_id, 'allow_browser');
  assert.equal(obs.decision.decided_locally, true);
});

// ── WebSocket: the handshake only ───────────────────────────────────────────────────────────

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
  assert.ok(obs, 'the handshake is captured, so tool identity and a session count are obtainable');
  assert.equal(obs.has_content, false);
  assert.equal(obs.content, undefined);
  assert.match(obs.tool_fingerprint, /^tf1:/);
  const queued = h.app.queue.size();
  assert.equal(queued, 0);
});

// ── channel-down reporting ──────────────────────────────────────────────────────────────────

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
  assert.equal(c.dropped, 3, 'drop-oldest with a counter');
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
  // This is NOT "no observations": observed moved even though the channel is dead, and the two
  // facts are reported together rather than one standing in for the other.
  assert.equal(report.counters.observed, before.observed + 1);
});

test('when the channel returns, the queued observations are merged out and the report says so', async () => {
  // Queue first, against a dead channel. The backoff is set to zero here so this test isolates the
  // ack semantics; the backoff has its own test below, and mixing the two would make this one fail
  // for a reason that is not its claim.
  const h = createHarness({ failConnect: true, connectCooldownMs: 0 });
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { version: 'b1', tenant_default_mode: 'm1' } });
  await settle();
  const errorsBefore = h.app.health.counters.snapshot().counters.errors;
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));

  // BEFORE the ack: present, and the failed attempt is counted. "Not lost" and "we tried and failed"
  // are different facts, and an operator needs the second one.
  await waitFor(() => h.app.queue.size() === 1, { label: 'the observation to be held' });
  assert.equal(h.app.queue.size(), 1, 'held in extension memory only');
  assert.equal(h.app.health.report().core, 'absent');
  assert.ok(
    h.app.health.counters.snapshot().counters.errors > errorsBefore,
    'the failed delivery attempt is counted, not silently swallowed',
  );
  assert.equal(h.app.queue.stats().dropped_total, 0, 'and it was queued, not dropped');

  // AFTER the ack: gone, folded into `emitted`, and only then.
  h.fake.state.failConnect = false;
  const sent = await h.app.pipeline.drainQueue(10);
  assert.equal(sent.sent, 1, 'the queued observation went out oldest-first');
  assert.equal(sent.failures.length, 0);
  await waitFor(() => h.app.queue.size() === 0, { label: 'the ack to remove the entry' });
  assert.equal(h.app.queue.stats().delivered_total, 1, 'removed because it was acked, and counted as such');
  const report = h.app.health.report();
  assert.equal(report.core, 'connected', 'connected means a message round-tripped, not that a port was handed out');
  assert.equal(report.counters.emitted, 1, 'emitted follows the ack');
});

test('a dead native host does not become a retry loop: connects are backed off, not hammered', async () => {
  // Without the cool-down each connect is followed by the browser delivering a disconnect for the
  // absent host and the next send opening another port: sustained CPU burn for a missing channel.
  const h = createHarness({ failConnect: true, connectCooldownMs: 5000 });
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { version: 'b1', tenant_default_mode: 'm1' } });
  await settle();

  const attemptsBefore = h.fake.state.connectAttempts;
  for (let i = 0; i < 20; i++) {
    await h.fake.drive(
      'body',
      chromeRequest({ requestId: `loop${i}`, url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }),
    );
    await settle(2);
  }
  const attempts = h.fake.state.connectAttempts - attemptsBefore;

  assert.ok(attempts <= 4, `20 observations against a dead host made ${attempts} connect attempts; a loop would make ~20+`);
  assert.ok(h.app.queue.size() > 0, 'and every observation is held rather than lost');
  const errors = h.app.health.counters.snapshot().counters.errors;
  assert.ok(errors <= 6, `the error counter must not climb per attempt while nothing new happens: ${errors}`);
});

test('nothing durable is written: the fake chrome has no storage API and nothing reaches for one', async () => {
  const h = await started();
  assert.equal(h.fake.chrome.storage, undefined);
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await h.fake.drive('body', chromeRequest({ requestId: 'r2', url: DRAFT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(DRAFT_BODY) }));
  await settle();
  assert.equal(h.core.observations().length >= 1, true, 'the extension still worked without any storage');
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
  assert.equal(frame.body.policy.version, 'bundle-1', 'mode-change attribution');
});

test('the alarms are installed for the health report and the policy poll', async () => {
  const h = createHarness();
  await h.app.start();
  const names = h.fake.state.alarms.map((a) => a.name);
  assert.ok(names.includes('capture-health'), 'the report is periodic, so a drain happens without traffic');
  assert.ok(names.includes('capture-policy-sync'), 'the bundle is re-read, so a mode change takes effect');
  assert.equal(h.fake.state.alarmHandlers.length, 1);
});

// ── the response contract ───────────────────────────────────────────────────────────────────

test('a weak submission is upgraded when the response contract agrees, on the higher-fidelity route', async () => {
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

test('a policy change re-derives the body lane: a tenant default of M0 removes it entirely', async () => {
  const h = await started();
  assert.deepEqual(h.fake.registration('body').urls, ['<all_urls>']);
  h.app.applyPolicy({ policy_version: 'bundle-2', bundle: { version: 'bundle-2', tenant_default_mode: 'm0' } });
  await settle();
  assert.equal(h.fake.registration('body'), null, 'the listener is removed, not just ignored');
  const removed = h.fake.registrations.filter((r) => r.action === 'remove');
  assert.ok(removed.length >= 1);
  assert.equal(h.app.policy.snapshot().policy_version, 'bundle-2');

  h.app.applyPolicy({ policy_version: 'bundle-3', bundle: { version: 'bundle-3', tenant_default_mode: 'm1' } });
  await settle();
  assert.deepEqual(h.fake.registration('body').urls, ['<all_urls>'], 'and a mode that reads content brings it back without a restart');
});

test('the signed envelope is not a bundle: fed to the worker it resolves M0 with no rules', async () => {
  // What capture-core used to send: the envelope, not the payload it carries.
  const h = createHarness();
  await h.app.start();
  const payload = { version: '5', tenant_default_mode: 'm2', rules: [BLOCK_BROWSER], sanctioned_tools: [] };
  const envelope = { key_id: 'policy-key-1', algorithm: 'ed25519', payload, signature: 'c2ln' };
  assert.equal(h.app.applyPolicy({ policy_version: '5', bundle: envelope }).applied, false);
  assert.equal(h.app.policy.modeFor({}).mode, 'm0');
  assert.deepEqual(h.app.policy.rules().rules, []);
  assert.equal(h.fake.registration('body'), null);

  // The payload capture-core now sends resolves as the device does.
  assert.equal(h.app.applyPolicy({ policy_version: '5', bundle: payload }).applied, true);
  assert.equal(h.app.policy.modeFor({}).mode, 'm2');
  assert.equal(h.app.policy.rules().rules.length, 1);
  assert.ok(h.fake.registration('body'));
});

test('an unusable bundle leaves the previous policy enforcing and is reported', async () => {
  const h = await started();
  const result = h.app.applyPolicy({ policy_version: 'b-bad', bundle: 'not an object' });
  assert.equal(result.applied, false);
  assert.equal(h.app.policy.snapshot().policy_version, 'bundle-1', 'the previous bundle is retained');
  assert.ok(h.app.health.counters.snapshot().counters.errors >= 1);
});

// ── the blocking capability ─────────────────────────────────────────────────────────────────
test('with webRequestBlocking granted, both lanes register as blocking (the deployed case)', async () => {
  const h = await started();
  assert.deepEqual(h.fake.registration('metadata').extra, ['blocking'], 'a force-installed extension can block');
  assert.deepEqual(h.fake.registration('body').extra, ['blocking', 'requestBody']);
  assert.equal(h.app.blockingAvailable, true);
  assert.equal(h.app.lanes.enforcement, 'blocking');
  assert.equal(h.app.reportHealth().enforcement, 'blocking');
});

test('without the grant, the lanes register WITHOUT blocking and observation still works', async () => {
  // The state of any install not force-installed by policy: a blocking registration would be
  // accepted and never invoked, so the extension would observe nothing.
  const h = createHarness();
  h.fake.state.blockingGranted = false;
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { version: 'b1', tenant_default_mode: 'm1' } });
  await settle();

  assert.equal(h.app.blockingAvailable, false);
  assert.equal(h.app.lanes.enforcement, 'observation_only');
  assert.deepEqual(h.fake.registration('metadata').extra, [], 'no `blocking`: asking for it makes the listener inert');
  assert.deepEqual(h.fake.registration('body').extra, ['requestBody'], 'requestBody is orthogonal and still needed');
  assert.equal(h.app.reportHealth().enforcement, 'observation_only', 'reported, never silent');
  assert.ok(h.app.health.counters.snapshot().errors_by_code.enforcement_unavailable >= 1);

  // And observation actually happens.
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await settle();
  assert.equal(h.app.health.counters.snapshot().counters.observed, 1, 'never less inspection, silently');
  const obs = h.core.lastObservation();
  assert.equal(obs.decision.action, 'logged');
  assert.equal(obs.decision.decided_locally, true);
});

test('a capability that cannot be read defaults to blocking, never to lost collection', async () => {
  // The deployed case is the force-installed one, and observation does not depend on the answer,
  // so an unanswerable probe must not silently disable enforcement.
  const h = createHarness();
  delete h.fake.chrome.permissions;
  await h.app.start();
  assert.equal(h.app.blockingAvailable, true);
  assert.equal(h.app.lanes.enforcement, 'blocking');
});

test('the body lane keeps its filter when blocking is unavailable', async () => {
  const h = createHarness();
  h.fake.state.blockingGranted = false;
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { version: 'b1', tenant_default_mode: 'm1' } });
  await settle();
  assert.deepEqual(h.fake.registration('body').urls, ['<all_urls>']);
  assert.deepEqual(h.fake.registration('body').extra, ['requestBody']);
  assert.equal(h.app.lanes.bodyLaneInstalled, true);
});

// ── the emit path, and the window the browser exposed ───────────────────────────────────────

/** A port that accepts a post and silently discards it: exactly what Chrome does before the
 *  disconnect for a missing host has been delivered. It never acks and never disconnects. */
function blackHolePort() {
  return {
    postMessage: () => {},
    onMessage: { addListener: () => {} },
    onDisconnect: { addListener: () => {} },
    disconnect: () => {},
  };
}

test('an observation emitted while the channel looks up but cannot deliver ends up QUEUED, not counted emitted', async () => {
  // `connectNative()` succeeded, so `isConnected()` is true, and the browser has not delivered the
  // disconnect. A direct send in this window would post into a dead port and count `emitted` for an
  // observation that was lost.
  const h = createHarness({ nativeTimeoutMs: 150 });
  h.fake.chrome.runtime.connectNative = () => blackHolePort();
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { version: 'b1', tenant_default_mode: 'm1' } });
  await settle();

  assert.equal(h.app.native.isConnected(), true, 'the channel looks up, which is the trap');

  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await waitFor(() => h.app.queue.size() > 0, { label: 'the observation to be queued' });

  const c = h.app.health.counters.snapshot().counters;
  assert.equal(c.observed, 1);
  assert.equal(c.emitted, 0, 'nothing was acknowledged, so nothing is counted as emitted');
  assert.equal(h.app.queue.size(), 1, 'and the observation is held, not lost');
  assert.equal(observationFrames(h.core).length, 0, 'the black hole swallowed it: no frame reached a core');

  // Once the send gives up, the entry is still there for the next drain — nothing was consumed.
  await settle(30);
  assert.equal(h.app.queue.size(), 1, 'a failed send leaves the entry in place');
  assert.equal(h.app.health.counters.snapshot().counters.emitted, 0);
});

test('an observation leaves the queue only when capture-core acks it', async () => {
  // The other half: with a real channel the entry is enqueued, sent, acked, and only then removed
  // and counted. `emitted` therefore means "accepted by capture-core".
  const h = await started();
  await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
  await waitFor(() => h.core.observations().length === 1, { label: 'the frame to reach capture-core' });
  await waitFor(() => h.app.queue.size() === 0, { label: 'the ack to remove the entry' });
  assert.equal(h.app.queue.stats().delivered_total, 1);
  assert.equal(h.app.health.counters.snapshot().counters.emitted, 1, 'counted when acked, not when posted');
  assert.equal(h.app.queue.stats().dropped_total, 0, 'and nothing was dropped on the way');
});

test('a burst coalesces into at most one drain in flight, and every entry is acked', async () => {
  const h = await started();
  for (let i = 0; i < 5; i++) {
    await h.fake.drive(
      'body',
      chromeRequest({ requestId: `burst${i}`, url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }),
    );
  }
  await waitFor(() => h.core.observations().length === 5, { label: 'all five frames to arrive' });
  await waitFor(() => h.app.queue.size() === 0, { label: 'all five acks' });

  // The claim is about *acks*, not sends: an entry leaves the queue only because capture-core
  // accepted it, so the ack count and the delivery count must agree exactly, and no frame may be
  // sent twice by overlapping drains.
  assert.equal(h.app.queue.stats().delivered_total, 5, 'five acks removed five entries');
  assert.equal(h.app.health.counters.snapshot().counters.emitted, 5, 'and `emitted` counts acks');
  assert.equal(h.core.observations().length, 5, 'exactly five frames arrived: no duplicate sends');
  assert.equal(new Set(h.core.observations().map((o) => o.client_id)).size, 5, 'and five distinct observations');
  assert.equal(h.app.queue.stats().dropped_total, 0);
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

test('an automation marker raises the context evidence but is never required', async () => {
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
