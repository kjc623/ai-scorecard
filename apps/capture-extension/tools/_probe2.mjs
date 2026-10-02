// Scratch probe 2: why did the extension not load?
import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const EXT = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = resolve(REPO, '.tools', 'tmp', 'chrome-probe2');
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });

const args = [
  '--headless=new',
  '--disable-gpu',
  '--no-first-run',
  '--no-default-browser-check',
  '--enable-logging=stderr',
  '--v=1',
  `--user-data-dir=${PROFILE}`,
  '--remote-debugging-port=0',
  `--disable-extensions-except=${EXT}`,
  `--load-extension=${EXT}`,
  'about:blank',
];
const child = spawn(CHROME, args, { stdio: ['ignore', 'pipe', 'pipe'] });
let out = '';
child.stderr.on('data', (d) => (out += d.toString()));
child.stdout.on('data', (d) => (out += d.toString()));

const portFile = join(PROFILE, 'DevToolsActivePort');
let port = null;
for (let i = 0; i < 150; i++) {
  if (existsSync(portFile)) {
    const l = readFileSync(portFile, 'utf8').split('\n');
    if (l[0]) { port = Number(l[0]); break; }
  }
  await new Promise((r) => setTimeout(r, 100));
}

const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map();
ws.onmessage = (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
const send = (method, params = {}, sessionId) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) })); });

// What the browser knows about extensions, via CDP.
try {
  const ext = await send('Extensions.loadUnpacked', { path: EXT });
  console.log('Extensions.loadUnpacked ->', JSON.stringify(ext).slice(0, 400));
} catch (e) {
  console.log('Extensions.loadUnpacked threw:', e.message);
}

await new Promise((r) => setTimeout(r, 1500));
const targets = await send('Target.getTargets');
console.log('--- targets after loadUnpacked ---');
for (const t of targets.result.targetInfos) console.log(` ${t.type} | ${t.url.slice(0, 100)}`);

writeFileSync(join(PROFILE, 'chrome-log.txt'), out, 'utf8');
const lines = out.split('\n').filter((l) => /extension|manifest|ERROR|Failed/i.test(l));
console.log('--- log lines mentioning extension/manifest/error ---');
console.log(lines.slice(0, 40).join('\n') || '(none)');
console.log('--- log line count:', out.split('\n').length);

child.kill('SIGKILL');
ws.close();
