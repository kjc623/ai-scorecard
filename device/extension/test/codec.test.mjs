/**
 * test/codec.test.mjs — the strict decode rule and the byte primitives the digest depends on.
 *
 * The property under test: the bytes the digest is taken over are the bytes the browser sent, on
 * both the UTF-8 and the binary path, with no replacement characters anywhere. A lossy decode
 * changes the digest, and so `dedup_key` and dedup across routes.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';

import {
  base64ToBytes,
  bytesToBase64,
  bytesToHex,
  concatBytes,
  decodeBody,
  sha256Prefixed,
  toBytes,
  utf8Strict,
} from '../src/codec.js';
import { normaliseBody, rawByteLength } from '../src/request-body.js';
import { INVALID_UTF8, LATIN1_TEXT, toArrayBuffer } from '../test-support/fake-chrome.mjs';
import { fakeCrypto } from '../test-support/harness.mjs';

const encoder = new TextEncoder();

test('strict UTF-8 decode succeeds on valid text and returns the exact characters', () => {
  const bytes = encoder.encode('{"prompt":"héllo — 日本語"}');
  const r = utf8Strict(bytes);
  assert.equal(r.ok, true);
  assert.equal(r.text, '{"prompt":"héllo — 日本語"}');
});

test('strict UTF-8 decode FAILS on invalid bytes rather than substituting U+FFFD', () => {
  const r = utf8Strict(INVALID_UTF8);
  assert.equal(r.ok, false);
  assert.equal(r.reason, 'invalid_utf8');

  // The anti-property: a non-fatal decode would produce a string containing U+FFFD. If this ever
  // becomes `ok: true`, every digest taken over it is wrong.
  const lossy = new TextDecoder('utf-8', { fatal: false }).decode(INVALID_UTF8);
  assert.ok(lossy.includes('\uFFFD'), 'the lossy decoder must be the one that mangles it');
  assert.notEqual(lossy.length, 0);
});

test('decodeBody falls back to binary and keeps the bytes byte-identical', () => {
  const d = decodeBody(LATIN1_TEXT);
  assert.equal(d.encoding, 'binary');
  assert.equal(d.text, null);
  assert.equal(d.reason, 'invalid_utf8');
  assert.deepEqual([...d.bytes], [...LATIN1_TEXT], 'the bytes must survive the failed decode');
});

test('UTF-8 and binary paths hash the same input bytes → one digest, so dedup can collapse them', async () => {
  const crypto = fakeCrypto();
  const utf8Bytes = encoder.encode('{"prompt":"same bytes"}');
  // A payload that is valid UTF-8 through one route and arrives labelled binary through another
  // must still produce one digest: the bytes are what is hashed, never the decoded string.
  const a = await sha256Prefixed(crypto, utf8Bytes);
  const b = await sha256Prefixed(crypto, new Uint8Array(utf8Bytes));
  assert.equal(a, b);
  assert.match(a, /^sha256:[0-9a-f]{64}$/);

  const invalid = await sha256Prefixed(crypto, INVALID_UTF8);
  const alsoInvalid = await sha256Prefixed(crypto, decodeBody(INVALID_UTF8).bytes);
  assert.equal(invalid, alsoInvalid, 'a binary-labelled payload hashes identically to its bytes');
});

test('sha256 matches the platform implementation', async () => {
  const crypto = fakeCrypto();
  const bytes = encoder.encode('shadow ai capture');
  const mine = await sha256Prefixed(crypto, bytes);
  const platform = bytesToHex(new Uint8Array(await webcrypto.subtle.digest('SHA-256', bytes)));
  assert.equal(mine, `sha256:${platform}`);
});

test('base64 round-trips every byte value and every length modulo 3', () => {
  for (let len = 0; len <= 34; len++) {
    const bytes = new Uint8Array(len);
    for (let i = 0; i < len; i++) bytes[i] = (i * 37 + 11) & 0xff;
    const round = base64ToBytes(bytesToBase64(bytes));
    assert.deepEqual([...round], [...bytes], `length ${len} must round-trip`);
  }
});

test('rawByteLength sums a chunked raw body without joining it', () => {
  const body = { raw: [{ bytes: toArrayBuffer('abc') }, { bytes: toArrayBuffer('de') }] };
  assert.equal(rawByteLength(body), 5);
  assert.equal(rawByteLength({}), 0);
  assert.equal(rawByteLength(null), 0);
});

test('normaliseBody reports the whole size but holds only the prefix when over cap', () => {
  const big = new Uint8Array(1000).fill(0x61);
  const body = normaliseBody({ raw: [{ bytes: big.buffer }] }, { capBytes: 64 });
  assert.equal(body.source, 'over_cap');
  assert.equal(body.size, 1000, 'size reports the payload as sent');
  assert.equal(body.bytes.byteLength, 64, 'only the prefix is materialised');
  assert.equal(body.truncated_reason, 'body_over_cap');
  // The prefix is still decoded strictly: its encoding is a fact about the payload the caller has
  // to report, and labelling valid UTF-8 as binary would be its own small lie.
  assert.equal(body.decode.encoding, 'utf8');
  assert.equal(body.decode.text.length, 64);
});

test('an over-cap prefix that cannot be decoded is labelled binary, not silently replaced', () => {
  const big = new Uint8Array(1000);
  big.set([0xc3, 0x28, 0xff, 0xfe], 0);
  const body = normaliseBody({ raw: [{ bytes: big.buffer }] }, { capBytes: 64 });
  assert.equal(body.source, 'over_cap');
  assert.equal(body.size, 1000);
  assert.equal(body.decode.encoding, 'binary');
  assert.equal(body.decode.text, null);
});

test('normaliseBody keeps a form body as key/value pairs and hands over its prompt field', () => {
  const body = normaliseBody({ formData: { prompt: ['hello'], model: ['gpt'] } });
  assert.equal(body.source, 'form');
  assert.deepEqual(body.form, { prompt: ['hello'], model: ['gpt'] });
  assert.equal(new TextDecoder().decode(body.bytes), 'hello', 'the content is what the person typed');
  assert.equal(body.size, 'prompt=hello&model=gpt'.length, 'the size is of the whole form');
  assert.equal(body.decode.encoding, 'utf8');
});

test('normaliseBody hands over a form without a prompt field whole', () => {
  for (const formData of [{ title: ['notes'], body: ['draft'] }, { prompt: ['  '], body: ['draft'] }]) {
    const body = normaliseBody({ formData });
    assert.equal(body.source, 'form');
    assert.equal(new TextDecoder().decode(body.bytes), Object.entries(formData).map(([k, v]) => `${k}=${v[0]}`).join('&'));
    assert.equal(body.size, body.bytes.byteLength);
  }
});

test('normaliseBody on an absent body is size 0 and not an error', () => {
  const body = normaliseBody(null);
  assert.equal(body.source, 'none');
  assert.equal(body.size, 0);
  assert.equal(body.bytes.byteLength, 0);
});

test('toBytes accepts every buffer shape Chrome might hand over', () => {
  assert.deepEqual([...toBytes('ab')], [0x61, 0x62]);
  assert.deepEqual([...toBytes(new Uint8Array([1, 2]))], [1, 2]);
  assert.deepEqual([...toBytes(new Uint8Array([1, 2]).buffer)], [1, 2]);
  assert.deepEqual([...toBytes(new Uint8Array([1, 2, 3]).subarray(1))], [2, 3]);
  assert.deepEqual([...toBytes(null)], []);
  assert.deepEqual([...concatBytes([new Uint8Array([1]), new Uint8Array([2, 3])])], [1, 2, 3]);
});
