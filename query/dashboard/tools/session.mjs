// session.mjs — the dashboard server's side of sign-in, kept out of serve.mjs so it can be tested.
//
// WHY THIS IS SMALL NOW. Task 11 built an OpenID Connect client in this file: the dashboard was the
// relying party for one identity provider and forwarded that provider's access token. That cannot
// serve "any customer IdP" — a Google or Okta access token is opaque or not meant for us — so
// control-api became the product's one identity service (the shared contract, §3 and §6). It is
// the relying party for every customer IdP, keeps the server-side session, and mints short-lived
// product access tokens that query-api, content-vault and control-api's admin API all verify.
//
// What is left here is a thin backend-for-frontend:
//   * an identity client for control-api's internal API (begin, complete, token, revoke), called
//     with the shared internal bearer and never reachable from a browser;
//   * a per-session cache of the product access token, refreshed through /internal/v1/auth/token
//     shortly before it expires. The session lives in control-api, so any instance of this server
//     can serve any session: a cache miss is a refresh, never a sign-out;
//   * the cookies (the opaque session id, and the sign-in attempt while the browser is at the IdP),
//     the same-origin check that stands in for a CSRF token, and the role-to-page map the
//     navigation is hidden by.
//
// The browser holds one opaque cookie and never a token. Zero dependencies: node:crypto only.

import { createHash } from 'node:crypto';

/** The analyst-app roles, mirroring query-api's roles.js. Two copies because the packages are
 *  separately deployable and zero-dependency; a drift is caught by the report, not a build. `dev`
 *  is the lab's development principal (SAC_DEV_TRUST_PRINCIPAL), which carries every capability
 *  there and so is offered every page here. A product token can never name it. */
export const ROLE_CAPABILITIES = Object.freeze({
  viewer: Object.freeze(['aggregate', 'device']),
  analyst: Object.freeze(['aggregate', 'device', 'subject', 'search']),
  content_reader: Object.freeze(['aggregate', 'device', 'subject', 'search', 'content']),
  admin: Object.freeze(['aggregate', 'audit', 'settings', 'export', 'sanction']),
  dev: Object.freeze(['aggregate', 'device', 'subject', 'search', 'content', 'audit', 'settings', 'export', 'sanction']),
});

/** The roles a product token may carry. `dev` is not one of them. */
export const PRODUCT_ROLES = Object.freeze(['viewer', 'analyst', 'content_reader', 'admin']);

/**
 * The navigation a role may see. The dashboard's own hiding is defence in depth: query-api and
 * control-api refuse the reads and writes anyway, and this decides what is offered.
 */
const PAGE_CAPABILITY = Object.freeze({
  posture: 'aggregate',
  tools: 'aggregate',
  teams: 'aggregate',
  person: 'subject',
  devices: 'device',
  audit: 'audit',
  explore: 'search',
  deployment: 'settings',
});

const ALWAYS_VISIBLE = Object.freeze(['posture', 'tools', 'teams']);

/** The capabilities of one role. An unknown role carries none. */
export function capabilitiesFor(role) {
  return ROLE_CAPABILITIES[role] ?? Object.freeze([]);
}

/** Does a role carry a capability? */
export function can(role, capability) {
  return capabilitiesFor(role).includes(capability);
}

/** Does any of a set of roles carry a capability? A person can hold several. */
export function canAny(roles, capability) {
  return (roles ?? []).some((role) => can(role, capability));
}

/** The page ids a role may open. `null` (no session) means "show everything", the sample mode. */
export function pagesFor(role) {
  if (!role) return null;
  return pagesForRoles([role]);
}

/** The page ids a set of roles may open: the union, because the roles are not a ladder. */
export function pagesForRoles(roles) {
  if (!Array.isArray(roles) || roles.length === 0) return null;
  const pages = Object.entries(PAGE_CAPABILITY)
    .filter(([, capability]) => canAny(roles, capability))
    .map(([page]) => page);
  return [...new Set([...ALWAYS_VISIBLE, ...pages])];
}

/** The role the navigation is labelled by: the one with the most capabilities. query-api checks the full set. */
export function primaryRole(roles) {
  return (roles ?? []).slice().sort((a, b) => capabilitiesFor(b).length - capabilitiesFor(a).length)[0] ?? null;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * The principal control-api returned, checked rather than trusted blindly: a tenant that is not a
 * uuid or a principal with no product role is not a session this server will hold. control-api
 * refuses a person with no role itself (`no_role`); this is the same rule kept at the edge.
 */
export function principalFrom(raw) {
  const tenant = String(raw?.tenant ?? '').trim().toLowerCase();
  if (!UUID.test(tenant)) return null;
  const roles = (Array.isArray(raw?.roles) ? raw.roles : []).filter((r) => PRODUCT_ROLES.includes(r));
  if (roles.length === 0) return null;
  const actor = String(raw?.actor ?? '').trim();
  if (actor === '') return null;
  const idp = raw?.idp === 'entra' || raw?.idp === 'oidc' ? raw.idp : null;
  return Object.freeze({ tenant, actor, roles: Object.freeze([...new Set(roles)]), idp });
}

// -------------------------------------------------------------------------------------------
// control-api's internal identity API
// -------------------------------------------------------------------------------------------

/**
 * A client for control-api's internal identity API (contract §3). Every answer is a value, never a
 * throw: `{ ok: true, status, body }` or `{ ok: false, status, error }`, where status 0 and error
 * `identity_unreachable` mean the service could not be reached. The server turns each into a
 * page or a JSON refusal; nothing here decides what a person reads.
 *
 * @param {object} input
 * @param {string} input.baseUrl         SAC_CONTROL_URL
 * @param {string} input.internalToken   SAC_INTERNAL_TOKEN, the shared bearer for /internal/*
 * @param {typeof fetch} [input.fetchImpl]
 * @param {number} [input.timeoutMs]
 */
export function createIdentityClient({ baseUrl, internalToken, fetchImpl = globalThis.fetch, timeoutMs = 10_000 } = {}) {
  const base = String(baseUrl ?? '').replace(/\/$/, '');
  if (base === '') throw new Error('createIdentityClient needs the control-api base URL');
  if (!internalToken) throw new Error('createIdentityClient needs the internal bearer token');

  async function call(path, body) {
    let res;
    try {
      res = await fetchImpl(`${base}${path}`, {
        method: 'POST',
        headers: { authorization: `Bearer ${internalToken}`, 'content-type': 'application/json', accept: 'application/json', 'accept-encoding': 'identity' },
        body: JSON.stringify(body),
        redirect: 'manual',
        signal: AbortSignal.timeout(timeoutMs),
      });
    } catch {
      return { ok: false, status: 0, error: 'identity_unreachable' };
    }
    let parsed = null;
    if (res.status !== 204) {
      try {
        parsed = await res.json();
      } catch {
        parsed = null;
      }
    }
    if (res.ok) return { ok: true, status: res.status, body: parsed ?? {} };
    const code = typeof parsed?.error === 'string' ? parsed.error : parsed?.error?.code;
    return { ok: false, status: res.status, error: typeof code === 'string' && /^[a-z_]{1,64}$/.test(code) ? code : 'identity_error' };
  }

  return Object.freeze({
    /** Start a sign-in. Exactly what the person chose: an email to discover by, Microsoft, or an invite. */
    begin({ email, provider, invite, redirectUri }) {
      const body = { redirect_uri: redirectUri };
      if (email) body.email = email;
      if (provider) body.provider = provider;
      if (invite) body.invite = invite;
      return call('/internal/v1/auth/begin', body);
    },
    complete({ attempt, code, state }) {
      return call('/internal/v1/auth/complete', { attempt, code, state });
    },
    token(session) {
      return call('/internal/v1/auth/token', { session });
    },
    revoke(session) {
      return call('/internal/v1/auth/revoke', { session });
    },
  });
}

// -------------------------------------------------------------------------------------------
// The product-token cache
// -------------------------------------------------------------------------------------------

/** The cache key for a session id. The id itself is a credential and is not kept as a key. */
export function sessionKey(session) {
  return createHash('sha256').update(String(session)).digest('hex');
}

/**
 * A short in-memory cache of the product access token per session. A token is used until it is
 * within `refreshBeforeMs` of expiring, then re-minted through /internal/v1/auth/token; concurrent
 * requests for one session share a single refresh. If control-api cannot be reached, a token that
 * has not yet expired is still used, because it is still valid; an expired one is not.
 *
 * @param {object} input
 * @param {ReturnType<typeof createIdentityClient>} input.identity
 * @param {() => number} [input.now]
 * @param {number} [input.refreshBeforeMs]
 * @param {number} [input.maxEntries]
 */
export function createTokenCache({ identity, now = () => Date.now(), refreshBeforeMs = 60_000, maxEntries = 5000 } = {}) {
  const entries = new Map();
  const inflight = new Map();

  function store(key, { access_token: token, expires_in: expiresIn, principal: raw }) {
    const principal = principalFrom(raw);
    if (typeof token !== 'string' || token === '' || !principal) return null;
    const seconds = Number.isFinite(Number(expiresIn)) ? Math.max(0, Number(expiresIn)) : 0;
    const entry = Object.freeze({ token, principal, expiresAt: now() + seconds * 1000 });
    entries.delete(key);
    entries.set(key, entry);
    if (entries.size > maxEntries) {
      const t = now();
      for (const [k, e] of entries) if (e.expiresAt <= t) entries.delete(k);
      // Still over: drop the least recently stored. A dropped session is re-minted on its next request.
      while (entries.size > maxEntries) entries.delete(entries.keys().next().value);
    }
    return entry;
  }

  async function refresh(session, key) {
    const previous = entries.get(key);
    const answer = await identity.token(session);
    if (answer.ok) {
      const entry = store(key, answer.body);
      return entry ? { ok: true, token: entry.token, principal: entry.principal } : { ok: false, reason: 'session_ended' };
    }
    if (answer.status === 401 || answer.status === 403 || answer.status === 404) {
      entries.delete(key);
      return { ok: false, reason: 'session_ended' };
    }
    if (previous && previous.expiresAt > now()) return { ok: true, token: previous.token, principal: previous.principal };
    return { ok: false, reason: 'identity_unavailable' };
  }

  return Object.freeze({
    /** Hold the token that came back with a completed sign-in, so the first page costs no refresh. */
    seed(session, body) {
      return store(sessionKey(session), body) !== null;
    },
    /** The product token and principal for a session, or why there is none. */
    async get(session) {
      if (!session) return { ok: false, reason: 'no_session' };
      const key = sessionKey(session);
      const entry = entries.get(key);
      if (entry && entry.expiresAt - now() > refreshBeforeMs) return { ok: true, token: entry.token, principal: entry.principal };
      if (!inflight.has(key)) {
        inflight.set(key, refresh(session, key).finally(() => inflight.delete(key)));
      }
      return inflight.get(key);
    },
    drop(session) {
      entries.delete(sessionKey(session));
    },
    get size() {
      return entries.size;
    },
  });
}

// -------------------------------------------------------------------------------------------
// Cookies
// -------------------------------------------------------------------------------------------

export const SESSION_COOKIE = 'sac_session';
export const SIGNIN_COOKIE = 'sac_signin';
/** control-api's session max age (contract §3). The server-side session decides; this only stops a browser keeping a dead id. */
export const SESSION_MAX_AGE_SEC = 8 * 3600;
/** control-api holds a sign-in attempt for ten minutes. */
export const SIGNIN_MAX_AGE_SEC = 10 * 60;

/** A Set-Cookie value. HttpOnly and SameSite=Lax always; Secure when the public URL is https. */
export function cookieHeader(name, value, { path = '/', maxAgeSec, secure = false } = {}) {
  const parts = [`${name}=${value}`, 'HttpOnly', 'SameSite=Lax', `Path=${path}`];
  if (maxAgeSec !== undefined) parts.push(`Max-Age=${Math.max(0, Math.floor(maxAgeSec))}`);
  if (secure) parts.push('Secure');
  return parts.join('; ');
}

/** The session cookie for one opaque id. Lax so the IdP's redirect back to /callback carries it. */
export function sessionCookie(id, { maxAgeSec = SESSION_MAX_AGE_SEC, secure = false } = {}) {
  return cookieHeader(SESSION_COOKIE, id, { path: '/', maxAgeSec, secure });
}

/** The sign-in attempt cookie: sent only to /callback, gone after ten minutes. */
export function signinCookie(value, { maxAgeSec = SIGNIN_MAX_AGE_SEC, secure = false } = {}) {
  return cookieHeader(SIGNIN_COOKIE, value, { path: '/callback', maxAgeSec, secure });
}

/** The cookie value of `name`, or null. */
export function readCookie(header, name = SESSION_COOKIE) {
  if (typeof header !== 'string' || header === '') return null;
  for (const part of header.split(';')) {
    const [k, ...rest] = part.trim().split('=');
    if (k === name) return rest.join('=') || null;
  }
  return null;
}

/** Parse a Cookie header into a plain object, for tests. */
export function parseCookies(header) {
  const out = {};
  if (typeof header !== 'string') return out;
  for (const part of header.split(';')) {
    const idx = part.indexOf('=');
    if (idx > 0) out[part.slice(0, idx).trim()] = part.slice(idx + 1).trim();
  }
  return out;
}

/** A Cookie header with this server's own cookies removed, for a request forwarded elsewhere. */
export function withoutOwnCookies(header) {
  if (typeof header !== 'string' || header === '') return '';
  return header.split(';').map((p) => p.trim()).filter((p) => p !== '' && !p.startsWith(`${SESSION_COOKIE}=`) && !p.startsWith(`${SIGNIN_COOKIE}=`)).join('; ');
}

/**
 * The attempt cookie's value: the opaque attempt id and where to return afterwards. Kept in the
 * browser rather than in this process, so the instance that receives /callback need not be the
 * one that began the sign-in.
 */
export function encodeAttempt({ attempt, next }) {
  return Buffer.from(JSON.stringify({ a: String(attempt), n: safeNext(next) })).toString('base64url');
}

export function decodeAttempt(value) {
  if (typeof value !== 'string' || value === '' || value.length > 4096) return null;
  try {
    const parsed = JSON.parse(Buffer.from(value, 'base64url').toString('utf8'));
    if (typeof parsed?.a !== 'string' || parsed.a === '') return null;
    return { attempt: parsed.a, next: safeNext(parsed.n) };
  } catch {
    return null;
  }
}

/** Where a sign-in may return to: a path on this origin, never another host. */
export function safeNext(value, fallback = '/') {
  if (typeof value !== 'string' || value.length > 2048) return fallback;
  if (!value.startsWith('/') || value.startsWith('//') || value.startsWith('/\\')) return fallback;
  if (/[\u0000-\u001f\\]/.test(value)) return fallback;
  return value;
}

// -------------------------------------------------------------------------------------------
// The same-origin check (CSRF)
// -------------------------------------------------------------------------------------------

/**
 * Is a state-changing request from this dashboard's own pages? The session cookie is SameSite=Lax,
 * which already keeps it off a cross-site POST in current browsers; this is the explicit check
 * beside it. A browser says where a request came from in Sec-Fetch-Site (all current engines) or,
 * failing that, Origin; either one naming another site is a refusal. A request with neither
 * header is not a browser's cross-site request (every browser sends Origin on one), so it passes:
 * that is curl or a test, which carries no victim's cookie.
 *
 * @param {Record<string, string|string[]|undefined>} headers
 * @param {ReadonlyArray<string>} allowedOrigins the public URL's origin and the request's own
 */
export function sameOriginRequest(headers, allowedOrigins) {
  const site = headerValue(headers['sec-fetch-site']);
  if (site) return site === 'same-origin';
  const origin = headerValue(headers.origin);
  if (origin) return allowedOrigins.includes(origin);
  return true;
}

function headerValue(value) {
  return Array.isArray(value) ? value[0] : value;
}

/** Is this method one that changes something? GET and HEAD do not, by contract. */
export function isStateChanging(method) {
  return !['GET', 'HEAD', 'OPTIONS'].includes(String(method ?? 'GET').toUpperCase());
}
