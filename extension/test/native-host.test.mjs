/**
 * native-host.test.mjs — the checks that are about the browser-side seam: the pinned extension
 * identity, and the host registration the extension needs to reach `capture-core`.
 *
 * Why this file exists, and why it is a *browser* seam rather than another unit test. Two things
 * stood between this repository and a connected native channel: nothing registered the host, and an
 * unpacked extension's id is derived from its checkout path, so `allowed_origins` had no stable id
 * to name. Both are now artefacts — a `key` in the manifest and `tools/native-host.*` — and an
 * artefact that no test reads is one rename away from being wrong. The rules asserted here are
 * Chromium's, and the one that matters most (`extensionIdFrom`) is checked against Chromium's own
 * algorithm rather than re-implemented and trusted.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, createPublicKey } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  HOST_NAME,
  LAUNCHER_FILE,
  MANIFEST_FILE,
  extensionIdFromManifest,
  findHostExe,
  hostArgs,
  hostManifest,
} from '../tools/native-host.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const PKG = resolve(HERE, '..');
const manifest = JSON.parse(readFileSync(join(PKG, 'manifest.json'), 'utf8'));

/** Chromium's rule, written independently of the module under test (extension_id.cc). */
function idPerChromium(keyBase64) {
  const digest = createHash('sha256').update(Buffer.from(keyBase64, 'base64')).digest().subarray(0, 16);
  let out = '';
  for (const b of digest) out += b.toString(16).padStart(2, '0');
  return out.replace(/[0-9a-f]/g, (c) => 'abcdefghijklmnop'[parseInt(c, 16)]);
}

test('the extension identity is pinned, so its id does not depend on the checkout path', () => {
  assert.equal(typeof manifest.key, 'string', 'without a key the id is derived from the directory and cannot be named by allowed_origins');
  const der = Buffer.from(manifest.key, 'base64');
  assert.ok(der.length > 200, 'the key must be a SubjectPublicKeyInfo DER, not an empty or truncated value');
  assert.equal(der[0], 0x30, 'DER must start with a SEQUENCE (0x30)');
  // Parse it rather than grep the bytes: a DER OID is binary, and looking for the ASCII string
  // "rsaEncryption" inside it is a check that could never pass for a real key.
  const key = createPublicKey({ key: der, format: 'der', type: 'spki' });
  assert.equal(key.asymmetricKeyType, 'rsa', 'the pinned key must be an RSA public key');
  assert.equal(key.asymmetricKeyDetails.modulusLength, 2048, 'the pinned key is 2048-bit; it is an identity, not a secret');
});

test('the id this repository reports is the id Chromium derives, byte for byte', () => {
  assert.equal(extensionIdFromManifest(), idPerChromium(manifest.key));
  const id = extensionIdFromManifest();
  assert.match(id, /^[a-p]{32}$/, 'a Chromium extension id is 32 characters from a-p');
});

test('the native host name is one string, spelled the same by the extension and the registration', () => {
  const nativeSrc = readFileSync(join(PKG, 'src', 'native.js'), 'utf8');
  assert.match(
    nativeSrc,
    new RegExp(`export const NATIVE_APP = '${HOST_NAME.replace(/\./g, '\\.')}'`),
    'a mismatch here is a channel that never connects and never says why',
  );
  assert.equal(hostManifest({ hostExe: 'x', extensionId: 'a'.repeat(32) }).name, HOST_NAME);
});

test('allowed_origins names exactly the extension this manifest will load as', () => {
  const m = hostManifest({ hostExe: 'C:\\tmp\\capture-core.exe', extensionId: extensionIdFromManifest() });
  assert.deepEqual(m.allowed_origins, [`chrome-extension://${extensionIdFromManifest()}/`]);
  // The trailing slash is required by Chromium; without it the origin does not match and the host
  // is refused with no useful diagnostic.
  assert.ok(m.allowed_origins[0].endsWith('/'), 'a Chromium origin always carries its trailing slash');
  assert.equal(m.type, 'stdio', 'native messaging is stdio or it is nothing');
  // `path` is the LAUNCHER, not the binary, and this is the defect the first connected run found:
  // Chromium launches a native host with no arguments, and `capture-core` refuses to start without
  // `--spool-dir`. Pointing `path` at the bare binary produced a host that exited immediately, which
  // the browser reported as "Can't find manifest for native messaging host" — naming the wrong
  // problem entirely. The flags live in the launcher; the binary is `_executable`.
  assert.equal(m.path, LAUNCHER_FILE);
  assert.equal(m._executable, 'C:\\tmp\\capture-core.exe', 'the launcher must launch a real binary');
  assert.ok(m._args.includes('--native-host'), 'and the launcher must pass the flags the binary needs');
});

test('the host manifest carries a name, a path and an origin — and nothing Chromium rejects', () => {
  const m = hostManifest({ hostExe: 'x', extensionId: 'a'.repeat(32) });
  assert.match(m.description, /\S/);
  // Chromium ignores unknown keys but rejects a manifest missing a required one. The keys that
  // matter are exactly these four; `_args` is documentation for a human reading the registry.
  for (const required of ['name', 'path', 'type', 'allowed_origins']) {
    assert.ok(required in m, `${required} is required by Chromium`);
  }
  assert.ok(JSON.parse(JSON.stringify(m)), 'the manifest must survive serialisation for the file Chromium reads');
});

test('the host runs in M0: no bundle, and identity present on every envelope', () => {
  const args = hostArgs('x');
  assert.ok(args.includes('--native-host'), 'the endpoint must be launched into its native-messaging mode');
  assert.equal(args.includes('--bundle'), false, 'no bundle is how §13.3 rule 5 resolves every destination to M0');
  for (const flag of ['--tenant-id', '--device-id', '--user-ref']) {
    assert.ok(args.includes(flag), `${flag} must be stamped: an envelope without identity is refused at ingest`);
  }
  assert.ok(args.includes('--spool-dir') && args.includes('--spool-key'));
  // The key must not live inside the spool directory: a key beside its own ciphertext is not
  // encryption at rest, which is the same rule the endpoint's own flags enforce.
  const spoolDir = args[args.indexOf('--spool-dir') + 1];
  const spoolKey = args[args.indexOf('--spool-key') + 1];
  assert.equal(spoolKey.startsWith(spoolDir + sep), false, 'the spool key must be outside the spool directory');
});

test('the host manifest is written under a disposable, ignored directory', () => {
  assert.match(MANIFEST_FILE.replace(/\\/g, '/'), /\/\.tools\/tmp\//, 'run state belongs in .tools/tmp, never in the package');
  assert.ok(MANIFEST_FILE.endsWith(`${HOST_NAME}.json`), 'one file per host name, so a human can find it');
  assert.equal(findHostExe('definitely-not-here.exe'), null, 'a missing binary is reported, not assumed');
});
