#!/usr/bin/env node
// extension/tools/emit-frames.mjs
//
// Emits the golden native-messaging frames that endpoint/integration consumes. The frames are built
// by THIS package's own `observationBody()` and `frame()`, so what the cross-component harness reads
// is what the extension actually produces - not a fixture that agrees with the Go types by
// construction, which is how the content-encoding break survived every check on both sides.
//
//   node extension/tools/emit-frames.mjs                 # writes the golden files
//   node extension/tools/emit-frames.mjs <out-dir>       # writes them elsewhere
//   FRAME_OUT_DIR=<out-dir> node extension/tools/emit-frames.mjs
//
// Writes one JSON case per payload class into endpoint/integration/testdata/native/ by default.
//
// `buildCases()` is exported so this package's own suite can run the generator without writing
// anything, and without duplicating the case construction - duplication is exactly what let the
// content-encoding break through. `test/golden-frames.test.mjs` uses it to assert the six cases stay
// decodable, and that the committed golden files still match what this generator produces.
//
// The four payload classes are deliberate. Plain ASCII and binary are the easy ones. The third -
// a payload that *is* valid base64 - is the case a "does it error?" test cannot see: raw text that
// happens to decode is not an error, it is silently different bytes, and the digest then describes
// content nobody sent. The fourth is M0, where there must be no content at all.

import { mkdirSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { frame, observationBody, TYPE, ROUTE } from '../src/messages.js';

const HERE = dirname(fileURLToPath(import.meta.url));
const DEFAULT_OUT = resolve(HERE, '..', '..', 'endpoint', 'integration', 'testdata', 'native');

const digestOf = (bytes) => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
const bytesOf = (s) => new TextEncoder().encode(s);

/** One observation, framed exactly as the extension frames it. */
function caseFor({ name, why, fields, expects }) {
  const body = observationBody(fields);
  const msg = frame(TYPE.OBSERVATION, body, 'case-' + name);
  return { name: `${name}.json`, payload: { name, why, frame: msg, expects } };
}

/** The six cases, built by the extension's own frame code. No filesystem access. */
export function buildCases() {
  const cases = [];

  // 1. Plain ASCII text.
  {
    const text = 'Summarise the Q4 revenue deck for the board.';
    const bytes = bytesOf(text);
    cases.push(
      caseFor({
        name: 'text-ascii',
        why: 'the ordinary case: a user prompt that is not base64 and would fail outright if sent raw',
        fields: {
          client_id: 'c-ascii',
          route: ROUTE.EXT_WEB_REQUEST,
          tool_fingerprint: 'genai.web.chat.v1:chatgpt',
          occurred_at: '2026-10-02T18:00:00.000Z',
          monotonic_offset_ms: 123456,
          size_bytes: bytes.length,
          has_content: true,
          content: text,
          content_digest: digestOf(bytes),
        },
        expects: {
          decodable: true,
          has_content: true,
          content_bytes: bytes.length,
          content_text: text,
          content_digest: digestOf(bytes),
        },
      }),
    );
  }

  // 2. A payload that is itself valid base64. The silent-corruption case.
  {
    const text = 'aGVsbG8gd29ybGQ=';
    const bytes = bytesOf(text);
    cases.push(
      caseFor({
        name: 'text-that-looks-like-base64',
        why:
          'raw text that is also valid base64: if this is sent unencoded it decodes WITHOUT error to ' +
          'different bytes ("hello world") while the digest describes what the user typed - the failure ' +
          'a decode-error test cannot see',
        fields: {
          client_id: 'c-b64look',
          route: ROUTE.EXT_WEB_REQUEST,
          tool_fingerprint: 'genai.web.chat.v1:chatgpt',
          occurred_at: '2026-10-02T18:00:01.000Z',
          monotonic_offset_ms: 123457,
          size_bytes: bytes.length,
          has_content: true,
          content: text,
          content_digest: digestOf(bytes),
        },
        expects: {
          decodable: true,
          has_content: true,
          content_bytes: bytes.length,
          content_text: text,
          content_digest: digestOf(bytes),
        },
      }),
    );
  }

  // 3. Non-ASCII UTF-8, where a byte-length mistake would also show up.
  {
    const text = 'Résumé — 日本語のテキスト — naïve café';
    const bytes = bytesOf(text);
    cases.push(
      caseFor({
        name: 'text-utf8-multibyte',
        why: 'multi-byte UTF-8: proves the encoding is over bytes, not code units, and that size_bytes counts octets',
        fields: {
          client_id: 'c-utf8',
          route: ROUTE.EXT_PAGE_CONTEXT,
          tool_fingerprint: 'genai.web.chat.v1:claude',
          occurred_at: '2026-10-02T18:00:02.000Z',
          monotonic_offset_ms: 123458,
          size_bytes: bytes.length,
          has_content: true,
          content: text,
          content_digest: digestOf(bytes),
        },
        expects: {
          decodable: true,
          has_content: true,
          content_bytes: bytes.length,
          content_text: text,
          content_digest: digestOf(bytes),
        },
      }),
    );
  }

  // 4. Binary that is not text at all - the path that already base64-encoded before the fix.
  {
    const bytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x01]);
    cases.push(
      caseFor({
        name: 'binary-undecodable',
        why: 'bytes that are not valid UTF-8: the binary path, which must survive a round trip unchanged',
        fields: {
          client_id: 'c-binary',
          route: ROUTE.EXT_PAGE_CONTEXT,
          tool_fingerprint: 'genai.web.chat.v1:claude',
          occurred_at: '2026-10-02T18:00:03.000Z',
          monotonic_offset_ms: 123459,
          size_bytes: bytes.length,
          has_content: true,
          content: bytes,
          content_is_binary: true,
          content_digest: digestOf(bytes),
        },
        expects: {
          decodable: true,
          has_content: true,
          content_bytes: bytes.length,
          content_is_binary: true,
          content_digest: digestOf(bytes),
        },
      }),
    );
  }

  // 5. M0: no content read at all, so no content field on the wire.
  {
    cases.push(
      caseFor({
        name: 'm0-no-content',
        why: 'M0: the mode forbids reading content, so the frame must carry no content field and the Go type must decode an absent one',
        fields: {
          client_id: 'c-m0',
          route: ROUTE.EXT_WEB_REQUEST,
          tool_fingerprint: 'genai.web.chat.v1:gemini',
          occurred_at: '2026-10-02T18:00:04.000Z',
          monotonic_offset_ms: 123460,
          size_bytes: 4096,
          has_content: false,
        },
        expects: { decodable: true, has_content: false, content_bytes: 0 },
      }),
    );
  }

  // 6. Over-cap: hashed and sized, emitted degraded, no bytes transferred.
  {
    cases.push(
      caseFor({
        name: 'over-cap-no-bytes',
        why: 'an over-cap body is sized and hashed and its classification is degraded; no content is sent',
        fields: {
          client_id: 'c-overcap',
          route: ROUTE.EXT_WEB_REQUEST,
          tool_fingerprint: 'genai.web.chat.v1:gemini',
          occurred_at: '2026-10-02T18:00:05.000Z',
          monotonic_offset_ms: 123461,
          size_bytes: 2_000_000,
          has_content: false,
          over_cap: true,
          degraded_reason: 'content_over_cap',
          content_digest: digestOf(new Uint8Array(0)),
        },
        expects: {
          decodable: true,
          has_content: false,
          content_bytes: 0,
          content_digest: digestOf(new Uint8Array(0)),
        },
      }),
    );
  }

  return cases;
}

/** Write the cases. Returns the paths written. */
export function emitCases(outDir = DEFAULT_OUT) {
  const cases = buildCases();
  mkdirSync(outDir, { recursive: true });
  const written = [];
  for (const c of cases) {
    const path = join(outDir, c.name);
    writeFileSync(path, JSON.stringify(c.payload, null, 2) + '\n', 'utf8');
    written.push(path);
  }
  return written;
}

export { DEFAULT_OUT };

// Only run as a script. Importing this module must have no filesystem side effect, so that a test
// can build the same frames without touching the Lead's golden directory.
const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  const outDir = process.argv[2] || process.env.FRAME_OUT_DIR || DEFAULT_OUT;
  const written = emitCases(outDir);
  console.log(`wrote ${written.length} golden frame(s) to ${outDir}`);
  for (const p of written) console.log(`  ${p.split(/[\\/]/).pop()}`);
}
