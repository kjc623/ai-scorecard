/**
 * request-body.js — normalises a chrome.webRequest payload, and caps what is held in memory.
 *
 * chrome.webRequest hands back two shapes: form encodings as `requestBody.formData` (parsed
 * key/value pairs) and everything else as `requestBody.raw` (an array of ArrayBuffer chunks). AI
 * submissions are the raw case in practice: a strict UTF-8 decode with a binary fallback.
 *
 * The bytes handed to the digest are the bytes the browser sent. Nothing is decoded with
 * replacement characters, so this route's digest equals the one the proxy route computes for the
 * same wire bytes, which is what cross-route dedup needs.
 */

import { concatBytes, decodeBody, toBytes } from './codec.js';

/** Default cap for a body the extension holds in memory. */
export const DEFAULT_BODY_CAP_BYTES = 1 << 20; // 1 MiB

/**
 * @typedef {object} NormalisedBody
 * @property {'none'|'form'|'raw'|'over_cap'} source
 * @property {number} size  byte length of the payload as sent
 * @property {Uint8Array} bytes  the exact bytes; empty for `over_cap` beyond `prefix`
 * @property {{encoding: 'utf8'|'binary', text: string|null}} decode
 * @property {Record<string, string[]>|null} form
 * @property {string|null} truncated_reason
 */

/** Sum the raw chunk array without joining it yet, so the cap can be applied before the copy. */
export function rawByteLength(requestBody) {
  const raw = requestBody && requestBody.raw;
  if (!Array.isArray(raw)) return 0;
  let n = 0;
  for (const part of raw) {
    if (part && part.bytes) n += part.bytes.byteLength || 0;
  }
  return n;
}

/** Flatten requestBody.raw into one Uint8Array. */
export function joinRaw(requestBody) {
  const raw = requestBody && requestBody.raw;
  if (!Array.isArray(raw) || raw.length === 0) return new Uint8Array(0);
  if (raw.length === 1 && raw[0] && raw[0].bytes) return toBytes(raw[0].bytes);
  return concatBytes(raw.filter((p) => p && p.bytes).map((p) => toBytes(p.bytes)));
}

/** Form data arrives parsed; flatten to name -> values. A bare string value is one value. */
export function normaliseForm(formData) {
  if (!formData || typeof formData !== 'object') return null;
  const out = {};
  for (const [name, values] of Object.entries(formData)) {
    const list = Array.isArray(values) ? values : [values];
    out[name] = list.map((v) => {
      if (typeof v === 'string') return v;
      if (v && typeof v === 'object' && typeof v.value === 'string') return v.value;
      return '';
    });
  }
  return out;
}

/** The `application/x-www-form-urlencoded` / `multipart/form-data` view of a form body. */
export function formToBytes(form) {
  const enc = new TextEncoder();
  const parts = [];
  for (const [name, values] of Object.entries(form || {})) {
    for (const v of values) parts.push(enc.encode(`${name}=${v}`));
  }
  return concatBytes(parts.map((p, i) => (i === 0 ? p : concatBytes([new Uint8Array([38]), p]))));
}

/**
 * Normalise a body under a cap.
 *
 * Over the cap the body is not read wholesale into memory: `prefix` holds only the first
 * `capBytes`, `size` reports the whole payload, and `truncated_reason` is set so the caller
 * reports a degraded observation rather than a clean one. Callers hash the bytes they hold.
 *
 * @param {{formData?: any, raw?: any}|null|undefined} requestBody
 * @param {{capBytes?: number}} [opts]
 * @returns {NormalisedBody}
 */
export function normaliseBody(requestBody, opts = {}) {
  const capBytes = Number.isFinite(opts.capBytes) ? opts.capBytes : DEFAULT_BODY_CAP_BYTES;

  const form = requestBody && requestBody.formData && typeof requestBody.formData === 'object'
    ? normaliseForm(requestBody.formData)
    : null;
  if (form) {
    const size = Object.entries(form).reduce(
      (n, [k, vs]) => n + vs.reduce((m, v) => m + byteLenOf(k) + byteLenOf(v) + 2, 0),
      0,
    );
    if (size > capBytes) {
      return {
        source: 'over_cap',
        size,
        bytes: new Uint8Array(0),
        prefix: new Uint8Array(0),
        decode: { encoding: 'binary', text: null },
        form,
        truncated_reason: 'form_over_cap',
      };
    }
    const bytes = formToBytes(form);
    const decode = decodeBody(bytes);
    return { source: 'form', size: bytes.byteLength, bytes, decode, form, truncated_reason: null };
  }

  const rawLen = rawByteLength(requestBody);
  if (rawLen === 0) {
    return {
      source: 'none',
      size: 0,
      bytes: new Uint8Array(0),
      decode: { encoding: 'utf8', text: '' },
      form: null,
      truncated_reason: null,
    };
  }

  if (rawLen > capBytes) {
    // Copy only the prefix; the whole body is never materialised. The prefix is still *decoded
    // strictly*, because its encoding is a fact about the payload the caller has to report: an
    // over-cap body that is valid UTF-8 must not be labelled binary, and one that is not must be.
    const prefix = joinRaw(requestBody).subarray(0, capBytes).slice();
    return {
      source: 'over_cap',
      size: rawLen,
      bytes: prefix,
      prefix,
      decode: decodeBody(prefix),
      form: null,
      truncated_reason: 'body_over_cap',
    };
  }

  const bytes = joinRaw(requestBody);
  const decode = decodeBody(bytes);
  return { source: 'raw', size: bytes.byteLength, bytes, decode, form: null, truncated_reason: null };
}

function byteLenOf(s) {
  return typeof s === 'string' ? new TextEncoder().encode(s).byteLength : 0;
}
