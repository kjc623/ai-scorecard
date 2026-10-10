import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync } from 'node:crypto';
import { cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { inflateRawSync } from 'node:zlib';

import { CRX_FILE, buildCrx, isExtensionVersion } from '../tools/build-crx.mjs';
import { extensionIdFromKey } from '../tools/extension-id.mjs';

const PKG = resolve(fileURLToPath(import.meta.url), '..', '..');

/** A copy of the extension whose manifest pins a throwaway key, and that key. */
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'sac-crx-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const { publicKey, privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
  const root = join(dir, 'extension');
  for (const entry of ['background', 'content', 'src', 'test']) cpSync(join(PKG, entry), join(root, entry), { recursive: true });
  const manifest = JSON.parse(readFileSync(join(PKG, 'manifest.json'), 'utf8'));
  manifest.key = publicKey.export({ type: 'spki', format: 'der' }).toString('base64');
  writeFileSync(join(root, 'manifest.json'), JSON.stringify(manifest));
  return { dir, root, key: privateKey.export({ type: 'pkcs8', format: 'pem' }), id: extensionIdFromKey(manifest.key) };
}

/** The entries of the ZIP archive inside a CRX3 file, name to bytes. */
function crxEntries(crx) {
  assert.equal(crx.subarray(0, 4).toString('latin1'), 'Cr24');
  assert.equal(crx.readUInt32LE(4), 3, 'CRX format version 3');
  const zip = crx.subarray(12 + crx.readUInt32LE(8));
  const eocd = zip.lastIndexOf(Buffer.from([0x50, 0x4b, 0x05, 0x06]));
  const entries = new Map();
  for (let p = zip.readUInt32LE(eocd + 16), n = zip.readUInt16LE(eocd + 10); n > 0; n--) {
    const [method, size, nameLen, extraLen, commentLen, offset] = [10, 20, 28, 30, 32, 42].map((o, i) => (i === 1 || i === 5 ? zip.readUInt32LE(p + o) : zip.readUInt16LE(p + o)));
    const name = zip.subarray(p + 46, p + 46 + nameLen).toString('utf8');
    const data = offset + 30 + zip.readUInt16LE(offset + 26) + zip.readUInt16LE(offset + 28);
    const raw = zip.subarray(data, data + size);
    entries.set(name, method === 8 ? inflateRawSync(raw) : raw);
    p += 46 + nameLen + extraLen + commentLen;
  }
  return entries;
}

const UPDATE_URL = 'https://console.example.invalid/v1/extension/updates.xml';

test('the CRX is signed under the pinned id and carries the release version and only what a browser loads', async (t) => {
  const { dir, root, key, id } = fixture(t);
  const out = join(dir, 'out');
  const result = await buildCrx({ keyPem: key, version: '2.5.7', updateUrl: UPDATE_URL, outDir: out, root });
  const crx = readFileSync(join(out, CRX_FILE));
  assert.deepEqual(result, { id, version: '2.5.7', file: CRX_FILE, sha256: createHash('sha256').update(crx).digest('hex'), size: crx.length });

  const entries = crxEntries(crx);
  const packaged = JSON.parse(entries.get('manifest.json'));
  assert.equal(packaged.version, '2.5.7');
  assert.equal(packaged.update_url, UPDATE_URL, 'an installed extension checks for updates at its own update_url');
  assert.ok(entries.has('background/service-worker.js') && entries.has('content/content-boot.js') && entries.has('src/native.js'));
  assert.deepEqual([...entries.keys()].filter((n) => n.startsWith('test/')), [], 'tests are not packaged');

  const again = await buildCrx({ keyPem: key, version: '2.5.7', updateUrl: UPDATE_URL, outDir: join(dir, 'again'), root });
  assert.equal(again.sha256, result.sha256, 'the same inputs give the same CRX');
});

test('a key that is not the private half of manifest.json\'s key is refused', async (t) => {
  const { dir, root } = fixture(t);
  const other = generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey.export({ type: 'pkcs8', format: 'pem' });
  await assert.rejects(buildCrx({ keyPem: other, updateUrl: UPDATE_URL, outDir: join(dir, 'out'), root }), /not manifest\.json's "key"/);
  await assert.rejects(buildCrx({ keyPem: undefined, updateUrl: UPDATE_URL, outDir: join(dir, 'out'), root }), /no signing key/);
});

test('a CRX without an absolute update URL is refused: the installed extension would never update', async (t) => {
  const { dir, root, key } = fixture(t);
  await assert.rejects(buildCrx({ keyPem: key, outDir: join(dir, 'out'), root }), /not an absolute http\(s\) URL/);
  await assert.rejects(buildCrx({ keyPem: key, updateUrl: 'console.example.invalid/updates.xml', outDir: join(dir, 'out'), root }), /not an absolute http\(s\) URL/);
});

test('versions follow Chromium\'s format', () => {
  for (const v of ['1', '0.1.0', '1.2.3.4', '65535.0.1']) assert.ok(isExtensionVersion(v), v);
  for (const v of ['', '1.2.3.4.5', '01.2', '1.65536', '1.2-beta', 'v1']) assert.ok(!isExtensionVersion(v), v);
});
