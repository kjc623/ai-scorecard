// auth.js — the authenticated session, from a signed token.
//
// WHY THIS FILE EXISTS. Until task 11 the only trusted source of a tenant was a development
// header (`x-sac-dev-tenant`) accepted behind SAC_DEV_TRUST_PRINCIPAL=1. That is a lab
// arrangement, not authentication: a header is something the caller writes. This module
// replaces it with the thing the design named (docs/04 §2.1, docs/06 §4.1): an OpenID
// Connect session, where the tenant, the actor and the roles arrive inside an RS256-signed
// token and are read from nothing else.
//
// It is deliberately small and dependency-free, like the rest of this service. Node's
// `crypto` verifies an RSA signature and builds a public key from a JWK, so the whole flow
// is base64url decoding, a JWKS fetch, and one `crypto.verify`. Adding a JWT library would
// break the offline, zero-dependency constraint for a function this size.
//
// What is verified, and why each matters:
//   - the signature, against the issuer's JWKS (the key is selected by `kid`);
//   - `alg` is RS256, from an allow-list, so `alg: none` or an HMAC confusion cannot pass;
//   - `iss` equals the configured issuer exactly;
//   - `aud` contains the configured audience, so a token minted for a different API is refused;
//   - `exp` and `nbf` with a small clock tolerance.
// The tenant claim must be a UUID and the roles claim a list of known role names, because a
// token with no tenant or no role is refused rather than defaulted — the fail-closed rule of
// docs/04 §2.1.

import { createPublicKey, verify as cryptoVerify } from 'node:crypto';
import { isKnownRole } from '../roles.js';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
/** A few seconds of tolerance for a token minted on a clock a moment ahead of ours. */
const CLOCK_TOLERANCE_SEC = 60;
/** How long a fetched JWKS is trusted before a refetch. */
const JWKS_TTL_MS = 5 * 60_000;

/** base64url -> Buffer, rejecting a segment that is not base64url. */
export function decodeSegment(segment) {
  if (typeof segment !== 'string' || segment === '') throw new Error('empty JWT segment');
  return Buffer.from(segment.replace(/-/g, '+').replace(/_/g, '/'), 'base64');
}

/** The unverified header and payload, for choosing a key and reading claims. Never trusted alone. */
export function decodeJwt(token) {
  if (typeof token !== 'string') throw new Error('token is not a string');
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('a JWT has three dot-separated segments');
  const header = JSON.parse(decodeSegment(parts[0]).toString('utf8'));
  const payload = JSON.parse(decodeSegment(parts[1]).toString('utf8'));
  return { header, payload, signingInput: `${parts[0]}.${parts[1]}`, signature: decodeSegment(parts[2]) };
}

/**
 * Verify one RS256 JWT against a set of JWKs.
 *
 * @param {string} token
 * @param {object} opts
 * @param {string} opts.issuer
 * @param {string} opts.audience
 * @param {ReadonlyArray<object>} opts.keys  JWKs from the issuer
 * @param {() => Date} [opts.now]
 * @returns {object} the verified payload
 */
export function verifyJwt(token, { issuer, audience, keys, now = () => new Date() }) {
  const { header, payload, signingInput, signature } = decodeJwt(token);
  if (header.alg !== 'RS256') throw new Error(`unsupported token alg ${JSON.stringify(header.alg)}; only RS256 is accepted`);
  const jwk = (keys ?? []).find((k) => !header.kid || k.kid === header.kid) ?? (keys ?? [])[0];
  if (!jwk) throw new Error('the issuer published no key for this token');
  if (jwk.kty !== 'RSA') throw new Error(`the issuer key is ${JSON.stringify(jwk.kty)}, not RSA`);
  let key;
  try {
    key = createPublicKey({ key: jwk, format: 'jwk' });
  } catch (error) {
    throw new Error(`the issuer key is not a usable JWK: ${error.message}`);
  }
  const ok = cryptoVerify('RSA-SHA256', Buffer.from(signingInput, 'utf8'), key, signature);
  if (!ok) throw new Error('the token signature does not verify');

  const seconds = Math.floor(now().getTime() / 1000);
  if (payload.iss !== issuer) throw new Error(`token issuer ${JSON.stringify(payload.iss)} is not ${JSON.stringify(issuer)}`);
  const audiences = Array.isArray(payload.aud) ? payload.aud : [payload.aud];
  if (!audiences.includes(audience)) throw new Error(`token audience ${JSON.stringify(payload.aud)} does not name ${JSON.stringify(audience)}`);
  if (typeof payload.exp !== 'number' || payload.exp + CLOCK_TOLERANCE_SEC < seconds) throw new Error('the token has expired');
  if (typeof payload.nbf === 'number' && payload.nbf - CLOCK_TOLERANCE_SEC > seconds) throw new Error('the token is not valid yet');
  return payload;
}

/**
 * A tiny JWKS cache: fetch once, reuse, and refetch only when a token names a key the cache
 * does not hold (a rotation) or the TTL has passed. A failed refetch does not empty the cache.
 */
export function createJwksCache({ jwksUrl, fetchImpl = globalThis.fetch, now = () => Date.now(), ttlMs = JWKS_TTL_MS } = {}) {
  let keys = null;
  let fetchedAt = 0;
  async function load(force = false) {
    if (!force && keys && now() - fetchedAt < ttlMs) return keys;
    const res = await fetchImpl(jwksUrl);
    if (!res?.ok) throw new Error(`JWKS fetch from ${jwksUrl} returned ${res?.status ?? 'no status'}`);
    const body = await res.json();
    if (!Array.isArray(body?.keys) || body.keys.length === 0) throw new Error(`JWKS at ${jwksUrl} carried no keys`);
    keys = body.keys;
    fetchedAt = now();
    return keys;
  }
  return {
    async keyFor(kid) {
      const current = await load();
      if (!kid || current.some((k) => k.kid === kid)) return current;
      // The token names a key we do not hold. One forced refetch, then give up: a key that an
      // issuer does not publish is a forgery, not a lag.
      return load(true);
    },
    /** Test seam: drop the cache. */
    clear() {
      keys = null;
      fetchedAt = 0;
    },
  };
}

/**
 * Build the token verifier from configuration. `enabled` is false when no issuer is
 * configured, which is the memory-lab case: then the development principal is the only path,
 * and only behind its own flag.
 */
export function createVerifier({ issuer = '', audience = '', jwksUrl = '', tenantClaim = 'sac_tenant', rolesClaim = 'roles', fetchImpl = globalThis.fetch, now = () => new Date() } = {}) {
  if (!issuer || !audience) {
    return Object.freeze({ enabled: false, async verify() { throw new Error('no issuer is configured'); } });
  }
  const keys = createJwksCache({ jwksUrl: jwksUrl || `${issuer.replace(/\/$/, '')}/jwks`, fetchImpl, now: () => now().getTime() });
  return Object.freeze({
    enabled: true,
    /**
     * Verify a token and turn it into the session facts, or throw. A token that is signed but
     * names no tenant or no known role is refused here, not defaulted.
     */
    async verify(token) {
      const claims = verifyJwt(token, { issuer, audience, keys: await keys.keyFor(decodeJwt(token).header.kid), now });
      const tenant = String(claims[tenantClaim] ?? '').trim().toLowerCase();
      if (!UUID.test(tenant)) throw new Error(`token claim ${JSON.stringify(tenantClaim)} is not a tenant uuid`);
      const rawRoles = claims[rolesClaim];
      const roles = (Array.isArray(rawRoles) ? rawRoles : typeof rawRoles === 'string' ? [rawRoles] : []).filter(isKnownRole);
      if (roles.length === 0) throw new Error(`token claim ${JSON.stringify(rolesClaim)} names no known role`);
      const actorId = String(claims.preferred_username ?? claims.email ?? claims.sub ?? '').trim();
      if (actorId === '') throw new Error('the token names no actor');
      return { tenant, actorId, roles, subject: String(claims.sub ?? actorId), caseReference: null };
    },
  });
}
