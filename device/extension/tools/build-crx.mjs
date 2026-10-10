#!/usr/bin/env node
// Packages the extension as a signed CRX3, the file control-api serves for the Chrome and Edge
// force-install policy.
//
//   node tools/build-crx.mjs --key extension-signing.pem --version 1.4.0 \
//     --update-url https://<analyst-fqdn>/v1/extension/updates.xml --out dist
//   SAC_EXTENSION_SIGNING_KEY="<PEM>" node tools/build-crx.mjs --version 1.4.0 --update-url ... --out dist
//
// The signing key must be the private half of manifest.json's `key`. Chromium derives the extension
// id from the key that signs the CRX, and the native messaging host's allowed_origins and the
// force-install policy both name the id pinned by manifest.json, so a different key is refused.
// The packaged manifest carries --version (default: manifest.json's), which is what an update
// manifest compares. The packaged manifest also carries --update-url: the force-install policy's URL
// serves only the first install, and an installed extension checks for updates at its manifest's
// update_url (with none, at the browser's store, which does not know it). Prints
// {id, version, file, sha256, size} as JSON.

import { createHash, createPrivateKey, createPublicKey } from 'node:crypto';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import crx3 from 'crx3';

import { extensionIdFromKey } from './extension-id.mjs';

export const CRX_FILE = 'shadow-ai-capture.crx';
const PACKAGE_ROOT = resolve(fileURLToPath(import.meta.url), '..', '..');
// What a browser loads. Tests, tools and package metadata stay out of the CRX.
const PACKAGED = ['manifest.json', 'background', 'content', 'src'];
// A fixed ZIP timestamp makes the CRX a function of its inputs: the same sources, version and key
// give the same bytes and the same sha256.
const ZIP_TIME = Date.UTC(2024, 0, 1);

/** Chromium's version format: one to four dot-separated integers, each 0-65535, no leading zeros. */
export function isExtensionVersion(v) {
  return typeof v === 'string' && /^(0|[1-9]\d{0,4})(\.(0|[1-9]\d{0,4})){0,3}$/.test(v) && v.split('.').every((n) => Number(n) <= 65535);
}

function isUpdateUrl(u) {
  try {
    const { protocol } = new URL(u);
    return protocol === 'https:' || protocol === 'http:';
  } catch {
    return false;
  }
}

/**
 * Build <outDir>/shadow-ai-capture.crx from the extension at `root`, signed with `keyPem`.
 * @returns {Promise<{id: string, version: string, file: string, sha256: string, size: number}>}
 */
export async function buildCrx({ keyPem, version, updateUrl, outDir, root = PACKAGE_ROOT }) {
  const manifest = JSON.parse(readFileSync(join(root, 'manifest.json'), 'utf8'));
  const packagedVersion = version ?? manifest.version;
  if (!isExtensionVersion(packagedVersion)) throw new Error(`version ${packagedVersion} is not a Chromium extension version (1 to 4 integers, each 0-65535)`);
  if (!isUpdateUrl(updateUrl)) throw new Error(`update URL ${updateUrl} is not an absolute http(s) URL: pass --update-url https://<analyst-fqdn>/v1/extension/updates.xml`);
  if (!keyPem) throw new Error('no signing key: pass --key FILE or set SAC_EXTENSION_SIGNING_KEY');

  let publicKey;
  try {
    publicKey = createPublicKey(createPrivateKey(keyPem)).export({ type: 'spki', format: 'der' }).toString('base64');
  } catch (err) {
    throw new Error(`the signing key is not a PEM private key: ${err.message}`);
  }
  if (publicKey !== manifest.key) {
    throw new Error(
      `the signing key's public half is not manifest.json's "key": the CRX would install as ${extensionIdFromKey(publicKey)}, not ${extensionIdFromKey(manifest.key)}`,
    );
  }

  const work = mkdtempSync(join(tmpdir(), 'sac-crx-'));
  try {
    const stage = join(work, 'extension');
    for (const entry of PACKAGED) cpSync(join(root, entry), join(stage, entry), { recursive: true });
    writeFileSync(join(stage, 'manifest.json'), JSON.stringify({ ...manifest, version: packagedVersion, update_url: updateUrl }, null, 2) + '\n');
    const keyPath = join(work, 'signing.pem');
    writeFileSync(keyPath, keyPem, { mode: 0o600 });

    mkdirSync(outDir, { recursive: true });
    const crxPath = join(resolve(outDir), CRX_FILE);
    const info = await crx3([join(stage, 'manifest.json')], { keyPath, crxPath, forceDateTime: ZIP_TIME });
    const id = extensionIdFromKey(manifest.key);
    if (info.appId !== id) throw new Error(`crx3 signed the package as ${info.appId}, expected ${id}`);

    return {
      id,
      version: packagedVersion,
      file: CRX_FILE,
      sha256: createHash('sha256').update(readFileSync(crxPath)).digest('hex'),
      size: statSync(crxPath).size,
    };
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

function argValue(args, flag) {
  const i = args.indexOf(flag);
  return i === -1 ? undefined : args[i + 1];
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const keyFile = argValue(args, '--key');
  try {
    const keyPem = keyFile ? readFileSync(keyFile, 'utf8') : process.env.SAC_EXTENSION_SIGNING_KEY;
    const result = await buildCrx({ keyPem, version: argValue(args, '--version'), updateUrl: argValue(args, '--update-url'), outDir: argValue(args, '--out') ?? join(PACKAGE_ROOT, 'dist') });
    console.log(JSON.stringify(result, null, 2));
  } catch (err) {
    console.error(`build-crx: ${err.message}`);
    process.exit(1);
  }
}
