#!/usr/bin/env node
// Enable cli_shim.node_require on a sac-bundle-produced bundle.
//
// sac-bundle signs the policy bundle with a generated Ed25519 key but has no --node-require flag,
// and cli.shim only writes node-proxy.cjs + NODE_OPTIONS=--require when the bundle sets
// cli_shim.node_require. This re-signs the bundle (payload + node_require) under a fresh policy key
// and rewrites policy-key.pub, so capture-core verifies the exact bundle device-entry.sh points it at.
import { readFileSync, writeFileSync } from 'node:fs';
import { generateKeyPairSync, sign } from 'node:crypto';

const dir = process.argv[2];
if (!dir) {
  console.error('usage: node enable-node-require.mjs BUNDLE_DIR');
  process.exit(2);
}

const bundlePath = `${dir}/bundle.json`;
const envelope = JSON.parse(readFileSync(bundlePath, 'utf8'));
const payload = envelope.payload;
if (!payload || typeof payload !== 'object') {
  console.error('bundle.json has no payload object');
  process.exit(1);
}

payload.cli_shim = payload.cli_shim ?? {};
payload.cli_shim.node_require = true;

// The signature covers the payload bytes exactly as they are serialised here; the envelope embeds
// the payload as a raw JSON value, so the bytes the device verifies are the bytes we signed.
const payloadBytes = Buffer.from(JSON.stringify(payload), 'utf8');
const { publicKey, privateKey } = generateKeyPairSync('ed25519');
const signature = sign(null, payloadBytes, privateKey);

// The Ed25519 public key is the final 32 bytes of the SPKI DER; capture-core reads it as hex.
const spki = publicKey.export({ type: 'spki', format: 'der' });
const pubHex = spki.subarray(spki.length - 32).toString('hex');

writeFileSync(
  bundlePath,
  JSON.stringify({ key_id: envelope.key_id ?? 'policy-key-1', algorithm: 'ed25519', payload, signature: signature.toString('base64') }),
);
writeFileSync(`${dir}/policy-key.pub`, pubHex + '\n');

console.log(`node_require enabled; policy key ${pubHex}`);
