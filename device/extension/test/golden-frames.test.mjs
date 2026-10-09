/**
 * test/golden-frames.test.mjs — the golden native-messaging frames `device/integration` decodes
 * through the real Go types.
 *
 * `tools/emit-frames.mjs` builds them with this package's own `observationBody()` and `frame()`, so
 * they cannot agree with the Go types merely by construction. Three properties:
 *
 *   1. the generator produces six cases, each decodable with a matching digest;
 *   2. the committed golden files still match what the generator produces (a drift check);
 *   3. importing the generator writes nothing; only the CLI writes, where it is told.
 *
 * The other direction is the `policy_bundle` frame capture-core answers a policy_sync with
 * (`POLICY_FRAME`, checked against capture-core's own answer in its native_test.go): the extension
 * applies it and resolves the mode, the seed hosts and a rule decision the way its `expects` says.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { buildCases, emitCases, DEFAULT_OUT, POLICY_FRAME } from '../tools/emit-frames.mjs';
import { base64ToBytes } from '../src/codec.js';
import { decideSync } from '../src/enforce.js';
import { createHarness } from '../test-support/harness.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const PKG = resolve(HERE, '..');
const GOLDEN_DIR = DEFAULT_OUT;

const digestOf = (bytes) => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
/** Base64 exactly as `encoding/json` reads a `[]byte`. */
const jsonByteField = (s) => Buffer.from(s, 'base64').toString('base64') === s;

test('the generator emits six cases, each with a frame the consumer can decode', () => {
  const cases = buildCases();
  assert.equal(cases.length, 6, 'the payload classes the seam must cover: ascii, base64-looking, utf8, binary, m0, over-cap');
  assert.deepEqual(
    cases.map((c) => c.name),
    [
      'text-ascii.json',
      'text-that-looks-like-base64.json',
      'text-utf8-multibyte.json',
      'binary-undecodable.json',
      'm0-no-content.json',
      'over-cap-no-bytes.json',
    ],
  );

  for (const { name, payload } of cases) {
    const frame = payload.frame;
    assert.equal(frame.type, 'observation', `${name}: the frame type is the protocol's`);
    assert.equal(frame.version, 1, `${name}: the protocol version is carried`);
    assert.ok(frame.id, `${name}: each frame carries its correlation id`);
    const body = frame.body;

    // The encoding property, checked the way a Go consumer checks it.
    if (payload.expects.content_bytes > 0) {
      assert.equal(typeof body.content, 'string', `${name}: content is a JSON string`);
      assert.ok(jsonByteField(body.content), `${name}: and it is valid base64, or encoding/json errors out`);
      const decoded = base64ToBytes(body.content);
      assert.equal(decoded.byteLength, payload.expects.content_bytes, `${name}: it decodes to the expected length`);
      assert.equal(digestOf(decoded), body.content_digest, `${name}: and its digest is the one on the frame`);
      if (payload.expects.content_text !== undefined) {
        assert.equal(Buffer.from(decoded).toString('utf8'), payload.expects.content_text, `${name}: byte-identical to what was sent`);
      }
    } else {
      assert.equal('content' in body, false, `${name}: no content field at all`);
    }

    assert.equal(body.has_content, payload.expects.has_content, `${name}: has_content as declared`);
    if (payload.expects.content_is_binary) {
      assert.equal(body.content_is_binary, true, `${name}: the binary marker is carried`);
    }
  }
});

test('the base64-looking case is the silent one: raw text would decode to different bytes without erroring', () => {
  // Sending `content` raw is the mutation this guards against: it fails here if the encoder is
  // ever removed.
  const c = buildCases().find((x) => x.name === 'text-that-looks-like-base64.json');
  const typed = 'aGVsbG8gd29ybGQ=';

  // What the frame sends, and what a consumer sees.
  const sent = base64ToBytes(c.payload.frame.body.content);
  assert.equal(Buffer.from(sent).toString('utf8'), typed, 'the user typed base64 text and that is what arrives');

  // What sending `content` raw would have produced: no error, different bytes, digest now describing
  // content nobody sent.
  const raw = Buffer.from(typed, 'base64');
  assert.equal(raw.toString('utf8'), 'hello world', 'which is why it is silent rather than loud');
  assert.notDeepEqual([...raw], [...sent]);
  assert.notEqual(digestOf(raw), c.payload.frame.body.content_digest, 'and the digest would not have described the bytes on the wire');

  // And the loud half, for completeness: ordinary ASCII sent raw is not valid base64 at all.
  const ascii = buildCases().find((x) => x.name === 'text-ascii.json');
  const rawAscii = base64ToBytes('Summarise the Q4 revenue deck for the board.');
  assert.notDeepEqual([...rawAscii], [...base64ToBytes(ascii.payload.frame.body.content)]);
});

test('importing the generator writes nothing: a test must not touch another component\'s tree', () => {
  // The generator's default output is device/integration's golden directory. Importing it builds
  // cases and stops; only the CLI writes.
  const before = existsSync(GOLDEN_DIR) ? readdirSync(GOLDEN_DIR).sort() : null;
  const cases = buildCases();
  assert.equal(cases.length, 6);
  const after = existsSync(GOLDEN_DIR) ? readdirSync(GOLDEN_DIR).sort() : null;
  assert.deepEqual(after, before, 'no file appeared or vanished by importing the module');
});

test('the CLI writes the same six cases to a directory it is told to use', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'emit-frames-'));
  try {
    const out = execFileSync(process.execPath, [join(PKG, 'tools', 'emit-frames.mjs'), tmp], { encoding: 'utf8' });
    assert.match(out, /wrote 6 golden frame\(s\)/);
    const written = readdirSync(tmp).sort();
    assert.equal(written.length, 6);
    assert.deepEqual(written.sort(), buildCases().map((c) => c.name).sort());
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
});

test('the committed golden files still match what the generator produces', { skip: existsSync(GOLDEN_DIR) ? false : 'device/integration/testdata is not present in this checkout' }, () => {
  const tmp = mkdtempSync(join(tmpdir(), 'emit-drift-'));
  try {
    emitCases(tmp);
    const produced = readdirSync(tmp).sort();
    const committed = readdirSync(GOLDEN_DIR).filter((f) => f.endsWith('.json')).sort();
    assert.deepEqual(committed, produced, 'the committed set must be exactly what the generator emits');

    for (const name of produced) {
      const a = readFileSync(join(tmp, name), 'utf8');
      const b = readFileSync(join(GOLDEN_DIR, name), 'utf8');
      assert.equal(
        b,
        a,
        `${name} has drifted from what tools/emit-frames.mjs produces. Re-run: node device/extension/tools/emit-frames.mjs`,
      );
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
});

test('the golden frames are the ones the device-side harness decodes', { skip: existsSync(GOLDEN_DIR) ? false : 'device/integration/testdata is not present' }, () => {
  // A shape check against the real files, so this suite notices if the golden set is replaced by
  // something hand-written that does not carry the extension's frame structure.
  for (const file of readdirSync(GOLDEN_DIR).filter((f) => f.endsWith('.json'))) {
    const payload = JSON.parse(readFileSync(join(GOLDEN_DIR, file), 'utf8'));
    assert.ok(payload.name, `${file}: names its case`);
    assert.ok(payload.why, `${file}: says why the case exists`);
    assert.equal(payload.frame.type, 'observation', `${file}: is an observation frame`);
    assert.equal(typeof payload.frame.body.has_content, 'boolean', `${file}: carries has_content`);
    assert.equal(typeof payload.frame.body.tool_fingerprint, 'string', `${file}: carries a tool fingerprint`);
    if (payload.frame.body.has_content) {
      assert.ok(jsonByteField(payload.frame.body.content), `${file}: content is valid base64`);
    }
  }
});

test('the policy_bundle frame capture-core sends resolves in the extension as the device resolves it', async () => {
  const { frame, expects } = JSON.parse(readFileSync(POLICY_FRAME, 'utf8'));
  assert.equal(frame.type, 'policy_bundle');
  assert.equal(frame.version, 1);
  for (const key of ['key_id', 'algorithm', 'payload', 'signature']) {
    assert.equal(key in frame.body.bundle, false, `the bundle is the decoded payload, not the signed envelope (${key})`);
  }

  const h = createHarness();
  await h.app.start();
  assert.equal(h.app.applyPolicy(frame.body).applied, true);
  const policy = h.app.policy;
  assert.equal(policy.snapshot().policy_version, expects.policy_version);
  for (const { tool_fingerprint, mode } of expects.modes) {
    assert.equal(policy.modeFor({ tool_fingerprint }).mode, mode, tool_fingerprint);
  }
  assert.equal(policy.needsCoreMode(), expects.asks_capture_core);
  assert.deepEqual(policy.discoverySets().seed, expects.seed_hosts);
  assert.deepEqual(h.fake.registration('body').urls, ['<all_urls>'], 'a tenant default that reads content opens the body lane');

  const d = decideSync({ bundle: policy.rules(), ...expects.decision.input, labels: [], labels_known: false });
  assert.equal(d.rule_id, expects.decision.rule_id);
  assert.equal(d.message, expects.decision.message);
  assert.equal(d.decision.action, { block: 'blocked', warn: 'warned', allow: 'logged' }[expects.decision.action]);
});
