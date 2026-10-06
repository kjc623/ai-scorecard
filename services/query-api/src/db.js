// db.js — the PostgreSQL pool, how it authenticates, and the tenant-scoped transaction.
//
// In Azure the server accepts only Microsoft Entra authentication: each new connection presents an
// access token for the container's managed identity as its password, fetched from the Container
// Apps identity endpoint and cached until five minutes before it expires. SAC_PG_PASSWORD, when
// set, is used instead (the local lab).
//
// Every tenant-scoped statement runs inside `inTransaction`, which binds the session tenant with
// `set_config('app.tenant_id', $1, true)`. Row-level security on every tenant table compares
// against that setting, and `true` makes it transaction-local, so a pooled connection never carries
// one request's tenant into the next.

import pg from 'pg';

/** The Entra resource PostgreSQL flexible server accepts tokens for. */
export const ENTRA_POSTGRES_RESOURCE = 'https://ossrdbms-aad.database.windows.net';
const IDENTITY_API_VERSION = '2019-08-01';
const TOKEN_REFRESH_MARGIN_MS = 5 * 60_000;
const TOKEN_FETCH_TIMEOUT_MS = 10_000;

/**
 * A password function for node-postgres that returns a managed-identity token, fetching a new one
 * only when the cached token is within five minutes of expiry. Concurrent connections share one
 * fetch.
 *
 * @param {{endpoint: string, header: string, clientId?: string}} identity
 * @param {{fetchImpl?: typeof fetch, now?: () => number}} [opts]
 * @returns {() => Promise<string>}
 */
export function entraTokenSource({ endpoint, header, clientId = '' }, { fetchImpl = globalThis.fetch, now = Date.now } = {}) {
  let cached = null;
  let pending = null;

  async function fetchToken() {
    const url = new URL(endpoint);
    url.searchParams.set('api-version', IDENTITY_API_VERSION);
    url.searchParams.set('resource', ENTRA_POSTGRES_RESOURCE);
    if (clientId) url.searchParams.set('client_id', clientId);
    const res = await fetchImpl(url, {
      headers: { 'X-IDENTITY-HEADER': header },
      signal: AbortSignal.timeout(TOKEN_FETCH_TIMEOUT_MS),
    });
    if (res.status !== 200) throw new Error(`the identity endpoint answered ${res.status} for ${ENTRA_POSTGRES_RESOURCE}`);
    const body = await res.json();
    if (typeof body?.access_token !== 'string' || body.access_token === '') {
      throw new Error('the identity endpoint returned no access token');
    }
    // expires_on is epoch seconds, as a string or a number. Anything unreadable is treated as a
    // ten-minute token, so a malformed answer costs a refetch rather than a stale password.
    const seconds = Number(body.expires_on);
    const expiresAt = Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : now() + 10 * 60_000;
    return { token: body.access_token, expiresAt };
  }

  return async function password() {
    if (cached && now() < cached.expiresAt - TOKEN_REFRESH_MARGIN_MS) return cached.token;
    pending ??= fetchToken().finally(() => {
      pending = null;
    });
    cached = await pending;
    return cached.token;
  };
}

// --- Result types -------------------------------------------------------------------------------
//
// Lossless or explicit: int8 and numeric become numbers only when the number is exactly the value
// that arrived, and otherwise stay the decimal text. A zoneless timestamp is read as UTC (every
// stored timestamp is UTC), and a date becomes UTC midnight, so neither depends on the zone the
// process runs in. 'infinity' and values outside the JavaScript Date range stay text.

const OID = Object.freeze({ int8: 20, date: 1082, timestamp: 1114, timestamptz: 1184, numeric: 1700 });

function parseInt8(text) {
  const value = Number(text);
  return Number.isSafeInteger(value) ? value : text;
}

/** A decimal in one normal form, so '1.10', '1.1' and '1.1e0' compare equal. */
function canonicalDecimal(text) {
  let rest = text.trim();
  let sign = '';
  if (rest.startsWith('+')) rest = rest.slice(1);
  if (rest.startsWith('-')) {
    sign = '-';
    rest = rest.slice(1);
  }
  const [mantissa, exponent = '0'] = rest.toLowerCase().split('e');
  const [integer = '0', fraction = ''] = mantissa.split('.');
  const digits = `${integer.replace(/^0+(?=\d)/, '')}${fraction}`;
  const stripped = digits.replace(/^0+/, '');
  if (stripped === '') return '0';
  const significant = stripped.replace(/0+$/, '');
  const shifted = Number(exponent) - fraction.length + (stripped.length - significant.length);
  return `${sign}${significant}e${shifted}`;
}

function parseNumeric(text) {
  const value = Number(text);
  return Number.isFinite(value) && canonicalDecimal(text) === canonicalDecimal(String(value)) ? value : text;
}

const DATE = /^(\d{4,})-(\d{2})-(\d{2})(?: (BC))?$/;
const TIMESTAMP = /^(\d{4,})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(?:([+-])(\d{2})(?::?(\d{2}))?(?::?(\d{2}))?)?(?: (BC))?$/;

/** Date.UTC maps years 0-99 into the twentieth century; setUTCFullYear does not. */
function utc(year, month, day, hour = 0, minute = 0, second = 0, milli = 0) {
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  date.setUTCHours(hour, minute, second, milli);
  return date;
}

function parseDate(text) {
  const m = DATE.exec(text);
  if (!m) return text;
  return utc(m[4] ? 1 - Number(m[1]) : Number(m[1]), Number(m[2]), Number(m[3]));
}

/** Microseconds beyond the millisecond are dropped, never rounded into the next millisecond. */
function parseTimestamp(text) {
  const m = TIMESTAMP.exec(text);
  if (!m) return text;
  const [, rawYear, month, day, hour, minute, second, fraction, sign, offHour, offMinute, offSecond, bc] = m;
  const milli = fraction ? Number(fraction.slice(0, 3).padEnd(3, '0')) : 0;
  const date = utc(bc ? 1 - Number(rawYear) : Number(rawYear), Number(month), Number(day), Number(hour), Number(minute), Number(second), milli);
  if (sign) {
    const offset = (Number(offHour) * 3600 + Number(offMinute ?? 0) * 60 + Number(offSecond ?? 0)) * 1000;
    date.setTime(date.getTime() + (sign === '-' ? offset : -offset));
  }
  return Number.isFinite(date.getTime()) ? date : text;
}

/** The type parsers this service reads results with. Everything else uses node-postgres's own. */
export const TYPES = (() => {
  const types = new pg.TypeOverrides();
  types.setTypeParser(OID.int8, parseInt8);
  types.setTypeParser(OID.numeric, parseNumeric);
  types.setTypeParser(OID.date, parseDate);
  types.setTypeParser(OID.timestamp, parseTimestamp);
  types.setTypeParser(OID.timestamptz, parseTimestamp);
  return types;
})();

/**
 * The connection pool.
 *
 * @param {object} cfg  `loadConfig().pg`
 * @param {object} [opts]
 * @param {number} [opts.max]          the connection ceiling
 * @param {typeof fetch} [opts.fetchImpl]  for the identity endpoint
 * @param {{warn: Function}} [opts.log]
 * @returns {import('pg').Pool}
 */
export function createPool(cfg, { max = 10, fetchImpl, log } = {}) {
  const pool = new pg.Pool({
    host: cfg.host,
    port: cfg.port,
    database: cfg.database,
    user: cfg.user,
    password: cfg.password !== '' ? cfg.password : entraTokenSource(cfg.identity, { fetchImpl }),
    // TLS verifies the server certificate and host name. There is no "encrypt without verifying".
    ssl: cfg.sslMode === 'disable' ? false : { rejectUnauthorized: true },
    application_name: 'query-api',
    types: TYPES,
    max,
    idleTimeoutMillis: 30_000,
    connectionTimeoutMillis: 10_000,
  });
  // An idle connection the server drops emits here. Without a listener it would crash the process;
  // the pool has already discarded the connection.
  pool.on('error', (error) => log?.warn?.('idle database connection failed', { error: error.message }));
  return pool;
}

const SCOPE_SQL = "SELECT set_config('app.tenant_id', $1, true), set_config('statement_timeout', $2, true), set_config('lock_timeout', $3, true)";

/**
 * Run `fn(client)` inside one transaction scoped to `tenant`, with a statement timeout and a lock
 * timeout of half that, so a read blocked behind a lock fails as a lock timeout rather than using
 * its whole budget. Commits when `fn` resolves and rolls back when it throws.
 *
 * @template T
 * @param {{query: Function}} client  a connection checked out of the pool
 * @param {{tenant: string, statementTimeoutMs: number}} scope
 * @param {(client: object) => Promise<T>} fn
 * @returns {Promise<T>}
 */
export async function inTransaction(client, { tenant, statementTimeoutMs }, fn) {
  if (typeof tenant !== 'string' || tenant === '') throw new Error('a tenant-scoped transaction needs a tenant');
  if (!Number.isInteger(statementTimeoutMs) || statementTimeoutMs <= 0) throw new Error('a transaction needs a statement timeout');
  await client.query('BEGIN');
  try {
    await client.query(SCOPE_SQL, [tenant, `${statementTimeoutMs}ms`, `${Math.max(1, Math.floor(statementTimeoutMs / 2))}ms`]);
    const result = await fn(client);
    await client.query('COMMIT');
    return result;
  } catch (error) {
    try {
      await client.query('ROLLBACK');
    } catch {
      // The original error is the one worth reporting.
    }
    throw error;
  }
}
