/**
 * test/content-roundtrip.test.mjs — the acceptance property for the defect the verifier found:
 *
 *   **a frame produced by the extension's own code must round-trip through device/protocol's Go
 *   type to byte-identical content with a matching digest.**
 *
 * Why the whole suite was blind to it, and why this file exists rather than an assertion somewhere
 * else: every other check compared *field names*. `contract.test.mjs` compares `json:"name"` tags;
 * the fake core used to check presence; the Go tests construct structs directly and never see JSON.
 * The encoding was nobody's job. So this file tests the encoding as such, and — where the Go
 * toolchain is available — compiles and runs a real Go consumer of the real type.
 *
 * The four payload classes are the ones the Lead named, and the third and fourth are the two a
 * naive "does it error?" test misses:
 *   1. plain ASCII text;
 *   2. non-ASCII UTF-8 (multi-byte sequences);
 *   3. binary bytes that are not valid UTF-8;
 *   4. **text that is itself valid base64** — the silent case. A raw (non-encoded) `content` would
 *      not fail here; it would decode to *different bytes* than the user typed while
 *      `content_digest` was taken over the original.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, writeFileSync, rmSync, cpSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { observationBody, encodeContent, decodeContentFrame, validateObservation } from '../src/messages.js';
import { sha256Prefixed } from '../src/codec.js';
import { fakeCrypto } from '../test-support/harness.mjs';
import { createHarness, settle } from '../test-support/harness.mjs';
import { chromeRequest } from '../test-support/fake-chrome.mjs';
import { CHAT_BODY } from '../test-support/fixtures.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
// This file is extension/test/, so three levels up is one above the repository root; two is the root.
const REPO_ROOT = resolve(HERE, '..', '..');
const CHAT_URL = 'https://chat.example-ai.invalid/v1/chat/completions';

/** Base64 as `encoding/json` reads a `[]byte`: strict, and a failure is an error, not a guess. */
function jsonByteField(s) {
  if (typeof s !== 'string' || s.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(s)) {
    return { ok: false, reason: 'illegal base64 data' };
  }
  const buf = Buffer.from(s, 'base64');
  if (buf.toString('base64') !== s) return { ok: false, reason: 'illegal base64 data' };
  return { ok: true, bytes: buf };
}

/** The four payload classes, each with the text a lossy or mis-encoded round trip would corrupt. */
const PAYLOADS = [
  {
    name: 'plain ASCII text',
    bytes: Buffer.from('Summarise the quarterly figures.'),
    expectBinary: false,
  },
  {
    name: 'non-ASCII UTF-8 (multi-byte sequences)',
    bytes: Buffer.from('Résumé — 四半期の数字, ünïcödé ✓'),
    expectBinary: false,
  },
  {
    name: 'binary bytes that are not valid UTF-8',
    bytes: Buffer.from([0xc3, 0x28, 0xff, 0xfe, 0x00, 0x80, 0x7f]),
    expectBinary: true,
  },
  {
    // The silent one. `Q2VydGlmaWVkIHNlY3JldA==` is valid base64 AND valid text; a raw `content`
    // would decode to `Certified secret` — different bytes from what was typed, while the digest
    // was taken over the base64 string itself.
    name: 'text that is itself valid base64 (the silent case)',
    bytes: Buffer.from('Q2VydGlmaWVkIHNlY3JldA=='),
    expectBinary: false,
  },
  {
    name: 'empty-ish whitespace and newlines',
    bytes: Buffer.from('  \n\t{"a":1}\n  '),
    expectBinary: false,
  },
];

test('the frame builder base64-encodes text and binary alike, and it decodes back byte-identically', async () => {
  const crypto = fakeCrypto();
  for (const p of PAYLOADS) {
    const digest = await sha256Prefixed(crypto, new Uint8Array(p.bytes));
    const body = observationBody({
      client_id: 'c1',
      route: 'ext.web_request',
      tool_fingerprint: 'tf1:x',
      occurred_at: '2026-10-02T00:00:00.000Z',
      monotonic_offset_ms: 1,
      size_bytes: p.bytes.byteLength,
      has_content: true,
      content: new Uint8Array(p.bytes),
      content_digest: digest,
      content_is_binary: p.expectBinary,
    });

    // 1. The wire field is a JSON string of base64 — what `[]byte` means to encoding/json.
    assert.equal(typeof body.content, 'string', `${p.name}: content is a string on the wire`);
    const decoded = jsonByteField(body.content);
    assert.equal(decoded.ok, true, `${p.name}: the wire value must be valid base64, or Go errors out`);
    assert.deepEqual([...decoded.bytes], [...p.bytes], `${p.name}: the decoded bytes are the bytes that were hashed`);

    // 2. The digest still describes those bytes. This is the property that was broken.
    assert.equal(
      await sha256Prefixed(crypto, new Uint8Array(decoded.bytes)),
      body.content_digest,
      `${p.name}: content_digest must match the digest of the decoded content`,
    );

    // 3. The frame is still a well-formed observation by the protocol's own rules.
    assert.equal(validateObservation(body), null, `${p.name}: the frame validates`);
  }
});

test('the silent case, stated as its own assertion: base64-looking text must not be reinterpreted', async () => {
  const crypto = fakeCrypto();
  const typed = Buffer.from('Q2VydGlmaWVkIHNlY3JldA==');
  const digest = await sha256Prefixed(crypto, new Uint8Array(typed));
  const body = observationBody({
    client_id: 'c1',
    route: 'ext.web_request',
    tool_fingerprint: 'tf1:x',
    occurred_at: '2026-10-02T00:00:00.000Z',
    monotonic_offset_ms: 1,
    size_bytes: typed.byteLength,
    has_content: true,
    content: new Uint8Array(typed),
    content_digest: digest,
  });

  const decoded = jsonByteField(body.content).bytes;
  assert.equal(decoded.toString('utf8'), 'Q2VydGlmaWVkIHNlY3JldA==', 'the user typed base64 text and that is what arrives');
  assert.notEqual(decoded.toString('utf8'), 'Certified secret', 'NOT the decoded meaning of that text');

  // And the counterexample this test exists to prevent: had `content` been the raw string, Go
  // would have decoded it to different bytes while content_digest described the original.
  const asIfRawText = jsonByteField(typed.toString('utf8')).bytes;
  assert.equal(asIfRawText.toString('utf8'), 'Certified secret');
  assert.notDeepEqual([...asIfRawText], [...typed], 'which is exactly the content-identity break');
  assert.notEqual(await sha256Prefixed(crypto, new Uint8Array(asIfRawText)), body.content_digest);
});

test('encodeContent is the single encoder, and it round-trips every byte value', () => {
  // The encoder accepts the three things a call site might hold — bytes, text, and an already
  // base64 string — so that no caller has a reason to bypass it.
  const bytes = new Uint8Array(256);
  for (let i = 0; i < 256; i++) bytes[i] = i;
  assert.deepEqual([...decodeContentFrame(encodeContent(bytes))], [...bytes], 'bytes');
  assert.deepEqual([...decodeContentFrame(encodeContent('héllo'))], [...Buffer.from('héllo')], 'text');
  assert.deepEqual([...decodeContentFrame(encodeContent('AAEC', { alreadyBase64: true }))], [0, 1, 2], 'an already-encoded value');
  assert.equal(encodeContent(new Uint8Array([0xc3, 0x28])), 'wyg=', 'binary encodes without a decode attempt');
});

test('an empty payload produces no content field rather than an empty one', () => {
  const body = observationBody({
    client_id: 'c1',
    route: 'ext.web_request',
    tool_fingerprint: 'tf1:x',
    occurred_at: '2026-10-02T00:00:00.000Z',
    monotonic_offset_ms: 1,
    size_bytes: 0,
    has_content: false,
  });
  assert.equal('content' in body, false);
  assert.equal(validateObservation(body), null);
});

test('the extension\'s own pipeline output round-trips: bytes in, identical bytes out, digest intact', async () => {
  const h = createHarness();
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'b1', bundle: { policy_version: 'b1', default_mode: 'm1' } });
  await settle();
  h.core.received.length = 0;

  for (const p of PAYLOADS) {
    // Build a payload that is structurally a submission, carrying the test bytes inside a string
    // value so the bytes travel the real path rather than being synthesised into a frame.
    const encoded = p.expectBinary
      ? Buffer.concat([
          Buffer.from('{"model":"m","messages":[{"role":"user","content":"'),
          p.bytes,
          Buffer.from('"}]}'),
        ])
      : Buffer.from(
          JSON.stringify({
            model: 'm',
            messages: [{ role: 'user', content: Buffer.from(p.bytes).toString('utf8') }],
          }),
        );

    h.core.emitted.length = 0;
    await h.fake.drive(
      'body',
      chromeRequest({ requestId: `rt-${p.name}`, url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: encoded }),
    );
    await settle(2);

    const obs = h.core.lastObservation();
    assert.ok(obs, `${p.name}: the pipeline emitted an observation`);
    const decoded = h.core.lastDecodedContent();
    assert.deepEqual([...decoded], [...encoded], `${p.name}: the frame decodes to exactly the bytes the browser sent`);
    assert.equal(
      await h.core.lastComputedDigest(),
      obs.content_digest,
      `${p.name}: the digest a consumer computes over the decoded content matches content_digest`,
    );
    if (p.expectBinary) {
      assert.equal(obs.content_is_binary, true, `${p.name}: marked binary`);
    }
  }
});

// ── the real Go type, when a toolchain is present ────────────────────────────────────────────

const GO_PRESENT = probeGo();

function probeGo() {
  try {
    const env = goEnv();
    const r = spawnSync(env.GO || 'go', ['version'], { encoding: 'utf8', env });
    return r.status === 0 ? r.stdout.trim() : null;
  } catch {
    return null;
  }
}

function goEnv() {
  return {
    ...process.env,
    GOCACHE: join(REPO_ROOT, '.tools', 'gocache'),
    GOPROXY: 'off',
    GOTOOLCHAIN: 'local',
    GOFLAGS: '-mod=mod',
    GO111MODULE: 'on',
    GOWORK: 'off',
    PATH: `${process.env.PATH}${process.platform === 'win32' ? ';' : ':'}${process.env.LOCALAPPDATA || ''}\\Programs\\Go\\bin`,
  };
}

test('a real Go consumer of device/protocol.ObservationMessage decodes the extension frame byte-identically', { skip: GO_PRESENT ? false : 'no Go toolchain on PATH: see the NOT VERIFIED note in README.md' }, async () => {
  // The strongest form of the check: the extension's own frame, fed to the real Go type through
  // encoding/json, with the consumer computing the digest itself.
  const tmp = mkdtempSync(join(tmpdir(), 'capture-roundtrip-'));
  try {
    const protocolDir = join(REPO_ROOT, 'endpoint', 'protocol');
    assert.ok(existsSync(join(protocolDir, 'native.go')), 'device/protocol must be present');
    cpSync(protocolDir, join(tmp, 'protocol'), { recursive: true });

    // A frame built by the extension's own code, not by the test.
    const crypto = fakeCrypto();
    const payloads = [];
    for (const p of PAYLOADS) {
      const digest = await sha256Prefixed(crypto, new Uint8Array(p.bytes));
      const body = observationBody({
        client_id: `c-${p.name}`,
        route: 'ext.web_request',
        tool_fingerprint: 'tf1:x',
        occurred_at: '2026-10-02T00:00:00.000Z',
        monotonic_offset_ms: 1,
        size_bytes: p.bytes.byteLength,
        has_content: true,
        content: new Uint8Array(p.bytes),
        content_digest: digest,
      });
      payloads.push({ name: p.name, expected_b64: Buffer.from(p.bytes).toString('base64'), frame: { type: 'observation', version: 1, id: `m-${payloads.length}`, body } });
    }
    writeFileSync(join(tmp, 'frames.json'), JSON.stringify(payloads), 'utf8');

    writeFileSync(
      join(tmp, 'main.go'),
      `package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/shadow-ai-capture/device/protocol"
)

func main() {
	raw, err := os.ReadFile("frames.json")
	if err != nil { panic(err) }
	var frames []struct {
		Name        string          \`json:"name"\`
		ExpectedB64 string          \`json:"expected_b64"\`
		Frame       protocol.NativeMessage \`json:"frame"\`
	}
	if err := json.Unmarshal(raw, &frames); err != nil { panic(err) }
	for _, f := range frames {
		var obs protocol.ObservationMessage
		if err := json.Unmarshal(f.Frame.Body, &obs); err != nil {
			fmt.Printf("FAIL\\t%s\\tdecode: %v\\n", f.Name, err)
			continue
		}
		got := hex.EncodeToString(obs.Content)
		want := ""
		for _, b := range mustB64(f.ExpectedB64) { want += fmt.Sprintf("%02x", b) }
		sum := sha256.Sum256(obs.Content)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		status := "OK"
		if got != want { status = "FAIL_BYTES" }
		if digest != obs.ContentDigest { status = "FAIL_DIGEST" }
		fmt.Printf("%s\\t%s\\tbytes=%d\\tdigest_match=%v\\n", status, f.Name, len(obs.Content), digest == obs.ContentDigest)
	}
}

func mustB64(s string) []byte {
	b, err := base64Decode(s)
	if err != nil { panic(err) }
	return b
}
`,
      'utf8',
    );

    // `base64.StdEncoding.DecodeString` without importing it twice.
    writeFileSync(
      join(tmp, 'b64.go'),
      `package main

import "encoding/base64"

func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
`,
      'utf8',
    );

    writeFileSync(
      join(tmp, 'go.mod'),
      // `device/protocol` is its own module (it has a go.mod), so the consumer reaches it by a
      // `replace` rather than by a relative import path. This is the real module and the real type.
      `module roundtrip

go 1.22

require github.com/shadow-ai-capture/device/protocol v0.0.0

replace github.com/shadow-ai-capture/device/protocol => ./protocol
`,
      'utf8',
    );

    const env = goEnv();
    const build = spawnSync('go', ['build', './...'], { cwd: tmp, encoding: 'utf8', env });
    assert.equal(build.status, 0, `go build failed:\n${build.stdout}\n${build.stderr}`);

    const run = spawnSync('go', ['run', '.'], { cwd: tmp, encoding: 'utf8', env });
    assert.equal(run.status, 0, `go run failed:\n${run.stdout}\n${run.stderr}`);

    const lines = run.stdout.trim().split('\n');
    assert.equal(lines.length, PAYLOADS.length, `expected one line per payload, got:\n${run.stdout}`);
    for (const line of lines) {
      assert.match(line, /^OK\t/, `Go consumer rejected a frame: ${line}`);
      assert.match(line, /digest_match=true/, `Go consumer's digest disagrees: ${line}`);
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
});
