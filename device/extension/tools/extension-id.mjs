// The extension id Chromium derives from a public key: SHA-256 over the SubjectPublicKeyInfo DER,
// the first 16 bytes, each hex digit mapped 0-f to a-p. manifest.json's `key` pins it, so the id the
// native messaging host allows and the id the force-install policy names are known before any build.

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

export const MANIFEST_PATH = join(dirname(fileURLToPath(import.meta.url)), '..', 'manifest.json');

/** The id for a base64 SubjectPublicKeyInfo DER, the form manifest.json's `key` holds. */
export function extensionIdFromKey(keyBase64) {
  const digest = createHash('sha256').update(Buffer.from(keyBase64, 'base64')).digest().subarray(0, 16);
  return [...digest].map((b) => 'abcdefghijklmnop'[b >> 4] + 'abcdefghijklmnop'[b & 15]).join('');
}

/** The pinned id of the extension in this repository. */
export function extensionId(manifestPath = MANIFEST_PATH) {
  const { key } = JSON.parse(readFileSync(manifestPath, 'utf8'));
  if (typeof key !== 'string' || key === '') throw new Error(`${manifestPath} has no "key"; the extension id cannot be pinned`);
  return extensionIdFromKey(key);
}
