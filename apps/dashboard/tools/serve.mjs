// serve.mjs — a static file server for `module.html`, the ES-module entry.
//
// `index.html` opens straight from the filesystem and needs no server. This exists for the other
// entry point: `module.html` loads `src/*.js` as real ES modules with real imports, which is the
// layout to develop against, and a browser will only do that over http.
//
//   node tools/serve.mjs            # http://127.0.0.1:8787/
//   node tools/serve.mjs --port 9000
//
// Zero dependencies: node:http and node:fs only. It serves this directory and nothing else.

import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { extname, join, normalize, resolve, sep } from 'node:path';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = Number(process.argv[process.argv.indexOf('--port') + 1]) || 8787;

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

const server = createServer(async (request, response) => {
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

server.listen(PORT, '127.0.0.1', () => {
  console.log(`dashboard (ES-module entry): http://127.0.0.1:${PORT}/module.html`);
  console.log('index.html needs no server: open the file directly.');
});
