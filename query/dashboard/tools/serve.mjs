// serve.mjs — the dashboard's own server: static files, the OIDC session, and the forwarder.
//
// WHAT CHANGED IN TASK 11. This server used to invent a principal for every request: it added
// x-sac-dev-tenant / x-sac-dev-actor from SAC_DEV_TENANT / SAC_DEV_ACTOR, and query-api
// trusted them because SAC_DEV_TRUST_PRINCIPAL=1. That made every audit row
// `analyst@lab.test` and let anyone who reached the Search page read stored prompts.
//
// With SAC_OIDC_ISSUER set, this is now a real OpenID Connect client (authorization code +
// PKCE, see session.mjs). The browser holds one opaque cookie; the access token lives here,
// on the server, and is forwarded to query-api in the Authorization header. query-api
// verifies that token for itself, so the tenant and roles are read from a signed token and
// never from a header the client can write.
//
// STATIC FILES ARE BEHIND THE SESSION. index.html is sample mode and is opened from the
// filesystem, and that is untouched; but the served dashboard refuses an unauthenticated
// request rather than rendering a page that would then fail every read. The one deliberate
// exception is the minted retrieval URL: it is a capability, it carries no caller identity,
// and it is the single path this server forwards straight to content-vault (docs/02 §11).
//
//   node tools/serve.mjs            # http://127.0.0.1:8787/
//   node tools/serve.mjs --port 9000
//   node tools/serve.mjs --host 0.0.0.0   # inside a container
//   node tools/serve.mjs --api http://127.0.0.1:8082   # also forward the query endpoint
//
// Zero dependencies: node:http, node:fs and node:crypto only. It serves this directory and
// nothing else.

import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { extname, join, normalize, resolve, sep } from 'node:path';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  createOidcClient,
  createSessionStore,
  pagesFor,
  pkce,
  nonce as makeNonce,
  principalFromClaims,
  readCookie,
  sessionCookie,
} from './session.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = Number(process.argv[process.argv.indexOf('--port') + 1]) || 8787;
// Loopback unless told otherwise: a container has to listen on all interfaces to be published.
const HOST = process.argv.includes('--host') ? process.argv[process.argv.indexOf('--host') + 1] : '127.0.0.1';

const API = (process.argv.includes('--api') ? process.argv[process.argv.indexOf('--api') + 1] : process.env.SAC_QUERY_API_URL ?? '').replace(/\/$/, '');
// The vault, reached directly for a minted retrieval URL. This is the one forwarded path that
// deliberately bypasses query-api, because content must never transit query-api (docs/02 §11).
const VAULT = (process.env.SAC_CONTENT_VAULT_URL ?? '').replace(/\/$/, '');

// The authenticated session. With no issuer this server falls back to the development
// principal, which is the memory lab's arrangement and is said out loud at startup.
const OIDC_ISSUER = (process.env.SAC_OIDC_ISSUER ?? '').replace(/\/$/, '');
const AUTH_REQUIRED = OIDC_ISSUER !== '';
const OIDC_CLIENT_ID = process.env.SAC_OIDC_CLIENT_ID ?? 'sac-dashboard';
const OIDC_CLIENT_SECRET = process.env.SAC_OIDC_CLIENT_SECRET ?? '';
const OIDC_REDIRECT_URL = process.env.SAC_OIDC_REDIRECT_URL ?? '';
const OIDC_SCOPE = process.env.SAC_OIDC_SCOPES ?? 'openid profile email';
const COOKIE_SECURE = process.env.SAC_COOKIE_SECURE === '1';
// In development mode these stand in for the session, exactly as before.
const DEV_TENANT = process.env.SAC_DEV_TENANT ?? '';
const DEV_ACTOR = process.env.SAC_DEV_ACTOR ?? 'dashboard-dev';
// The tenant each dashboard is pinned to is NOT read here: it comes from the token. This is
// an optional extra guard so the sample dashboard cannot show the owner's tenant even if a
// sign-in is misdirected. Empty means unpinned.
const PINNED_TENANT = (process.env.SAC_DASHBOARD_TENANT ?? '').trim().toLowerCase();

const QUERY_PATH = '/v1/query';
const CONTENT_RETRIEVAL_PATH = '/v1/content/retrieval';
// The two content reads the Explore page makes. They are forwarded exactly as the query is: to
// query-api, which establishes who is asking and forwards them to the content vault.
const FORWARDED_PATHS = Object.freeze([QUERY_PATH, '/v1/content-search', CONTENT_RETRIEVAL_PATH]);
// A minted retrieval URL is GET /v1/content/retrieval/<tenant>/<grant>; the POST above has no
// trailing segment and is untouched.
const RETRIEVAL_PREFIX = `${CONTENT_RETRIEVAL_PATH}/`;
const MAX_BODY_BYTES = 256 * 1024;
const LOGIN_ATTEMPT_MS = 5 * 60_000;

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.md': 'text/markdown; charset=utf-8',
};

const oidc = AUTH_REQUIRED
  ? createOidcClient({ issuer: OIDC_ISSUER, clientId: OIDC_CLIENT_ID, clientSecret: OIDC_CLIENT_SECRET, redirectUri: OIDC_REDIRECT_URL || undefined, scope: OIDC_SCOPE })
  : null;
const sessions = createSessionStore();
/** In-flight sign-ins: state -> the PKCE verifier, nonce and return path. */
const signIns = new Map();

/** Refuse anything that escapes the served directory. */
function safeJoin(root, requestPath) {
  const decoded = decodeURIComponent(requestPath.split('?')[0]);
  const candidate = normalize(join(root, decoded));
  if (!candidate.startsWith(root + sep) && candidate !== root) return null;
  return candidate;
}

function refuse(response, status, resultState, code, message, extraHeaders = {}) {
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store', ...extraHeaders });
  response.end(JSON.stringify({ api_version: '1', query_version: '1', result_state: resultState, error: { code, message } }));
}

function requestUrl(request) {
  return new URL(request.url ?? '/', `http://${request.headers.host ?? 'localhost'}`);
}

/** The redirect_uri the issuer sends the browser back to. Configured, or from the request host. */
function redirectUriOf(request) {
  if (OIDC_REDIRECT_URL) return OIDC_REDIRECT_URL;
  const proto = request.headers['x-forwarded-proto'] ?? 'http';
  const host = request.headers.host ?? `${HOST}:${PORT}`;
  return `${proto}://${host}/callback`;
}

/** The live session for a request, or null. */
function sessionOf(request) {
  if (!AUTH_REQUIRED) {
    return { actor: DEV_ACTOR, tenant: DEV_TENANT, role: 'dev', roles: ['dev'], accessToken: null, dev: true };
  }
  const id = readCookie(request.headers.cookie);
  if (!id) return null;
  const s = sessions.get(id);
  if (!s) return null;
  if (PINNED_TENANT && s.tenant !== PINNED_TENANT) return null;
  return s;
}

// -------------------------------------------------------------------------------------------
// The sign-in flow
// -------------------------------------------------------------------------------------------

async function handleLogin(request, response) {
  if (!AUTH_REQUIRED) {
    // Development mode has no sign-in; return the browser to where it was headed.
    const url = requestUrl(request);
    response.writeHead(302, { location: url.searchParams.get('next') || '/', 'cache-control': 'no-store' });
    response.end();
    return;
  }
  const url = requestUrl(request);
  const state = Buffer.from(cryptoRandom()).toString('base64url');
  const { verifier, challenge } = pkce();
  const n = makeNonce();
  const next = url.searchParams.get('next') || '/';
  // `hint` is the lab stand-in's account chooser. A real provider shows its own sign-in page
  // and ignores it; it is never a credential.
  const hint = url.searchParams.get('hint') || '';
  const redirectUri = redirectUriOf(request);
  signIns.set(state, { verifier, nonce: n, next, hint, redirectUri, expiresAt: Date.now() + LOGIN_ATTEMPT_MS });
  const authorize = await oidc.authorizeUrl({ state, nonce: n, codeChallenge: challenge, loginHint: hint, redirectUri });
  response.writeHead(302, { location: authorize, 'cache-control': 'no-store' });
  response.end();
}

async function handleCallback(request, response) {
  if (!AUTH_REQUIRED) return refuse(response, 404, 'not_found', 'not_found', 'no sign-in is configured');
  const url = requestUrl(request);
  const state = url.searchParams.get('state');
  const code = url.searchParams.get('code');
  const attempt = signIns.get(state);
  signIns.delete(state);
  if (!attempt || Date.now() > attempt.expiresAt) {
    return refuse(response, 400, 'unsupported_query_shape', 'bad_sign_in', 'the sign-in attempt is unknown or has expired');
  }
  if (url.searchParams.get('error')) {
    return refuse(response, 400, 'unsupported_query_shape', 'sign_in_refused', String(url.searchParams.get('error_description') ?? url.searchParams.get('error')));
  }
  if (!code) return refuse(response, 400, 'unsupported_query_shape', 'bad_sign_in', 'the callback carried no code');
  try {
    const tokens = await oidc.exchange({ code, codeVerifier: attempt.verifier, redirectUri: attempt.redirectUri });
    const claims = await oidc.verifyIdToken(tokens.id_token, { nonce: attempt.nonce });
    const principal = principalFromClaims(claims);
    const id = sessions.create({ ...principal, accessToken: tokens.access_token, idToken: tokens.id_token });
    response.writeHead(302, { location: attempt.next || '/', 'set-cookie': sessionCookie(id, { secure: COOKIE_SECURE }), 'cache-control': 'no-store' });
    response.end();
  } catch (error) {
    refuse(response, 502, 'unauthorised_role', 'sign_in_failed', `the sign-in could not be completed: ${error?.message ?? error}`);
  }
}

function handleLogout(request, response) {
  const id = readCookie(request.headers.cookie);
  if (id) sessions.destroy(id);
  response.writeHead(302, { location: '/login', 'set-cookie': sessionCookie('', { maxAgeSec: 0, secure: COOKIE_SECURE }), 'cache-control': 'no-store' });
  response.end();
}

// -------------------------------------------------------------------------------------------
// Forwarding
// -------------------------------------------------------------------------------------------

/** Forward one request to the configured query-api and relay its answer and status unchanged. */
async function forwardQuery(request, response, path, session) {
  if (!API) {
    refuse(response, 503, 'busy', 'no_api_configured', 'This server was started without --api, so there is no query API behind it.');
    return;
  }
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) {
      refuse(response, 413, 'query_too_broad', 'body_too_large', 'The request body is larger than this forwarder accepts.');
      return;
    }
    chunks.push(chunk);
  }
  // The session is the credential. A real session forwards the access token, which query-api
  // verifies; the development fallback forwards the lab header behind its flag.
  const authHeaders = session?.accessToken
    ? { authorization: `Bearer ${session.accessToken}` }
    : session?.dev && DEV_TENANT
      ? { 'x-sac-dev-tenant': DEV_TENANT, 'x-sac-dev-actor': DEV_ACTOR }
      : {};
  try {
    const upstream = await fetch(`${API}${path}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', ...authHeaders },
      body: Buffer.concat(chunks),
    });
    const body = Buffer.from(await upstream.arrayBuffer());
    response.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json; charset=utf-8', 'cache-control': 'no-store' });
    response.end(body);
  } catch (error) {
    refuse(response, 503, 'busy', 'api_unreachable', `The query API at ${API} could not be reached: ${error?.cause?.code ?? error?.message ?? error}`);
  }
}

/**
 * Forward one minted retrieval URL straight to the vault. It is the browser's direct read of
 * granted content: the URL carries the single-use grant, so no principal header is added and no
 * content passes through query-api. This is the one route exempt from the session.
 */
async function forwardRetrieval(request, response, target) {
  if (!VAULT) {
    refuse(response, 503, 'busy', 'no_vault_configured', 'This server was started without SAC_CONTENT_VAULT_URL, so a retrieval URL cannot be redeemed.');
    return;
  }
  try {
    const upstream = await fetch(`${VAULT}${target}`);
    const body = Buffer.from(await upstream.arrayBuffer());
    response.writeHead(upstream.status, {
      'content-type': upstream.headers.get('content-type') ?? 'application/octet-stream',
      'cache-control': 'no-store',
    });
    response.end(body);
  } catch (error) {
    refuse(response, 503, 'busy', 'vault_unreachable', `The content vault at ${VAULT} could not be reached: ${error?.cause?.code ?? error?.message ?? error}`);
  }
}

// -------------------------------------------------------------------------------------------
// The router
// -------------------------------------------------------------------------------------------

const server = createServer(async (request, response) => {
  const url = requestUrl(request);
  const path = url.pathname;

  if (path === '/healthz') {
    response.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
    response.end(JSON.stringify({ status: 'ok', auth: AUTH_REQUIRED ? 'oidc' : 'development' }));
    return;
  }
  if (path === '/login') return void handleLogin(request, response).catch((error) => refuse(response, 502, 'busy', 'sign_in_failed', String(error?.message ?? error)));
  if (path === '/callback') return void handleCallback(request, response).catch((error) => refuse(response, 502, 'busy', 'sign_in_failed', String(error?.message ?? error)));
  if (path === '/logout') return handleLogout(request, response);

  // The minted retrieval URL is a capability and carries no caller identity: it is the one
  // unauthenticated path, by design (docs/02 §11, the product's single deliberate exception).
  if (path.startsWith(RETRIEVAL_PREFIX)) {
    if (request.method !== 'GET') {
      response.writeHead(405, { allow: 'GET' }).end();
      return;
    }
    await forwardRetrieval(request, response, request.url ?? path);
    return;
  }

  const session = sessionOf(request);

  // The session endpoint tells the page who it is, so the navigation can hide what the role
  // cannot use. It is not a credential and carries no token.
  if (path === '/session') {
    if (!session) return refuse(response, 401, 'unauthorised_role', 'role', 'no signed-in session');
    response.writeHead(200, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
    response.end(JSON.stringify({ actor: session.actor, tenant: session.tenant, role: session.role, roles: session.roles, pages: pagesFor(session.role) }));
    return;
  }

  if (!session) {
    // An API request is refused; a page request is sent to sign in and returned afterwards.
    if (path.startsWith('/v1/')) {
      refuse(response, 401, 'unauthorised_role', 'role', 'no signed-in session');
      return;
    }
    response.writeHead(302, { location: `/login?next=${encodeURIComponent(request.url ?? '/')}`, 'cache-control': 'no-store' });
    response.end();
    return;
  }

  if (FORWARDED_PATHS.includes(path)) {
    if (request.method !== 'POST') {
      response.writeHead(405, { allow: 'POST' }).end();
      return;
    }
    await forwardQuery(request, response, path, session);
    return;
  }

  // The Search page is where content is reachable; a role that may not search is not served it.
  if ((path === '/explore.html' || path === '/explore') && AUTH_REQUIRED && !(session.roles ?? []).some((r) => pagesFor(r)?.includes('explore'))) {
    refuse(response, 403, 'unauthorised_role', 'role', 'this role may not open the Search page');
    return;
  }

  const target = safeJoin(ROOT, request.url === '/' ? '/module.html' : request.url ?? '/');
  if (!target) {
    response.writeHead(403).end('forbidden');
    return;
  }
  try {
    const info = await stat(target);
    const file = info.isDirectory() ? join(target, 'index.html') : target;
    const body = await readFile(file);
    response.writeHead(200, { 'content-type': TYPES[extname(file)] ?? 'application/octet-stream', 'cache-control': 'no-store' });
    response.end(body);
  } catch {
    response.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' });
    response.end('not found');
  }
});

// A random state value for the authorization request. Kept local so this file needs no crypto
// import beyond what session.mjs already uses.
function cryptoRandom() {
  // eslint-disable-next-line global-require
  return globalThis.crypto.getRandomValues(new Uint8Array(16));
}

server.listen(PORT, HOST, () => {
  console.log(`dashboard (ES-module entry): http://${HOST}:${PORT}/module.html`);
  console.log(`explore (search page):        http://${HOST}:${PORT}/explore.html`);
  console.log('index.html needs no server: open the file directly.');
  if (AUTH_REQUIRED) console.log(`sign-in: ${OIDC_ISSUER} (client ${OIDC_CLIENT_ID}); static files and /v1/* need a session`);
  else console.log('sign-in: NOT configured. This server is adding a development principal header; it is a lab arrangement, not authentication.');
  if (API) console.log(`live data: http://${HOST}:${PORT}/explore.html?transport=live  (forwarding to ${API}${DEV_TENANT ? `, tenant ${DEV_TENANT}` : ''})`);
  if (VAULT) console.log(`content retrieval: minted vault URLs are forwarded straight to ${VAULT} (never through query-api)`);
});
