/**
 * tool-fingerprint.js — `tool_fingerprint = "tf2:" + base32(SHA-256(canonical({v: 2, host})))`.
 *
 * The fingerprint identifies the tool a request went to, so it is derived from the one signal that
 * is the same for every request to that tool: the destination host, lower-cased, without a port.
 * The path, the method, the content type and the body's shape vary from one prompt to the next and
 * belong to the predicate, which decides whether a request is a submission, never to the identity.
 * The `tf2:` prefix versions the derivation so it can change without silently re-keying history.
 *
 * The egress proxy derives its own `tls_` fingerprint from the host and path, so one tool seen
 * through both routes yields one fingerprint per route; the catalogue names both.
 */

import { sha256Hex } from './codec.js';
import { hostOf } from './predicate.js';

/** RFC 4648 base32, unpadded, lower-case. */
const B32 = 'abcdefghijklmnopqrstuvwxyz234567';

export function base32(bytes) {
  let out = '';
  let acc = 0;
  let bits = 0;
  for (const b of bytes) {
    acc = (acc << 8) | b;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += B32[(acc >> bits) & 31];
    }
  }
  if (bits > 0) out += B32[(acc << (5 - bits)) & 31];
  return out;
}

/** Canonical (sorted, shape-replaced) form: two observations of one tool give one string. */
export function canonical(vector) {
  return JSON.stringify(vector, replacerSorted(vector));
}

function replacerSorted(vector) {
  return function replacer(_key, value) {
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      const out = {};
      for (const k of Object.keys(value).sort()) out[k] = value[k];
      return out;
    }
    return value;
  };
}

/**
 * @param {import('./adapter.js').Adapter} adapter
 * @param {{url?: string, host?: string}} req  the request record; only its destination is read
 * @returns {Promise<{fingerprint: string, vector: object, canonical: string}>}
 */
export async function computeToolFingerprint(adapter, req) {
  const host = (hostOf(req?.url) || String(req?.host || '')).toLowerCase().replace(/:\d+$/, '');
  const vector = { v: 2, host };
  const canon = canonical(vector);
  const hex = await sha256Hex(adapter.crypto, new TextEncoder().encode(canon));
  const bytes = hexToBytes(hex);
  return { fingerprint: `tf2:${base32(bytes)}`, vector, canonical: canon };
}

function hexToBytes(hex) {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}
