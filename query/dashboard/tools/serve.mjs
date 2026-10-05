// serve.mjs — a static file server for `module.html`, the ES-module entry.
//
// `index.html` opens straight from the filesystem and needs no server. This exists for the other
// entry point: `module.html` loads `src/*.js` as real ES modules with real imports, which is the
// layout to develop against, and a browser will only do that over http.
//
//   node tools/serve.mjs            # http://127.0.0.1:8787/
//   node tools/serve.mjs --port 9000
//   node tools/serve.mjs --host 0.0.0.0   # inside a container
//   node tools/serve.mjs --api http://127.0.0.1:8082   # also forward the query endpoint
//
// WITH --api (or SAC_QUERY_API_URL) this server forwards the one query endpoint to a real query-api,
// so a page it serves can read live data from its own origin. The lab's query-api trusts a
// development principal header (SAC_DEV_TRUST_PRINCIPAL=1). That header is added HERE, from
// SAC_DEV_TENANT and SAC_DEV_ACTOR, and never by the browser: the tenant is not something a page
// may choose. This is a development forwarder, not the deployed session, which is not built yet.
//
// WITH SAC_CONTENT_VAULT_URL it also serves a minted retrieval URL. The vault mints a same-origin
// path (docs/02 §11), the browser fetches it from this origin, and this server forwards it straight
// to the vault — never through query-api, which must not see a content byte. In a deployment the
// analyst web tier or the ingress plays this part.
//
// Zero dependencies: node:http and node:fs only. It serves this directory and nothing else.

import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { extname, join, normalize, resolve, sep } from 'node:path';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = Number(process.argv[process.argv.indexOf('--port') + 1]) || 8787;
// Loopback unless told otherwise: a container has to listen on all interfaces to be published.
const HOST = process.argv.includes('--host') ? process.argv[process.argv.indexOf('--host') + 1] : '127.0.0.1';

const API = (process.argv.includes('--api') ? process.argv[process.argv.indexOf('--api') + 1] : process.env.SAC_QUERY_API_URL ?? '').replace(/\/$/, '');
// The vault, reached directly for a minted retrieval URL. This is the one forwarded path that
// deliberately bypasses query-api, because content must never transit query-api (docs/02 §11).
const VAULT = (process.env.SAC_CONTENT_VAULT_URL ?? '').replace(/\/$/, '');
const DEV_TENANT = process.env.SAC_DEV_TENANT ?? '';
const DEV_ACTOR = process.env.SAC_DEV_ACTOR ?? 'dashboard-dev';
const QUERY_PATH = '/v1/query';
const CONTENT_RETRIEVAL_PATH = '/v1/content/retrieval';
// The two content reads the Explore page makes. They are forwarded exactly as the query is: to
// query-api, which establishes who is asking and forwards them to the content vault.
const FORWARDED_PATHS = Object.freeze([QUERY_PATH, '/v1/content-search', CONTENT_RETRIEVAL_PATH]);
// A minted retrieval URL is GET /v1/content/retrieval/<tenant>/<grant>; the POST above has no
// trailing segment and is untouched.
const RETRIEVAL_PREFIX = `${CONTENT_RETRIEVAL_PATH}/`;
const MAX_BODY_BYTES = 256 * 1024;

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.md': 'text/markdown; charset=utf-8',
};

/** Refuse anything that escapes the served directory. */
function safeJoin(root, requestPath) {
  const decoded = decodeURIComponent(requestPath.split('?')[0]);
  const candidate = normalize(join(root, decoded));
  if (!candidate.startsWith(root + sep) && candidate !== root) return null;
  return candidate;
}

function refuse(response, status, resultState, code, message) {
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
  response.end(JSON.stringify({ api_version: '1', query_version: '1', result_state: resultState, error: { code, message } }));
}

/** Forward one request to the configured query-api and relay its answer and status unchanged. */
async function forwardQuery(request, response, path) {
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
  try {
    const upstream = await fetch(`${API}${path}`, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...(DEV_TENANT ? { 'x-sac-dev-tenant': DEV_TENANT, 'x-sac-dev-actor': DEV_ACTOR } : {}),
      },
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
 * content passes through query-api. A deployment's web tier or ingress stands in for this.
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

const server = createServer(async (request, response) => {
  const path = (request.url ?? '').split('?')[0];
  if (path.startsWith(RETRIEVAL_PREFIX)) {
    if (request.method !== 'GET') {
      response.writeHead(405, { allow: 'GET' }).end();
      return;
    }
    await forwardRetrieval(request, response, request.url ?? path);
    return;
  }
  if (FORWARDED_PATHS.includes(path)) {
    if (request.method !== 'POST') {
      response.writeHead(405, { allow: 'POST' }).end();
      return;
    }
    await forwardQuery(request, response, path);
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

server.listen(PORT, HOST, () => {
  console.log(`dashboard (ES-module entry): http://${HOST}:${PORT}/module.html`);
  console.log(`explore (search page):        http://${HOST}:${PORT}/explore.html`);
  console.log('index.html needs no server: open the file directly.');
  if (API) console.log(`live data: http://${HOST}:${PORT}/explore.html?transport=live  (forwarding to ${API}, tenant ${DEV_TENANT || 'NOT SET'})`);
  if (VAULT) console.log(`content retrieval: minted vault URLs are forwarded straight to ${VAULT} (never through query-api)`);
});
