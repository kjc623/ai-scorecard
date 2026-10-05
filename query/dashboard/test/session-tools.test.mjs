// session-tools.test.mjs — the dashboard's OIDC client and role-to-navigation map.
//
// These are node-side helpers (tools/session.mjs), tested without a browser or the lab: a
// key generated here signs the id_token, and a fake fetch stands in for the issuer's
// discovery, JWKS and token endpoints. The role boundary the browser sees (which nav ids
// survive) is derived from the same map query-api enforces.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign as cryptoSign } from 'node:crypto';

import {
  can,
  capabilitiesFor,
  createOidcClient,
  createSessionStore,
  pagesFor,
  parseCookies,
  pkce,
  principalFromClaims,
  readCookie,
  sessionCookie,
  verifyJwt,
} from '../tools/session.mjs';

const b64url = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const JWK = { ...publicKey.export({ format: 'jwk' }), kid: 'k1', use: 'sig', alg: 'RS256' };
const ISSUER = 'https://idp.lab.test';
const CLIENT = 'sac-dashboard';
const TENANT = '00000000-0000-4000-8000-0000000000aa';

function idToken(claims, { kid = 'k1' } = {}) {
  const header = b64url({ alg: 'RS256', typ: 'JWT', kid });
  const payload = b64url(claims);
  const input = `${header}.${payload}`;
  return `${input}.${cryptoSign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url')}`;
}

function claims(overrides = {}) {
  const now = Math.floor(Date.now() / 1000);
  return { iss: ISSUER, aud: CLIENT, sub: 's1', email: 'viewer@lab.test', nonce: 'n1', roles: ['viewer'], sac_tenant: TENANT, iat: now, exp: now + 300, ...overrides };
}

// ---------------------------------------------------------------------------------------------
// Roles to pages
// ---------------------------------------------------------------------------------------------

test('each role maps to the pages it may open', () => {
  assert.deepEqual(pagesFor('viewer').sort(), ['devices', 'posture', 'tools', 'teams'].sort());
  for (const role of ['analyst', 'content_reader']) {
    assert.ok(pagesFor(role).includes('person'), `${role} may open Users`);
    assert.ok(pagesFor(role).includes('explore'), `${role} may open Search`);
  }
  assert.ok(pagesFor('admin').includes('audit'), 'admin reads the audit trail');
  assert.equal(pagesFor('admin').includes('person'), false, 'admin reads no per-person page');
  assert.equal(pagesFor('viewer').includes('audit'), false);
  assert.equal(pagesFor(null), null, 'no session means the sample mode shows everything');
});

test('content_reader carries the content capability; analyst does not', () => {
  assert.equal(can('content_reader', 'content'), true);
  assert.equal(can('analyst', 'content'), false);
  assert.equal(can('analyst', 'search'), true, 'search is an analyst capability');
  assert.equal(can('viewer', 'search'), false);
  assert.deepEqual(capabilitiesFor('nobody'), []);
});

// ---------------------------------------------------------------------------------------------
// The session store and cookies
// ---------------------------------------------------------------------------------------------

test('a session is created, read, and can be destroyed; expired ones are forgotten', () => {
  let clock = 1000;
  const store = createSessionStore({ ttlMs: 100, now: () => clock, random: () => 'sid-1' });
  const id = store.create({ actor: 'a@lab.test', tenant: TENANT, role: 'analyst', roles: ['analyst'], accessToken: 'at' });
  assert.equal(id, 'sid-1');
  assert.equal(store.get(id).actor, 'a@lab.test');
  clock = 1200;
  assert.equal(store.get(id), null, 'an expired session is not returned');
  assert.equal(store.size, 0);
});

test('the session cookie is opaque and httpOnly, and reads back', () => {
  const header = sessionCookie('abc', { secure: true });
  assert.match(header, /^sac_session=abc; HttpOnly; SameSite=Lax; Path=\/; Max-Age=\d+; Secure$/);
  assert.equal(readCookie(`other=x; ${header}`), 'abc');
  assert.deepEqual(parseCookies('a=1; b=2'), { a: '1', b: '2' });
  assert.equal(readCookie('nothing=1'), null);
});

// ---------------------------------------------------------------------------------------------
// PKCE and id_token verification
// ---------------------------------------------------------------------------------------------

test('the PKCE challenge is the S256 hash of the verifier', () => {
  const { verifier, challenge } = pkce();
  assert.equal(challenge, createHash('sha256').update(verifier).digest('base64url'));
  assert.notEqual(verifier.length, 0);
});

test('verifyJwt checks signature, issuer, audience, expiry and nonce', () => {
  const keys = [JWK];
  assert.equal(verifyJwt(idToken(claims()), { issuer: ISSUER, audience: CLIENT, keys, nonce: 'n1' }).sub, 's1');
  assert.throws(() => verifyJwt(idToken(claims({ iss: 'https://other' })), { issuer: ISSUER, audience: CLIENT, keys }), /issuer/);
  assert.throws(() => verifyJwt(idToken(claims({ aud: 'other' })), { issuer: ISSUER, audience: CLIENT, keys }), /audience/);
  assert.throws(() => verifyJwt(idToken(claims({ exp: 1 })), { issuer: ISSUER, audience: CLIENT, keys }), /expired/);
  assert.throws(() => verifyJwt(idToken(claims()), { issuer: ISSUER, audience: CLIENT, keys, nonce: 'wrong' }), /nonce/);
});

test('principalFromClaims refuses a token with no tenant or no known role', () => {
  assert.deepEqual(principalFromClaims(claims({ roles: ['analyst'] })), { actor: 'viewer@lab.test', tenant: TENANT, role: 'analyst', roles: ['analyst'] });
  assert.throws(() => principalFromClaims(claims({ sac_tenant: 'not-a-uuid' })), /tenant/);
  assert.throws(() => principalFromClaims(claims({ roles: ['superuser'] })), /role/);
});

// ---------------------------------------------------------------------------------------------
// The OIDC client
// ---------------------------------------------------------------------------------------------

function fakeIssuer() {
  const calls = { posts: [] };
  const fetchImpl = async (url, options = {}) => {
    if (url.endsWith('/.well-known/openid-configuration')) {
      return { ok: true, status: 200, json: async () => ({ authorization_endpoint: `${ISSUER}/authorize`, token_endpoint: `${ISSUER}/token`, jwks_uri: `${ISSUER}/jwks` }) };
    }
    if (url.endsWith('/jwks')) return { ok: true, status: 200, json: async () => ({ keys: [JWK] }) };
    if (url.endsWith('/token')) {
      calls.posts.push({ url, body: options.body });
      return { ok: true, status: 200, json: async () => ({ access_token: 'access-1', id_token: idToken(claims()), token_type: 'Bearer', expires_in: 900 }) };
    }
    throw new Error(`unexpected fetch ${url}`);
  };
  return { fetchImpl, calls };
}

test('the authorize URL carries PKCE, state and nonce, and honours the lab login hint', async () => {
  const { fetchImpl } = fakeIssuer();
  const client = createOidcClient({ issuer: ISSUER, clientId: CLIENT, redirectUri: 'http://dash/callback', fetchImpl });
  const url = new URL(await client.authorizeUrl({ state: 'st', nonce: 'no', codeChallenge: 'ch', loginHint: 'reader@lab.test' }));
  assert.equal(url.searchParams.get('code_challenge_method'), 'S256');
  assert.equal(url.searchParams.get('code_challenge'), 'ch');
  assert.equal(url.searchParams.get('state'), 'st');
  assert.equal(url.searchParams.get('nonce'), 'no');
  assert.equal(url.searchParams.get('login_hint'), 'reader@lab.test');
  assert.equal(url.searchParams.get('redirect_uri'), 'http://dash/callback');
});

test('the token exchange posts the code and verifier, and verifyIdToken accepts the result', async () => {
  const { fetchImpl, calls } = fakeIssuer();
  const client = createOidcClient({ issuer: ISSUER, clientId: CLIENT, redirectUri: 'http://dash/callback', fetchImpl });
  const tokens = await client.exchange({ code: 'the-code', codeVerifier: 'the-verifier' });
  assert.equal(tokens.access_token, 'access-1');
  assert.match(calls.posts[0].body, /code_verifier=the-verifier/);
  const verified = await client.verifyIdToken(tokens.id_token, { nonce: 'n1' });
  assert.equal(verified.sac_tenant, TENANT);
  await assert.rejects(() => client.verifyIdToken(tokens.id_token, { nonce: 'wrong' }), /nonce/);
});
