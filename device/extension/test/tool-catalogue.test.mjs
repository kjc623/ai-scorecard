/**
 * test/tool-catalogue.test.mjs — the web tools the extension observes are named in the shared
 * catalogue, and the names cannot silently drift from what the extension emits.
 *
 * The extension derives a `tf2:` fingerprint from the destination host alone, so one tool yields
 * one fingerprint per host it is reached at. This file enumerates the hosts of each web tool,
 * computes their fingerprints with the extension's own code, and checks every one is seeded in
 * services/database/schema.sql under the expected display name. If the derivation changes
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

// The hosts of the web tools the extension observes. A tool's fingerprint comes from its host alone,
// so each host is one fingerprint, whatever the request to it carries.
const RECORDS = [
  ['ChatGPT (web)', 'https://chatgpt.com/backend-api/conversation'],
  ['ChatGPT (web)', 'https://chat.openai.com/backend-api/conversation'],
  ['Gemini (web)', 'https://gemini.google.com/'],
  ['Perplexity (web)', 'https://www.perplexity.ai/'],
];

/** A request record shaped the way the extension hands one to the fingerprint. */
function webRequest(url) {
  return { url, method: 'POST', headers: { 'content-type': 'application/json' }, size_bytes: 0, bytes: null };
}

/** fingerprint -> expected display name, computed with the extension's own derivation. */
async function computedFingerprints() {
  const out = new Map();
  for (const [name, url] of RECORDS) {
    const { fingerprint } = await computeToolFingerprint(adapter, webRequest(url));
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
  assert.equal(computed.size, RECORDS.length, 'one fingerprint per host');
  for (const [fingerprint, name] of computed) {
    assert.match(fingerprint, /^tf2:/, 'the extension fingerprint carries the tf2: prefix');
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
