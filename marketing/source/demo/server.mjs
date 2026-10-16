// A demo console: the repository's dashboard pages, answered with sample data instead of query-api
// and control-api. It is for recording and screenshots only; nothing here reaches a real tenant.
//
//   node server.mjs [port]
//
// Query documents are answered from demo-data.mjs by template or source name, falling back to the
// dashboard's own test fixtures; admin reads, prompt search, content retrieval and list exports from
// demo-data.mjs. The product name in served pages comes from ../brand.json. Unanswered requests are
// logged, so a gap shows up on the console.

import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

import { pagesForRoles } from '../../../services/dashboard/server/session.mjs';
import { fixtureTransport } from '../../../services/dashboard/test/fixtures.mjs';
import * as demo from './demo-data.mjs';

const REPO = fileURLToPath(new URL('../../../', import.meta.url));
const DASH = join(REPO, 'services/dashboard');
const SPLASH = join(REPO, 'device/extension/splash');
const brand = JSON.parse(await readFile(new URL('../brand.json', import.meta.url), 'utf8'));
// Production by default, so the console shows only the pages a customer's console offers.
const ENVIRONMENT = process.env.DEMO_ENV ?? 'prod';
const SESSION = { ...demo.SESSION, role: demo.SESSION.roles[0], pages: pagesForRoles(demo.SESSION.roles, { environment: ENVIRONMENT }) };
const fixtures = fixtureTransport('realistic');

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.json': 'application/json' };

function send(res, status, body, type = 'application/json', headers = {}) {
  res.writeHead(status, { 'content-type': type, 'cache-control': 'no-store', ...headers });
  res.end(typeof body === 'string' || Buffer.isBuffer(body) ? body : JSON.stringify(body));
}

async function readBody(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const text = Buffer.concat(chunks).toString('utf8');
  return text ? JSON.parse(text) : undefined;
}

async function serveFile(res, root, rel) {
  const path = normalize(join(root, rel));
  if (!path.startsWith(root)) return send(res, 403, { error: 'forbidden' });
  try {
    let data = await readFile(path);
    const type = TYPES[extname(path)] ?? 'application/octet-stream';
    if (type.startsWith('text/html')) data = data.toString('utf8').replaceAll('Sundial', brand.product);
    send(res, 200, data, type);
  } catch {
    send(res, 404, { error: 'not_found' });
  }
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://demo');
  const path = url.pathname;
  try {
    if (path === '/v1/query' && req.method === 'POST') {
      const body = await readBody(req);
      const key = body?.template ?? body?.source ?? 'unknown';
      const answer = demo.query(key, body);
      if (answer) return send(res, 200, answer);
      console.log(`query ${key}: fixture`);
      return send(res, 200, await fixtures.send(body));
    }
    if (path === '/session' || path.endsWith('/session')) return send(res, 200, SESSION);
    if (path.startsWith('/admin/v1/')) {
      const key = `${req.method} ${path}${url.search}`;
      const answer = demo.admin(key, req.method === 'GET' ? undefined : await readBody(req));
      if (answer) return send(res, answer.status ?? 200, answer.body ?? answer);
      console.log(`admin ${key}: no answer`);
      return send(res, 404, { error: 'not_found', message: `no demo answer for ${key}` });
    }
    if (path.startsWith('/v1/content') || path.startsWith('/v1/list-export') || path.startsWith('/v1/export/')) {
      const answer = demo.content(`${req.method} ${path}`, req.method === 'POST' ? await readBody(req) : undefined);
      if (answer) return send(res, answer.status, answer.body, answer.type, answer.headers);
    }
    if (path.startsWith('/v1/')) {
      console.log(`unanswered ${req.method} ${path}`);
      return send(res, 404, { error: 'not_found' });
    }
    if (path.startsWith('/splash/')) return serveFile(res, SPLASH, path.slice('/splash/'.length));
    return serveFile(res, DASH, path === '/' ? 'index.html' : path.slice(1));
  } catch (error) {
    console.error(error);
    send(res, 500, { error: 'demo_server', message: String(error?.message ?? error) });
  }
});

const port = Number(process.argv[2] ?? 8790);
server.listen(port, '127.0.0.1', () => console.log(`demo console (${ENVIRONMENT}) on http://127.0.0.1:${port}/`));
