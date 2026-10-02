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
 *   node apps/capture-extension/tools/in-browser-check.mjs            # all checks
 *   node apps/capture-extension/tools/in-browser-check.mjs --keep     # keep the profile for inspection
 *   node apps/capture-extension/tools/in-browser-check.mjs --evidence <file>
 *
 * Everything runs against a `node:http` server on 127.0.0.1 that this script starts itself. No vendor
 * destination is contacted.
 */

import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const PKG = resolve(HERE, '..');
const REPO = resolve(PKG, '..', '..');

const EDGE_CANDIDATES = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  '/usr/bin/microsoft-edge',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
];

const args = process.argv.slice(2);
const KEEP = args.includes('--keep');
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
    const framesRaw = await swCtx.evaluate(
      'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.queue.peek(20).map(function (e) { return e.payload; }) : null)',
    );
    const frames = framesRaw ? JSON.parse(framesRaw) : null;
    const observation = frames && frames.find((f) => f.route && f.client_id);

    if (!observation) {
      verdict('3', 'M0 produces an observation carrying no content field', 'FAIL',
        'no observation reached the extension queue, so the M0 property could not be exercised',
        [`queue: ${await swCtx.evaluate('JSON.stringify(globalThis.__captureApp.queue.stats())')}`, 'this check depends on check 2']);
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

    // ── check 4: the absent native channel degrades and the page still works ────────────
    const coreState = await swCtx.evaluate(
      'JSON.stringify(globalThis.__captureApp ? globalThis.__captureApp.health.report() : null)',
    );
    const core = coreState ? JSON.parse(coreState) : null;
    const pageResult = await pageCtx.evaluate('window.__result || "unknown"');
    const nativeHostLogged = /Can't find manifest for native messaging host|native messaging host/i.test(launched.getLog());
    const degradedOk =
      core && core.core === 'absent' && core.state === 'degraded' && typeof pageResult === 'string' && pageResult.startsWith('completed:');
    verdict('4', 'the absent native channel degrades rather than breaks', degradedOk ? 'PASS' : 'FAIL',
      degradedOk
        ? `capture-core absent + extension degraded, and the page's request completed normally`
        : `core=${core && core.core} state=${core && core.state} page=${pageResult}`,
      [
        `the page reported: ${pageResult}`,
        `health report: core=${core && core.core} state=${core && core.state} collector=${core && core.collector}`,
        `counters: ${core ? JSON.stringify(core.counters) : 'n/a'}`,
        `queue depth: ${core && core.queue ? core.queue.depth : 'n/a'} (held in memory, nothing durable)`,
        `browser logged the native-host lookup: ${nativeHostLogged ? 'yes' : 'no'}`,
        '§3.4: a failed connect is capture-core `absent` plus extension-side `degraded`, never "no observations".',
        '§7.4/brief §6 fail-open: the user\'s request is unaffected.',
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
say('  * check 5 (inline block) — the unpacked load revokes webRequestBlocking; see the check above.');
say('  * the content script reading a real user-selected File — no file is attached in this run.');
say('  * a connected native channel — no host is registered on this host.');
say('  * Google Chrome specifically — Chrome 154 refuses --load-extension; this ran in Edge (Chromium).');

try {
  mkdirSync(dirname(EVIDENCE_PATH), { recursive: true });
  writeFileSync(EVIDENCE_PATH, lines.join('\n') + '\n', 'utf8');
  console.log(`\nevidence written to ${EVIDENCE_PATH}`);
} catch (e) {
  console.log(`\ncould not write evidence: ${e.message}`);
}

process.exit(failed.length > 0 ? 1 : exitCode === 0 ? 0 : exitCode);
