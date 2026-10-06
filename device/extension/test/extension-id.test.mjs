import assert from 'node:assert/strict';
import { createHash, createPublicKey } from 'node:crypto';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { MANIFEST_PATH, extensionId, extensionIdFromKey } from '../tools/extension-id.mjs';

const manifest = JSON.parse(readFileSync(MANIFEST_PATH, 'utf8'));

test('manifest.json pins the extension id with a 2048-bit RSA public key', () => {
  const key = createPublicKey({ key: Buffer.from(manifest.key, 'base64'), format: 'der', type: 'spki' });
  assert.equal(key.asymmetricKeyType, 'rsa');
  assert.equal(key.asymmetricKeyDetails.modulusLength, 2048);
});

test("the id is Chromium's: SHA-256 of the key, first 16 bytes, hex digits mapped to a-p", () => {
  const hex = createHash('sha256').update(Buffer.from(manifest.key, 'base64')).digest('hex').slice(0, 32);
  const chromium = [...hex].map((c) => String.fromCharCode('a'.charCodeAt(0) + parseInt(c, 16))).join('');
  assert.equal(extensionId(), chromium);
  assert.equal(extensionIdFromKey(manifest.key), chromium);
  assert.match(chromium, /^[a-p]{32}$/);
});
