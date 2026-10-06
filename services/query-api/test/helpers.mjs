// helpers.mjs — shared test scaffolding. Not a test file itself.
import { createHmac, generateKeyPairSync, sign } from 'node:crypto';

export const TENANT = '00000000-0000-4000-8000-0000000000aa';
export const NOW = new Date('2026-10-02T12:00:00Z');
export const WINDOW = Object.freeze({ from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' });

/** A minimal, well-formed query document that each test perturbs in exactly one way. */
export function baseDoc(overrides = {}) {
  return {
    query_version: '1',
    source: 'mart.v_tool_usage',
    bucket: 'day',
    dimensions: ['tool'],
    measures: ['submissions', 'users'],
    filters: [],
    window: { ...WINDOW },
    limit: 50,
    ...overrides,
  };
}

/** Capture the typed rejection from a call, or rethrow if it did not reject. */
export function rejection(fn) {
  try {
    fn();
  } catch (error) {
    return error;
  }
  throw new Error('expected a rejection, but the call succeeded');
}

/**
 * A token issuer with control-api's token shape, for the session tests.
 *
 * It holds a P-256 key generated per test run (no key is committed), publishes it as a JWKS
 * through a fetch double that counts every fetch, and mints compact JWS by hand rather than
 * through the library under test, so a test can build exactly the malformed token it needs: a
 * wrong alg, a missing typ, a DER signature, a foreign key under a published kid.
 */
export function createTestIssuer({ issuer = 'http://control-api.test:8080' } = {}) {
  const fresh = () => generateKeyPairSync('ec', { namedCurve: 'P-256' });
  const signing = fresh();
  const published = [{ ...signing.publicKey.export({ format: 'jwk' }), kid: 'k1', use: 'sig', alg: 'ES256' }];
  let fetches = 0;
  const b64 = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');

  const claimsOf = (overrides = {}) => {
    const now = Math.floor(Date.now() / 1000);
    return {
      iss: issuer,
      aud: ['sac-query', 'sac-vault', 'sac-control'],
      sub: '5d1c0de0-0000-4000-8000-00000000c0aa:idp-subject-1',
      sac_tenant: TENANT,
      actor: 'reader@lab.test',
      roles: ['content_reader'],
      idp: 'oidc',
      sid: '0a1b2c3d4e5f6071',
      iat: now,
      exp: now + 300,
      jti: `jti-${now}-${Math.random().toString(16).slice(2)}`,
      ...overrides,
    };
  };

  /**
   * Mint a token. `header` overrides the protected header; `key` signs with another private key;
   * `encoding: 'der'` produces the ASN.1 signature a JWS must not carry; `alg: 'none'` leaves the
   * signature empty.
   */
  const mint = (overrides = {}, { header = {}, key = signing.privateKey, encoding = 'ieee-p1363', hmacSecret = null, rsaKey = null } = {}) => {
    const h = { alg: 'ES256', typ: 'at+jwt', kid: 'k1', ...header };
    for (const name of Object.keys(h)) if (h[name] === undefined) delete h[name];
    const claims = claimsOf(overrides);
    for (const name of Object.keys(claims)) if (claims[name] === undefined) delete claims[name];
    const input = `${b64(h)}.${b64(claims)}`;
    let signature = '';
    if (hmacSecret) signature = createHmac('sha256', hmacSecret).update(input).digest('base64url');
    else if (rsaKey) signature = sign('sha256', Buffer.from(input), rsaKey).toString('base64url');
    else if (h.alg !== 'none') signature = sign('sha256', Buffer.from(input), { key, dsaEncoding: encoding }).toString('base64url');
    return `${input}.${signature}`;
  };

  const fetchImpl = async () => {
    fetches += 1;
    const body = { keys: published.map((k) => ({ ...k })) };
    return { status: 200, ok: true, json: async () => body };
  };

  return {
    issuer,
    claimsOf,
    mint,
    fetchImpl,
    get fetches() {
      return fetches;
    },
    /** The published public JWK, e.g. to use its `x` as an HMAC secret in an alg-confusion test. */
    publishedJwk: () => ({ ...published[0] }),
    /** Publish another key under `kid` and return its private half: a rotation. */
    publish(kid, keyPair = fresh()) {
      published.push({ ...keyPair.publicKey.export({ format: 'jwk' }), kid, use: 'sig', alg: 'ES256' });
      return keyPair.privateKey;
    },
    /** A key pair the issuer never published. */
    foreignKey: () => fresh().privateKey,
  };
}
