// auth.test.mjs — the product access token, verified for real.
//
// The HTTP tests hold the role boundaries with a real verifier too; this suite exercises the
// verifier itself: an ES256 signature checked against the issuer's JWKS, and each property a
// token must have refused when it is wrong. Tokens are built by hand in helpers.mjs, not by the
// library under test, so each malformed shape is exactly the one named. Keys are generated per run.

import test from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';

import { createVerifier, defaultJwksUrl, MAX_TOKEN_BYTES, sessionFromClaims } from '../src/http/auth.js';
import { createTestIssuer, TENANT } from './helpers.mjs';

const now = () => Math.floor(Date.now() / 1000);

function setup({ audience = 'sac-query', cooldownMs } = {}) {
  const issuer = createTestIssuer();
  const verifier = createVerifier({
    issuer: issuer.issuer,
    audience,
    fetchImpl: issuer.fetchImpl,
    ...(cooldownMs === undefined ? {} : { cooldownMs }),
  });
  return { issuer, verifier };
}

// ---------------------------------------------------------------------------------------------
// The token that should pass
// ---------------------------------------------------------------------------------------------

test('an ES256 product token becomes the session principal', async () => {
  const { issuer, verifier } = setup();
  const p = await verifier.verify(issuer.mint());
  assert.equal(p.tenant, TENANT);
  assert.equal(p.actorId, 'reader@lab.test', 'the actor comes from the actor claim');
  assert.equal(p.subject, '5d1c0de0-0000-4000-8000-00000000c0aa:idp-subject-1');
  assert.deepEqual(p.roles, ['content_reader']);
  assert.equal(p.sessionId, '0a1b2c3d4e5f6071', 'sid is carried for the audit row');
  assert.equal(p.idp, 'oidc');
  assert.equal(issuer.fetches, 1, 'the JWKS was fetched once');
  await verifier.verify(issuer.mint({ actor: 'second@lab.test' }));
  assert.equal(issuer.fetches, 1, 'and reused for the next token');
});

test('the JWKS location defaults to {issuer}/.well-known/jwks.json', () => {
  assert.equal(defaultJwksUrl('http://control-api:8080/'), 'http://control-api:8080/.well-known/jwks.json');
});

// ---------------------------------------------------------------------------------------------
// Issuer, audience, algorithm, header
// ---------------------------------------------------------------------------------------------

test('a token from another issuer is refused', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ iss: 'http://control-api.test:8080/' })), /iss/);
  await assert.rejects(() => verifier.verify(issuer.mint({ iss: 'https://login.microsoftonline.com/x/v2.0' })), /iss/);
});

test('a token that does not name this service as an audience is refused', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ aud: ['sac-vault', 'sac-control'] })), /aud/);
  await assert.rejects(() => verifier.verify(issuer.mint({ aud: 'sac-vault' })), /aud/);
  await verifier.verify(issuer.mint({ aud: 'sac-query' }));
});

test('alg none is refused, and is refused before any key is fetched', async () => {
  const { issuer, verifier } = setup();
  const unsigned = issuer.mint({}, { header: { alg: 'none' } });
  // The RFC 7519 unsecured form (empty signature) fails the shape check; with a signature segment
  // attached it reaches the algorithm check. Neither reaches a key.
  await assert.rejects(() => verifier.verify(unsigned), /compact JWS/);
  await assert.rejects(() => verifier.verify(`${unsigned}AAAA`), /alg "none" is not ES256/);
  assert.equal(issuer.fetches, 0);
});

test('HS256 keyed with the published public key (the alg-confusion attack) is refused', async () => {
  const { issuer, verifier } = setup();
  const jwk = issuer.publishedJwk();
  for (const secret of [jwk.x, JSON.stringify(jwk)]) {
    await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { alg: 'HS256' }, hmacSecret: secret })), /alg/);
  }
  assert.equal(issuer.fetches, 0);
});

test('RS256 is refused even when the signature is valid for an RSA key', async () => {
  const { issuer, verifier } = setup();
  const { privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { alg: 'RS256' }, rsaKey: privateKey })), /alg/);
});

test('a header without typ at+jwt, without kid, or with an unknown crit is refused', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { typ: 'JWT' } })), /typ/);
  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { typ: undefined } })), /typ/);
  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { kid: undefined } })), /kid/);
  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { crit: ['x-sac-ext'], 'x-sac-ext': 1 } })), /crit|Extension/i);
});

test('an embedded jwk in the header is not trusted: the key comes only from the JWKS', async () => {
  const { issuer, verifier } = setup();
  const foreign = generateKeyPairSync('ec', { namedCurve: 'P-256' });
  const token = issuer.mint({}, { key: foreign.privateKey, header: { jwk: foreign.publicKey.export({ format: 'jwk' }) } });
  await assert.rejects(() => verifier.verify(token), /signature/);
});

// ---------------------------------------------------------------------------------------------
// The signature itself
// ---------------------------------------------------------------------------------------------

test('a tampered payload fails the signature', async () => {
  const { issuer, verifier } = setup();
  const [h, , s] = issuer.mint().split('.');
  const forged = `${h}.${Buffer.from(JSON.stringify(issuer.claimsOf({ sac_tenant: '00000000-0000-4000-8000-0000000000bb' }))).toString('base64url')}.${s}`;
  await assert.rejects(() => verifier.verify(forged), /signature/);
});

test('a signature by a key the issuer never published, under its kid, is refused', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({}, { key: issuer.foreignKey() })), /signature/);
});

test('an ASN.1 DER signature is refused: a JWS carries the raw 64-byte R||S', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({}, { encoding: 'der' })), /signature/);
});

test('an oversized or malformed bearer is refused before anything is parsed or fetched', async () => {
  const { issuer, verifier } = setup();
  const big = issuer.mint({ actor: 'a'.repeat(MAX_TOKEN_BYTES) });
  await assert.rejects(() => verifier.verify(big), /plausible size/);
  await assert.rejects(() => verifier.verify('not-a-jwt'), /compact JWS/);
  await assert.rejects(() => verifier.verify('a.b.c.d'), /compact JWS/);
  assert.equal(issuer.fetches, 0);
});

// ---------------------------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------------------------

test('an expired token is refused; one inside the 60 s leeway is accepted', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ iat: now() - 400, exp: now() - 120 })), /exp/);
  await verifier.verify(issuer.mint({ iat: now() - 300, exp: now() - 30 }));
  await assert.rejects(() => verifier.verify(issuer.mint({ exp: undefined })), /exp/);
});

test('a token not yet valid is refused; nbf inside the leeway is accepted', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ nbf: now() + 300 })), /nbf/);
  await verifier.verify(issuer.mint({ nbf: now() + 30 }));
});

test('a token older than ten minutes is refused whatever its exp says, and iat is required', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ iat: now() - 700, exp: now() + 3600 })), /iat/);
  await assert.rejects(() => verifier.verify(issuer.mint({ iat: now() + 600 })), /iat/);
  await assert.rejects(() => verifier.verify(issuer.mint({ iat: undefined })), /iat/);
});

// ---------------------------------------------------------------------------------------------
// The JWKS: rotation and a kid nobody published
// ---------------------------------------------------------------------------------------------

test('an unknown kid refetches the JWKS once: a rotated key is then accepted, a forged kid refused', async () => {
  const { issuer, verifier } = setup({ cooldownMs: 0 });
  await verifier.verify(issuer.mint());
  assert.equal(issuer.fetches, 1);

  const rotated = issuer.publish('k2');
  const p = await verifier.verify(issuer.mint({}, { header: { kid: 'k2' }, key: rotated }));
  assert.equal(p.tenant, TENANT, 'the rotated key verifies after one refetch');
  assert.equal(issuer.fetches, 2);

  await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { kid: 'nobody' } })), /matching key|JWKS/i);
  assert.equal(issuer.fetches, 3, 'the forged kid caused one refetch and was then refused');
});

test('kid-miss refetches are rate-limited: inside the cooldown an unknown kid costs no fetch', async () => {
  const { issuer, verifier } = setup(); // the production cooldown, 30 s
  await verifier.verify(issuer.mint());
  for (let i = 0; i < 5; i += 1) {
    await assert.rejects(() => verifier.verify(issuer.mint({}, { header: { kid: `forged-${i}` } })), /matching key|JWKS/i);
  }
  assert.equal(issuer.fetches, 1, 'five forged kids did not cause five fetches');
});

// ---------------------------------------------------------------------------------------------
// The product claims
// ---------------------------------------------------------------------------------------------

test('an unknown role is dropped; a token left with no product role is refused', async () => {
  const { issuer, verifier } = setup();
  const p = await verifier.verify(issuer.mint({ roles: ['analyst', 'superuser', 'dev', 'analyst'] }));
  assert.deepEqual(p.roles, ['analyst'], 'unknown roles are dropped and duplicates collapse');
  await assert.rejects(() => verifier.verify(issuer.mint({ roles: ['superuser'] })), /role/);
  await assert.rejects(() => verifier.verify(issuer.mint({ roles: [] })), /role/);
  await assert.rejects(() => verifier.verify(issuer.mint({ roles: undefined })), /role/);
  await assert.rejects(() => verifier.verify(issuer.mint({ roles: 'admin' })), /role/, 'roles is an array');
});

test('a token with no tenant, no actor or no subject is refused rather than defaulted', async () => {
  const { issuer, verifier } = setup();
  await assert.rejects(() => verifier.verify(issuer.mint({ sac_tenant: 'not-a-uuid' })), /tenant/);
  await assert.rejects(() => verifier.verify(issuer.mint({ sac_tenant: undefined, tid: TENANT })), /tenant/);
  await assert.rejects(() => verifier.verify(issuer.mint({ actor: undefined, preferred_username: 'x@lab.test' })), /actor/);
  await assert.rejects(() => verifier.verify(issuer.mint({ actor: 'evil\r\nx-sac-tenant: other' })), /actor/);
  await assert.rejects(() => verifier.verify(issuer.mint({ sub: undefined })), /sub/);
});

test('sessionFromClaims drops a malformed sid instead of refusing the read', () => {
  const base = { sac_tenant: TENANT.toUpperCase(), actor: ' a@lab.test ', sub: 's', roles: ['viewer'] };
  const p = sessionFromClaims({ ...base, sid: 'not hex!' });
  assert.equal(p.sessionId, null);
  assert.equal(p.tenant, TENANT, 'the tenant is normalised to lower case');
  assert.equal(p.actorId, 'a@lab.test');
});

test('a verifier needs an issuer', () => {
  assert.throws(() => createVerifier({ issuer: '' }), /issuer/);
});
