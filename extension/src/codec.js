/**
 * codec.js — byte, text and digest primitives. Pure; no chrome.*.
 *
 * A body is decoded with a **fatal** UTF-8 decoder; on failure the payload is binary and the raw
 * bytes are kept. It is never decoded with replacement characters: U+FFFD substitution would
 * change the bytes the digest is taken over, and so `content_digest` and cross-route dedup.
 */

import { ExtError } from './adapter.js';

const FATAL_DECODER = new TextDecoder('utf-8', { fatal: true, ignoreBOM: false });

/** @param {ArrayBuffer|Uint8Array|string|null|undefined} input */
export function toBytes(input) {
  if (input == null) return new Uint8Array(0);
  if (typeof input === 'string') return new TextEncoder().encode(input);
  if (input instanceof Uint8Array) return input;
  if (input instanceof ArrayBuffer) return new Uint8Array(input);
  if (ArrayBuffer.isView(input)) return new Uint8Array(input.buffer, input.byteOffset, input.byteLength);
  throw new ExtError('internal_error', 'not a byte source');
}

export function byteLength(input) {
  return toBytes(input).byteLength;
}

/**
 * Strict UTF-8 decode.
 * @returns {{ok: true, text: string}|{ok: false, reason: 'invalid_utf8'}}
 */
export function utf8Strict(bytes) {
  const b = toBytes(bytes);
  try {
    return { ok: true, text: FATAL_DECODER.decode(b) };
  } catch {
    return { ok: false, reason: 'invalid_utf8' };
  }
}

/**
 * Decode strictly, and on failure fall back to "binary".
 * `encoding: 'binary'` means the payload is not text — it is carried as bytes and only
 * ever represented by its digest and size.
 * @returns {{encoding: 'utf8', text: string, bytes: Uint8Array} |
 *           {encoding: 'binary', text: null, bytes: Uint8Array, reason: 'invalid_utf8'}}
 */
export function decodeBody(bytes) {
  const b = toBytes(bytes);
  const strict = utf8Strict(b);
  if (strict.ok) return { encoding: 'utf8', text: strict.text, bytes: b };
  return { encoding: 'binary', text: null, bytes: b, reason: 'invalid_utf8' };
}

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

/** Standard base64, dependency-free (btoa does not exist in a service worker's module scope everywhere). */
export function bytesToBase64(input) {
  const b = toBytes(input);
  let out = '';
  for (let i = 0; i < b.length; i += 3) {
    const n = (b[i] << 16) | ((b[i + 1] || 0) << 8) | (b[i + 2] || 0);
    out += B64[(n >> 18) & 63] + B64[(n >> 12) & 63] +
      (i + 1 < b.length ? B64[(n >> 6) & 63] : '=') +
      (i + 2 < b.length ? B64[n & 63] : '=');
  }
  return out;
}

export function base64ToBytes(s) {
  const clean = String(s).replace(/[^A-Za-z0-9+/]/g, '');
  const out = new Uint8Array(Math.floor((clean.length * 3) / 4));
  let o = 0;
  let acc = 0;
  let bits = 0;
  for (const ch of clean) {
    acc = (acc << 6) | B64.indexOf(ch);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (acc >> bits) & 0xff;
    }
  }
  return out.subarray(0, o);
}

export function bytesToHex(input) {
  const b = toBytes(input);
  let s = '';
  for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, '0');
  return s;
}

export function concatBytes(parts) {
  const arrs = parts.map(toBytes);
  const total = arrs.reduce((n, a) => n + a.byteLength, 0);
  const out = new Uint8Array(total);
  let o = 0;
  for (const a of arrs) {
    out.set(a, o);
    o += a.byteLength;
  }
  return out;
}

/**
 * Hex SHA-256 over the bytes.
 * @param {Crypto} crypto WebCrypto from the adapter, never the global.
 */
export async function sha256Hex(crypto, bytes) {
  const buf = await crypto.subtle.digest('SHA-256', toBytes(bytes));
  return bytesToHex(new Uint8Array(buf));
}

/** `sha256:<64 lowercase hex>`, the digest form the event envelope admits. */
export async function sha256Prefixed(crypto, bytes) {
  return `sha256:${await sha256Hex(crypto, bytes)}`;
}
