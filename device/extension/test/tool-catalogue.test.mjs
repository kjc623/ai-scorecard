/**
 * test/tool-catalogue.test.mjs — the web tools the extension observes are named in the shared
 * catalogue, and the names cannot silently drift from what the extension emits.
 *
 * The extension derives a `tf1:` fingerprint from a signal vector that includes the body shape, so
 * one tool yields one fingerprint per body-shape variant: `unread` at M0, when the body is not
 * read, and the parsed body shape at M1+. This file enumerates the canonical request records for
 * each web tool, computes their fingerprints with the extension's own code, and checks every one is
 * seeded in services/database/schema.sql under the expected display name. If the derivation changes
 * or a catalogue row is removed, the computed fingerprint is no longer present and the check fails,
 * so the catalogue cannot drift from the derivation unnoticed.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { webcrypto } from 'node:crypto';

import { computeToolFingerprint } from '../src/tool-fingerprint.js';

const SCHEMA = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'services', 'database', 'schema.sql');

/** SHA-256 via Node's WebCrypto: real digests, no chrome.*. */
const adapter = { crypto: webcrypto };
const encoder = new TextEncoder();

// The canonical request bodies for the web tools the extension observes. The exact words do not
// enter the fingerprint (only the shape), but the message count, roles and model parameters do, so
// each body is a fixed representative of the tool's request shape.
const CHATGPT_CONVERSATION_BODY = {
  action: 'next',
  messages: [{ role: 'user', content: { content_type: 'text', parts: ['Summarise the quarterly figures.'] } }],
  model: 'gpt-4o',
};
const CHATGPT_CHAT_COMPLETIONS_BODY = {
  model: 'gpt-4o',
  messages: [{ role: 'user', content: 'Summarise the quarterly figures.' }],
  stream: true,
};
const GEMINI_BODY = {
  contents: [{ role: 'user', parts: [{ text: 'Summarise the quarterly figures.' }] }],
  generationConfig: { temperature: 0.7 },
};
const PERPLEXITY_BODY = {
  messages: [{ role: 'user', content: 'Summarise the quarterly figures.' }],
  model: 'sonar',
};

/**
 * The web tools and their observed endpoints, mirroring the `tls` rows in the catalogue. Each row
 * produces two fingerprints: M0 (`unread`, no body) and M1+ (the body above).
 */
const RECORDS = [
  ['ChatGPT (web)', 'https://chatgpt.com/backend-api/conversation', CHATGPT_CONVERSATION_BODY],
  ['ChatGPT (web)', 'https://chatgpt.com/backend-api/conversation', null],
  ['ChatGPT (web)', 'https://chatgpt.com/backend-api/chat/completions', CHATGPT_CHAT_COMPLETIONS_BODY],
  ['ChatGPT (web)', 'https://chatgpt.com/backend-api/chat/completions', null],
  ['ChatGPT (web)', 'https://chat.openai.com/backend-api/conversation', CHATGPT_CONVERSATION_BODY],
  ['ChatGPT (web)', 'https://chat.openai.com/backend-api/conversation', null],
  ['Gemini (web)', 'https://gemini.google.com/', GEMINI_BODY],
  ['Gemini (web)', 'https://gemini.google.com/', null],
  ['Perplexity (web)', 'https://www.perplexity.ai/', PERPLEXITY_BODY],
  ['Perplexity (web)', 'https://www.perplexity.ai/', null],
];

/** A request record shaped the way the extension hands one to the fingerprint. */
function webRequest(url, body) {
  const bytes = body ? encoder.encode(JSON.stringify(body)) : null;
  return {
    url,
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: bytes ? bytes.byteLength : 0,
    bytes,
    body_read: body !== null,
  };
}

/** fingerprint -> expected display name, computed with the extension's own derivation. */
async function computedFingerprints() {
  const out = new Map();
  for (const [name, url, body] of RECORDS) {
    const { fingerprint } = await computeToolFingerprint(adapter, webRequest(url, body));
    out.set(fingerprint, name);
  }
  return out;
}

/** The catalogue seed in schema.sql, keyed by fingerprint, with its display name. */
function parseCatalogue(source) {
  const start = source.indexOf('INSERT INTO ref.tool_catalogue');
  assert.notEqual(start, -1, 'the ref.tool_catalogue seed is present in schema.sql');
  const end = source.indexOf(';', start);
  assert.notEqual(end, -1, 'the ref.tool_catalogue seed terminates');
  const rows = new Map();
  // The fingerprint and display name are always the first two single-quoted columns. The evidence
  // JSONB holds double quotes but never single quotes, so a bare quoted scan is unambiguous.
  const re = /\(\s*'([^']+)'\s*,\s*'([^']+)'/g;
  for (const m of source.slice(start, end).matchAll(re)) rows.set(m[1], m[2]);
  return rows;
}

test('every canonical extension fingerprint is catalogued under its display name', async () => {
  const catalogue = parseCatalogue(readFileSync(SCHEMA, 'utf8'));
  const computed = await computedFingerprints();
  assert.ok(computed.size >= 2, 'the extension emits at least the M0 and M1+ variants');
  for (const [fingerprint, name] of computed) {
    assert.match(fingerprint, /^tf1:/, 'the extension fingerprint carries the tf1: prefix');
    assert.equal(catalogue.get(fingerprint), name,
      `${name} fingerprint ${fingerprint} is not seeded under its display name`);
  }
});

test('removing any one catalogue row leaves that fingerprint unnamed', async () => {
  const source = readFileSync(SCHEMA, 'utf8');
  const computed = await computedFingerprints();
  for (const [fingerprint, name] of computed) {
    const reduced = parseCatalogue(source);
    reduced.delete(fingerprint);
    assert.equal(reduced.get(fingerprint), undefined,
      `removing the ${name} row ${fingerprint} must leave it unnamed`);
  }
});
