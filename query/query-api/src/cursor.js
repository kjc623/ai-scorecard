// cursor.js — keyset pagination for list reads.
//
// A cursor is a base64url token over canonical JSON, signed with HMAC-SHA256 under the
// deployment's cursor key (SAC_CURSOR_KEY), so every replica accepts a cursor any replica issued.
// The signature alone is not what makes it safe: `dsl_hash` binds the token to the exact normalised
// query, `tenant` to the session, `exp` bounds its life, and `order` fixes which columns the keyset
// comparison may use. A cursor replayed against another question, another tenant, or after a
// deploy that changed an ordering key fails as `cursor_expired` instead of restarting silently at
// an offset.
//
// Only list reads are paged, and no list orders by a person, so a cursor never carries a subject
// reference.

import { createHmac, timingSafeEqual } from 'node:crypto';
import { CURSOR_TTL_MS } from './registry.js';
import { REASON, cursorExpired } from './errors.js';
import { canonicalJson } from './validate.js';

const VERSION = 'c1';

function keyOf(key) {
  if (Buffer.isBuffer(key) && key.length > 0) return key;
  if (typeof key === 'string' && key !== '') return Buffer.from(key, 'utf8');
  throw new Error('a cursor key is required');
}

function sign(payloadB64, key) {
  return createHmac('sha256', key).update(payloadB64).digest('base64url');
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
 * Encode a cursor.
 *
 * @param {{tenant: string, dslHash: string, order: ReadonlyArray<string>, upper: string, position: object}} input
 * @param {{key: Buffer|string, now?: number, ttlMs?: number}} options
 * @returns {string}
 */
export function encodeCursor(input, options) {
  const key = keyOf(options?.key);
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
  const body = Buffer.from(canonicalJson(payload), 'utf8').toString('base64url');
  return `${body}.${sign(body, key)}`;
}

/**
 * Decode and verify a cursor. Every failure is `cursor_expired` with the specific reason, because
 * the client's action is the same in all of them: restart at page one, told why.
 *
 * @param {string} token
 * @param {{tenant?: string, dslHash?: string, order?: ReadonlyArray<string>}} expected
 * @param {{key: Buffer|string, now?: number}} options
 * @returns {CursorPayload}
 */
export function decodeCursor(token, expected, options) {
  const key = keyOf(options?.key);
  const now = options.now ?? Date.now();
  if (typeof token !== 'string' || token.length === 0) {
    throw cursorExpired(REASON.CURSOR_EXPIRED, 'Cursor is empty.', {});
  }
  const dot = token.lastIndexOf('.');
  if (dot <= 0) {
    throw cursorExpired(REASON.CURSOR_UNKNOWN, 'Cursor is not one of ours.', {});
  }
  const body = token.slice(0, dot);
  const a = Buffer.from(token.slice(dot + 1), 'utf8');
  const b = Buffer.from(sign(body, key), 'utf8');
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
    // Neither tenant is echoed.
    throw cursorExpired(REASON.CURSOR_TENANT_MISMATCH, 'Cursor was issued for a different session.', {});
  }
  if (expected.dslHash !== undefined && payload.dsl_hash !== expected.dslHash) {
    throw cursorExpired(REASON.CURSOR_MISMATCH, 'Cursor belongs to a different query; restart from page one.', {});
  }
  if (expected.order !== undefined && canonicalJson(payload.order) !== canonicalJson(expected.order)) {
    throw cursorExpired(REASON.CURSOR_MISMATCH, 'The ordering key of this query has changed since the cursor was issued.', {
      cursor_order: payload.order,
      current_order: expected.order,
    });
  }
  return Object.freeze({ ...payload, order: Object.freeze([...payload.order]), pos: Object.freeze({ ...payload.pos }) });
}

/**
 * Page bookkeeping for a list read. `next_cursor` is null only when there is no next page, never
 * merely because the page came back short: rows erased mid-pagination make a page shorter without
 * ending the result set.
 *
 * @param {object} input
 * @param {ReadonlyArray<object>} input.rows     rows fetched, at most limit + 1
 * @param {number} input.limit
 * @param {((row: object) => string)|null} input.resume  the cursor that resumes after a row
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
