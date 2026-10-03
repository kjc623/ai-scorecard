/**
 * tool-fingerprint.js — §8.1's `tool_fingerprint` as far as one browser route can honestly
 * compute it.
 *
 * `tool_fingerprint = "tf1:" + base32(SHA-256(canonical(signal_vector)))` (A10's versioned
 * prefix, so the derivation can change without silently re-keying history).
 *
 * §8.3 items 1 and 2 are the constraints that shape this file: the vector is canonicalised and
 * **route-specific signals are excluded** — "where a signal exists on only one route (the
 * extension sees the tab URL, the proxy sees the ClientHello) it may *resolve* a fingerprint but
 * never *define* one". So the vector here contains only signals both routes can produce:
 * destination, method, normalised path shape, content type, body shape family. The tab URL, the
 * request id, the timestamp and the automation marker are all absent by construction.
 *
 * At M0 the body is not read, so `body_shape` is `unread` and the fingerprint is weaker. That is
 * a coverage property of M0 (§11.2), not something to paper over: it is recorded in the vector
 * rather than hidden, and §8.3's device-local resolution cache is what keeps one tool from
 * producing two fingerprints.
 */

import { sha256Hex } from './codec.js';
import { shapeVector } from './predicate.js';

/** RFC 4648 base32, unpadded, lower-case: what §8.1's `base32(SHA-256(...))` means here. */
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

/** Identity for a WebSocket handshake (§7.4/E4): identity and volume only, never content. */
export function websocketVector(req) {
  const v = shapeVector(req);
  v.method = 'GET';
  v.upgrade = 'websocket';
  v.body_shape = 'handshake_none';
  return v;
}

/**
 * The device-local resolution cache of §8.3 item 3: "a cache: rebuildable from observations,
 * never authoritative, and its loss costs a re-derivation, not a wrong answer." It is bounded
 * because it is memory in a service worker that can be killed at any time.
 */
export function createResolutionCache({ max = 512 } = {}) {
  const map = new Map();
  function keyOf(route, destination) {
    return `${route}|${destination}`;
  }
  function get(route, destination) {
    const k = keyOf(route, destination);
    if (!map.has(k)) return null;
    const v = map.get(k);
    map.delete(k); // LRU touch
    map.set(k, v);
    return v;
  }
  function put(route, destination, fingerprint) {
    const k = keyOf(route, destination);
    if (map.has(k)) map.delete(k);
    map.set(k, fingerprint);
    while (map.size > max) map.delete(map.keys().next().value);
  }
  return { get, put, size: () => map.size };
}

function hexToBytes(hex) {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}
