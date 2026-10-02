// Scratch: how can the extension detect that blocking is unavailable? (silent refusal, no throw)
import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..', '..', '..');
const PKG = resolve(REPO, 'apps', 'capture-extension');
const PROFILE = join(REPO, '.tools', 'tmp', 'cap-probe');
const EDGE = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });
const child = spawn(EDGE, ['--headless=new','--disable-gpu','--no-first-run','--no-default-browser-check',
  `--user-data-dir=${PROFILE}`,'--remote-debugging-port=0',`--load-extension=${PKG}`,'about:blank'],
  { stdio: ['ignore','pipe','pipe'] });
let log = ''; child.stderr.on('data', (d) => (log += d)); child.stdout.on('data', (d) => (log += d));

const pf = join(PROFILE, 'DevToolsActivePort');
let port = null;
for (let i = 0; i < 200; i++) { if (existsSync(pf)) { const l = readFileSync(pf,'utf8').split('\n')[0]; if (l) { port = Number(l); break; } } await sleep(100); }
const v = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
const ws = new WebSocket(v.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map();
ws.onmessage = (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
const send = (method, params = {}, sessionId) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) })); });

let sw = null;
for (let i = 0; i < 60 && !sw; i++) {
  const t = await send('Target.getTargets');
  sw = t.result.targetInfos.find((x) => x.type === 'service_worker' && /background\/service-worker\.js$/.test(x.url));
  if (!sw) await sleep(200);
}
const ev = async (e) => {
  // Re-attach every time: an MV3 worker can be torn down and re-created, and a stale session then
  // evaluates in a context where `chrome` is not bound. Attaching per call makes the probe honest
  // about that rather than reporting "chrome is not defined" as if it were a fact about the code.
  const t = await send('Target.getTargets');
  const target = t.result.targetInfos.find((x) => x.type === 'service_worker' && /background\/service-worker\.js$/.test(x.url));
  if (!target) return 'NO SW TARGET';
  const { sessionId } = (await send('Target.attachToTarget', { targetId: target.targetId, flatten: true })).result;
  await send('Runtime.enable', {}, sessionId);
  const r = await send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true }, sessionId);
  if (r.result?.exceptionDetails) return `THREW: ${r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text}`;
  return r.result?.result?.value ?? JSON.stringify(r.result);
};

console.log('permissions.contains webRequestBlocking:', await ev(`chrome.permissions.contains({permissions:['webRequestBlocking']}).then(v=>String(v)).catch(e=>'THREW '+e.message)`));
console.log('permissions.getAll:', await ev(`chrome.permissions.getAll().then(p=>JSON.stringify(p.permissions)).catch(e=>'THREW '+e.message)`));
console.log('manifest.permissions:', await ev(`JSON.stringify(chrome.runtime.getManifest().permissions)`));
console.log('hasListeners before:', await ev(`String(chrome.webRequest.onBeforeRequest.hasListeners())`));
console.log('addListener blocking:', await ev(`(()=>{try{chrome.webRequest.onBeforeRequest.addListener(()=>{}, {urls:['<all_urls>']}, ['blocking']);return 'NO THROW';}catch(e){return 'THREW: '+e.message}})()`));
console.log('hasListeners after blocking add:', await ev(`String(chrome.webRequest.onBeforeRequest.hasListeners())`));
console.log('addListener plain:', await ev(`(()=>{try{chrome.webRequest.onBeforeRequest.addListener(()=>{}, {urls:['<all_urls>']});return 'NO THROW';}catch(e){return 'THREW: '+e.message}})()`));
console.log('hasListeners after plain add:', await ev(`String(chrome.webRequest.onBeforeRequest.hasListeners())`));
console.log('runtime.getManifest().version:', await ev(`chrome.runtime.getManifest().version`));
console.log('--- blocking message in log? ---', /blocking webRequest/.test(log));

child.kill('SIGKILL');
ws.close();
rmSync(PROFILE, { recursive: true, force: true });
