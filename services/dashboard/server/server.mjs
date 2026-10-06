// server.mjs — the dashboard server: the pages, sign-in, and the forwarders behind them.
//
// A thin backend-for-frontend over control-api's identity service. It
//
//   * runs the sign-in pages: /signin, then /signin/start asks control-api to begin (by work email,
//     by "Sign in with Microsoft", or with an onboarding invite) and sends the browser to the
//     provider; /callback hands the code back to control-api and sets the session cookie;
//     /signout revokes the session;
//   * holds the product token per session in memory (session.mjs), re-minted through
//     /internal/v1/auth/token shortly before it expires;
//   * forwards /v1/* to query-api and /admin/v1/* to control-api with
//     `Authorization: Bearer <product token>`, and never the cookie; /onboard/* goes to control-api
//     untouched, because the invite in it is the credential and there is no session yet;
//   * forwards a minted retrieval URL straight to content-vault, so content never transits
//     query-api; the single-use grant in the URL is the capability;
//   * refuses a state-changing /v1 or /admin/v1 request that did not come from this origin.
//
// Every page and read needs a session except the sign-in pages and their two stylesheets,
// /onboard/*, the minted retrieval URL and the two probes.

import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { extname, join, normalize, resolve, sep, dirname } from 'node:path';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { fileURLToPath } from 'node:url';

import {
  SIGNIN_COOKIE,
  canAny,
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
  signinCookie,
  withoutOwnCookies,
} from './session.mjs';
import { renderSigninPage, signinMessage } from './signin-page.mjs';

/** The package root: the pages, their stylesheets and src/ are served from here. */
export const PACKAGE_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

const CONTENT_RETRIEVAL_PATH = '/v1/content/retrieval';
// A minted retrieval URL is GET /v1/content/retrieval/<tenant>/<grant>; the POST that mints it has
// no trailing segment and goes to query-api like every other /v1 request.
const RETRIEVAL_PREFIX = `${CONTENT_RETRIEVAL_PATH}/`;
const MAX_BODY_BYTES = 256 * 1024;
/** The sign-in page's own assets. Everything else served from disk needs a session. */
const PUBLIC_ASSETS = new Set(['/styles.css', '/signin.css']);
/** What this server serves from disk: the pages, their stylesheets, and the modules they import. */
const SERVABLE = /^\/(?:[\w.-]+\.(?:html|css)|src\/[\w.-]+\.js)$/;
/** An opaque session id must be a valid cookie value (RFC 6265 cookie-octet) before it is set as one. */
const COOKIE_OCTETS = /^[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]{16,1024}$/;
const RELAYED_HEADERS = Object.freeze(['content-type', 'content-disposition', 'content-length', 'etag', 'last-modified', 'retry-after']);
/**
 * control-api's identity refusals, each to the page that says it. The page shows control-api's
 * own code beside ours when they differ. Any other code is a generic failure, shown with its code.
 */
const IDENTITY_REFUSALS = new Map([
  ['no_sso_connection', 'no_sso_connection'], ['tenant_not_onboarded', 'tenant_not_onboarded'],
  ['onboarding_incomplete', 'tenant_not_onboarded'], ['no_role', 'no_role'], ['connection_disabled', 'connection_disabled'],
  ['user_deactivated', 'user_deactivated'], ['tenant_closed', 'tenant_closed'],
  ['attempt_unknown', 'signin_expired'], ['attempt_expired', 'signin_expired'], ['state_mismatch', 'signin_expired'],
  ['invite_unknown', 'invite_invalid'], ['invite_used', 'invite_invalid'], ['invite_expired', 'invite_invalid'],
  ['provider_unavailable', 'unavailable'], ['unavailable', 'unavailable'],
]);
const HOP_BY_HOP = new Set(['connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer', 'transfer-encoding', 'upgrade', 'host', 'content-length', 'accept-encoding']);

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
};

/** A configuration this server refuses to start with. */
export class ConfigError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ConfigError';
  }
}

/** The environment variables this server requires, by configuration key. */
export const REQUIRED_ENV = Object.freeze({
  controlUrl: 'SAC_CONTROL_URL',
  internalToken: 'SAC_INTERNAL_TOKEN',
  publicUrl: 'SAC_PUBLIC_URL',
  queryApiUrl: 'SAC_QUERY_API_URL',
  vaultUrl: 'SAC_CONTENT_VAULT_URL',
});

const DEFAULT_PORT = 8080;

/** `host:port`, `:port` or `host`; empty is 0.0.0.0:8080, because a container must bind every interface. */
export function parseAddr(value) {
  const text = String(value ?? '').trim();
  if (text === '') return { host: '0.0.0.0', port: DEFAULT_PORT };
  const idx = text.lastIndexOf(':');
  if (idx < 0) return { host: text, port: DEFAULT_PORT };
  const host = text.slice(0, idx).replace(/^\[|\]$/g, '') || '0.0.0.0';
  const port = Number(text.slice(idx + 1));
  if (!Number.isInteger(port) || port < 0 || port > 65_535) throw new ConfigError(`SAC_HTTP_ADDR has an invalid port: ${JSON.stringify(text)}`);
  return { host, port };
}

function absoluteUrl(name, value) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new ConfigError(`${name} is not a URL: ${JSON.stringify(value)}`);
  }
  if (!/^https?:$/.test(url.protocol) || url.host === '') throw new ConfigError(`${name} must be an absolute http(s) URL; got ${JSON.stringify(value)}`);
  return value.replace(/\/+$/, '');
}

/**
 * Check a configuration and return it normalised, or throw a ConfigError naming every missing or
 * malformed variable. The server never starts half-configured.
 */
export function checkConfig(config) {
  const missing = Object.entries(REQUIRED_ENV).filter(([key]) => String(config[key] ?? '').trim() === '').map(([, name]) => name);
  if (missing.length > 0) throw new ConfigError(`missing ${missing.join(', ')}`);
  const out = { ...config, internalToken: String(config.internalToken).trim() };
  for (const key of ['controlUrl', 'publicUrl', 'queryApiUrl', 'vaultUrl']) out[key] = absoluteUrl(REQUIRED_ENV[key], String(config[key]).trim());
  return out;
}

/** The configuration, from the environment. */
export function configFromEnv(env = process.env) {
  const { host, port } = parseAddr(env.SAC_HTTP_ADDR);
  return checkConfig({
    root: PACKAGE_ROOT,
    host,
    port,
    controlUrl: env.SAC_CONTROL_URL,
    internalToken: env.SAC_INTERNAL_TOKEN,
    publicUrl: env.SAC_PUBLIC_URL,
    queryApiUrl: env.SAC_QUERY_API_URL,
    vaultUrl: env.SAC_CONTENT_VAULT_URL,
  });
}

/**
 * Build the server. It is returned unstarted, so a test can listen on port 0 against a fake
 * control-api and query-api.
 *
 * @param {object} config  checkConfig's fields, plus optional fetchImpl, now, log and root
 */
export function createDashboardServer(config) {
  const cfg = { root: PACKAGE_ROOT, ...checkConfig(config) };
  const fetchImpl = cfg.fetchImpl ?? globalThis.fetch;
  const log = cfg.log ?? console;
  const identity = createIdentityClient({ baseUrl: cfg.controlUrl, internalToken: cfg.internalToken, fetchImpl });
  const tokens = createTokenCache({ identity, now: cfg.now });
  const publicOrigin = new URL(cfg.publicUrl).origin;
  // Cookies are Secure whenever the address people use is https.
  const secure = publicOrigin.startsWith('https:');

  // ------------------------------------------------------------------------------------------
  // Small helpers
  // ------------------------------------------------------------------------------------------

  function requestUrl(req) {
    return new URL(req.url ?? '/', 'http://dashboard.invalid');
  }

  function protoOf(req) {
    return String(req.headers['x-forwarded-proto'] ?? '').split(',')[0].trim() || 'http';
  }

  /** The origins a same-origin request may name: the public URL's, and the address this request reached. */
  function allowedOrigins(req) {
    const list = [publicOrigin];
    if (req.headers.host) list.push(`${protoOf(req)}://${req.headers.host}`);
    return list;
  }

  function page(res, status, html, extra = {}) {
    res.writeHead(status, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-store', ...extra });
    res.end(html);
  }

  function signinPage(res, code, { next = '/', email = '', detail = null, extra = {}, status = null } = {}) {
    page(res, status ?? (code ? signinMessage(code).status : 200), renderSigninPage({ code, detail, next, email }), extra);
  }

  function redirect(res, location, extra = {}) {
    res.writeHead(302, { location, 'cache-control': 'no-store', ...extra });
    res.end();
  }

  function json(res, status, body) {
    res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
    res.end(JSON.stringify(body));
  }

  /**
   * A refusal on a /v1 path. It carries both answer shapes the page reads — a query envelope's
   * `result_state` and a content answer's `state` — so either transport renders it as a refusal.
   */
  function refuseV1(res, status, code, message, resultState = 'unauthorised_role') {
    json(res, status, { api_version: '1', query_version: '1', result_state: resultState, state: 'refused', error: { code, message } });
  }

  function refuseAdmin(res, status, code, message) {
    json(res, status, { error: code, message });
  }

  async function readBody(req) {
    const chunks = [];
    let size = 0;
    for await (const chunk of req) {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) return null;
      chunks.push(chunk);
    }
    return Buffer.concat(chunks);
  }

  const clearSession = () => sessionCookie('', { maxAgeSec: 0, secure });
  const clearSignin = () => signinCookie('', { maxAgeSec: 0, secure });

  // ------------------------------------------------------------------------------------------
  // The session
  // ------------------------------------------------------------------------------------------

  /** Who is asking: `{ ok: true, session }` or `{ ok: false, reason }`. */
  async function sessionOf(req) {
    const id = readCookie(req.headers.cookie);
    if (!id) return { ok: false, reason: 'no_session' };
    const got = await tokens.get(id);
    if (!got.ok) return got;
    return { ok: true, session: Object.freeze({ id, token: got.token, ...got.principal, role: primaryRole(got.principal.roles) }) };
  }

  function isApiPath(path) {
    return path.startsWith('/v1/') || path.startsWith('/admin/v1/') || path === '/session';
  }

  /** No session: an API call is refused with JSON, a page is sent to sign in and returned afterwards. */
  function unauthenticated(req, res, url, reason) {
    const path = url.pathname;
    const ended = reason === 'session_ended' || reason === 'tenant_closed';
    const extra = ended ? { 'set-cookie': clearSession() } : {};
    if (isApiPath(path)) {
      const unavailable = reason === 'identity_unavailable';
      const status = unavailable ? 503 : 401;
      const code = unavailable ? 'identity_unavailable' : reason;
      const message = unavailable ? 'The sign-in service cannot be reached; the session could not be checked.' : 'No signed-in session. Sign in at /signin.';
      if (extra['set-cookie']) res.setHeader('set-cookie', extra['set-cookie']);
      if (path.startsWith('/v1/')) return refuseV1(res, status, code, message, unavailable ? 'busy' : 'unauthorised_role');
      return refuseAdmin(res, status, code, message);
    }
    if (reason === 'identity_unavailable') return signinPage(res, 'unavailable');
    const next = safeNext(`${url.pathname}${url.search}`);
    const notice = ended ? `&notice=${reason}` : '';
    return redirect(res, `/signin?next=${encodeURIComponent(next)}${notice}`, extra);
  }

  // ------------------------------------------------------------------------------------------
  // Sign-in
  // ------------------------------------------------------------------------------------------

  function handleSignin(req, res, url) {
    const next = safeNext(url.searchParams.get('next') ?? '/');
    // Only our own notices are taken from the address; an error is never, so a link cannot make
    // this page say something it did not decide.
    const notice = ['signed_out', 'session_ended', 'tenant_closed'].includes(url.searchParams.get('notice')) ? url.searchParams.get('notice') : null;
    signinPage(res, notice, { next });
  }

  async function handleSigninStart(req, res, url) {
    let params = url.searchParams;
    if (req.method === 'POST') {
      if (!sameOriginRequest(req.headers, allowedOrigins(req))) return signinPage(res, 'signin_failed', { detail: 'cross_site_request', status: 403 });
      const body = await readBody(req);
      if (body === null) return signinPage(res, 'signin_failed');
      params = new URLSearchParams(body.toString('utf8'));
    } else if (req.method !== 'GET') {
      res.writeHead(405, { allow: 'GET, POST' }).end();
      return undefined;
    }
    const next = safeNext(params.get('next') ?? '/');
    const email = String(params.get('email') ?? '').trim();
    // "entra" is the Microsoft button; "oidc" arrives with an invite from control-api's onboarding.
    const provider = ['entra', 'oidc'].includes(params.get('provider')) ? params.get('provider') : null;
    const invite = String(params.get('invite') ?? '').trim();
    if (!email && !provider && !invite) return signinPage(res, 'email_required', { next });
    if (email && !/^[^\s@]{1,64}@[^\s@]+\.[^\s@]{2,}$/.test(email)) return signinPage(res, 'email_required', { next, email });

    const answer = await identity.begin({ email: email || null, provider, invite: invite || null, redirectUri: `${publicOrigin}/callback` });
    if (!answer.ok) {
      if (answer.status === 0) return signinPage(res, 'unavailable', { next, email });
      const known = IDENTITY_REFUSALS.get(answer.error) ?? 'signin_failed';
      return signinPage(res, known, { next, email, detail: known === answer.error ? null : answer.error });
    }
    const attempt = answer.body?.attempt;
    const authorize = answer.body?.authorize_url;
    let target = null;
    try {
      target = new URL(String(authorize));
    } catch {
      target = null;
    }
    if (typeof attempt !== 'string' || attempt === '' || !target || !/^https?:$/.test(target.protocol)) {
      log.warn?.('dashboard: control-api began a sign-in without a usable attempt or authorize_url');
      return signinPage(res, 'signin_failed', { next, email });
    }
    return redirect(res, target.href, { 'set-cookie': signinCookie(encodeAttempt({ attempt, next }), { secure }), 'referrer-policy': 'no-referrer' });
  }

  async function handleCallback(req, res, url) {
    const extra = { 'set-cookie': clearSignin(), 'referrer-policy': 'no-referrer' };
    const attempt = decodeAttempt(readCookie(req.headers.cookie, SIGNIN_COOKIE));
    const providerError = url.searchParams.get('error');
    if (providerError) return signinPage(res, 'idp_refused', { next: attempt?.next ?? '/', detail: providerError, extra });
    if (!attempt) return signinPage(res, 'signin_expired', { extra });
    const code = url.searchParams.get('code');
    const state = url.searchParams.get('state');
    if (!code || !state) return signinPage(res, 'signin_failed', { next: attempt.next, detail: 'missing_code', extra });

    const answer = await identity.complete({ attempt: attempt.attempt, code, state });
    if (!answer.ok) {
      if (answer.status === 0) return signinPage(res, 'unavailable', { next: attempt.next, extra });
      const known = IDENTITY_REFUSALS.get(answer.error) ?? 'signin_failed';
      return signinPage(res, known, { next: attempt.next, detail: known === answer.error ? null : answer.error, extra });
    }
    const session = answer.body?.session;
    const principal = principalFrom(answer.body?.principal);
    if (typeof session !== 'string' || !COOKIE_OCTETS.test(session) || !principal) {
      if (typeof session === 'string' && session !== '') await identity.revoke(session);
      log.warn?.('dashboard: control-api completed a sign-in with an unusable session or principal; it was revoked');
      return signinPage(res, principal || typeof answer.body?.principal !== 'object' ? 'signin_failed' : 'no_role', { next: attempt.next, extra });
    }
    tokens.seed(session, answer.body);
    return redirect(res, attempt.next, { 'set-cookie': [sessionCookie(session, { secure }), clearSignin()], 'referrer-policy': 'no-referrer' });
  }

  async function handleSignout(req, res) {
    // A sign-out link followed from another site is refused; a typed address or this page's own
    // button is not.
    const site = String(req.headers['sec-fetch-site'] ?? '');
    if (site && site !== 'none' && !sameOriginRequest(req.headers, allowedOrigins(req))) {
      return signinPage(res, 'signin_failed', { detail: 'cross_site_request', status: 403 });
    }
    const id = readCookie(req.headers.cookie);
    if (id) {
      tokens.drop(id);
      const revoked = await identity.revoke(id);
      if (!revoked.ok) log.warn?.(`dashboard: revoking a session at sign-out failed (${revoked.error}); the cookie is cleared regardless`);
    }
    return redirect(res, '/signin?notice=signed_out', { 'set-cookie': clearSession() });
  }

  /**
   * control-api's onboarding hands the browser here with ?invite=&provider= (or ?hint=<email>) to
   * begin the activating sign-in, which /signin/start does. Anything else goes to /signin.
   */
  function handleLogin(res, url) {
    const start = new URLSearchParams();
    for (const name of ['invite', 'provider']) if (url.searchParams.get(name)) start.set(name, url.searchParams.get(name));
    if (url.searchParams.get('hint')) start.set('email', url.searchParams.get('hint'));
    start.set('next', safeNext(url.searchParams.get('next') ?? '/'));
    return redirect(res, start.has('invite') || start.has('provider') || start.has('email') ? `/signin/start?${start}` : `/signin?next=${encodeURIComponent(start.get('next'))}`);
  }

  // ------------------------------------------------------------------------------------------
  // Forwarding
  // ------------------------------------------------------------------------------------------

  /** The response headers a forwarded API answer keeps: what describes the body, and our own x-sac-*. */
  function relayedHeaders(upstream) {
    const headers = { 'cache-control': 'no-store' };
    const encoded = upstream.headers.has('content-encoding');
    for (const name of RELAYED_HEADERS) {
      if (name === 'content-length' && encoded) continue;
      const value = upstream.headers.get(name);
      if (value !== null) headers[name] = value;
    }
    for (const [name, value] of upstream.headers) if (name.startsWith('x-sac-')) headers[name] = value;
    return headers;
  }

  async function relayBody(req, res, upstream) {
    if (!upstream.body || req.method === 'HEAD') {
      res.end();
      return;
    }
    try {
      await pipeline(Readable.fromWeb(upstream.body), res);
    } catch {
      res.destroy();
    }
  }

  /**
   * Forward one API request. The caller's cookie and Authorization header are not sent: the only
   * credential upstream sees is the one this server attaches.
   */
  async function forwardApi(req, res, { base, target, token, timeoutMs, refuse, name }) {
    let body;
    if (!['GET', 'HEAD'].includes(req.method)) {
      body = await readBody(req);
      if (body === null) return refuse(res, 413, 'body_too_large', 'The request body is larger than this server forwards.');
    }
    const headers = { 'accept-encoding': 'identity', authorization: `Bearer ${token}` };
    for (const h of ['content-type', 'accept', 'accept-language', 'if-none-match', 'if-match', 'x-request-id']) {
      if (req.headers[h]) headers[h] = req.headers[h];
    }
    let upstream;
    try {
      upstream = await fetchImpl(`${base}${target}`, { method: req.method, headers, body, redirect: 'manual', signal: AbortSignal.timeout(timeoutMs) });
    } catch (error) {
      const timedOut = error?.name === 'TimeoutError';
      log.warn?.(`dashboard: ${name} ${timedOut ? 'timed out' : 'could not be reached'} (${error?.cause?.code ?? error?.name ?? 'error'})`);
      return refuse(res, timedOut ? 504 : 503, timedOut ? 'upstream_timeout' : 'upstream_unreachable', `The ${name} ${timedOut ? 'did not answer in time' : 'could not be reached'}.`);
    }
    res.writeHead(upstream.status, relayedHeaders(upstream));
    return relayBody(req, res, upstream);
  }

  /**
   * Forward one onboarding request to control-api, untouched: method, path, query, body, its
   * headers and cookies, and its answer — status, Location and Set-Cookie included. The only
   * things removed are hop-by-hop headers and this server's own cookies; nothing is added.
   */
  async function forwardOnboard(req, res, url) {
    const headers = { 'accept-encoding': 'identity', 'x-forwarded-host': String(req.headers.host ?? ''), 'x-forwarded-proto': protoOf(req) };
    for (const [name, value] of Object.entries(req.headers)) {
      if (HOP_BY_HOP.has(name) || name === 'cookie') continue;
      headers[name] = Array.isArray(value) ? value.join(', ') : value;
    }
    const cookies = withoutOwnCookies(req.headers.cookie);
    if (cookies) headers.cookie = cookies;
    let body;
    if (!['GET', 'HEAD'].includes(req.method)) {
      body = await readBody(req);
      if (body === null) return page(res, 413, renderSigninPage({ code: 'signin_failed', detail: 'body_too_large' }));
    }
    let upstream;
    try {
      upstream = await fetchImpl(`${cfg.controlUrl}${url.pathname}${url.search}`, { method: req.method, headers, body, redirect: 'manual', signal: AbortSignal.timeout(30_000) });
    } catch {
      return signinPage(res, 'unavailable');
    }
    const out = {};
    const encoded = upstream.headers.has('content-encoding');
    for (const [name, value] of upstream.headers) {
      if (HOP_BY_HOP.has(name) && name !== 'content-length') continue;
      if (name === 'set-cookie' || (encoded && (name === 'content-encoding' || name === 'content-length'))) continue;
      out[name] = value;
    }
    const setCookies = typeof upstream.headers.getSetCookie === 'function' ? upstream.headers.getSetCookie() : [];
    if (setCookies.length > 0) out['set-cookie'] = setCookies;
    res.writeHead(upstream.status, out);
    return relayBody(req, res, upstream);
  }

  /** Forward one minted retrieval URL straight to the vault: the grant is the capability, so no principal is added. */
  async function forwardRetrieval(req, res, target) {
    let upstream;
    try {
      upstream = await fetchImpl(`${cfg.vaultUrl}${target}`, { headers: { 'accept-encoding': 'identity' }, signal: AbortSignal.timeout(60_000) });
    } catch {
      return refuseV1(res, 503, 'vault_unreachable', 'The content vault could not be reached.', 'busy');
    }
    res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/octet-stream', 'cache-control': 'no-store' });
    return relayBody(req, res, upstream);
  }

  // ------------------------------------------------------------------------------------------
  // Static files
  // ------------------------------------------------------------------------------------------

  async function serveStatic(req, res, path) {
    if (!SERVABLE.test(path)) {
      res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' }).end('not found');
      return;
    }
    let candidate = null;
    try {
      candidate = normalize(join(cfg.root, decodeURIComponent(path)));
    } catch {
      candidate = null;
    }
    if (!candidate || !candidate.startsWith(cfg.root + sep)) {
      res.writeHead(403).end('forbidden');
      return;
    }
    try {
      const info = await stat(candidate);
      if (!info.isFile()) throw new Error('not a file');
      const body = await readFile(candidate);
      res.writeHead(200, { 'content-type': TYPES[extname(candidate)] ?? 'application/octet-stream', 'cache-control': 'no-store' });
      res.end(req.method === 'HEAD' ? undefined : body);
    } catch {
      res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' }).end('not found');
    }
  }

  // ------------------------------------------------------------------------------------------
  // The router
  // ------------------------------------------------------------------------------------------

  async function route(req, res) {
    const url = requestUrl(req);
    const path = url.pathname;
    // Every answer: no framing by another site, no sniffing, and no path or query leaked onward.
    res.setHeader('x-frame-options', 'DENY');
    res.setHeader('x-content-type-options', 'nosniff');
    res.setHeader('referrer-policy', 'same-origin');

    if (path === '/healthz') return json(res, 200, { status: 'ok' });
    if (path === '/readyz') return json(res, 200, { status: 'ready' });
    if (path === '/signin') return handleSignin(req, res, url);
    if (path === '/signin/start') return handleSigninStart(req, res, url);
    if (path === '/callback') return handleCallback(req, res, url);
    if (path === '/signout') return handleSignout(req, res);
    if (path === '/login') return handleLogin(res, url);
    if (path === '/onboard' || path.startsWith('/onboard/')) return forwardOnboard(req, res, url);
    if (path.startsWith(RETRIEVAL_PREFIX)) {
      if (req.method !== 'GET') return void res.writeHead(405, { allow: 'GET' }).end();
      return forwardRetrieval(req, res, `${path}${url.search}`);
    }
    if (PUBLIC_ASSETS.has(path)) return serveStatic(req, res, path);

    const who = await sessionOf(req);
    if (!who.ok) return unauthenticated(req, res, url, who.reason);
    const session = who.session;

    // Who the page is, so the navigation can hide what the roles cannot use. Not a credential.
    if (path === '/session') {
      return json(res, 200, { actor: session.actor, tenant: session.tenant, idp: session.idp, role: session.role, roles: session.roles, pages: pagesForRoles(session.roles) });
    }

    const api = path.startsWith('/v1/');
    const admin = path.startsWith('/admin/v1/');
    if ((api || admin) && isStateChanging(req.method) && !sameOriginRequest(req.headers, allowedOrigins(req))) {
      const message = 'A change must come from this dashboard\'s own pages.';
      return api ? refuseV1(res, 403, 'cross_site_request', message) : refuseAdmin(res, 403, 'cross_site_request', message);
    }
    if (api) {
      const refuse = (r, s, c, m) => refuseV1(r, s, c, m, s === 413 ? 'query_too_broad' : 'busy');
      return forwardApi(req, res, { base: cfg.queryApiUrl, target: `${path}${url.search}`, token: session.token, timeoutMs: 60_000, refuse, name: 'query API' });
    }
    if (admin) {
      // control-api checks the role on the token; this keeps a non-admin's request from leaving at all.
      if (!canAny(session.roles, 'settings')) return refuseAdmin(res, 403, 'forbidden', 'Only an admin can use these settings.');
      return forwardApi(req, res, { base: cfg.controlUrl, target: `${path}${url.search}`, token: session.token, timeoutMs: 120_000, refuse: refuseAdmin, name: 'admin API' });
    }

    // The Search page is where content is reachable; a role that may not search is not served it.
    if ((path === '/explore.html' || path === '/explore') && !canAny(session.roles, 'search')) {
      return signinPage(res, 'not_permitted');
    }
    if (path === '/') return serveStatic(req, res, '/index.html');
    if (path === '/explore') return serveStatic(req, res, '/explore.html');
    return serveStatic(req, res, path);
  }

  return createServer((req, res) => {
    route(req, res).catch((error) => {
      // The reason goes to the server's log; the person gets a sentence.
      log.error?.(`dashboard: ${req.method} ${requestUrl(req).pathname} failed: ${error?.message ?? error}`);
      if (res.headersSent) return void res.destroy();
      if (isApiPath(requestUrl(req).pathname)) return void json(res, 500, { error: 'internal_error', message: 'The dashboard server could not complete this request.' });
      return void signinPage(res, 'signin_failed');
    });
  });
}
