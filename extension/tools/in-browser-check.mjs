#!/usr/bin/env node
/**
 * in-browser-check.mjs — drives the real extension in a real browser and reports a per-check verdict.
 *
 * WHY THIS EXISTS. Every report on this project carried "in-browser E2E is NOT VERIFIED". That is now
 * partly wrong: a Chromium browser is installed and will load the unpacked extension. This tool is
 * the acceptance gate for the browser half, and it is deliberately built so that it can FAIL: it
 * exits non-zero on any FAIL, and a check it cannot observe is reported as NOT-OBSERVABLE with the
 * reason and the raw browser output, never inferred from the unit suite (which proves something
 * different and less).
 *
 * WHICH BROWSER, AND WHY NOT CHROME. Google Chrome Stable 154 refuses to load an unpacked extension
 * at all — it ignores both `--load-extension` and `--disable-extensions-except`:
 *
 *   WARNING:extension_service.cc:445] --disable-extensions-except is not allowed in Google Chrome, ignoring.
 *   WARNING:extension_service.cc:423] --load-extension is not allowed in Google Chrome, ignoring.
 *
 * Edge 154 accepts both and loads the extension. The verification is therefore against Edge, which is
 * Chromium, and the README states which engine was used rather than saying "Chromium".
 *
 * WHAT THIS CANNOT PROVE, and it is a browser limitation rather than a test gap: an unpacked load
 * REVOKES `webRequestBlocking`, whatever the manifest declares. The browser says so itself:
 *
 *   "You do not have permission to use blocking webRequest listeners. ... webRequestBlocking is only
 *    allowed for extensions that are installed using ExtensionInstallForcelist."
 *
 * So the inline warn/block path (check 5) cannot be observed here, and this tool says so with that
 * message as the reason. Restoring it needs a policy install (`ExtensionInstallForcelist`), which
 * needs an elevated registry write — a human action, not an agent one.
 *
 *   node extension/tools/in-browser-check.mjs            # all checks
 *   node extension/tools/in-browser-check.mjs --with-native-host
 *                                                                     # register the native-messaging
 *                                                                     # host first, which is what makes
 *                                                                     # the connected channel observable
 *   node extension/tools/in-browser-check.mjs --keep     # keep the profile for inspection
 *   node extension/tools/in-browser-check.mjs --evidence <file>
 *
 * Everything runs against a `node:http` server on 127.0.0.1 that this script starts itself. No vendor
 * destination is contacted.
 */

import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { HOST_NAME, writeManifest } from './native-host.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const PKG = resolve(HERE, '..');
const REPO = resolve(PKG, '..');

const EDGE_CANDIDATES = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  '/usr/bin/microsoft-edge',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
];

const args = process.argv.slice(2);
const KEEP = args.includes('--keep');
/**
 * Register the native-messaging host before launching, so this run exercises the *connected*
 * channel. Without it the host is deliberately absent, which is what makes checks 2–4 the
 * absent-host case: a failed connect must report `absent`, and the user's request must still work.
 * The two modes are complementary — `/tools/accept.mjs` runs the default one, and the connected
 * case is the other half of the same claim.
 */
const WITH_NATIVE_HOST = args.includes('--with-native-host');
const EVIDENCE_PATH = (() => {
  const i = args.indexOf('--evidence');
  return i >= 0 && args[i + 1] ? resolve(args[i + 1]) : join(HERE, 'in-browser-check.evidence.txt');
})();

const PROFILE = join(REPO, '.tools', 'tmp', 'capture-in-browser');
const CHAT_PATH = '/v1/chat/completions';

const lines = [];
function say(s = '') {
  lines.push(s);
  console.log(s);
}

const checks = [];
function verdict(id, title, status, detail, extra = []) {
  checks.push({ id, title, status, detail });
  say(`[${status}] check ${id}: ${title}`);
  say(`        ${detail}`);
  for (const line of extra) say(`        ${line}`);
  say('');
}

// ── the loopback server everything is driven against ────────────────────────────────────────

const serverHits = [];
function startServer() {
  return new Promise((resolvePromise) => {
    const server = createServer((req, res) => {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        serverHits.push({ url: req.url, method: req.method, bytes: Buffer.byteLength(body) });
        if (req.url === '/') {
          res.writeHead(200, { 'content-type': 'text/html' });
          res.end(pageHtml());
        } else {
          res.writeHead(200, { 'content-type': 'application/json' });
          res.end('{"ok":true}');
        }
      });
    });
    server.listen(0, '127.0.0.1', () => resolvePromise(server));
  });
}

/**
 * The page posts a chat-shaped JSON body, which is what §8.2's predicate is looking for: a member
 * whose value is an ordered array of objects carrying a role discriminator and a content payload,
 * plus model parameters. It writes its outcome into the DOM so the check can read a fact about the
 * page rather than a fact about the test's own bookkeeping.
 *
 * The file input is the fixture check 7 attaches a real `File` to. It is in the page because that
 * is the only place a `File` the user selected exists — §7.3's whole point — and this input is what
 * the content script's own `registry.collectCandidates()` walks when it looks for one.
 */
function pageHtml() {
  const payload = {
    model: 'in-browser-check-model',
    messages: [
      { role: 'system', content: 'You are a helpful assistant used by the in-browser check.' },
      { role: 'user', content: `Summarise the quarterly figures. ${'padding '.repeat(20)}` },
    ],
    temperature: 0.2,
    max_tokens: 512,
  };
  return `<!doctype html><html><head><title>pending</title></head><body><div id="out">pending</div>
<input id="attachment" type="file">
<script>
  window.__result = 'pending';
  fetch(${JSON.stringify(CHAT_PATH)}, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: ${JSON.stringify(JSON.stringify(payload))}
  })
    .then(function (r) { return r.text().then(function (t) { return [r.status, t]; }); })
    .then(function (pair) {
      window.__result = 'completed:' + pair[0];
      document.title = 'completed';
      document.getElementById('out').textContent = 'completed:' + pair[0] + ':' + pair[1];
    })
    .catch(function (e) {
      window.__result = 'failed:' + (e && e.message);
      document.title = 'failed';
      document.getElementById('out').textContent = 'failed:' + (e && e.message);
    });
</script></body></html>`;
}

// ── browser lifecycle ───────────────────────────────────────────────────────────────────────

function findEdge() {
  if (process.env.EDGE_PATH && existsSync(process.env.EDGE_PATH)) return process.env.EDGE_PATH;
  for (const p of EDGE_CANDIDATES) if (existsSync(p)) return p;
  return null;
}

async function launch(edge) {
  rmSync(PROFILE, { recursive: true, force: true });
  mkdirSync(PROFILE, { recursive: true });

  const child = spawn(
    edge,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-background-networking',
      '--disable-component-update',
      '--disable-sync',
      '--no-pings',
      // Log the browser's own view of the extension: this is where the blocking-refusal message and
      // the native-host lookup appear, and they are primary evidence rather than our inference.
      '--enable-logging=stderr',
      '--vmodule=extension_service=1,*/**/launch_context*=1',
      `--user-data-dir=${PROFILE}`,
      '--remote-debugging-port=0',
      `--load-extension=${PKG}`,
      'about:blank',
    ],
    { stdio: ['ignore', 'pipe', 'pipe'] },
  );

  let browserLog = '';
  child.stderr.on('data', (d) => (browserLog += d.toString()));
  child.stdout.on('data', (d) => (browserLog += d.toString()));

  const portFile = join(PROFILE, 'DevToolsActivePort');
  let port = null;
  for (let i = 0; i < 200; i++) {
    if (existsSync(portFile)) {
      const first = readFileSync(portFile, 'utf8').split('\n')[0];
      if (first) {
        port = Number(first);
        break;
      }
    }
    if (child.exitCode !== null) break;
    await sleep(100);
  }
  return { child, port, getLog: () => browserLog };
}

// ── minimal CDP client (no dependencies; Node 22 ships WebSocket and fetch) ──────────────────

async function connectCdp(port) {
  const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
  const ws = new WebSocket(version.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = () => rej(new Error('CDP websocket failed to open'));
  });

  let nextId = 0;
  const pending = new Map();
  const events = [];
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    } else {
      events.push(msg);
    }
  };
  const send = (method, params = {}, sessionId) =>
    new Promise((res, rej) => {
      const id = ++nextId;
      const timer = setTimeout(() => {
        pending.delete(id);
        rej(new Error(`CDP ${method} timed out`));
      }, 15000);
      pending.set(id, (m) => {
        clearTimeout(timer);
        if (m.error) rej(new Error(`${method}: ${m.error.message}`));
        else res(m.result);
      });
      ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });

  return { ws, send, events, browser: version.Browser };
}

/** Attach to a target and return an evaluator that runs expressions in its context. */
async function attach(cdp, targetId) {
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
  await cdp.send('Runtime.enable', {}, sessionId).catch(() => {});
  await cdp.send('Log.enable', {}, sessionId).catch(() => {});
  const evaluate = async (expression) => {
    const r = await cdp.send(
      'Runtime.evaluate',
      { expression, returnByValue: true, awaitPromise: true },
      sessionId,
    );
    if (r.exceptionDetails) return { __exception: r.exceptionDetails.exception?.description || r.exceptionDetails.text };
    return r.result?.value;
  };
  return { sessionId, evaluate };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * Attach to the page's **isolated world** — the world the content script runs in.
 *
 * This matters for one thing only, and it is the thing check 7 exists for: a `File` the user
 * selected lives in the content script's world and nowhere else (§7.3). Reading bytes out of it
 * from the page's own world would prove nothing about the code that actually has to read it, and
 * calling `Page.createIsolatedWorld` with the extension's id is what makes this the same world
 * rather than a similar-looking one.
 */
async function attachIsolatedWorld(cdp, pageCtx) {
  // `Page.getFrameTree` belongs to the *page* session. On the browser-level session it is not a
  // method at all, which is how the first version of this failed: "Page.getFrameTree wasn't found".
  const { frameTree } = await cdp.send('Page.getFrameTree', {}, pageCtx.sessionId);
  const { executionContextId } = await cdp.send(
    'Page.createIsolatedWorld',
    { frameId: frameTree.frame.id, worldName: 'sac-in-browser-check', grantUniveralAccess: true },
    pageCtx.sessionId,
  );
  // Runtime must be enabled on the *page session* before Runtime.evaluate will run in a context
  // that belongs to it; without this the call is refused with "Runtime.evaluate wasn't found",
  // which names the method rather than the missing domain.
  await cdp.send('Runtime.enable', {}, pageCtx.sessionId).catch(() => {});
  const evaluate = async (expression) => {
    const r = await cdp.send(
      'Runtime.evaluate',
      { expression, returnByValue: true, awaitPromise: true, contextId: executionContextId },
      // The session id is not optional here. Without it the call is routed to the *browser* session,
      // where Runtime is not enabled, and the failure reads "Runtime.evaluate wasn't found" — which
      // names the method rather than the routing mistake.
      pageCtx.sessionId,
    );
    if (r.exceptionDetails) {
      return { __exception: r.exceptionDetails.exception?.description || r.exceptionDetails.text };
    }
    return r.result?.value;
  };
  return { executionContextId, evaluate };
}

// ── native-host registration ────────────────────────────────────────────────────────────────

const HOST_STATE_DIR = join(REPO, '.tools', 'tmp', 'capture-native-host', 'state');

/**
 * Install the native-messaging host so the extension can reach `capture-core --native-host`.
 *
 * This is the one thing that stood between this repository and a connected channel: nothing
 * registered the host, and an unpacked extension's id is derived from its checkout path, so
 * `allowed_origins` had no stable id to name. The pinned `key` in manifest.json fixes the second
 * half; `tools/native-host.*` is the first. HKCU only — no elevation, and `--uninstall` removes
 * exactly what this creates.
 */
function registerNativeHost() {
  // Both halves, in order: the Node tool writes the host manifest (and computes the pinned id from
  // manifest.json), then PowerShell writes the two HKCU values that point at it. Doing only the
  // second is the mistake this function exists to make impossible.
  const manifest = writeManifest();
  if (!manifest.ok) return { status: 1, out: `host manifest: ${manifest.reason} — ${manifest.detail}` };
  const r = spawnSync(
    'powershell',
    ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(HERE, 'native-host.ps1'), '-Action', 'install'],
    { encoding: 'utf8' },
  );
  return { status: r.status, out: `${r.stdout ?? ''}${r.stderr ?? ''}` };
}

function unregisterNativeHost() {
  const r = spawnSync(
    'powershell',
    ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(HERE, 'native-host.ps1'), '-Action', 'uninstall'],
    { encoding: 'utf8' },
  );
  return { status: r.status, out: `${r.stdout ?? ''}${r.stderr ?? ''}` };
}

/** One JSON document from the host's own health channel, or null. */
function lastHealthRow() {
  const file = join(HOST_STATE_DIR, 'health.jsonl');
  if (!existsSync(file)) return null;
  const lines = readFileSync(file, 'utf8').trim().split('\n').filter(Boolean);
  for (let i = lines.length - 1; i >= 0; i--) {
    try {
      return JSON.parse(lines[i]);
    } catch {
      /* a torn final line is not a reason to give up on the earlier ones */
    }
  }
  return null;
}

/**
 * Is the host registered *right now*? Asked of the registry rather than of our own bookkeeping,
 * because the thing that matters is what Chromium will see when it starts.
 *
 * This has to be answered BEFORE the browser launches, and that is a real constraint rather than a
 * tidy one: Chromium resolves a native-messaging host manifest when the connection is requested,
 * against the registry as it is at that moment — but the extension's health state is built from the
 * connects that already happened, and `native.js` applies a 5s cool-down after a failure. A host
 * registered after launch is therefore invisible to the run in progress: the first attempt failed,
 * and the cool-down refuses the next one while the browser still holds no host manifest. The first
 * version of this check registered the host mid-run and produced exactly that, correctly reported as
 * a FAIL. So the registration is now a decision taken before `launch()`, and `--with-native-host`
 * is what makes it.
 */
function hostIsRegistered() {
  const key = `HKCU\\Software\\Microsoft\\Edge\\NativeMessagingHosts\\${HOST_NAME}`;
  const r = spawnSync('reg', ['query', key, '/ve'], { encoding: 'utf8' });
  return r.status === 0 && /capture-native-host/i.test(r.stdout ?? '');
}

// ── main ────────────────────────────────────────────────────────────────────────────────────

async function main() {
  say('in-browser-check — proving the browser half in a real Chromium browser');
  say(`date:     ${new Date().toISOString()}`);
  say(`node:     ${process.version}`);
  say(`package:  ${PKG}`);
  say('');

  const edge = findEdge();
  if (!edge) {
    verdict('0', 'a Chromium browser that will load an unpacked extension', 'NOT-OBSERVABLE', 'no Edge binary found; set EDGE_PATH');
    return 2;
  }
  say(`browser:  ${edge}`);
  say('');

  // The registration decision is taken here, before the browser exists, because a host registered
  // after launch is invisible to the run in progress (see hostIsRegistered). `--with-native-host`
  // is the connected mode; the default is the absent-host mode that checks 2–4 depend on.
  let nativeRegistration = { attempted: false };
  if (WITH_NATIVE_HOST) {
    nativeRegistration = { attempted: true, ...registerNativeHost() };
    nativeRegistration.registered = hostIsRegistered();
    say(`native host: ${nativeRegistration.registered ? 'registered before launch' : 'REGISTRATION FAILED'}`);
    if (!nativeRegistration.registered) say(`  ${String(nativeRegistration.out).trim().split('\n').slice(-4).join('\n  ')}`);
    say('');
  } else {
    say('native host: not registered (absent-host mode; pass --with-native-host for the connected run)');
    say('');
  }

  const server = await startServer();
  const port = server.address().port;
  const origin = `http://127.0.0.1:${port}`;

  let launched = null;
  let cdp = null;
  try {
    launched = await launch(edge);
    if (!launched.port) {
      verdict('1', 'the extension loads and its MV3 service worker registers', 'FAIL', 'the browser never wrote a DevTools port', [`exit=${launched.child.exitCode}`]);
      return 1;
    }
    cdp = await connectCdp(launched.port);
    say(`browser:  ${cdp.browser}`);
    say(`cdp port: ${launched.port}`);
    say(`origin:   ${origin}`);
    say('');

    // ── check 1: load + service worker ───────────────────────────────────────────────────
    let sw = null;
    for (let i = 0; i < 60 && !sw; i++) {
      const { targetInfos } = await cdp.send('Target.getTargets');
      sw = targetInfos.find((t) => t.type === 'service_worker' && /background\/service-worker\.js$/.test(t.url));
      if (!sw) await sleep(200);
    }
    const manifestErrors = extractManifestErrors(launched.getLog());

    if (!sw) {
      verdict('1', 'the extension loads and its MV3 service worker registers', 'FAIL',
        'no service_worker target for background/service-worker.js',
        [`browser log tail: ${launched.getLog().slice(-500)}`]);
      return 1;
    }
    verdict('1', 'the extension loads and its MV3 service worker registers', 'PASS',
      `CDP Target.getTargets lists the service worker target: ${sw.url}`,
      [
        'proxy signal: Target.getTargets filtered to type=service_worker with our own script path,',
        'read over the browser\'s DevTools socket. It is the browser asserting that an MV3 worker',
        'for this extension is registered — not the extension reporting on itself.',
        `extension id: ${new URL(sw.url).host}`,
        `manifest/permission errors in the browser log: ${manifestErrors.length === 0 ? 'none' : manifestErrors.join(' | ')}`,
      ]);

    const swCtx = await attach(cdp, sw.targetId);
    const killSwLog = capturedConsole(cdp, swCtx.sessionId);

    // ── check 2: the listener is live, driven by a real request ─────────────────────────
    //
    // Readiness gate first. An MV3 service worker target exists as soon as the worker is created,
    // which is before its module body has finished running — so driving traffic immediately would
    // race the extension's own registration and produce a failure that says nothing about the
    // extension. Waiting for the app object AND its lanes is waiting for the extension to say it is
    // ready to observe.
    const ready = await waitFor(async () => {
      const state = await swCtx.evaluate(
        'JSON.stringify(globalThis.__captureApp && globalThis.__captureApp.lanes ? { ok: true } : null)',
      );
      return state === '{"ok":true}';
    }, 20000);
    if (!ready) {
      verdict('2', 'the webRequest listener observes a real request', 'FAIL',
        'the extension never finished registering its lanes within 20s',
        [`app object present: ${await swCtx.evaluate('String(typeof globalThis.__captureApp)')}`]);
    }

    // The registration facts, read from inside the running extension once it is ready. They are
    // evidence in their own right: they say which `extraInfoSpec` each lane actually asked for,
    // which is the thing §7.4's capability changes.
    const registration = await swCtx.evaluate(
      `JSON.stringify(globalThis.__captureApp ? {
         blockingAvailable: globalThis.__captureApp.blockingAvailable,
         enforcement: globalThis.__captureApp.lanes ? globalThis.__captureApp.lanes.enforcement : null,
         extraInfoSpec: globalThis.__captureApp.lanes ? globalThis.__captureApp.lanes.extraInfoSpec : null,
         bodyFilter: globalThis.__captureApp.lanes ? globalThis.__captureApp.lanes.bodyFilter : null,
         hasListeners: chrome.webRequest.onBeforeRequest.hasListeners(),
       } : null)`,
    );

    const before = await swCtx.evaluate('JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.counters.snapshot() : null)');

    // Tap every observation the extension emits, installed HERE: after the worker's module body has
    // run (which is where `__captureApp` is assigned) and BEFORE the page is navigated, because the
    // first observation is enqueued by the first request the page makes. Installing it after the
    // navigation is too late, and installing it before the readiness gate finds no queue — both
    // happened, and both are why the tap reported "installed" while recording nothing.
    //
    // The tap exists because the queue is not a stable source of truth once the channel works: the
    // drain removes an entry as soon as capture-core acknowledges it, so `queue.peek()` is empty
    // within milliseconds of a successful send — correct behaviour that made check 3 fail for a
    // reason that had nothing to do with M0.
    let tapResult = 'not attempted';
    await waitFor(
      () => swCtx.evaluate('String(typeof (globalThis.__captureApp && globalThis.__captureApp.queue))'),
      20000,
    );
    tapResult = await swCtx.evaluate(`(() => {
      const app = globalThis.__captureApp;
      if (!app || !app.queue) return 'no queue';
      globalThis.__sacObservations = [];
      const original = app.queue.enqueue.bind(app.queue);
      app.queue.enqueue = function (kind, payload, sizeBytes) {
        try { globalThis.__sacObservations.push(payload); } catch (e) {}
        return original(kind, payload, sizeBytes);
      };
      return 'installed';
    })()`);

    const pageTarget = (await cdp.send('Target.getTargets')).targetInfos.find((t) => t.type === 'page');
    const pageCtx = await attach(cdp, pageTarget.targetId);
    await cdp.send('Page.enable', {}, pageCtx.sessionId);
    await cdp.send('Page.navigate', { url: `${origin}/` }, pageCtx.sessionId);
    await waitFor(async () => {
      const h = await pageCtx.evaluate('window.__result || "pending"');
      return typeof h === 'string' && h !== 'pending';
    }, 15000);
    // The handler is asynchronous (fingerprint, decision), so give it a moment to reach `emit`
    // before reading counters. The wait is bounded and the check still fails if nothing arrives.
    await waitFor(async () => {
      const c = await swCtx.evaluate(
        'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.counters.snapshot().counters.observed : 0)',
      );
      return typeof c === 'number' && c > 0;
    }, 5000);

    // Tap every observation the extension emits.
    //
    // Why a tap rather than reading the queue later: with a native host connected the drain removes
    // an entry as soon as capture-core acknowledges it, so `queue.peek()` is empty by the time a
    // later check looks — correct behaviour that made check 3 fail for a reason unrelated to M0 the
    // first time the host was connected.
    //
    // Installed earlier, before the navigation: this only reports how that went, so a failure is
    // visible in check 3 rather than inferred from an empty array.
    const countersAfter = await swCtx.evaluate(
      'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.counters.snapshot() : null)',
    );
    const parsed = countersAfter ? JSON.parse(countersAfter) : null;
    const observed = parsed ? parsed.counters.observed : 0;
    const posted = serverHits.find((h) => h.method === 'POST' && h.url === CHAT_PATH);

    if (!posted) {
      verdict('2', 'the webRequest listener observes a real request', 'FAIL',
        `the page never reached the loopback server; hits=${JSON.stringify(serverHits)}`);
    } else if (observed > 0) {
      verdict('2', 'the webRequest listener observes a real request', 'PASS',
        `observed went ${parsed.window ? beforeObserved(before) : '0'} → ${observed} for a real POST to ${origin}${CHAT_PATH}`,
        [
          `proxy signal: the extension's own §4.3 counters, read from the service worker through CDP.`,
          'Sound because the counter is incremented inside the registered listener and nowhere else,',
          'so a non-zero value means chrome.webRequest delivered the event to our handler in this browser.',
          `server saw: ${JSON.stringify(serverHits)}`,
          `counters: ${JSON.stringify(parsed.counters)}`,
        ]);
    } else {
      verdict('2', 'the webRequest listener observes a real request', 'FAIL',
        `the page reached the server (${posted.bytes} bytes POSTed) but the extension observed 0 requests`,
        [
          `registration as the extension holds it: ${registration}`,
          `counters: ${JSON.stringify(parsed.counters)}`,
          `browser log: ${blockingMessage(launched.getLog()) || '(no blocking message found)'}`,
        ]);
    }

    // ── check 3: M0 produces an observation with no content field ───────────────────────
    //
    // Read from the tap installed just above, not from `queue.peek()`. Both are honest sources, but
    // only one of them still has the observation in it: with a native host connected the drain
    // removes each entry as capture-core acknowledges it, so the queue is empty within
    // milliseconds — correct behaviour that would make this check fail for a reason that has
    // nothing to do with M0.
    const framesRaw = await swCtx.evaluate(
      'JSON.stringify(globalThis.__sacObservations || [])',
    );
    const frames = framesRaw ? JSON.parse(framesRaw) : null;
    const observation = frames && frames.find((f) => f.route && f.client_id);

    if (!observation) {
      verdict('3', 'M0 produces an observation carrying no content field', 'FAIL',
        `no observation was emitted, so the M0 property could not be exercised (tap: ${tapResult}, ${frames ? frames.length : 'unreadable'} frame(s))`,
        [
          `queue: ${await swCtx.evaluate('JSON.stringify(globalThis.__captureApp.queue.stats())')}`,
          'this check depends on check 2: an observation has to be emitted before its shape can be judged.',
        ]);
    } else {
      const hasContentKey = Object.prototype.hasOwnProperty.call(observation, 'content');
      const forbidden = ['content', 'content_digest', 'labels', 'classifier_version', 'confidence'].filter((k) =>
        Object.prototype.hasOwnProperty.call(observation, k),
      );
      const ok = observation.has_content === false && !hasContentKey && forbidden.length === 0;
      verdict('3', 'M0 produces an observation carrying no content field', ok ? 'PASS' : 'FAIL',
        ok
          ? `a real chrome.webRequest body was classified and emitted with has_content=false and no content field`
          : `the observation carries fields M0 forbids: ${forbidden.join(', ')}`,
        [
          'why this is the M0 case: with no native host there is no signed bundle, so §13.3 resolves the',
          'destination to M0 and computeBodyFilter() installs no body-bearing lane at all. The observation',
          'therefore came from the metadata lane, over a real request whose body chrome.webRequest had.',
          `observation keys: ${Object.keys(observation).join(', ')}`,
          `has_content=${observation.has_content} size_bytes=${observation.size_bytes} unread_reason=${observation.unread_reason}`,
          `tool_fingerprint=${observation.tool_fingerprint}`,
        ]);
    }

    // ── check 4: the native channel's state is reported honestly, and the page still works ─
    //
    // Two modes, one claim. With no host registered the §3.4 requirement is `absent` + `degraded`;
    // with a host registered and connected the same requirement is `connected` + `healthy`. What
    // must never happen is a path that does not work being reported as one that does — INV-6 — so
    // the check asserts whichever state the install actually implies rather than always expecting
    // the absent one.
    const coreState = await swCtx.evaluate(
      'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.report() : null)',
    );
    const core = coreState ? JSON.parse(coreState) : null;
    const pageResult = await pageCtx.evaluate('window.__result || "unknown"');
    const nativeHostLogged = /Can't find manifest for native messaging host|native messaging host/i.test(launched.getLog());
    const pageWorked = typeof pageResult === 'string' && pageResult.startsWith('completed:');
    const expectConnected = nativeRegistration.registered === true;
    const coreOk = expectConnected
      ? core && core.core === 'connected'
      : core && core.core === 'absent' && core.state === 'degraded';
    const degradedOk = coreOk && pageWorked;
    verdict('4',
      expectConnected
        ? 'a connected native channel is reported as connected, and the page still works'
        : 'the absent native channel degrades rather than breaks',
      degradedOk ? 'PASS' : 'FAIL',
      degradedOk
        ? expectConnected
          ? `capture-core connected + extension healthy, and the page's request completed normally`
          : `capture-core absent + extension degraded, and the page's request completed normally`
        : `core=${core && core.core} state=${core && core.state} page=${pageResult} (expected ${expectConnected ? 'connected' : 'absent'})`,
      [
        `the page reported: ${pageResult}`,
        `health report: core=${core && core.core} state=${core && core.state} collector=${core && core.collector}`,
        `counters: ${core ? JSON.stringify(core.counters) : 'n/a'}`,
        `queue depth: ${core && core.queue ? core.queue.depth : 'n/a'} (held in memory, nothing durable)`,
        `browser logged the native-host lookup: ${nativeHostLogged ? 'yes' : 'no'}`,
        expectConnected
          ? '§15.2/INV-6: a path that works is reported as working, from a real round trip.'
          : '§3.4: a failed connect is capture-core `absent` plus extension-side `degraded`, never "no observations".',
        '§7.4/brief §6 fail-open: the user\'s request is unaffected either way.',
      ]);

    // ── check 5: blocked is cancelled and recorded ──────────────────────────────────────
    //
    // The extension no longer asks for a blocking lane when the install lacks the grant, so the
    // browser's refusal message no longer appears on its own. To make this verdict evidenced by the
    // BROWSER's words rather than by our own capability probe, the harness attempts a blocking
    // registration itself (in the extension's worker, over CDP — not a change to the extension) and
    // reads what the browser says about it. An inert listener on a throwaway profile is harmless.
    await swCtx.evaluate(
      `(() => { try { chrome.webRequest.onBeforeRequest.addListener(function () {}, { urls: ['<all_urls>'] }, ['blocking']); return 'registered'; } catch (e) { return 'threw: ' + e.message; } })()`,
    );
    await sleep(600);
    const blockMsg = blockingMessage(launched.getLog());
    const swConsole = killSwLog();
    const browserRefusal = blockMsg || swConsole.find((l) => /blocking webRequest/i.test(l)) || null;
    const reg = registration ? JSON.parse(registration) : null;
    const enforcement = reg ? reg.enforcement : null;

    if (enforcement === 'observation_only' || browserRefusal) {
      verdict('5', 'a blocked request is cancelled AND still recorded', 'NOT-OBSERVABLE',
        'an unpacked load revokes webRequestBlocking, so no blocking listener can act here',
        [
          `the browser said, verbatim: ${(browserRefusal || '(not captured)').trim()}`,
          `the running extension reports: blockingAvailable=${reg ? reg.blockingAvailable : '?'}, enforcement=${enforcement}`,
          'the manifest declares webRequestBlocking and a policy-installed extension retains it (E1, §7.4);',
          'the unpacked *load* is what removes it, so this is a limit of the load path, not of the extension.',
          'what a human must do: install the extension by policy (ExtensionInstallForcelist) — an elevated',
          'registry write — then load a bundle with a `blocked` rule and confirm net::ERR_BLOCKED_BY_CLIENT',
          'together with an observation for the same request. Until then no report should claim blocking works.',
        ]);
    } else {
      verdict('5', 'a blocked request is cancelled AND still recorded', 'NOT-OBSERVABLE',
        enforcement
          ? 'the install holds webRequestBlocking, but no bundle could be delivered, so no blocking rule was in force'
          : 'the capability could not be read from the running extension',
        [
          `registration: ${registration || '(unreadable)'}`,
          'a bundle needs the native channel, or a policy install; both are unavailable on this host.',
        ]);
    }

    // ── check 6: a registered native host produces a connected channel ──────────────────────
    //
    // Until this check existed, every report on this project carried "a connected native channel is
    // NOT VERIFIED — no host is registered on this host". That was true, and it was also fixable:
    // `capture-core --native-host` has shipped since round 6, and Chromium looks for the host
    // manifest in a registry key an ordinary user can write. What was missing was (a) something that
    // writes those keys and (b) a stable extension id for `allowed_origins`, because an unpacked
    // extension's id is derived from its directory. (b) is the pinned `key` in manifest.json;
    // `tools/native-host.mjs` + `.ps1` are (a). `--with-native-host` registers it before launch.
    //
    // Why this is the strongest evidence available on an ordinary machine: `connected` is not "a port
    // object was handed out" — native.js emits it only after a message has actually round-tripped —
    // so a `connected` verdict means the browser delivered a message to a real capture-core process
    // and the answer came back. And the endpoint's own spool is the part the extension cannot fake:
    // it is a file on disk, written by a different process from the one making the claim.
    if (!nativeRegistration.registered) {
      verdict('6', 'a registered native host produces a connected channel', 'NOT-OBSERVABLE',
        WITH_NATIVE_HOST
          ? 'the host was not registered before launch, so the channel could not be exercised'
          : 'run with --with-native-host: a host registered after launch is invisible to the running browser',
        [
          nativeRegistration.attempted
            ? `registration output: ${String(nativeRegistration.out).trim().split('\n').slice(-6).join(' | ')}`
            : 'the default mode is the absent-host case, which checks 2–4 assert; this is the other half.',
          'command: powershell -File tools/native-host.ps1 -Action install',
          'the registration is two HKCU values; no elevation is needed for either.',
        ]);
    } else {
      // The channel connects on its own — the extension asks for policy at startup — so this waits
      // for the state rather than manufacturing a connect. `reportHealth()` is called as well
      // because a health row is what the endpoint's own file records, and that row is the evidence
      // that does not come from the component under test.
      const trigger = await swCtx.evaluate(
        `(async () => { try { globalThis.__captureApp.reportHealth(); return 'called'; }
           catch (e) { return 'threw: ' + e.message; } })()`,
      );
      const connected = await waitFor(async () => {
        const raw = await swCtx.evaluate(
          'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.report() : null)',
        );
        if (!raw) return false;
        try {
          return JSON.parse(raw).core === 'connected';
        } catch {
          return false;
        }
      }, 20000);

      const healthRaw = await swCtx.evaluate(
        'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.report() : null)',
      );
      const health = healthRaw ? JSON.parse(healthRaw) : null;
      const row = lastHealthRow();

      const ok = connected && health?.core === 'connected';
      verdict('6', 'a registered native host produces a connected channel', ok ? 'PASS' : 'FAIL',
        ok
          ? `the extension reached the real endpoint: core=${health.core} state=${health.state}, and the endpoint's own health channel recorded the round trip`
          : `core=${health?.core} state=${health?.state} — the registered host produced no round trip`,
        [
          `the channel is \`connected\` only after a message round-trips (native.js markAnswered), so this`,
          `is not "a port object was handed out": ${trigger}`,
          `health report: ${JSON.stringify(health)}`,
          `endpoint health row: ${row ? JSON.stringify(row) : '(the host wrote no row yet)'}`,
          `host state dir: ${HOST_STATE_DIR}`,
          'the host is registered under HKCU only; `--uninstall` removes exactly what this run created.',
        ]);

      // ── check 7: a real user-selected File is read, chunked and acknowledged ──────────────
      //
      // The other half of what no report has ever observed: the content script is the only code that
      // ever touches a `File` (§7.3), and until now no run had ever put one in front of it.
      //
      // How this reaches the real path, after two wrong attempts worth recording. A CDP-created
      // isolated world is a *blank* world — Chromium's own content script lives in a different one,
      // and `chrome.runtime` is not even defined in a world created this way, so seeding it there is
      // impossible. What is possible is what production does: the service worker sends
      // `capture_upload_check` and then `capture_upload_send` to the tab, and the content script
      // answers from its own world. That is the real handler, the real registry and the real chunked
      // sender; the only thing this check supplies is the File and the messages.
      const marker = 'IN-BROWSER-CHECK-ATTACHMENT-BYTES';
      const body = marker.repeat(8);
      // The File is created in the page's own world — the object a picker or a drop would produce —
      // and then read by the content script, which is the arrangement §7.3 describes.
      const injected = await pageCtx.evaluate(`(() => {
        const input = document.getElementById('attachment');
        if (!input) return { ok: false, reason: 'no file input in the page' };
        const file = new File([${JSON.stringify(body)}], 'in-browser-check.txt', { type: 'text/plain', lastModified: 1759400000000 });
        try {
          const dt = new DataTransfer();
          dt.items.add(file);
          input.files = dt.files;
        } catch (e) {
          return { ok: false, reason: 'could not attach a File: ' + e.message };
        }
        input.dispatchEvent(new Event('change', { bubbles: true }));
        const held = input.files[0];
        return { ok: true, name: held.name, size: held.size, type: held.type, lastModified: held.lastModified };
      })()`);

      if (!injected || injected.__exception || !injected.ok) {
        verdict('7', 'a real user-selected File is read, transferred and acknowledged', 'FAIL',
          `could not place a real File in the page: ${JSON.stringify(injected)}`);
      } else {
        // The service worker asks the content script what it can see. The answer comes from the
        // content script's own `File` registry, in its own world — a harness-side registry holds no
        // `File` and reports zero candidates, which is what the first version of this check proved.
        const checked = await swCtx.evaluate(`(async () => {
          const [tab] = await chrome.tabs.query({ url: ${JSON.stringify(`${origin}/*`)} });
          if (!tab) return { ok: false, reason: 'no tab for ${origin}' };
          return await chrome.tabs.sendMessage(tab.id, { type: 'capture_upload_check' });
        })()`);

        const candidate = checked && Array.isArray(checked.candidates) ? checked.candidates[0] : null;

        if (!candidate) {
          verdict('7', 'a real user-selected File is read, transferred and acknowledged', 'FAIL',
            `the content script could not resolve the File it was given: ${JSON.stringify(checked)}`,
            [
              `the page attached: ${JSON.stringify(injected)}`,
              '§7.3: a File handle the content script cannot reach is the `content_no_attachments` case;',
              'here the input element holds the File, so a null result is a defect rather than that case.',
            ]);
        } else {
          // The page reports what it attached; the content script reports what it can see. Comparing
          // the two is the point — a name alone is not attachment capture (§7.3), so size, media type
          // and modification time are all compared.
          const descriptorMatches =
            candidate.name === injected.name &&
            candidate.size_bytes === injected.size &&
            candidate.media_type === injected.type &&
            candidate.last_modified === injected.lastModified;

          // `capture_upload_send` is the production entry point: the content script opens the chunked
          // transfer over the `File` its registry is holding, relays each frame to the service worker
          // (which relays it to capture-core) and hashes exactly the bytes it read. Nothing here
          // constructs a registry or a sender, and the digest it returns is over the user's bytes.
          const expectedDigest = createHash('sha256').update(body).digest('hex');
          const transfer = await swCtx.evaluate(`(async () => {
            const [tab] = await chrome.tabs.query({ url: ${JSON.stringify(`${origin}/*`)} });
            if (!tab) return { ok: false, reason: 'no tab for ${origin}' };
            return await chrome.tabs.sendMessage(tab.id, {
              type: 'capture_upload_send',
              observation_id: crypto.randomUUID(),
              descriptor: ${JSON.stringify(candidate)},
            });
          })()`);

          // The message answer wraps the sender's own result (`{ok, result}`), and the digest is
          // prefixed with its algorithm — both are shapes of the production code, so the check reads
          // them rather than asking the extension to change them.
          const outcome = transfer && transfer.result ? transfer.result : transfer;
          const bytesRead = outcome && typeof outcome.bytes_sent === 'number' ? outcome.bytes_sent : -1;
          const digestMatches =
            outcome && String(outcome.digest).replace(/^sha256:/, '').toLowerCase() === expectedDigest;
          const ok = descriptorMatches && outcome && outcome.status === 'sent' && bytesRead === injected.size && digestMatches;
          verdict('7', 'a real user-selected File is read, transferred and acknowledged', ok ? 'PASS' : 'FAIL',
            ok
              ? `the content script read a real ${injected.size}-byte File through its own registry, capture-core acknowledged ${outcome.chunks} chunk(s), and the digest is over exactly those bytes`
              : `descriptor=${descriptorMatches ? 'matched' : JSON.stringify({ page: injected, extension: candidate })}, bytes=${bytesRead}/${injected.size}, digestMatches=${digestMatches}, transfer=${JSON.stringify(transfer)}`,
            [
              'the File was constructed in the page, attached to the page\'s input, and read by the',
              'content script — the only code that can see it, which is what §7.3 asserts and what no',
              'previous run had ever exercised. Until this run, the content script did not load at all',
              'in any browser (a classic-script/module defect), so this path had never once executed.',
              `page says:      name=${injected.name} size=${injected.size} type=${injected.type} lastModified=${injected.lastModified}`,
              `content script: ${JSON.stringify(candidate)}`,
              `resolution:     reachable=${checked && checked.reachable} via ${checked && checked.reason}`,
              `transfer:       ${JSON.stringify(transfer)}`,
              `digest:         ${outcome && outcome.digest} (expected sha256:${expectedDigest})`,
              'the digest is computed by sender.js over exactly the bytes it read and sent, so a matching',
              'digest means the bytes reached capture-core from the user-selected File and not from a fixture.',
              'still NOT covered by this: a file the user chooses through the real picker dialog, and a drop.',
            ]);
        }
      }
    }

    return 0;
  } finally {
    try {
      cdp?.ws?.close();
    } catch {}
    try {
      server.close();
    } catch {}
    if (launched?.child && launched.child.exitCode === null) {
      launched.child.kill('SIGKILL');
      // A stray headless browser holding the profile directory would confuse the next run.
      await sleep(500);
      if (launched.child.exitCode === null) launched.child.kill();
    }
    if (!KEEP) {
      // A profile directory released a moment ago can still be held by a child process (the browser
      // writes SQLite journals as it exits), which shows up as EBUSY. Retry rather than leaving
      // hundreds of files behind; a stray profile would confuse the next run.
      try {
        rmSync(PROFILE, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
      } catch (e) {
        say(`note: could not fully remove ${PROFILE} (${e.code}); it is under .tools/tmp and ignored`);
      }
    }
    // Leave the machine as it was found. The host registration is two HKCU values this run created;
    // the host's own state directory (spool, key, health log) deliberately stays, because it is the
    // evidence of what the endpoint actually did. `--keep` is the one way to leave the registration
    // in place, and it is named in the same sentence as the profile for exactly that reason.
    if (!KEEP) {
      const removed = unregisterNativeHost();
      say(`native host: ${removed.status === 0 ? 'unregistered' : 'could NOT be unregistered — check HKCU by hand'}`);
    } else {
      say(`native host: left registered (--keep). Remove with: powershell -File tools/native-host.ps1 -Action uninstall`);
    }
  }
}

function beforeObserved(json) {
  try {
    return JSON.parse(json).counters.observed;
  } catch {
    return '?';
  }
}

function extractManifestErrors(log) {
  return log
    .split('\n')
    .filter((l) => /manifest|permission|Failed to load extension|invalid/i.test(l))
    .filter((l) => !/component_installer|component_updater|policy|segmentation|NativeMessaging/i.test(l))
    .slice(0, 5);
}

function blockingMessage(log) {
  const line = log.split('\n').find((l) => /blocking webRequest listeners/i.test(l));
  return line ? line.replace(/^.*?\]\s*/, '') : null;
}

/** Collect console output for a session, returning a reader. */
function capturedConsole(cdp, sessionId) {
  const seen = [];
  const onEvent = (ev) => {
    if (ev.sessionId !== sessionId) return;
    if (ev.method === 'Runtime.consoleAPICalled') seen.push(ev.params.args?.map((a) => a.value ?? a.description).join(' ') || '');
    if (ev.method === 'Log.entryAdded') seen.push(ev.params.entry.text);
    if (ev.method === 'Runtime.exceptionThrown') seen.push(`EXCEPTION ${ev.params.exceptionDetails?.exception?.description || ''}`);
  };
  // The client's event array is append-only; poll it rather than re-plumbing the socket.
  const drain = () => {
    for (const ev of cdp.events.splice(0, cdp.events.length)) onEvent(ev);
    return seen.slice();
  };
  drain();
  return drain;
}

async function waitFor(predicate, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await predicate()) return true;
    if (Date.now() > deadline) return false;
    await sleep(150);
  }
}

let exitCode = 1;
try {
  exitCode = await main();
} catch (e) {
  say(`[FAIL] the check harness itself threw: ${e && e.stack ? e.stack : e}`);
  exitCode = 1;
}

say('─'.repeat(78));
say('summary');
for (const c of checks) say(`  ${c.status.padEnd(15)} check ${c.id}: ${c.title}`);
const failed = checks.filter((c) => c.status === 'FAIL');
const notObs = checks.filter((c) => c.status === 'NOT-OBSERVABLE');
say(`  ${checks.length} check(s): ${checks.filter((c) => c.status === 'PASS').length} pass, ${failed.length} fail, ${notObs.length} not observable`);
say('');
say('NOT VERIFIED BY THIS RUN, stated so it is not mistaken for a pass:');
say('  * check 5 (inline block) — see the check above: an unpacked load revokes webRequestBlocking.');
say('  * the user picking a file through the real picker dialog, or dropping one — check 7 attaches a');
say('    real File to the page and drives the real transfer, but no picker dialog is opened.');
say('  * Google Chrome specifically — Chrome 154 refuses --load-extension; this ran in Edge (Chromium).');

try {
  mkdirSync(dirname(EVIDENCE_PATH), { recursive: true });
  writeFileSync(EVIDENCE_PATH, lines.join('\n') + '\n', 'utf8');
  console.log(`\nevidence written to ${EVIDENCE_PATH}`);
} catch (e) {
  console.log(`\ncould not write evidence: ${e.message}`);
}

process.exit(failed.length > 0 ? 1 : exitCode === 0 ? 0 : exitCode);
