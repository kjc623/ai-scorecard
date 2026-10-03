// cursor.js — §7 cursor pagination.
//
// Two encodings, and "the client cannot tell which it received" (§7.2):
//
//   * self-contained — a base64url token over canonical JSON, signed with HMAC-SHA256 under a
//     per-deployment key. Used when the ordering key contains no subject reference.
//   * server-side — a random opaque id resolving to a stored resume position, used when the
//     ordering key contains `user_ref`. A cursor is then subject-level data: not logged in
//     full, not echoed in errors, expired after 15 minutes.
//
// What makes the self-contained form safe is not the signature alone: it is that `dsl_hash`
// binds the token to the exact normalised query, `tenant` binds it to the session, `exp` bounds
// its life, and `order` fixes which columns the keyset comparison may use. A cursor replayed
// against another question, another tenant, or after a deploy that changed an ordering key
// fails as `cursor_expired` — "silent restart at an offset is the failure this avoids".

import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import { API_VERSION, CURSOR_TTL_MS, QUERY_VERSION } from './registry.js';
import { REASON, cursorExpired } from './errors.js';
import { canonicalJson } from './validate.js';

const VERSION = 'c1';

/** A per-process fallback key. Deployments are expected to set QUERY_CURSOR_HMAC_KEY. */
let fallbackKey = null;

/**
 * @param {Buffer|string} [key]
 * @returns {Buffer}
 */
export function resolveKey(key) {
  if (key) return Buffer.isBuffer(key) ? key : Buffer.from(String(key), 'utf8');
  const fromEnv = process.env.QUERY_CURSOR_HMAC_KEY;
  if (fromEnv && fromEnv.length >= 32) return Buffer.from(fromEnv, 'utf8');
  if (!fallbackKey) fallbackKey = randomBytes(32);
  return fallbackKey;
}

function b64url(buf) {
  return Buffer.from(buf).toString('base64url');
}

function sign(payloadB64, key) {
  return b64url(createHmac('sha256', key).update(payloadB64).digest());
}

/**
 * @typedef {object} CursorPayload
 * @property {string} v          cursor encoding version
 * @property {string} tenant     the session's tenant; a cursor under another tenant is refused
 * @property {string} dsl_hash   sha256 of the canonical normalised query
 * @property {ReadonlyArray<string>} order  ordering key names, in order
 * @property {string} upper      snapshot_upper_bound: rows newer than this are excluded
 * @property {number} exp        epoch ms
 * @property {Record<string, unknown>} pos  the keyset position, keyed by ordering key name
 */

/**
 * Encode a self-contained cursor.
 * @param {object} input
 * @returns {string}
 */
export function encodeCursor(input, options = {}) {
  const key = resolveKey(options.key);
  const now = options.now ?? Date.now();
  /** @type {CursorPayload} */
  const payload = {
    v: VERSION,
    tenant: input.tenant,
    dsl_hash: input.dslHash,
    order: [...input.order],
    upper: input.upper,
    exp: now + (options.ttlMs ?? CURSOR_TTL_MS),
    pos: { ...input.position },
  };
  const body = b64url(Buffer.from(canonicalJson(payload), 'utf8'));
  return `${body}.${sign(body, key)}`;
}

/**
 * Decode and verify a self-contained cursor. Every failure mode is `cursor_expired` (§13) with
 * the specific reason, because the client's action is the same in all of them: restart at page
 * one, told why.
 *
 * @param {string} token
 * @param {object} expected
 * @returns {CursorPayload}
 */
export function decodeCursor(token, expected, options = {}) {
  const key = resolveKey(options.key);
  const now = options.now ?? Date.now();
  if (typeof token !== 'string' || token.length === 0) {
    throw cursorExpired(REASON.CURSOR_EXPIRED, 'Cursor is empty.', {});
  }
  const dot = token.lastIndexOf('.');
  if (dot <= 0) {
    throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor is not one of ours.', {});
  }
  const body = token.slice(0, dot);
  const signature = token.slice(dot + 1);
  const expectedSignature = sign(body, key);
  const a = Buffer.from(signature, 'utf8');
  const b = Buffer.from(expectedSignature, 'utf8');
  if (a.length !== b.length || !timingSafeEqual(a, b)) {
    throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor signature does not verify.', {});
  }
  let payload;
  try {
    payload = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
  } catch {
    throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor payload is not readable.', {});
  }
  if (!payload || typeof payload !== 'object' || payload.v !== VERSION) {
    throw cursorExpired(REASON.CURSOR_VERSION_MISMATCH, 'Cursor was issued by a different version of this service.', {
      cursor_version: payload?.v ?? null,
      served: VERSION,
    });
  }
  if (payload.exp <= now) {
    throw cursorExpired(REASON.CURSOR_EXPIRED, 'Cursor has expired; restart from page one.', { expired_at: new Date(payload.exp).toISOString() });
  }
  if (expected.tenant !== undefined && payload.tenant !== expected.tenant) {
    // Deliberately does not echo either tenant.
    throw cursorExpired(REASON.CURSOR_TENANT_MISMATCH, 'Cursor was issued for a different session.', {});
  }
  if (expected.dslHash !== undefined && payload.dsl_hash !== expected.dslHash) {
    throw cursorExpired(REASON.CURSOR_MISMATCH, 'Cursor belongs to a different query; restart from page one.', {});
  }
  if (expected.order !== undefined && canonicalJson(payload.order) !== canonicalJson(expected.order)) {
    // A deploy that changes an ordering key invalidates in-flight pagination explicitly (§2.3).
    throw cursorExpired(REASON.CURSOR_MISMATCH, 'The ordering key of this query has changed since the cursor was issued.', {
      cursor_order: payload.order,
      current_order: expected.order,
    });
  }
  return Object.freeze({ ...payload, order: Object.freeze([...payload.order]), pos: Object.freeze({ ...payload.pos }) });
}

/**
 * True when the ordering key carries a subject reference. §7.2: those cursors are server-side,
 * "never a client-held encoding of a subject reference".
 * @param {ReadonlyArray<string>} orderKeys
 */
export function isSubjectBearingOrder(orderKeys) {
  return orderKeys.includes('subject') || orderKeys.includes('subject_ref');
}

/**
 * The server-side resume-position store. In-memory by default: a single query-api replica's
 * cursors live for 15 minutes and a process restart is a legitimate `cursor_expired`. The
 * interface is `put`/`take` so a Redis-backed store can replace it without touching the
 * pagination contract.
 */
export function createCursorStore(options = {}) {
  const ttlMs = options.ttlMs ?? CURSOR_TTL_MS;
  const now = options.now ?? (() => Date.now());
  const randomBytesFn = options.randomBytes ?? randomBytes;
  /** @type {Map<string, {exp:number, resume:object}>} */
  const entries = new Map();

  function sweep() {
    const t = now();
    for (const [id, entry] of entries) if (entry.exp <= t) entries.delete(id);
  }

  return Object.freeze({
    put(resume) {
      sweep();
      const id = randomBytesFn(24).toString('base64url');
      entries.set(id, { exp: now() + ttlMs, resume });
      return id;
    },
    take(id) {
      sweep();
      const entry = entries.get(id);
      if (!entry) {
        throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor is unknown or has expired; restart from page one.', {});
      }
      if (entry.exp <= now()) {
        entries.delete(id);
        throw cursorExpired(REASON.CURSOR_EXPIRED, 'Cursor has expired; restart from page one.', {});
      }
      return entry.resume;
    },
    size() {
      sweep();
      return entries.size;
    },
  });
}

/**
 * Page bookkeeping for a list read. `next_cursor` is null only when there is no next page —
 * never when the page came back short (§7.4: a short page is not the end of the results).
 *
 * @param {object} input
 * @param {ReadonlyArray<object>} input.rows     rows fetched, at most limit + 1
 * @param {number} input.limit
 * @param {object|null} input.resume             how to resume after the last returned row
 * @returns {{rows: ReadonlyArray<object>, page: object}}
 */
export function paginate(input) {
  const limit = input.limit ?? input.rows.length;
  const probe = input.rows.length > limit;
  const rows = probe ? input.rows.slice(0, limit) : input.rows;
  const last = rows[rows.length - 1] ?? null;
  const nextCursor = probe && last && input.resume ? input.resume(last) : null;
  return Object.freeze({
    rows: Object.freeze(rows),
    page: Object.freeze({
      returned: rows.length,
      next_cursor: nextCursor,
      snapshot_upper_bound: input.snapshotUpper ?? null,
      newer_events_exist: input.newerEventsExist ?? false,
    }),
  });
}

export { API_VERSION, QUERY_VERSION };
