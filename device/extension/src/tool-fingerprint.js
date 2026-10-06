/**
 * tool-fingerprint.js — `tool_fingerprint = "tf1:" + base32(SHA-256(canonical(signal_vector)))`.
 *
 * The `tf1:` prefix versions the derivation so it can change without silently re-keying history.
 * The vector holds the destination, method, normalised path shape, content type, body shape,
 * message count bucket, role values, and whether model parameters or tool declarations are
 * present. It excludes the tab URL, request id, timestamp and automation marker.
 *
 * The derivation is extension-specific: the egress proxy derives its own `tls_` fingerprint from
 * the host and path, so one tool seen through both routes yields one fingerprint per route, not one
 * fingerprint overall. Because the body shape and the message-count and role signals read from the
 * body enter the vector, one tool also yields different fingerprints at M0 — when the body is not
 * read, so `body_shape` is `unread` — and at M1+, when it is. Recording `unread` rather than
 * omitting the field keeps that weaker observation explicit in the vector.
 */

import { sha256Hex } from './codec.js';
import { shapeVector } from './predicate.js';

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
 * @param {object} req  the same record `predicateRequest` takes, plus `body_read: boolean`
 * @returns {Promise<{fingerprint: string, vector: object, canonical: string}>}
 */
export async function computeToolFingerprint(adapter, req) {
  const vector = shapeVector(req);
  if (!req || req.body_read === false) vector.body_shape = 'unread';
  const canon = canonical(vector);
  const hex = await sha256Hex(adapter.crypto, new TextEncoder().encode(canon));
  const bytes = hexToBytes(hex);
  return { fingerprint: `tf1:${base32(bytes)}`, vector, canonical: canon };
}

function hexToBytes(hex) {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}
