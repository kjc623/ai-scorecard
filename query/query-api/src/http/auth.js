// auth.js — the authenticated session, from the product's own access token.
//
// control-api is the one issuer: it is the relying party for every customer identity provider,
// resolves the tenant, the actor and the roles, and mints a short-lived ES256 access token. This
// module verifies that token and nothing else, using `jose` for the JOSE handling.
//
// What is verified, and why each matters:
//   - the token is a compact JWS of bounded size, so a megabyte header is refused unparsed;
//   - `alg` is ES256 and only ES256, so `none`, an HMAC confusion or RS256 cannot pass;
//   - `typ` is `at+jwt` and `kid` is named, as the issuer's header always carries them;
//   - an unrecognised `crit` parameter is refused;
//   - the key comes only from the configured JWKS (an embedded `jwk`/`jku`/`x5u` is never used),
//     and a kid the cache does not hold triggers at most one refetch per cooldown;
//   - `iss` equals the configured issuer exactly and `aud` names this service;
//   - `exp`, `nbf` and `iat` with 60 s of leeway, and the token is no older than ten minutes, so
//     a revoked session cannot outlive it here;
//   - the tenant is a uuid, the actor is named, and at least one role is a product role.
// A token that is signed but names no tenant or no known role is refused, never defaulted.

import { createRemoteJWKSet, customFetch, decodeProtectedHeader, jwtVerify } from 'jose';
import { isKnownRole } from '../roles.js';

/** The one signature algorithm the product issuer uses. */
export const TOKEN_ALG = 'ES256';
/** RFC 9068's media type for a JWT access token; the issuer's header carries it. */
export const TOKEN_TYP = 'at+jwt';
/** This service's own audience. The issuer names all three verifiers in one token. */
export const DEFAULT_AUDIENCE = 'sac-query';
/** Clock leeway for exp, nbf and iat. */
export const CLOCK_TOLERANCE_SEC = 60;
/** A token older than ten minutes is refused whatever its exp says. */
export const MAX_TOKEN_AGE_SEC = 600;
/** A product token is a few hundred bytes. Anything near this is not one, and is not parsed. */
export const MAX_TOKEN_BYTES = 8 * 1024;
/** How long a fetched JWKS is used before it is refreshed. */
const JWKS_CACHE_MAX_AGE_MS = 10 * 60_000;
/**
 * The shortest gap between two JWKS fetches caused by an unknown kid. A rotation is picked up
 * within this window; a stream of forged kids cannot turn into a stream of fetches.
 */
const JWKS_COOLDOWN_MS = 30_000;
const JWKS_TIMEOUT_MS = 5_000;

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const COMPACT_JWS = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;
/** The session id is a hex prefix of the session hash, for audit correlation only. */
const SESSION_ID = /^[0-9a-f]{8,64}$/i;
const MAX_ACTOR = 256;
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f]/;

/** `{issuer}/.well-known/jwks.json`, the issuer's JWKS location. */
export function defaultJwksUrl(issuer) {
  return `${String(issuer).replace(/\/+$/, '')}/.well-known/jwks.json`;
}

/**
 * The session facts a verified claim set carries, or a thrown refusal.
 *
 * Exported so the claim rules are testable without a signature; it is never called on claims
 * that have not been verified.
 */
export function sessionFromClaims(claims) {
  const tenant = typeof claims.sac_tenant === 'string' ? claims.sac_tenant.trim().toLowerCase() : '';
  if (!UUID.test(tenant)) throw new Error('the token\'s sac_tenant claim is not a tenant uuid');

  const actor = typeof claims.actor === 'string' ? claims.actor.trim() : '';
  if (actor === '' || actor.length > MAX_ACTOR || CONTROL.test(actor)) {
    throw new Error('the token\'s actor claim is missing or not a printable name');
  }
  const subject = typeof claims.sub === 'string' ? claims.sub.trim() : '';
  if (subject === '') throw new Error('the token names no subject');

  // A role this service does not know is dropped rather than refused: a newer issuer may name a
  // role a verifier has not learnt yet. A token left with no known role is refused outright.
  const roles = Array.isArray(claims.roles) ? [...new Set(claims.roles.filter(isKnownRole))] : [];
  if (roles.length === 0) throw new Error('the token\'s roles claim names no product role');

  // The session id only correlates audit rows with the session that wrote them, so a malformed one
  // is dropped rather than allowed to refuse a read.
  const sessionId = typeof claims.sid === 'string' && SESSION_ID.test(claims.sid) ? claims.sid.toLowerCase() : null;
  return {
    tenant,
    actorId: actor,
    subject,
    roles,
    sessionId,
    idp: claims.idp === 'entra' || claims.idp === 'oidc' ? claims.idp : null,
    caseReference: null,
  };
}

/** One line for the log: jose's machine code when it has one, never the token. */
function reasonOf(error) {
  const code = typeof error?.code === 'string' ? `${error.code}: ` : '';
  return `${code}${error?.message ?? error}`;
}

/**
 * Build the token verifier.
 *
 * @param {object} opts
 * @param {string} opts.issuer        exact `iss`
 * @param {string} [opts.audience]    this service's audience
 * @param {string} [opts.jwksUrl]     defaults to {issuer}/.well-known/jwks.json
 * @param {typeof fetch} [opts.fetchImpl]  test seam for the JWKS fetch
 * @param {() => Date} [opts.now]     test seam for the claim clock
 * @param {number} [opts.cooldownMs]  test seam for the kid-miss refetch interval
 */
export function createVerifier({
  issuer = '',
  audience = DEFAULT_AUDIENCE,
  jwksUrl = '',
  fetchImpl = null,
  now = () => new Date(),
  cooldownMs = JWKS_COOLDOWN_MS,
} = {}) {
  if (!issuer) throw new Error('createVerifier needs the token issuer');
  const keys = createRemoteJWKSet(new URL(jwksUrl || defaultJwksUrl(issuer)), {
    cacheMaxAge: JWKS_CACHE_MAX_AGE_MS,
    cooldownDuration: cooldownMs,
    timeoutDuration: JWKS_TIMEOUT_MS,
    ...(fetchImpl ? { [customFetch]: fetchImpl } : {}),
  });

  return Object.freeze({
    issuer,
    audience,
    /**
     * Verify a token and turn it into the session facts, or throw an Error whose message is safe
     * to log. The token itself never appears in a message.
     */
    async verify(token) {
      if (typeof token !== 'string' || token.length > MAX_TOKEN_BYTES || !COMPACT_JWS.test(token)) {
        throw new Error('the bearer is not a compact JWS of a plausible size');
      }
      let header;
      try {
        header = decodeProtectedHeader(token);
      } catch (error) {
        throw new Error(`the token header is unreadable: ${reasonOf(error)}`);
      }
      // Checked before the key lookup, so a token naming no key (or the wrong algorithm) never
      // causes a JWKS fetch.
      if (header.alg !== TOKEN_ALG) throw new Error(`token alg ${JSON.stringify(header.alg)} is not ${TOKEN_ALG}`);
      if (typeof header.kid !== 'string' || header.kid === '') throw new Error('the token header names no kid');

      let claims;
      try {
        ({ payload: claims } = await jwtVerify(token, keys, {
          issuer,
          audience,
          algorithms: [TOKEN_ALG],
          typ: TOKEN_TYP,
          clockTolerance: CLOCK_TOLERANCE_SEC,
          maxTokenAge: MAX_TOKEN_AGE_SEC,
          requiredClaims: ['exp', 'iat', 'sub'],
          currentDate: now(),
        }));
      } catch (error) {
        throw new Error(`the token was refused: ${reasonOf(error)}`);
      }
      return sessionFromClaims(claims);
    },
  });
}
