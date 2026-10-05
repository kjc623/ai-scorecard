// observe.mjs — open one dashboard address in a real headless browser and save what it shows.
//
// `probe.mjs` drives the built page against a fake document; the tests assert on that. Neither is a
// browser, and "the page shows it" is a claim about a browser. This is the evidence for that claim:
// it loads the address, waits for the page's own requests to finish, and writes
//
//   <out>/<stamp>-<slug>.png    a full-page screenshot
//   <out>/<stamp>-<slug>.txt    the header below, then the text a person would read on the page
//   <out>/<stamp>-<slug>.html   the rendered DOM, for what text does not carry (a title on hover)
//
// and prints the .txt to stdout.
//
//   node tools/observe.mjs 'index.html?transport=live#devices'
//   node tools/observe.mjs 'explore.html?transport=live#events?class=payment_card' --expect payment_card
//   node tools/observe.mjs 'explore.html?transport=live#events' --click 'tr.x-row'     # open the first event
//   node tools/observe.mjs 'explore.html?transport=live' --fill '#x-text-query=invoice' --click '#x-text-form button[type=submit]'
//                                                                                  # type a prompt-text search and run it
//   node tools/observe.mjs http://127.0.0.1:8787/index.html --width 390     # a narrow screen
//
// Quote the address: `#` and `?` mean something to a shell.
//
// --expect <text>   wait for this text to be visible, and exit 1 if it never is. Repeatable.
// --absent <text>   exit 1 if this text is visible once the page has settled. Repeatable.
// --click <css>     click the first match after the page settles, then settle again. Repeatable,
//                   in order. A selector that matches nothing is exit 1.
// --fill <css>=<v>  set a field's value and dispatch input/change events, before any --click. Use
//                   for a clause that needs text typed into a form. Repeatable, in order.
// --out <dir>       default .integration/observe/ at the repository root (ignored by git: a
//                   screenshot of live data carries names and prompts).
//
// Exit 0 means the page loaded and every --expect/--absent/--click held. It does not mean the page
// is right; read the text. A console error or a failed request is reported in the header and is
// not by itself a failure, because a degraded page is a state the dashboard renders on purpose.
//
// The header also counts requests to any host but the page's own, and lists the page's own `/v1/`
// data requests by method and path, so a clause about how the page reached its data (a query, a
// search, a minted retrieval URL) can be read off the run. The dashboard loads nothing from another
// host, so that count is 0 or something is wrong.
//
// The browser is $SAC_BROWSER, else the first Chromium-family binary at a usual path. The harness
// image installs Fedora's `chromium-headless`. The address is resolved against $SAC_DASHBOARD_URL,
// else the lab dashboard as this machine reaches it.
//
// Zero dependencies: Node 22 ships WebSocket and fetch, which is all a DevTools client needs.

import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const REPO = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');

const BROWSER_CANDIDATES = [
  '/usr/lib64/chromium-browser/headless_shell',
  '/usr/bin/chromium-browser',
  '/usr/bin/chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/microsoft-edge',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
];

// The lab publishes the dashboard on the host's loopback. From inside a container that is the
// host's name, not this container's own loopback.
const DEFAULT_BASE = existsSync('/.dockerenv') ? 'http://host.docker.internal:8787/' : 'http://127.0.0.1:8787/';

const QUIET_MS = 500;
const SETTLE_TIMEOUT_MS = 20000;
const EXPECT_TIMEOUT_MS = 15000;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function parseArgs(argv) {
  const opts = { target: null, out: join(REPO, '.integration', 'observe'), expect: [], absent: [], click: [], fill: [], width: 1440, height: 900 };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const value = () => {
      if (i + 1 >= argv.length) throw new Error(`${a} needs a value`);
      return argv[++i];
    };
    if (a === '--out') opts.out = resolve(value());
    else if (a === '--expect') opts.expect.push(value());
    else if (a === '--absent') opts.absent.push(value());
    else if (a === '--click') opts.click.push(value());
    else if (a === '--fill') opts.fill.push(value());
    else if (a === '--width') opts.width = Number(value());
    else if (a === '--height') opts.height = Number(value());
    else if (a.startsWith('--')) throw new Error(`unknown option ${a}`);
    else if (opts.target === null) opts.target = a;
    else throw new Error(`one address at a time; got a second: ${a}`);
  }
  if (opts.target === null) throw new Error('give the address to open, for example \'index.html?transport=live#devices\'');
  return opts;
}

function findBrowser() {
  if (process.env.SAC_BROWSER && existsSync(process.env.SAC_BROWSER)) return process.env.SAC_BROWSER;
  return BROWSER_CANDIDATES.find((p) => existsSync(p)) ?? null;
}

async function launch(browser, profile, opts) {
  const child = spawn(
    browser,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-background-networking',
      '--disable-component-update',
      '--disable-sync',
      '--no-pings',
      '--hide-scrollbars',
      // A container gives an unprivileged user no namespace to build the sandbox from, and its
      // /dev/shm is 64 MB. The only page this opens is the lab's own dashboard.
      ...(process.platform === 'linux' ? ['--no-sandbox', '--disable-dev-shm-usage'] : []),
      `--window-size=${opts.width},${opts.height}`,
      `--user-data-dir=${profile}`,
      '--remote-debugging-port=0',
      'about:blank',
    ],
    { stdio: ['ignore', 'ignore', 'pipe'] },
  );
  let log = '';
  child.stderr.on('data', (d) => (log += d.toString()));

  const portFile = join(profile, 'DevToolsActivePort');
  for (let i = 0; i < 200; i++) {
    if (existsSync(portFile)) {
      const first = readFileSync(portFile, 'utf8').split('\n')[0];
      if (first) return { child, port: Number(first) };
    }
    if (child.exitCode !== null) break;
    await sleep(100);
  }
  child.kill();
  throw new Error(`the browser did not open a DevTools port:\n${log.split('\n').slice(-12).join('\n')}`);
}

async function connectCdp(port) {
  const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
  const ws = new WebSocket(version.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = () => rej(new Error('CDP websocket failed to open'));
  });

  let nextId = 0;
  const pending = new Map();
  const listeners = [];
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    } else {
      for (const fn of listeners) fn(msg);
    }
  };
  const send = (method, params = {}, sessionId) =>
    new Promise((res, rej) => {
      const id = ++nextId;
      const timer = setTimeout(() => {
        pending.delete(id);
        rej(new Error(`CDP ${method} timed out`));
      }, 30000);
      pending.set(id, (m) => {
        clearTimeout(timer);
        if (m.error) rej(new Error(`${method}: ${m.error.message}`));
        else res(m.result);
      });
      ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });

  return { ws, send, on: (fn) => listeners.push(fn), browser: version.Browser };
}

function slugOf(url) {
  const tail = `${url.pathname}${url.search}${url.hash}`.replace(/^\/+/, '');
  return (tail.replace(/[^A-Za-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 80)) || 'page';
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const base = process.env.SAC_DASHBOARD_URL ? process.env.SAC_DASHBOARD_URL.replace(/\/?$/, '/') : DEFAULT_BASE;
  const url = new URL(opts.target, base);

  const browser = findBrowser();
  if (!browser) {
    throw new Error('no Chromium-family browser found. Set SAC_BROWSER, or in the harness: dnf install chromium-headless');
  }

  const profile = mkdtempSync(join(tmpdir(), 'sac-observe-'));
  const { child, port } = await launch(browser, profile, opts);
  const failures = [];
  try {
    const cdp = await connectCdp(port);
    const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
    const send = (method, params) => cdp.send(method, params, sessionId);

    // What the page did while loading: this is the difference between "the table is empty" and
    // "the read behind the table failed".
    const inflight = new Set();
    const requestUrl = new Map();
    const consoleErrors = [];
    const failedRequests = [];
    const otherHosts = new Set();
    // The page's own data requests, by method and path. This is what shows that a retrieval URL
    // was fetched from its own origin rather than a query carrying content.
    const dataRequests = new Set();
    let lastActivity = Date.now();
    cdp.on((msg) => {
      if (msg.sessionId !== sessionId) return;
      const p = msg.params ?? {};
      if (msg.method === 'Network.requestWillBeSent') {
        inflight.add(p.requestId);
        requestUrl.set(p.requestId, p.request.url);
        lastActivity = Date.now();
        if (/^https?:/.test(p.request.url)) {
          const sent = new URL(p.request.url);
          if (sent.host !== url.host) otherHosts.add(sent.host);
          else if (sent.pathname.startsWith('/v1/')) dataRequests.add(`${p.request.method} ${sent.pathname}`);
        }
      } else if (msg.method === 'Network.responseReceived') {
        if (p.response.status >= 400) failedRequests.push(`${p.response.status} ${p.response.url}`);
      } else if (msg.method === 'Network.loadingFinished') {
        inflight.delete(p.requestId);
        lastActivity = Date.now();
      } else if (msg.method === 'Network.loadingFailed') {
        inflight.delete(p.requestId);
        lastActivity = Date.now();
        if (!p.canceled) failedRequests.push(`${p.errorText} ${requestUrl.get(p.requestId) ?? ''}`.trim());
      } else if (msg.method === 'Runtime.consoleAPICalled' && p.type === 'error') {
        consoleErrors.push(p.args.map((a) => a.value ?? a.description ?? '').join(' '));
      } else if (msg.method === 'Runtime.exceptionThrown') {
        consoleErrors.push(p.exceptionDetails.exception?.description ?? p.exceptionDetails.text);
      }
    });

    const evaluate = async (expression) => {
      const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
      return r.result?.value;
    };
    const visibleText = () => evaluate('document.body ? document.body.innerText : ""');

    // Settled is "the document is complete and the page has asked for nothing for a moment". A page
    // that keeps a request open never gets there; that is reported, not hidden.
    const settle = async () => {
      const deadline = Date.now() + SETTLE_TIMEOUT_MS;
      while (Date.now() < deadline) {
        if (inflight.size === 0 && Date.now() - lastActivity >= QUIET_MS && (await evaluate('document.readyState')) === 'complete') return true;
        await sleep(100);
      }
      return false;
    };

    await send('Page.enable');
    await send('Network.enable');
    await send('Runtime.enable');
    await send('Emulation.setDeviceMetricsOverride', { width: opts.width, height: opts.height, deviceScaleFactor: 1, mobile: false });

    const nav = await send('Page.navigate', { url: url.href });
    if (nav.errorText) throw new Error(`could not load ${url.href}: ${nav.errorText}`);
    let settled = await settle();

    // A field the clause needs typed into. The value is set through the native input setter and an
    // `input` event is dispatched, so a page listening for `input` (as the dashboard's search bar
    // does) sees exactly what a typist would produce; a `change` event follows for good measure.
    for (const spec of opts.fill) {
      const eq = spec.indexOf('=');
      if (eq < 1) {
        failures.push(`--fill ${spec}: expected <css-selector>=<value>`);
        break;
      }
      const selector = spec.slice(0, eq);
      const value = spec.slice(eq + 1);
      const ok = await evaluate(`(() => {
        const el = document.querySelector(${JSON.stringify(selector)});
        if (!el) return false;
        const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value')?.set;
        if (setter) setter.call(el, ${JSON.stringify(value)}); else el.value = ${JSON.stringify(value)};
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
      })()`);
      if (!ok) {
        failures.push(`--fill ${selector}: nothing on the page matches`);
        break;
      }
      lastActivity = Date.now();
      settled = (await settle()) && settled;
    }

    for (const selector of opts.click) {
      const hit = await evaluate(`(() => { const el = document.querySelector(${JSON.stringify(selector)}); if (!el) return false; el.scrollIntoView({ block: 'center' }); el.click(); return true; })()`);
      if (!hit) {
        failures.push(`--click ${selector}: nothing on the page matches`);
        break;
      }
      lastActivity = Date.now();
      settled = (await settle()) && settled;
    }
    for (const text of opts.expect) {
      const deadline = Date.now() + EXPECT_TIMEOUT_MS;
      let seen = false;
      while (!seen && Date.now() < deadline) {
        seen = (await visibleText()).includes(text);
        if (!seen) await sleep(200);
      }
      if (!seen) failures.push(`--expect ${JSON.stringify(text)}: not visible after ${EXPECT_TIMEOUT_MS / 1000} s`);
    }
    const text = await visibleText();
    for (const t of opts.absent) {
      if (text.includes(t)) failures.push(`--absent ${JSON.stringify(t)}: it is visible`);
    }

    const title = await evaluate('document.title');
    const finalUrl = await evaluate('document.location.href');
    const html = await evaluate('document.documentElement.outerHTML');
    const { cssContentSize } = await send('Page.getLayoutMetrics');
    const shot = await send('Page.captureScreenshot', {
      format: 'png',
      captureBeyondViewport: true,
      clip: { x: 0, y: 0, width: Math.ceil(cssContentSize.width), height: Math.min(Math.ceil(cssContentSize.height), 16000), scale: 1 },
    });

    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    const stem = join(opts.out, `${stamp}-${slugOf(url)}`);
    mkdirSync(opts.out, { recursive: true });

    const list = (items) => (items.length === 0 ? '0' : `${items.length}\n${items.map((s) => `    ${s}`).join('\n')}`);
    const header = [
      `address:             ${finalUrl}`,
      `title:               ${title}`,
      `observed at:         ${new Date().toISOString()}`,
      `browser:             ${cdp.browser}, ${opts.width}x${opts.height}`,
      `settled:             ${settled ? 'yes' : `no (requests still open after ${SETTLE_TIMEOUT_MS / 1000} s)`}`,
      `clicked:             ${opts.click.length === 0 ? 'nothing' : opts.click.join(' , ')}`,
      `filled:              ${opts.fill.length === 0 ? 'nothing' : opts.fill.map((s) => s.slice(0, s.indexOf('='))).join(' , ')}`,
      `console errors:      ${list(consoleErrors)}`,
      `failed requests:     ${list(failedRequests)}`,
      `other-host requests: ${list([...otherHosts])}`,
      `data requests:       ${list([...dataRequests])}`,
      `checks:              ${failures.length === 0 ? `${opts.expect.length + opts.absent.length + opts.click.length + opts.fill.length} held` : `FAILED\n${failures.map((s) => `    ${s}`).join('\n')}`}`,
      `screenshot:          ${stem}.png`,
      `rendered DOM:        ${stem}.html`,
    ].join('\n');
    const report = `${header}\n${'-'.repeat(80)}\n${text}\n`;

    writeFileSync(`${stem}.png`, Buffer.from(shot.data, 'base64'));
    writeFileSync(`${stem}.html`, html);
    writeFileSync(`${stem}.txt`, report);
    process.stdout.write(report);
  } finally {
    // A stray headless browser holding the profile directory would confuse the next run.
    child.kill();
    await sleep(200);
    rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
  }
  if (failures.length > 0) process.exitCode = 1;
}

main().catch((e) => {
  console.error(`observe: ${e.message}`);
  process.exitCode = 2;
});
