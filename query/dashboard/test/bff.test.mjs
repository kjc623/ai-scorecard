// bff.test.mjs — the dashboard server as a backend-for-frontend over control-api (tools/serve.mjs).
//
// Real HTTP on loopback, nothing else: a fake control-api (its internal identity API, its admin
// API and its onboarding pages) and a fake query-api are started on port 0, the dashboard server is
// pointed at them, and every assertion is about what crossed the wire — which cookie was set with
// which flags, which bearer reached which service, what was refused before it left.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer, request as httpRequest } from 'node:http';

import { createDashboardServer } from '../tools/serve.mjs';

const TENANT = '5a3c0de0-7e57-4a11-9000-0000000d3a01';
const OTHER_TENANT = '11111111-1111-1111-1111-111111111111';
const INTERNAL = 'internal-shared-secret';
const quiet = { warn() {}, error() {}, log() {} };

const ROLE_BY_CODE = Object.freeze({ viewer: ['viewer'], analyst: ['analyst'], reader: ['content_reader'], admin: ['admin'] });

function listen(server) {
  return new Promise((resolveListen) => server.listen(0, '127.0.0.1', () => resolveListen(server.address().port)));
}

async function readAll(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  return Buffer.concat(chunks);
}

/** A control-api: the four internal identity endpoints, the admin API and the onboarding pages. */
async function fakeControl() {
  const state = { calls: [], sessions: new Map(), minted: 0, tokenStatus: null, tenant: TENANT, unreachable: false };
  const server = createServer(async (req, res) => {
    const body = await readAll(req);
    const url = new URL(req.url, 'http://control');
    const call = { method: req.method, path: url.pathname, search: url.search, headers: req.headers, body: body.toString('utf8') };
    state.calls.push(call);
    const json = (status, value) => {
      res.writeHead(status, { 'content-type': 'application/json' });
      res.end(value === undefined ? undefined : JSON.stringify(value));
    };
    if (url.pathname.startsWith('/internal/')) {
      if (req.headers.authorization !== `Bearer ${INTERNAL}`) return json(401, { error: 'unauthenticated' });
      const input = JSON.parse(call.body || '{}');
      if (url.pathname === '/internal/v1/auth/begin') {
        if (String(input.email ?? '').endsWith('@nowhere.test')) return json(404, { error: 'no_sso_connection' });
        return json(200, { attempt: 'attempt-1', authorize_url: 'https://idp.example.test/authorize?state=st-1' });
      }
      if (url.pathname === '/internal/v1/auth/complete') {
        if (input.attempt !== 'attempt-1' || input.state !== 'st-1') return json(400, { error: 'bad_attempt' });
        if (['tenant_not_onboarded', 'no_role', 'connection_disabled', 'user_deactivated', 'invite_used', 'attempt_expired'].includes(input.code)) return json(403, { error: input.code });
        const roles = ROLE_BY_CODE[input.code] ?? ['viewer'];
        const session = `session-${input.code}-${state.sessions.size}-0123456789abcdef`;
        const principal = { tenant: state.tenant, actor: `${input.code}@lab.test`, roles, idp: 'oidc' };
        state.sessions.set(session, { principal, revoked: false });
        return json(200, { session, access_token: `at-${input.code}-0`, expires_in: 600, principal });
      }
      if (url.pathname === '/internal/v1/auth/token') {
        const s = state.sessions.get(input.session);
        if (!s || s.revoked || state.tokenStatus === 401) return json(401, { error: 'session_ended' });
        state.minted += 1;
        return json(200, { access_token: `at-refreshed-${state.minted}`, expires_in: 600, principal: s.principal });
      }
      if (url.pathname === '/internal/v1/auth/revoke') {
        const s = state.sessions.get(input.session);
        if (s) s.revoked = true;
        return json(204);
      }
    }
    if (url.pathname === '/admin/v1/deployment') return json(200, { device_verification: 'none', deployment_keys: [] });
    if (url.pathname === '/admin/v1/deployment/package') {
      res.writeHead(200, {
        'content-type': 'application/octet-stream',
        'content-disposition': 'attachment; filename="ShadowAICapture-sample.intunewin"',
        'x-sac-deployment-key-label': 'Laptops',
        'x-sac-deployment-key-id': 'k-1',
        'set-cookie': 'leak=1',
      });
      return res.end(Buffer.from([0, 1, 2, 3, 250, 251]));
    }
    if (url.pathname === '/admin/v1/deployment/verification') return json(204);
    if (url.pathname.startsWith('/onboard/')) {
      res.writeHead(302, { location: 'https://login.microsoftonline.com/organizations/v2.0/adminconsent?client_id=x', 'set-cookie': 'onboard_state=abc; HttpOnly; Path=/onboard' });
      return res.end();
    }
    return json(404, { error: 'not_found' });
  });
  const port = await listen(server);
  return { state, server, url: `http://127.0.0.1:${port}` };
}

/** A query-api that records what it was sent and answers one envelope. */
async function fakeQuery() {
  const calls = [];
  const server = createServer(async (req, res) => {
    const body = await readAll(req);
    calls.push({ method: req.method, path: req.url, headers: req.headers, body: body.toString('utf8') });
    res.writeHead(200, { 'content-type': 'application/json', 'set-cookie': 'upstream=1' });
    res.end(JSON.stringify({ api_version: '1', query_version: '1', result_state: 'ok', data: [] }));
  });
  const port = await listen(server);
  return { calls, server, url: `http://127.0.0.1:${port}` };
}

/** One request with full control of its headers: what a browser would send, cookie and all. */
function send(port, path, { method = 'GET', headers = {}, body } = {}) {
  return new Promise((resolveSend, reject) => {
    const req = httpRequest({ host: '127.0.0.1', port, path, method, headers: { host: `127.0.0.1:${port}`, ...headers } }, async (res) => {
      const raw = await readAll(res);
      resolveSend({ status: res.statusCode, headers: res.headers, text: raw.toString('utf8'), raw, json: () => JSON.parse(raw.toString('utf8')) });
    });
    req.on('error', reject);
    if (body !== undefined) req.write(body);
    req.end();
  });
}

const cookieValue = (setCookie, name) => {
  const line = (setCookie ?? []).find((c) => c.startsWith(`${name}=`));
  return line ? line.slice(name.length + 1).split(';')[0] : null;
};
const cookieLine = (setCookie, name) => (setCookie ?? []).find((c) => c.startsWith(`${name}=`)) ?? null;

/** A whole lab: fake upstreams and a dashboard over them. */
async function lab(t, overrides = {}) {
  const control = await fakeControl();
  const query = await fakeQuery();
  const clock = { now: 1_800_000_000_000 };
  const dashboard = createDashboardServer({
    controlUrl: control.url, internalToken: INTERNAL, queryApiUrl: query.url, log: quiet, now: () => clock.now, ...overrides,
  });
  const port = await listen(dashboard);
  t.after(() => {
    dashboard.close();
    control.server.close();
    query.server.close();
  });
  const origin = `http://127.0.0.1:${port}`;
  /** Sign in as a role through the real redirect dance; returns the session cookie value. */
  async function signIn(code, { next = '/index.html?transport=live' } = {}) {
    const started = await send(port, `/signin/start?email=${encodeURIComponent(`${code}@lab.test`)}&next=${encodeURIComponent(next)}`);
    assert.equal(started.status, 302, `begin for ${code}`);
    const attempt = cookieValue(started.headers['set-cookie'], 'sac_signin');
    const back = await send(port, `/callback?code=${code}&state=st-1`, { headers: { cookie: `sac_signin=${attempt}` } });
    return { response: back, session: cookieValue(back.headers['set-cookie'], 'sac_session') };
  }
  return { control, query, port, origin, clock, signIn };
}

// ---------------------------------------------------------------------------------------------
// The sign-in page and what an unauthenticated browser gets
// ---------------------------------------------------------------------------------------------

test('the sign-in page offers Microsoft and work-email SSO, loads nothing from another host, and runs no script', async (t) => {
  const { port } = await lab(t);
  const page = await send(port, '/signin?next=%2Fexplore.html');
  assert.equal(page.status, 200);
  assert.match(page.text, /Sign in with Microsoft/);
  assert.match(page.text, /Work email/);
  assert.match(page.text, /Continue with SSO/);
  assert.match(page.text, /name="next" value="\/explore.html"/);
  assert.doesNotMatch(page.text, /Lab development sign-in/, 'no development sign-in when an identity service is configured');
  assert.doesNotMatch(page.text, /<script/i);
  assert.doesNotMatch(page.text, /(?:src|href)="https?:/, 'every asset is local');
  assert.equal((await send(port, '/styles.css')).status, 200, 'the stylesheet is public');
  assert.equal((await send(port, '/signin.css')).status, 200);
  assert.equal(page.headers['x-frame-options'], 'DENY');
});

test('an unauthenticated page is sent to sign in; an unauthenticated API call is refused with JSON', async (t) => {
  const { port, query, control } = await lab(t);
  const pageRes = await send(port, '/index.html?transport=live');
  assert.equal(pageRes.status, 302);
  assert.equal(pageRes.headers.location, '/signin?next=%2Findex.html%3Ftransport%3Dlive');
  const api = await send(port, '/v1/query', { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{}' });
  assert.equal(api.status, 401);
  assert.equal(api.json().result_state, 'unauthorised_role');
  assert.equal(api.json().state, 'refused', 'the content reads read the same refusal');
  const admin = await send(port, '/admin/v1/deployment');
  assert.equal(admin.status, 401);
  assert.equal(admin.json().error, 'no_session');
  assert.equal((await send(port, '/session')).status, 401);
  assert.equal(query.calls.length, 0, 'nothing reached query-api');
  assert.equal(control.state.calls.filter((c) => c.path.startsWith('/admin/')).length, 0, 'nothing reached the admin API');
});

// ---------------------------------------------------------------------------------------------
// Begin and complete
// ---------------------------------------------------------------------------------------------

test('a work email begins a sign-in at control-api and sends the browser to the provider with a short-lived attempt cookie', async (t) => {
  const { port, control } = await lab(t);
  const res = await send(port, '/signin/start?email=reader%40lab.test&next=%2Fexplore.html%3Ftransport%3Dlive');
  assert.equal(res.status, 302);
  assert.equal(res.headers.location, 'https://idp.example.test/authorize?state=st-1');
  const begin = control.state.calls.find((c) => c.path === '/internal/v1/auth/begin');
  assert.equal(begin.headers.authorization, `Bearer ${INTERNAL}`);
  assert.deepEqual(JSON.parse(begin.body), { redirect_uri: `http://127.0.0.1:${port}/callback`, email: 'reader@lab.test' });
  const line = cookieLine(res.headers['set-cookie'], 'sac_signin');
  assert.match(line, /; HttpOnly; SameSite=Lax; Path=\/callback; Max-Age=600$/, 'HttpOnly, Lax, only sent to /callback, ten minutes, not Secure on http');
});

test('"Sign in with Microsoft" posts from the page and begins with provider=entra', async (t) => {
  const { port, origin, control } = await lab(t);
  const res = await send(port, '/signin/start', {
    method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded', origin, 'sec-fetch-site': 'same-origin' }, body: 'provider=entra&next=%2F',
  });
  assert.equal(res.status, 302);
  assert.deepEqual(JSON.parse(control.state.calls.find((c) => c.path === '/internal/v1/auth/begin').body), { redirect_uri: `${origin}/callback`, provider: 'entra' });
});

test('an onboarding invite is passed to begin, so the first sign-in can activate the connection', async (t) => {
  const { port, control } = await lab(t);
  await send(port, '/signin/start?provider=entra&invite=sacinv_abc');
  assert.deepEqual(JSON.parse(control.state.calls.find((c) => c.path === '/internal/v1/auth/begin').body).invite, 'sacinv_abc');
});

test('an email with no SSO connection gets a plain page that keeps the address', async (t) => {
  const { port } = await lab(t);
  const res = await send(port, '/signin/start?email=someone%40nowhere.test');
  assert.equal(res.status, 404);
  assert.match(res.text, /No single sign-on for that address/);
  assert.match(res.text, /value="someone@nowhere.test"/);
  assert.match(res.text, /no_sso_connection/, 'the short code a help desk can search for');
});

test('the callback completes at control-api and sets the session cookie with the right flags', async (t) => {
  const { control, signIn } = await lab(t);
  const { response, session } = await signIn('analyst', { next: '/explore.html?transport=live' });
  assert.equal(response.status, 302);
  assert.equal(response.headers.location, '/explore.html?transport=live');
  assert.ok(session, 'a session cookie was set');
  const line = cookieLine(response.headers['set-cookie'], 'sac_session');
  assert.match(line, /^sac_session=session-analyst-\d+-0123456789abcdef; HttpOnly; SameSite=Lax; Path=\/; Max-Age=28800$/);
  assert.match(cookieLine(response.headers['set-cookie'], 'sac_signin'), /Max-Age=0/, 'the attempt cookie is cleared');
  const complete = control.state.calls.find((c) => c.path === '/internal/v1/auth/complete');
  assert.deepEqual(JSON.parse(complete.body), { attempt: 'attempt-1', code: 'analyst', state: 'st-1' });
  assert.equal(response.headers['referrer-policy'], 'no-referrer', 'the code in the callback address goes nowhere');
});

test('with an https public URL the cookies are Secure and the redirect URI is the public one', async (t) => {
  const { port, control } = await lab(t, { publicUrl: 'https://dash.example.test', cookieSecure: true });
  const started = await send(port, '/signin/start?email=admin%40lab.test');
  assert.match(cookieLine(started.headers['set-cookie'], 'sac_signin'), /; Secure$/);
  assert.equal(JSON.parse(control.state.calls.find((c) => c.path === '/internal/v1/auth/begin').body).redirect_uri, 'https://dash.example.test/callback');
  const attempt = cookieValue(started.headers['set-cookie'], 'sac_signin');
  const back = await send(port, '/callback?code=admin&state=st-1', { headers: { cookie: `sac_signin=${attempt}` } });
  assert.match(cookieLine(back.headers['set-cookie'], 'sac_session'), /HttpOnly; SameSite=Lax; Path=\/; Max-Age=28800; Secure$/);
  assert.equal(back.headers.location, '/index.html?transport=live', 'a sign-in with nowhere to return to lands on the live dashboard');
});

for (const [code, title] of [['tenant_not_onboarded', 'Your organisation is not set up yet'], ['no_role', 'No access has been assigned to you'], ['connection_disabled', 'Sign-in is turned off for your organisation']]) {
  test(`a ${code} refusal is a plain page, with no session and no stack trace`, async (t) => {
    const { port } = await lab(t);
    const started = await send(port, '/signin/start?email=x%40lab.test');
    const attempt = cookieValue(started.headers['set-cookie'], 'sac_signin');
    const res = await send(port, `/callback?code=${code}&state=st-1`, { headers: { cookie: `sac_signin=${attempt}` } });
    assert.equal(res.status, 403);
    assert.match(res.text, new RegExp(title));
    assert.match(res.text, new RegExp(code));
    assert.equal(cookieValue(res.headers['set-cookie'], 'sac_session'), null, 'no session cookie');
    assert.doesNotMatch(res.text, /\bat [\w.<>]+ \(|Error:|node:internal|stack/i, 'no stack trace');
  });
}

test('a callback without its attempt cookie, or one the provider refused, says so plainly', async (t) => {
  const { port } = await lab(t);
  const expired = await send(port, '/callback?code=viewer&state=st-1');
  assert.equal(expired.status, 400);
  assert.match(expired.text, /That sign-in expired/);
  const refused = await send(port, '/callback?error=access_denied&error_description=%3Cscript%3Ealert(1)%3C%2Fscript%3E');
  assert.equal(refused.status, 400);
  assert.match(refused.text, /Your identity provider stopped the sign-in/);
  assert.match(refused.text, /access_denied/);
  assert.doesNotMatch(refused.text, /<script>alert/, 'the provider\'s own text is not echoed');
});

test('control-api unreachable at sign-in is a "try again" page, not an error dump', async (t) => {
  const { port } = await lab(t, { controlUrl: 'http://127.0.0.1:9', internalToken: INTERNAL });
  const res = await send(port, '/signin/start?email=a%40lab.test');
  assert.equal(res.status, 503);
  assert.match(res.text, /Sign-in is unavailable/);
  assert.doesNotMatch(res.text, /ECONNREFUSED|fetch failed/);
});

test('a dashboard pinned to a tenant refuses, and revokes, a sign-in for another', async (t) => {
  const { port, control } = await lab(t, { pinnedTenant: OTHER_TENANT });
  const started = await send(port, '/signin/start?email=x%40lab.test');
  const attempt = cookieValue(started.headers['set-cookie'], 'sac_signin');
  const res = await send(port, '/callback?code=admin&state=st-1', { headers: { cookie: `sac_signin=${attempt}` } });
  assert.equal(res.status, 403);
  assert.match(res.text, /serves another organisation/);
  assert.ok(control.state.calls.some((c) => c.path === '/internal/v1/auth/revoke'), 'the session was revoked');
  assert.equal(cookieValue(res.headers['set-cookie'], 'sac_session'), null);
});

test('the server will not start against control-api without the internal token', () => {
  assert.throws(() => createDashboardServer({ controlUrl: 'http://control', internalToken: '', log: quiet }), /SAC_INTERNAL_TOKEN/);
});

// ---------------------------------------------------------------------------------------------
// Forwarding
// ---------------------------------------------------------------------------------------------

test('/v1/* reaches query-api with the product token and never the cookie', async (t) => {
  const { port, origin, query, signIn } = await lab(t);
  const { session } = await signIn('reader');
  const body = JSON.stringify({ query_version: '1', template: 'q7_devices' });
  const res = await send(port, '/v1/query', {
    method: 'POST', headers: { cookie: `sac_session=${session}; other=1`, 'content-type': 'application/json', origin, 'sec-fetch-site': 'same-origin', authorization: 'Bearer forged' }, body,
  });
  assert.equal(res.status, 200);
  assert.equal(res.json().result_state, 'ok');
  assert.equal(res.headers['set-cookie'], undefined, 'an upstream cookie is not relayed to the browser');
  const seen = query.calls.at(-1);
  assert.equal(seen.path, '/v1/query');
  assert.equal(seen.headers.authorization, 'Bearer at-reader-0', 'the session\'s product token, not the one the browser sent');
  assert.equal(seen.headers.cookie, undefined, 'no cookie leaves this server');
  assert.equal(seen.body, body);
  const search = await send(port, '/v1/content-search', { method: 'POST', headers: { cookie: `sac_session=${session}`, 'content-type': 'application/json', origin }, body: '{"query":"x"}' });
  assert.equal(search.status, 200);
  assert.equal(query.calls.at(-1).path, '/v1/content-search', 'every /v1 path is forwarded, not only the query');
});

test('/admin/v1/* reaches control-api with the product token, and a package download arrives byte for byte', async (t) => {
  const { port, origin, control, signIn } = await lab(t);
  const { session } = await signIn('admin');
  const read = await send(port, '/admin/v1/deployment', { headers: { cookie: `sac_session=${session}` } });
  assert.equal(read.status, 200);
  const seen = control.state.calls.filter((c) => c.path === '/admin/v1/deployment').at(-1);
  assert.equal(seen.headers.authorization, 'Bearer at-admin-0');
  assert.equal(seen.headers.cookie, undefined);
  const pkg = await send(port, '/admin/v1/deployment/package', {
    method: 'POST', headers: { cookie: `sac_session=${session}`, 'content-type': 'application/json', origin, 'sec-fetch-site': 'same-origin' }, body: '{"format":"intunewin","label":"Laptops"}',
  });
  assert.equal(pkg.status, 200);
  assert.deepEqual([...pkg.raw], [0, 1, 2, 3, 250, 251]);
  assert.equal(pkg.headers['content-disposition'], 'attachment; filename="ShadowAICapture-sample.intunewin"');
  assert.equal(pkg.headers['x-sac-deployment-key-label'], 'Laptops');
  assert.equal(pkg.headers['set-cookie'], undefined);
  assert.equal(control.state.calls.at(-1).body, '{"format":"intunewin","label":"Laptops"}');
});

test('a non-admin\'s /admin/v1 request is refused here and never leaves', async (t) => {
  const { port, control, signIn } = await lab(t);
  const { session } = await signIn('reader');
  const res = await send(port, '/admin/v1/deployment', { headers: { cookie: `sac_session=${session}` } });
  assert.equal(res.status, 403);
  assert.equal(res.json().error, 'forbidden');
  assert.equal(control.state.calls.filter((c) => c.path.startsWith('/admin/')).length, 0);
});

test('a state-changing request from another site is refused before it is forwarded', async (t) => {
  const { port, query, control, signIn } = await lab(t);
  const reader = (await signIn('reader')).session;
  const admin = (await signIn('admin')).session;
  const cases = [
    ['/v1/query', reader, 'POST', { origin: 'https://evil.test' }],
    ['/v1/query', reader, 'POST', { 'sec-fetch-site': 'cross-site', origin: `http://127.0.0.1:${port}` }],
    ['/admin/v1/deployment/verification', admin, 'PUT', { 'sec-fetch-site': 'same-site' }],
    ['/admin/v1/deployment/package', admin, 'POST', { origin: 'null' }],
  ];
  for (const [path, session, method, headers] of cases) {
    const res = await send(port, path, { method, headers: { cookie: `sac_session=${session}`, 'content-type': 'application/json', ...headers }, body: '{}' });
    assert.equal(res.status, 403, `${method} ${path} ${JSON.stringify(headers)}`);
    assert.match(res.text, /cross_site_request/);
  }
  assert.equal(query.calls.length, 0, 'nothing reached query-api');
  assert.equal(control.state.calls.filter((c) => c.path.startsWith('/admin/')).length, 0, 'nothing reached the admin API');
  const read = await send(port, '/admin/v1/deployment', { headers: { cookie: `sac_session=${admin}`, 'sec-fetch-site': 'cross-site' } });
  assert.equal(read.status, 200, 'a read is not a change: only state-changing methods are checked');
});

test('/onboard/* goes to control-api untouched: no session, no bearer, its redirect and cookie relayed', async (t) => {
  const { port, control } = await lab(t);
  const res = await send(port, '/onboard/sacinv_abc/entra?step=1', { method: 'POST', headers: { cookie: 'sac_session=mine; onboard_pref=1', 'content-type': 'application/x-www-form-urlencoded' }, body: 'choice=entra' });
  assert.equal(res.status, 302);
  assert.match(res.headers.location, /^https:\/\/login\.microsoftonline\.com\/organizations\/v2\.0\/adminconsent/);
  assert.deepEqual(res.headers['set-cookie'], ['onboard_state=abc; HttpOnly; Path=/onboard']);
  const seen = control.state.calls.at(-1);
  assert.equal(seen.path, '/onboard/sacinv_abc/entra');
  assert.equal(seen.search, '?step=1');
  assert.equal(seen.method, 'POST');
  assert.equal(seen.body, 'choice=entra');
  assert.equal(seen.headers.authorization, undefined, 'no bearer is added');
  assert.equal(seen.headers.cookie, 'onboard_pref=1', 'this server\'s own cookie is not passed on');
});

// ---------------------------------------------------------------------------------------------
// The token cache, session end and sign-out
// ---------------------------------------------------------------------------------------------

test('the product token is re-minted before it expires, once for a burst of requests', async (t) => {
  const { port, origin, query, control, clock, signIn } = await lab(t);
  const { session } = await signIn('analyst');
  const ask = () => send(port, '/v1/query', { method: 'POST', headers: { cookie: `sac_session=${session}`, 'content-type': 'application/json', origin }, body: '{}' });
  await ask();
  assert.equal(query.calls.at(-1).headers.authorization, 'Bearer at-analyst-0', 'the sign-in token is used first, with no refresh');
  assert.equal(control.state.minted, 0);
  clock.now += 560_000; // 40 s before the 600 s token expires: inside the refresh margin
  await Promise.all([ask(), ask(), ask()]);
  assert.equal(control.state.minted, 1, 'one refresh for three requests');
  assert.ok(query.calls.slice(-3).every((c) => c.headers.authorization === 'Bearer at-refreshed-1'));
});

test('a session control-api has ended clears the cookie: an API call gets 401, a page goes to sign in', async (t) => {
  const { port, control, clock, signIn } = await lab(t);
  const { session } = await signIn('viewer');
  control.state.tokenStatus = 401;
  clock.now += 600_000;
  const api = await send(port, '/v1/query', { method: 'POST', headers: { cookie: `sac_session=${session}`, 'content-type': 'application/json' }, body: '{}' });
  assert.equal(api.status, 401);
  assert.equal(api.json().error.code, 'session_ended');
  assert.match(cookieLine(api.headers['set-cookie'], 'sac_session'), /Max-Age=0/);
  const pageRes = await send(port, '/index.html', { headers: { cookie: `sac_session=${session}` } });
  assert.equal(pageRes.status, 302);
  assert.match(pageRes.headers.location, /^\/signin\?next=%2Findex\.html&notice=session_ended$/);
});

test('sign-out revokes the session at control-api and clears the cookie', async (t) => {
  const { port, origin, control, signIn } = await lab(t);
  const { session } = await signIn('admin');
  const out = await send(port, '/signout', { method: 'POST', headers: { cookie: `sac_session=${session}`, origin, 'sec-fetch-site': 'same-origin' } });
  assert.equal(out.status, 302);
  assert.equal(out.headers.location, '/signin?notice=signed_out');
  assert.match(cookieLine(out.headers['set-cookie'], 'sac_session'), /^sac_session=; HttpOnly; SameSite=Lax; Path=\/; Max-Age=0$/);
  assert.equal(JSON.parse(control.state.calls.find((c) => c.path === '/internal/v1/auth/revoke').body).session, session);
  const after = await send(port, '/admin/v1/deployment', { headers: { cookie: `sac_session=${session}` } });
  assert.equal(after.status, 401, 'the old cookie no longer works, on this instance or any other');
  const page = await send(port, '/signin?notice=signed_out');
  assert.match(page.text, /You have signed out/);
});

// ---------------------------------------------------------------------------------------------
// What a role is shown, and what it is served
// ---------------------------------------------------------------------------------------------

test('GET /session names the pages each role may open; Search is not served to a viewer', async (t) => {
  const { port, signIn } = await lab(t);
  const viewer = (await signIn('viewer')).session;
  const admin = (await signIn('admin')).session;
  const reader = (await signIn('reader')).session;
  const viewerSession = (await send(port, '/session', { headers: { cookie: `sac_session=${viewer}` } })).json();
  assert.deepEqual(viewerSession.roles, ['viewer']);
  assert.equal(viewerSession.pages.includes('explore'), false);
  assert.equal(viewerSession.pages.includes('deployment'), false);
  assert.equal(viewerSession.actor, 'viewer@lab.test');
  assert.equal(JSON.stringify(viewerSession).includes('at-viewer'), false, 'the page is never given a token');
  const adminSession = (await send(port, '/session', { headers: { cookie: `sac_session=${admin}` } })).json();
  assert.ok(adminSession.pages.includes('deployment'));
  const refused = await send(port, '/explore.html?transport=live', { headers: { cookie: `sac_session=${viewer}` } });
  assert.equal(refused.status, 403);
  assert.match(refused.text, /Your role cannot open this page/);
  assert.equal((await send(port, '/explore.html?transport=live', { headers: { cookie: `sac_session=${reader}` } })).status, 200);
  assert.equal((await send(port, '/', { headers: { cookie: `sac_session=${reader}` } })).headers.location, '/index.html?transport=live');
  assert.equal((await send(port, '/tools/serve.mjs', { headers: { cookie: `sac_session=${reader}` } })).status, 404, 'only the pages are served, not the server\'s source');
});

// ---------------------------------------------------------------------------------------------
// The lab's development principal, unchanged
// ---------------------------------------------------------------------------------------------

test('without SAC_CONTROL_URL the development principal is forwarded, and the sign-in page says it is one', async (t) => {
  const query = await fakeQuery();
  const dashboard = createDashboardServer({ queryApiUrl: query.url, devTenant: TENANT, devActor: 'dev@lab.test', log: quiet });
  const port = await listen(dashboard);
  t.after(() => {
    dashboard.close();
    query.server.close();
  });
  const page = await send(port, '/signin');
  assert.match(page.text, /Lab development sign-in/);
  assert.doesNotMatch(page.text, /Sign in with Microsoft/, 'no sign-in form that cannot work');
  const res = await send(port, '/v1/query', { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{}' });
  assert.equal(res.status, 200);
  assert.equal(query.calls[0].headers['x-sac-dev-tenant'], TENANT);
  assert.equal(query.calls[0].headers['x-sac-dev-actor'], 'dev@lab.test');
  assert.equal(query.calls[0].headers.authorization, undefined);
  const session = (await send(port, '/session')).json();
  assert.equal(session.dev, true);
  assert.ok(session.pages.includes('explore') && session.pages.includes('audit'), 'the lab principal is offered every page, as query-api grants it every capability');
  assert.equal((await send(port, '/admin/v1/deployment')).status, 503, 'no admin API without control-api');
});

test('control-api\'s onboarding hand-off to /login?invite=&provider= begins the activating sign-in', async (t) => {
  const { port, control } = await lab(t);
  const hop = await send(port, '/login?invite=sacinv_abc&provider=oidc');
  assert.equal(hop.status, 302);
  assert.match(hop.headers.location, /^\/signin\/start\?invite=sacinv_abc&provider=oidc&next=%2F$/);
  const started = await send(port, hop.headers.location);
  assert.equal(started.status, 302);
  assert.deepEqual(JSON.parse(control.state.calls.find((c) => c.path === '/internal/v1/auth/begin').body), { redirect_uri: `http://127.0.0.1:${port}/callback`, provider: 'oidc', invite: 'sacinv_abc' });
  assert.equal((await send(port, '/login?next=%2Fexplore.html')).headers.location, '/signin?next=%2Fexplore.html');
});

test('control-api\'s other refusal codes get their own sentence, with control-api\'s code beside it', async (t) => {
  const { port } = await lab(t);
  for (const [code, title] of [['user_deactivated', 'Your account is not active'], ['invite_used', 'This onboarding link cannot be used'], ['attempt_expired', 'That sign-in expired']]) {
    const started = await send(port, '/signin/start?email=x%40lab.test');
    const attempt = cookieValue(started.headers['set-cookie'], 'sac_signin');
    const res = await send(port, `/callback?code=${code}&state=st-1`, { headers: { cookie: `sac_signin=${attempt}` } });
    assert.match(res.text, new RegExp(title), code);
    assert.match(res.text, new RegExp(code), `${code} is shown for a help desk`);
  }
});
