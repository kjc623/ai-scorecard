// Scratch: does ANY webRequest listener receive events in this unpacked load?
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const PKG = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = join(REPO, '.tools', 'tmp', 'evt-probe');
const EDGE = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const server = createServer((req, res) => {
  let b = ''; req.on('data', (c) => (b += c));
  req.on('end', () => {
    if (req.url === '/') {
      res.writeHead(200, { 'content-type': 'text/html' });
      res.end(`<!doctype html><html><body><script>fetch('/v1/chat/completions',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({model:'m',messages:[{role:'user',content:'${'y'.repeat(200)}'}]})}).then(r=>r.text()).then(()=>{document.title='done'})</script></body></html>`);
    } else { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{"ok":true}'); }
  });
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const PORT = server.address().port;

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });
const child = spawn(EDGE, ['--headless=new','--disable-gpu','--no-first-run','--no-default-browser-check',
  '--enable-logging=stderr','--vmodule=extension_service=1',
  `--user-data-dir=${PROFILE}`,'--remote-debugging-port=0',`--load-extension=${PKG}`,'about:blank'],
  { stdio: ['ignore','pipe','pipe'] });
let log = ''; child.stderr.on('data', (d) => (log += d)); child.stdout.on('data', (d) => (log += d));

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

// One persistent session, kept alive by evaluating; retry past the SW teardown race.
let sid = null;
const attach = async () => { sid = (await send('Target.attachToTarget', { targetId: (await findSw()).targetId, flatten: true })).result.sessionId; await send('Runtime.enable', {}, sid); };
await attach();
const ev = async (expr) => {
  for (let attempt = 0; attempt < 6; attempt++) {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true }, sid);
    const desc = r.result?.exceptionDetails?.exception?.description || '';
    if (/chrome is not defined|Cannot read properties of undefined/.test(desc) && attempt < 5) { await sleep(150); await attach(); continue; }
    if (r.result?.exceptionDetails) return `THREW: ${desc}`;
    return r.result?.result?.value ?? JSON.stringify(r.result);
  }
  return 'gave up';
};

console.log('hasListeners (extension lane):', await ev(`String(chrome.webRequest.onBeforeRequest.hasListeners())`));
console.log('install probe listener:', await ev(`
  globalThis.__probe = { plain: 0, blocking: 0 };
  try { chrome.webRequest.onBeforeRequest.addListener(function(){ globalThis.__probe.plain++; }, {urls:['<all_urls>']}); } catch(e) { globalThis.__probe.plainErr = e.message; }
  try { chrome.webRequest.onBeforeRequest.addListener(function(){ globalThis.__probe.blocking++; }, {urls:['<all_urls>']}, ['blocking']); } catch(e) { globalThis.__probe.blockingErr = e.message; }
  'installed'
`));

const page = (await send('Target.getTargets')).result.targetInfos.find((t) => t.type === 'page');
const psid = (await send('Target.attachToTarget', { targetId: page.targetId, flatten: true })).result.sessionId;
await send('Page.enable', {}, psid);
await send('Page.navigate', { url: `http://127.0.0.1:${PORT}/` }, psid);
await sleep(3000);
await attach();

console.log('probe counts:', await ev(`JSON.stringify(globalThis.__probe)`));
console.log('extension counters:', await ev(`JSON.stringify(globalThis.__captureApp.health.counters.snapshot().counters)`));
console.log('hasListeners now:', await ev(`String(chrome.webRequest.onBeforeRequest.hasListeners())`));

// The Log domain also carries the extension's own console output.
const logs = evs.filter((e) => e.method === 'Log.entryAdded' || e.method === 'Runtime.consoleAPICalled')
  .map((e) => e.params.entry?.text || (e.params.args || []).map((a) => a.value ?? a.description).join(' '));
console.log('--- in-browser console ---');
console.log(logs.slice(0, 10).join('\n') || '(none)');
console.log('--- blocking message in browser log? ---');
console.log(log.split('\n').filter((l) => /blocking webRequest/i.test(l)).join('\n') || '(none)');

child.kill('SIGKILL');
await sleep(500);
ws.close(); server.close();
try { rmSync(PROFILE, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 }); } catch {}
