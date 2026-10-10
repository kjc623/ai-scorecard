// session-tools.test.mjs — the dashboard server's session helpers (server/session.mjs).
//
// Node-side, no browser: a fake fetch plays control-api's internal identity
// API. The server-level behaviour (cookies on the wire, forwarding, CSRF) is in test/bff.test.mjs.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  can,
  canAny,
  capabilitiesFor,
  cookieHeader,
  createIdentityClient,
  createTokenCache,
  decodeAttempt,
  encodeAttempt,
  isStateChanging,
  pagesForRoles,
  primaryRole,
  principalFrom,
  readCookie,
  safeNext,
  sameOriginRequest,
  sessionCookie,
  sessionKey,
  signinCookie,
  withoutOwnCookies,
} from '../server/session.mjs';

const pagesFor = (role) => pagesForRoles([role]);

const TENANT = '00000000-0000-4000-8000-0000000000aa';

// ---------------------------------------------------------------------------------------------
// Roles to pages
// ---------------------------------------------------------------------------------------------

test('each role maps to the pages it may open', () => {
  assert.deepEqual(pagesFor('viewer').sort(), ['devices', 'posture', 'teams', 'tools']);
  for (const role of ['analyst', 'content_reader']) {
    assert.ok(pagesFor(role).includes('person'), `${role} may open Users`);
    assert.ok(pagesFor(role).includes('explore'), `${role} may open Search`);
    assert.equal(pagesFor(role).includes('deployment'), false, `${role} may not open Settings`);
  }
  assert.ok(pagesFor('admin').includes('audit'), 'admin reads the audit trail');
  assert.ok(pagesFor('admin').includes('deployment'), 'admin opens Settings → Deployment');
  for (const id of ['person', 'explore', 'devices']) assert.ok(pagesFor('admin').includes(id), `admin opens ${id}`);
  assert.equal(pagesFor('viewer').includes('audit'), false);
  assert.equal(pagesFor('viewer').includes('explore'), false, 'a viewer is not offered Search');
});

test('several roles open the union of their pages, because roles are not a ladder', () => {
  const pages = pagesForRoles(['admin', 'content_reader']);
  for (const id of ['audit', 'deployment', 'explore', 'person', 'devices']) assert.ok(pages.includes(id), id);
  assert.deepEqual(pagesForRoles([]), []);
  assert.equal(primaryRole(['viewer', 'content_reader']), 'content_reader');
  assert.equal(primaryRole(['content_reader', 'admin']), 'admin');
});

test('content_reader carries the content capability; analyst does not', () => {
  assert.equal(can('content_reader', 'content'), true);
  assert.equal(can('admin', 'content'), true, 'admin carries every capability');
  assert.equal(can('analyst', 'content'), false);
  assert.equal(can('analyst', 'search'), true, 'search is an analyst capability');
  assert.equal(can('viewer', 'search'), false);
  assert.equal(canAny(['viewer', 'admin'], 'settings'), true);
  assert.deepEqual(capabilitiesFor('nobody'), []);
});

test('a principal from control-api is checked: a uuid tenant, a product role and an actor', () => {
  assert.deepEqual(principalFrom({ tenant: TENANT.toUpperCase(), actor: 'a@x.test', roles: ['viewer', 'superuser'], idp: 'oidc' }),
    { tenant: TENANT, actor: 'a@x.test', roles: ['viewer'], idp: 'oidc' });
  assert.equal(principalFrom({ tenant: 'not-a-uuid', actor: 'a', roles: ['viewer'] }), null);
  assert.equal(principalFrom({ tenant: TENANT, actor: 'a', roles: ['superuser'] }), null, 'a role outside the product set is not a session');
  assert.equal(principalFrom({ tenant: TENANT, actor: '', roles: ['viewer'] }), null);
});

// ---------------------------------------------------------------------------------------------
// Cookies, the sign-in attempt, and where a sign-in may return to
// ---------------------------------------------------------------------------------------------

test('the session cookie is HttpOnly, SameSite=Lax, Path=/, and Secure only when asked', () => {
  assert.equal(sessionCookie('abc'), 'sac_session=abc; HttpOnly; SameSite=Lax; Path=/; Max-Age=28800');
  assert.match(sessionCookie('abc', { secure: true }), /; Secure$/);
  assert.match(sessionCookie('', { maxAgeSec: 0 }), /^sac_session=; .*Max-Age=0/);
  assert.equal(readCookie(`other=x; ${sessionCookie('abc')}`), 'abc');
  assert.equal(readCookie('nothing=1'), null);
});

test('the sign-in attempt cookie is sent only to /callback and lives ten minutes', () => {
  assert.equal(signinCookie('v'), 'sac_signin=v; HttpOnly; SameSite=Lax; Path=/callback; Max-Age=600');
  assert.equal(cookieHeader('n', 'v', { path: '/', secure: true }), 'n=v; HttpOnly; SameSite=Lax; Path=/; Secure');
});

test('the attempt round-trips with its return path, and a foreign return path is replaced', () => {
  assert.deepEqual(decodeAttempt(encodeAttempt({ attempt: 'att.1', next: '/index.html?view=classes' })), { attempt: 'att.1', next: '/index.html?view=classes' });
  assert.equal(decodeAttempt(encodeAttempt({ attempt: 'a', next: 'https://evil.test/' })).next, '/');
  assert.equal(decodeAttempt('not base64 json'), null);
  assert.equal(decodeAttempt(null), null);
});

test('safeNext keeps a path on this origin and nothing else', () => {
  assert.equal(safeNext('/explore.html?x=1'), '/explore.html?x=1');
  for (const bad of ['//evil.test/', '/\\evil.test', 'https://evil.test', 'javascript:alert(1)', '/a\u0000b', '']) assert.equal(safeNext(bad), '/', JSON.stringify(bad));
});

test('this server\'s own cookies are removed from a request forwarded elsewhere', () => {
  assert.equal(withoutOwnCookies('sac_session=s; theirs=1; sac_signin=a; other=2'), 'theirs=1; other=2');
  assert.equal(withoutOwnCookies(undefined), '');
});

test('the same-origin check refuses a cross-site browser request and lets a non-browser one through', () => {
  const origins = ['https://dash.example.test', 'http://dashboard:8787'];
  assert.equal(sameOriginRequest({ 'sec-fetch-site': 'same-origin' }, origins), true);
  assert.equal(sameOriginRequest({ 'sec-fetch-site': 'cross-site', origin: 'https://dash.example.test' }, origins), false, 'Sec-Fetch-Site decides when present');
  assert.equal(sameOriginRequest({ 'sec-fetch-site': 'same-site' }, origins), false, 'a sibling subdomain is another origin');
  assert.equal(sameOriginRequest({ origin: 'https://evil.test' }, origins), false);
  assert.equal(sameOriginRequest({ origin: 'null' }, origins), false);
  assert.equal(sameOriginRequest({ origin: 'http://dashboard:8787' }, origins), true);
  assert.equal(sameOriginRequest({}, origins), true, 'no browser signal: curl or a test, carrying no victim cookie');
  assert.equal(isStateChanging('POST'), true);
  assert.equal(isStateChanging('GET'), false);
});

// ---------------------------------------------------------------------------------------------
// The identity client and the token cache
// ---------------------------------------------------------------------------------------------

function fakeControl(handlers) {
  const calls = [];
  const fetchImpl = async (url, options = {}) => {
    const path = new URL(url).pathname;
    calls.push({ path, headers: options.headers, body: JSON.parse(options.body ?? '{}') });
    const handler = handlers[path];
    if (!handler) throw new Error(`unexpected ${path}`);
    const [status, body] = await handler(JSON.parse(options.body ?? '{}'));
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
  };
  return { fetchImpl, calls };
}

test('the identity client calls the four internal endpoints with the internal bearer', async () => {
  const { fetchImpl, calls } = fakeControl({
    '/internal/v1/auth/begin': () => [200, { attempt: 'at1', authorize_url: 'https://idp/authorize' }],
    '/internal/v1/auth/complete': () => [403, { error: 'no_role' }],
    '/internal/v1/auth/token': () => [401, { error: 'session_ended' }],
    '/internal/v1/auth/revoke': () => [204, null],
  });
  const client = createIdentityClient({ baseUrl: 'http://control:8080/', internalToken: 'internal-secret', fetchImpl });
  const begun = await client.begin({ email: 'a@x.test', redirectUri: 'http://dash/callback' });
  assert.deepEqual(begun, { ok: true, status: 200, body: { attempt: 'at1', authorize_url: 'https://idp/authorize' } });
  assert.deepEqual(calls[0].body, { redirect_uri: 'http://dash/callback', email: 'a@x.test' });
  assert.equal(calls[0].headers.authorization, 'Bearer internal-secret');
  assert.deepEqual(await client.complete({ attempt: 'at1', code: 'c', state: 's' }), { ok: false, status: 403, error: 'no_role' });
  assert.deepEqual(await client.token('s1'), { ok: false, status: 401, error: 'session_ended' });
  assert.equal((await client.revoke('s1')).ok, true);
  assert.throws(() => createIdentityClient({ baseUrl: 'http://c', internalToken: '' }), /internal bearer/);
});

test('an unreachable identity service is an answer, not a throw', async () => {
  const client = createIdentityClient({ baseUrl: 'http://control', internalToken: 't', fetchImpl: async () => { throw new TypeError('fetch failed'); } });
  assert.deepEqual(await client.token('s'), { ok: false, status: 0, error: 'identity_unreachable' });
});

test('the token cache serves a fresh token, refreshes before expiry, and shares one refresh between callers', async () => {
  let clock = 1_000_000;
  let minted = 0;
  const identity = {
    async token() {
      minted += 1;
      await new Promise((r) => setTimeout(r, 5));
      return { ok: true, status: 200, body: { access_token: `t${minted}`, expires_in: 600, principal: { tenant: TENANT, actor: 'a@x.test', roles: ['analyst'] } } };
    },
  };
  const cache = createTokenCache({ identity, now: () => clock, refreshBeforeMs: 60_000 });
  assert.equal(cache.seed('sid', { access_token: 't0', expires_in: 600, principal: { tenant: TENANT, actor: 'a@x.test', roles: ['analyst'] } }), true);
  assert.equal((await cache.get('sid')).token, 't0', 'the token from sign-in is used first');
  assert.equal(minted, 0);
  clock += 545_000; // 55 s before expiry: inside the refresh margin
  const [a, b, c] = await Promise.all([cache.get('sid'), cache.get('sid'), cache.get('sid')]);
  assert.equal(minted, 1, 'three requests, one refresh');
  assert.deepEqual([a.token, b.token, c.token], ['t1', 't1', 't1']);
  assert.equal((await cache.get('sid')).token, 't1', 'the refreshed token is cached');
  assert.equal(minted, 1);
});

test('a session control-api has ended is dropped; an unreachable control-api does not end a still-valid token', async () => {
  let clock = 0;
  let answer = { ok: false, status: 0, error: 'identity_unreachable' };
  const cache = createTokenCache({ identity: { token: async () => answer }, now: () => clock, refreshBeforeMs: 60_000 });
  cache.seed('sid', { access_token: 't0', expires_in: 100, principal: { tenant: TENANT, actor: 'a', roles: ['viewer'] } });
  const stillValid = await cache.get('sid');
  assert.equal(stillValid.ok, true, 'within the refresh margin, unreachable: the unexpired token is used');
  clock = 101_000;
  assert.deepEqual(await cache.get('sid'), { ok: false, reason: 'identity_unavailable' }, 'expired and unreachable: no token');
  answer = { ok: false, status: 401, error: 'session_ended' };
  assert.deepEqual(await cache.get('sid'), { ok: false, reason: 'session_ended' });
  assert.equal(cache.size, 0);
  assert.deepEqual(await cache.get(null), { ok: false, reason: 'no_session' });
});

test('a token refused with tenant_closed is dropped with a tenant_closed reason, distinct from session_ended', async () => {
  let clock = 0;
  const cache = createTokenCache({ identity: { token: async () => ({ ok: false, status: 403, error: 'tenant_closed' }) }, now: () => clock, refreshBeforeMs: 60_000 });
  cache.seed('sid', { access_token: 't0', expires_in: 100, principal: { tenant: TENANT, actor: 'a', roles: ['viewer'] } });
  clock = 101_000;
  assert.deepEqual(await cache.get('sid'), { ok: false, reason: 'tenant_closed' });
  assert.equal(cache.size, 0);
});

test('the cache is keyed by a hash of the session id, never the id itself', () => {
  assert.match(sessionKey('secret-session'), /^[0-9a-f]{64}$/);
  assert.notEqual(sessionKey('secret-session'), 'secret-session');
});
