// auth.test.mjs — the session token, verified for real.
//
// The HTTP tests inject a fake verifier to hold the role boundaries; this suite exercises the
// thing that fake stands in for: an RS256 signature checked against a JWKS, with each of the
// claims a token must carry (issuer, audience, expiry, tenant, role) refused when it is wrong.
//
// It signs with a key generated in the test, so nothing here needs the lab.

import test from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign as cryptoSign } from 'node:crypto';

import { createVerifier, decodeJwt, verifyJwt } from '../src/http/auth.js';

const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const JWK = { ...publicKey.export({ format: 'jwk' }), kid: 'test-key', use: 'sig', alg: 'RS256' };
const ISSUER = 'https://idp.lab.test';
const AUDIENCE = 'sac-query-api';
const TENANT = '00000000-0000-4000-8000-0000000000aa';

const b64url = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');

function token(claims, { alg = 'RS256', kid = 'test-key' } = {}) {
  const header = b64url({ alg, typ: 'JWT', kid });
  const payload = b64url(claims);
  const input = `${header}.${payload}`;
  const signature = alg === 'none' ? '' : cryptoSign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url');
  return `${input}.${signature}`;
}

function claimsOf(overrides = {}) {
  const now = Math.floor(Date.now() / 1000);
  return {
    iss: ISSUER, aud: AUDIENCE, sub: 'sub-1', preferred_username: 'reader@lab.test',
    roles: ['content_reader'], sac_tenant: TENANT, iat: now, exp: now + 300, ...overrides,
  };
}

const fetchJwks = async () => ({ ok: true, status: 200, json: async () => ({ keys: [JWK] }) });
const verifier = () => createVerifier({ issuer: ISSUER, audience: AUDIENCE, jwksUrl: `${ISSUER}/jwks`, fetchImpl: fetchJwks });

test('a well-formed, correctly signed token becomes the session principal', async () => {
  const p = await verifier().verify(token(claimsOf()));
  assert.equal(p.tenant, TENANT);
  assert.equal(p.actorId, 'reader@lab.test');
  assert.deepEqual(p.roles, ['content_reader']);
});

test('the tenant claim is configurable, because a real tenant names it differently', async () => {
  const v = createVerifier({ issuer: ISSUER, audience: AUDIENCE, jwksUrl: 'x', tenantClaim: 'tid', rolesClaim: 'app_roles', fetchImpl: fetchJwks });
  const p = await v.verify(token(claimsOf({ tid: TENANT, app_roles: ['admin'], sac_tenant: undefined })));
  assert.equal(p.tenant, TENANT);
  assert.deepEqual(p.roles, ['admin']);
});

test('a tampered payload fails the signature', async () => {
  const good = token(claimsOf());
  const [h, , s] = good.split('.');
  const forged = `${h}.${b64url(claimsOf({ sac_tenant: '00000000-0000-4000-8000-0000000000bb' }))}.${s}`;
  await assert.rejects(() => verifier().verify(forged), /signature/);
});

test('alg is restricted to RS256, so alg:none cannot pass', async () => {
  await assert.rejects(() => verifier().verify(token(claimsOf(), { alg: 'none' })), 'alg:none is refused');
  await assert.rejects(() => verifier().verify(token(claimsOf(), { alg: 'HS256' })), /RS256/);
});

test('a token for another issuer or audience is refused', async () => {
  await assert.rejects(() => verifier().verify(token(claimsOf({ iss: 'https://other.test' }))), /issuer/);
  await assert.rejects(() => verifier().verify(token(claimsOf({ aud: 'another-api' }))), /audience/);
});

test('an expired token is refused', async () => {
  const now = Math.floor(Date.now() / 1000);
  await assert.rejects(() => verifier().verify(token(claimsOf({ exp: now - 600 }))), /expired/);
});

test('a token with no tenant, no known role, or no actor is refused rather than defaulted', async () => {
  await assert.rejects(() => verifier().verify(token(claimsOf({ sac_tenant: 'not-a-uuid' }))), /tenant/);
  await assert.rejects(() => verifier().verify(token(claimsOf({ roles: ['superuser'] }))), /role/);
  await assert.rejects(() => verifier().verify(token(claimsOf({ sub: undefined, preferred_username: undefined, email: undefined }))), /actor/);
});

test('decodeJwt reads the header and payload without trusting them', () => {
  const { header, payload } = decodeJwt(token(claimsOf()));
  assert.equal(header.alg, 'RS256');
  assert.equal(payload.sac_tenant, TENANT);
});

test('verifyJwt needs a key that verifies; an empty JWKS is a refusal', () => {
  assert.throws(() => verifyJwt(token(claimsOf()), { issuer: ISSUER, audience: AUDIENCE, keys: [] }), /no key/);
});
