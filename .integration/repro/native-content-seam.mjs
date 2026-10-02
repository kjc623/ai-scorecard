// .integration/repro/native-content-seam.mjs
//
// Independently reproduces the extension -> capture-core wire frame for one observation, using
// the extension's own modules (no re-implementation), and writes it where the Go consumer can
// read it. The consumer is device/protocol's ObservationMessage, which is the declared seam type.
//
// Run: node .integration/repro/native-content-seam.mjs
// Then: cd .integration/repro/goconsume && go run . ../frame-text.json (etc.)

import { writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..', '..');

const messages = await import(new URL(`file://${join(ROOT, 'apps', 'capture-extension', 'src', 'messages.js').replaceAll('\\', '/')}`).href);
const { frame, observationBody, TYPE } = messages;
// The extension's own base64 helper, used by the attachment chunk path.
const sender = await import(new URL(`file://${join(ROOT, 'apps', 'capture-extension', 'src', 'attachments', 'sender.js').replaceAll('\\', '/')}`).href);
const { bytesToBase64 } = sender;

const base = {
  client_id: 'c-1',
  route: 'ext.web_request',
  tool_fingerprint: 'tf1:web-request',
  occurred_at: new Date(1759413600000).toISOString(),
  monotonic_offset_ms: 1234,
  size_bytes: 41,
  has_content: true,
};

// Case 1: the ordinary text payload. pipeline.js sends `body.decode.text` verbatim.
const text = 'Summarise the attached contract, please.';
writeFileSync(
  join(HERE, 'frame-text.json'),
  JSON.stringify(frame(TYPE.OBSERVATION, observationBody({ ...base, content: text, content_digest: 'sha256:' + 'a'.repeat(64) }))),
  'utf8',
);

// Case 2: a payload that happens to be valid base64. If the Go side base64-decodes it, the
// bytes it sees are not the bytes the user typed and the digest no longer matches them.
const looksBase64 = 'aGVsbG8gd29ybGQ=';
writeFileSync(
  join(HERE, 'frame-base64-looking.json'),
  JSON.stringify(frame(TYPE.OBSERVATION, observationBody({ ...base, content: looksBase64, content_digest: 'sha256:' + 'b'.repeat(64) }))),
  'utf8',
);

// Case 3: the binary payload, which pipeline.js base64-encodes before sending.
const bytes = new TextEncoder().encode('binary-ish payload');
writeFileSync(
  join(HERE, 'frame-binary.json'),
  JSON.stringify(frame(TYPE.OBSERVATION, observationBody({ ...base, content: bytesToBase64(bytes), content_is_binary: true, content_digest: 'sha256:' + 'c'.repeat(64) }))),
  'utf8',
);

console.log('wrote frame-text.json, frame-base64-looking.json, frame-binary.json');
console.log('frame-text.json body.content =', JSON.stringify(JSON.parse(JSON.stringify(frame(TYPE.OBSERVATION, observationBody({ ...base, content: text })))).body.content));
