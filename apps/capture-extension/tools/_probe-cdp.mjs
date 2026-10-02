// Scratch probe: learn the mechanics before writing the real tool. Deleted afterwards.
import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const EXT = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = resolve(REPO, '.tools', 'tmp', 'chrome-probe');
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });

const args = [
  '--headless=new',
  '--disable-gpu',
  '--no-first-run',
  '--no-default-browser-check',
  '--disable-background-networking',
  `--user-data-dir=${PROFILE}`,
  '--remote-debugging-port=0',
  `--disable-extensions-except=${EXT}`,
  `--load-extension=${EXT}`,
  'about:blank',
];
console.log('launching:', CHROME);
const child = spawn(CHROME, args, { stdio: ['ignore', 'pipe', 'pipe'] });
let stderr = '';
child.stderr.on('data', (d) => (stderr += d.toString()));

const portFile = join(PROFILE, 'DevToolsActivePort');
let port = null;
for (let i = 0; i < 100; i++) {
  if (existsSync(portFile)) {
    const lines = readFileSync(portFile, 'utf8').split('\n');
    if (lines[0]) { port = Number(lines[0]); break; }
  }
  await new Promise((r) => setTimeout(r, 100));
}
console.log('DevTools port:', port);

const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
console.log('browser:', version.Browser);
const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

let id = 0;
const pending = new Map();
ws.onmessage = (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
};
const send = (method, params = {}, sessionId) =>
  new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) })); });

const targets = await send('Target.getTargets');
console.log('--- all targets ---');
for (const t of targets.result.targetInfos) console.log(` ${t.type} | ${t.url.slice(0, 90)}`);

const sw = targets.result.targetInfos.find((t) => t.type === 'service_worker');
console.log('service worker target:', sw ? sw.url : 'NONE');

if (sw) {
  const att = await send('Target.attachToTarget', { targetId: sw.targetId, flatten: true });
  const sid = att.result.sessionId;
  await send('Runtime.enable', {}, sid);
  const ev = await send('Runtime.evaluate', { expression: 'typeof globalThis.__captureApp', returnByValue: true }, sid);
  console.log('typeof __captureApp:', JSON.stringify(ev.result));
  const ev2 = await send('Runtime.evaluate', { expression: 'JSON.stringify({state: globalThis.__captureApp && globalThis.__captureApp.health.report()})', returnByValue: true, awaitPromise: true }, sid);
  console.log('health report:', ev2.result?.result?.value || JSON.stringify(ev2.result));
}

console.log('--- chrome stderr (last 800) ---');
console.log(stderr.slice(-800));

child.kill('SIGKILL');
ws.close();
