// session.mjs — the dashboard's OIDC session, kept out of serve.mjs so it can be tested.
//
// WHY THIS EXISTS. Until task 11 the dashboard's server invented a principal: it added
// x-sac-dev-tenant / x-sac-dev-actor to every forwarded request, and query-api trusted them
// because SAC_DEV_TRUST_PRINCIPAL=1. That is a lab arrangement, not a sign-in: a header is
// something the caller writes. This module implements the real flow the design names
// (docs/04 §2.1, docs/06 §4.1): authorization code + PKCE against the customer's identity
// provider, an id_token verified for real, and an opaque server-side session.
//
// The session lives on the server. The browser holds one opaque cookie and never a token,
// so a script on the page cannot read a bearer or a tenant. The access token is kept
// server-side and forwarded to query-api, which verifies it for itself — this server is a
// client, not an authority.
//
// Zero dependencies, like the rest of the dashboard package: node:crypto verifies the RS256
// signature and builds a key from a JWK, so the whole flow is base64url, one JWKS fetch and
// one crypto.verify.

import { createHash, createPublicKey, randomBytes, verify as cryptoVerify } from 'node:crypto';

/** The analyst-app roles, mirroring query-api's roles.js. Two copies because the packages are
 *  separately deployable and zero-dependency; a drift is caught by the report, not a build. */
export const ROLE_CAPABILITIES = Object.freeze({
  viewer: Object.freeze(['aggregate', 'device']),
  analyst: Object.freeze(['aggregate', 'device', 'subject', 'search']),
  content_reader: Object.freeze(['aggregate', 'device', 'subject', 'search', 'content']),
  admin: Object.freeze(['aggregate', 'audit', 'settings', 'export', 'sanction']),
});

/**
 * The navigation a role may see. The dashboard's own hiding is defence in depth: query-api
 * refuses the reads anyway, and this decides what is offered.
 */
const PAGE_CAPABILITY = Object.freeze({
  posture: 'aggregate',
  tools: 'aggregate',
  teams: 'aggregate',
  person: 'subject',
  devices: 'device',
  audit: 'audit',
  explore: 'search',
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

/** The page ids a role may open. `null` (no session) means "show everything", the sample mode. */
export function pagesFor(role) {
  if (!role) return null;
  const pages = Object.entries(PAGE_CAPABILITY)
    .filter(([, capability]) => can(role, capability))
    .map(([page]) => page);
  return [...new Set([...ALWAYS_VISIBLE, ...pages])];
}

// -------------------------------------------------------------------------------------------
// The session store
// -------------------------------------------------------------------------------------------

/**
 * An in-memory session store. A restart drops every session, which is the right posture for
 * an opaque server-side session: the alternative is persisting tokens to disk.
 *
 * @param {object} [opts]
 * @param {number} [opts.ttlMs] idle-independent lifetime
 * @param {() => number} [opts.now]
 * @param {() => string} [opts.random]
 */
export function createSessionStore({ ttlMs = 8 * 3600_000, now = () => Date.now(), random = () => randomBytes(32).toString('base64url') } = {}) {
  const sessions = new Map();
  function prune(t) {
    for (const [id, s] of sessions) if (s.expiresAt <= t) sessions.delete(id);
  }
  return {
    /** Create a session from the verified claims and the tokens; returns the opaque id. */
    create({ actor, tenant, role, roles, accessToken, idToken }) {
      const id = random();
      const at = now();
      sessions.set(id, { id, actor, tenant, role, roles: roles ?? [role], accessToken, idToken: idToken ?? null, expiresAt: at + ttlMs });
      return id;
    },
    /** The live session for an id, or null. Expired entries are forgotten, not returned. */
    get(id) {
      const s = sessions.get(id);
      if (!s) return null;
      if (s.expiresAt <= now()) {
        sessions.delete(id);
        return null;
      }
      return s;
    },
    destroy(id) {
      sessions.delete(id);
    },
    get size() {
      prune(now());
      return sessions.size;
    },
  };
}

// -------------------------------------------------------------------------------------------
// PKCE and the JWKS
// -------------------------------------------------------------------------------------------

const base64url = (buf) => Buffer.from(buf).toString('base64url');

/** A PKCE verifier and its S256 challenge (RFC 7636). */
export function pkce() {
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash('sha256').update(verifier).digest());
  return { verifier, challenge };
}

/** A per-attempt nonce, so a replayed id_token cannot open a session. */
export function nonce() {
  return base64url(randomBytes(16));
}

function decodeSegment(segment) {
  if (typeof segment !== 'string' || segment === '') throw new Error('empty JWT segment');
  return Buffer.from(segment.replace(/-/g, '+').replace(/_/g, '/'), 'base64');
}

export function decodeJwt(token) {
  const parts = String(token).split('.');
  if (parts.length !== 3) throw new Error('a JWT has three dot-separated segments');
  return {
    header: JSON.parse(decodeSegment(parts[0]).toString('utf8')),
    payload: JSON.parse(decodeSegment(parts[1]).toString('utf8')),
    signingInput: `${parts[0]}.${parts[1]}`,
    signature: decodeSegment(parts[2]),
  };
}

/**
 * Verify one RS256 JWT. The dashboard checks the id_token it just received from the token
 * endpoint; a wrong signature, issuer, audience, expiry or nonce is a refusal.
 */
export function verifyJwt(token, { issuer, audience, keys, nonce: expectedNonce = null, now = () => Date.now() }) {
  const { header, payload, signingInput, signature } = decodeJwt(token);
  if (header.alg !== 'RS256') throw new Error(`unsupported token alg ${JSON.stringify(header.alg)}`);
  const jwk = (keys ?? []).find((k) => !header.kid || k.kid === header.kid) ?? (keys ?? [])[0];
  if (!jwk) throw new Error('the issuer published no key');
  const key = createPublicKey({ key: jwk, format: 'jwk' });
  if (!cryptoVerify('RSA-SHA256', Buffer.from(signingInput, 'utf8'), key, signature)) throw new Error('the token signature does not verify');
  const seconds = Math.floor(now() / 1000);
  if (payload.iss !== issuer) throw new Error('the token issuer is not the configured issuer');
  const audiences = Array.isArray(payload.aud) ? payload.aud : [payload.aud];
  if (!audiences.includes(audience)) throw new Error('the token audience does not name this client');
  if (typeof payload.exp !== 'number' || payload.exp + 60 < seconds) throw new Error('the token has expired');
  if (expectedNonce !== null && payload.nonce !== expectedNonce) throw new Error('the token nonce does not match this sign-in');
  return payload;
}

// -------------------------------------------------------------------------------------------
// The OIDC client
// -------------------------------------------------------------------------------------------

/**
 * A small authorization-code client. Discovery, a JWKS cache, the authorize URL, the token
 * exchange, and id_token verification.
 *
 * @param {object} input
 * @param {string} input.issuer
 * @param {string} input.clientId
 * @param {string} [input.clientSecret]
 * @param {string} input.redirectUri
 * @param {string} [input.scope]
 * @param {typeof fetch} [input.fetchImpl]
 * @param {() => number} [input.now]
 */
export function createOidcClient({ issuer, clientId, clientSecret = '', redirectUri, scope = 'openid profile email', fetchImpl = globalThis.fetch, now = () => Date.now() } = {}) {
  const base = String(issuer).replace(/\/$/, '');
  let metadata = null;
  let jwks = null;
  let jwksAt = 0;

  async function discover() {
    if (metadata) return metadata;
    const res = await fetchImpl(`${base}/.well-known/openid-configuration`);
    if (!res?.ok) throw new Error(`discovery at ${base} returned ${res?.status ?? 'no status'}`);
    metadata = await res.json();
    return metadata;
  }

  async function keys(force = false) {
    if (!force && jwks && now() - jwksAt < 5 * 60_000) return jwks;
    const m = await discover();
    const res = await fetchImpl(m.jwks_uri ?? `${base}/jwks`);
    if (!res?.ok) throw new Error(`JWKS fetch returned ${res?.status ?? 'no status'}`);
    const body = await res.json();
    if (!Array.isArray(body?.keys) || body.keys.length === 0) throw new Error('the issuer published no keys');
    jwks = body.keys;
    jwksAt = now();
    return jwks;
  }

  return Object.freeze({
    get issuer() {
      return base;
    },
    get clientId() {
      return clientId;
    },
    async authorizeUrl({ state, nonce: nonceValue, codeChallenge, loginHint = '', redirectUri: redirect = redirectUri }) {
      const m = await discover();
      const url = new URL(m.authorization_endpoint ?? `${base}/authorize`);
      url.searchParams.set('response_type', 'code');
      url.searchParams.set('client_id', clientId);
      url.searchParams.set('redirect_uri', redirect);
      url.searchParams.set('scope', scope);
      url.searchParams.set('state', state);
      url.searchParams.set('nonce', nonceValue);
      url.searchParams.set('code_challenge', codeChallenge);
      url.searchParams.set('code_challenge_method', 'S256');
      if (loginHint) url.searchParams.set('login_hint', loginHint);
      return url.toString();
    },
    /** Exchange a code. A public client sends no secret; a confidential client does. */
    async exchange({ code, codeVerifier, redirectUri: redirect = redirectUri }) {
      const m = await discover();
      const form = new URLSearchParams({
        grant_type: 'authorization_code',
        code,
        redirect_uri: redirect,
        client_id: clientId,
        code_verifier: codeVerifier,
      });
      if (clientSecret) form.set('client_secret', clientSecret);
      const res = await fetchImpl(m.token_endpoint ?? `${base}/token`, {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: form.toString(),
      });
      const body = await res.json().catch(() => ({}));
      if (!res?.ok) throw new Error(body.error_description ?? body.error ?? `the token endpoint returned ${res?.status}`);
      if (!body.access_token || !body.id_token) throw new Error('the token endpoint returned no tokens');
      return body;
    },
    /** Verify the id_token and return the session facts, or throw. */
    async verifyIdToken(idToken, { nonce: expectedNonce } = {}) {
      const header = decodeJwt(idToken).header;
      let current = await keys();
      if (header.kid && !current.some((k) => k.kid === header.kid)) current = await keys(true);
      const claims = verifyJwt(idToken, { issuer: base, audience: clientId, keys: current, nonce: expectedNonce, now });
      return claims;
    },
  });
}

/**
 * Turn verified token claims into the session facts. The tenant must be a uuid and the role a
 * known one; a sign-in that names neither is refused rather than guessed.
 */
export function principalFromClaims(claims, { tenantClaim = 'sac_tenant', rolesClaim = 'roles' } = {}) {
  const tenant = String(claims?.[tenantClaim] ?? '').trim().toLowerCase();
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(tenant)) {
    throw new Error(`the token names no tenant in claim ${tenantClaim}`);
  }
  const raw = claims?.[rolesClaim];
  const roles = (Array.isArray(raw) ? raw : typeof raw === 'string' ? [raw] : []).filter((r) => ROLE_CAPABILITIES[r]);
  if (roles.length === 0) throw new Error(`the token names no known role in claim ${rolesClaim}`);
  const actor = String(claims.preferred_username ?? claims.email ?? claims.sub ?? '').trim();
  if (actor === '') throw new Error('the token names no actor');
  // The most capable role decides what the navigation offers; query-api checks the full set.
  const role = roles.slice().sort((a, b) => capabilitiesFor(b).length - capabilitiesFor(a).length)[0];
  return { actor, tenant, role, roles };
}

/** A cookie header value for one session id. HttpOnly and SameSite, and Secure when told. */
export function sessionCookie(id, { name = 'sac_session', maxAgeSec = 8 * 3600, secure = false } = {}) {
  return `${name}=${id}; HttpOnly; SameSite=Lax; Path=/; Max-Age=${maxAgeSec}${secure ? '; Secure' : ''}`;
}

/** The cookie value of `name`, or null. */
export function readCookie(header, name = 'sac_session') {
  if (typeof header !== 'string' || header === '') return null;
  for (const part of header.split(';')) {
    const [k, ...rest] = part.trim().split('=');
    if (k === name) return rest.join('=');
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
