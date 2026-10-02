// Scratch probe 3: does the extension load, start its SW, and observe a loopback request?
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const EXT = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = resolve(REPO, '.tools', 'tmp', 'chrome-probe3');
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';

// ---- loopback server
const hits = [];
const server = createServer((req, res) => {
  let body = '';
  req.on('data', (c) => (body += c));
  req.on('end', () => {
    hits.push({ url: req.url, method: req.method, len: body.length });
    if (req.url === '/' ) {
      res.writeHead(200, { 'content-type': 'text/html' });
      res.end(`<!doctype html><html><body><div id="r">pending</div><script>
        fetch('/v1/chat/completions', {method:'POST',headers:{'content-type':'application/json'},
          body: JSON.stringify({model:'probe-model',messages:[{role:'user',content:'${'x'.repeat(200)}'}]})})
        .then(r => r.text()).then(t => { document.getElementById('r').textContent = 'done:' + t; })
        .catch(e => { document.getElementById('r').textContent = 'error:' + e.message; });
      </script></body></html>`);
    } else {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{"ok":true}');
    }
  });
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const PORT = server.address().port;
console.log('loopback server on 127.0.0.1:' + PORT);

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });

const child = spawn(CHROME, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--enable-unsafe-extension-debugging',
  `--user-data-dir=${PROFILE}`, '--remote-debugging-port=0',
  `--load-extension=${EXT}`,
  'about:blank',
], { stdio: ['ignore', 'pipe', 'pipe'] });
let log = '';
child.stderr.on('data', (d) => (log += d));
child.stdout.on('data', (d) => (log += d));

const portFile = join(PROFILE, 'DevToolsActivePort');
let port = null;
for (let i = 0; i < 150; i++) {
  if (existsSync(portFile)) { const l = readFileSync(portFile, 'utf8').split('\n'); if (l[0]) { port = Number(l[0]); break; } }
  await new Promise((r) => setTimeout(r, 100));
}
const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map();
const events = [];
ws.onmessage = (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } else events.push(m); };
const send = (method, params = {}, sessionId) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) })); });

await new Promise((r) => setTimeout(r, 800));
let targets = await send('Target.getTargets');
const mine = (t) => t.url.includes('ileppbadlclnikhkddpliolbgifliffh') || t.url.includes('service-worker.js');
console.log('targets at start:', targets.result.targetInfos.map((t) => `${t.type}:${t.url.slice(0, 60)}`));

let sw = targets.result.targetInfos.find((t) => t.type === 'service_worker' && t.url.includes('ileppbad'));
if (!sw) {
  console.log('no SW for the loaded extension; trying CDP Extensions.loadUnpacked');
  const r = await send('Extensions.loadUnpacked', { path: EXT });
  console.log('loadUnpacked:', JSON.stringify(r.result || r.error));
  await new Promise((r) => setTimeout(r, 1200));
  targets = await send('Target.getTargets');
  sw = targets.result.targetInfos.find((t) => t.type === 'service_worker' && !t.url.includes('fignfifoniblkonapihmkfakmlgkbkcf') && !t.url.includes('nkeimhog'));
}

// Drive a page to the loopback server.
const pageTarget = targets.result.targetInfos.find((t) => t.type === 'page');
const pageAtt = await send('Target.attachToTarget', { targetId: pageTarget.targetId, flatten: true });
const pageSid = pageAtt.result.sessionId;
await send('Page.enable', {}, pageSid);
await send('Page.navigate', { url: `http://127.0.0.1:${PORT}/` }, pageSid);
await new Promise((r) => setTimeout(r, 2500));

targets = await send('Target.getTargets');
console.log('--- targets after navigation ---');
for (const t of targets.result.targetInfos) console.log(` ${t.type} | ${t.url.slice(0, 90)}`);
console.log('server hits:', JSON.stringify(hits));

sw = targets.result.targetInfos.find((t) => t.type === 'service_worker' && !t.url.includes('fignfifoniblkonapihmkfakmlgkbkcf') && !t.url.includes('nkeimhog'));
console.log('extension SW found:', sw ? sw.url : 'NONE');
if (sw) {
  const att = await send('Target.attachToTarget', { targetId: sw.targetId, flatten: true });
  const sid = att.result.sessionId;
  await send('Runtime.enable', {}, sid);
  const ev = await send('Runtime.evaluate', { expression: 'JSON.stringify({app: typeof globalThis.__captureApp, counters: globalThis.__captureApp && globalThis.__captureApp.health.counters.snapshot(), queue: globalThis.__captureApp && globalThis.__captureApp.queue.stats(), core: globalThis.__captureApp && globalThis.__captureApp.health.report().core})', returnByValue: true }, sid);
  console.log('SW state:', ev.result?.result?.value || JSON.stringify(ev.result));
  const frames = await send('Runtime.evaluate', { expression: 'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.queue.peek(10) : null)', returnByValue: true }, sid);
  console.log('queued frames:', (frames.result?.result?.value || '').slice(0, 1200));
}

console.log('--- load-extension mentions in log ---');
console.log(log.split('\n').filter((l) => /load.extension|load_extension/i.test(l)).join('\n') || '(none)');
writeFileSync(join(PROFILE, 'probe3.log'), log, 'utf8');

child.kill('SIGKILL');
ws.close();
server.close();
