// Scratch: what is `errors` doing, and is 32769 a count or a reinterpretation?
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const PKG = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = join(REPO, '.tools', 'tmp', 'err-probe');
const EDGE = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const server = createServer((req, res) => {
  let b = ''; req.on('data', (c) => (b += c));
  req.on('end', () => {
    if (req.url === '/') {
      res.writeHead(200, { 'content-type': 'text/html' });
      res.end(`<!doctype html><html><body><script>fetch('/v1/chat/completions',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({model:'m',messages:[{role:'user',content:'${'z'.repeat(200)}'}]})}).then(r=>r.text()).then(()=>{document.title='done'})</script></body></html>`);
    } else { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{"ok":true}'); }
  });
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const PORT = server.address().port;

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });
const child = spawn(EDGE, ['--headless=new','--disable-gpu','--no-first-run','--no-default-browser-check',
  `--user-data-dir=${PROFILE}`,'--remote-debugging-port=0',`--load-extension=${PKG}`,'about:blank'],
  { stdio: ['ignore','pipe','pipe'] });
const pf = join(PROFILE, 'DevToolsActivePort');
let port = null;
for (let i = 0; i < 200; i++) { if (existsSync(pf)) { const l = readFileSync(pf,'utf8').split('\n')[0]; if (l) { port = Number(l); break; } } await sleep(100); }
const v = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
const ws = new WebSocket(v.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map(); const evs = [];
ws.onmessage = (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } else evs.push(m); };
const send = (method, params = {}, sessionId) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) })); });

const findSw = async () => (await send('Target.getTargets')).result.targetInfos.find((x) => x.type === 'service_worker' && /background\/service-worker\.js$/.test(x.url));
let sw = null;
for (let i = 0; i < 60 && !sw; i++) { sw = await findSw(); if (!sw) await sleep(200); }
let sid = null;
const attach = async () => { sid = (await send('Target.attachToTarget', { targetId: (await findSw()).targetId, flatten: true })).result.sessionId; await send('Runtime.enable', {}, sid); };
await attach();
const ev = async (expr) => {
  for (let a = 0; a < 6; a++) {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true }, sid);
    const d = r.result?.exceptionDetails?.exception?.description || '';
    if (/chrome is not defined|Cannot read properties of undefined/.test(d) && a < 5) { await sleep(150); await attach(); continue; }
    if (r.result?.exceptionDetails) return `THREW: ${d}`;
    return r.result?.result?.value ?? JSON.stringify(r.result);
  }
  return 'gave up';
};

// Instrument: wrap countError so we learn HOW MANY TIMES and FROM WHERE, without changing behaviour.
await ev(`
  (() => {
    const app = globalThis.__captureApp;
    globalThis.__probe = { calls: 0, byCode: {}, stack: null, firstAt: null };
    const cs = app.health.counters;
    const orig = cs.countError;
    cs.countError = function (code, n) {
      globalThis.__probe.calls += (typeof n === 'number' ? n : 1);
      globalThis.__probe.byCode[String(code)] = (globalThis.__probe.byCode[String(code)] || 0) + 1;
      if (!globalThis.__probe.stack) { globalThis.__probe.stack = new Error('first').stack.split('\\n').slice(1, 6).join(' | '); globalThis.__probe.firstAt = Date.now(); }
      return orig.call(cs, code, n);
    };
    return 'instrumented';
  })()
`);

const page = (await send('Target.getTargets')).result.targetInfos.find((t) => t.type === 'page');
const psid = (await send('Target.attachToTarget', { targetId: page.targetId, flatten: true })).result.sessionId;
await send('Page.enable', {}, psid);
await send('Page.navigate', { url: `http://127.0.0.1:${PORT}/` }, psid);
await sleep(4000);
await attach();

console.log('counters:', await ev(`JSON.stringify(globalThis.__captureApp.health.counters.snapshot().counters)`));
console.log('errors_by_code:', await ev(`JSON.stringify(globalThis.__captureApp.health.counters.snapshot().errors_by_code)`));
console.log('probe byCode:', await ev(`JSON.stringify(globalThis.__probe.byCode)`));
console.log('probe calls:', await ev(`JSON.stringify(globalThis.__probe.calls)`));
console.log('first stack:', await ev(`JSON.stringify(globalThis.__probe.stack)`));
console.log('typeof errors:', await ev(`typeof globalThis.__captureApp.health.counters.snapshot().counters.errors`));
console.log('raw errors value:', await ev(`String(globalThis.__captureApp.health.counters.snapshot().counters.errors)`));
console.log('window errors:', await ev(`String(globalThis.__captureApp.health.counters.snapshot().window.errors)`));
console.log('connectAttempts (via app.native):', await ev(`String(globalThis.__captureApp.native.requiresAttention())`));
console.log('queue stats:', await ev(`JSON.stringify(globalThis.__captureApp.queue.stats())`));
console.log('health core/state/detail:', await ev(`JSON.stringify({core: globalThis.__captureApp.health.report().core, state: globalThis.__captureApp.health.report().state, detail: globalThis.__captureApp.health.report().detail})`));

child.kill('SIGKILL');
await sleep(600);
ws.close(); server.close();
try { rmSync(PROFILE, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 }); } catch {}
