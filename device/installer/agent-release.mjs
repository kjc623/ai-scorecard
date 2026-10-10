// device/installer/agent-release.mjs - the signed statement an installed agent updates itself from
// (device/protocol/agentrelease.go): the Windows package's version, file, sha256 and size, signed
// with the release signing key (the classifier release's key, whose public half every package pins).

import { createPrivateKey, createPublicKey, sign, verify } from 'node:crypto';

export const AGENT_RELEASE_FILE = 'agent-release.json';

// DER prefixes that wrap a raw Ed25519 seed (PKCS#8) and public key (SPKI) for node:crypto.
const PKCS8_ED25519 = Buffer.from('302e020100300506032b657004220420', 'hex');
const SPKI_ED25519 = Buffer.from('302a300506032b6570032100', 'hex');

/** agent-release.json's text: the signed statement for a Windows package. */
export function signAgentRelease(seedHex, { version, file, sha256, size }) {
  const seed = Buffer.from(String(seedHex).trim(), 'hex');
  if (seed.length !== 32) throw new Error('the release signing key must be a 32-byte Ed25519 seed in hex');
  const key = createPrivateKey({ key: Buffer.concat([PKCS8_ED25519, seed]), format: 'der', type: 'pkcs8' });
  const payload = JSON.stringify({ type: 'agent_release', platform: 'windows-amd64', version, file, sha256, size });
  const signature = sign(null, Buffer.from(payload), key).toString('base64');
  // Assembled as text so the payload's bytes in the file are exactly the bytes signed.
  return `{"algorithm":"ed25519","payload":${payload},"signature":"${signature}"}\n`;
}

/**
 * The payload of a statement whose signature verifies under pubHex, or null. The signature covers
 * the payload's bytes as they appear in the file, so they are cut from the raw text, never re-encoded.
 */
export function verifyAgentRelease(raw, pubHex) {
  const m = /"payload":(\{[^{}]*\})/.exec(raw);
  const signature = JSON.parse(raw).signature;
  if (!m || typeof signature !== 'string') return null;
  const key = createPublicKey({ key: Buffer.concat([SPKI_ED25519, Buffer.from(pubHex, 'hex')]), format: 'der', type: 'spki' });
  return verify(null, Buffer.from(m[1]), key, Buffer.from(signature, 'base64')) ? JSON.parse(m[1]) : null;
}
