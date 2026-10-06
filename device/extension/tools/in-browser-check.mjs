#!/usr/bin/env node
// Loads the extension into a real Chromium-family browser and checks what only a browser can show.
//
//   node tools/in-browser-check.mjs        # SAC_BROWSER names the browser; otherwise a known path
//
// The checks drive a page served from 127.0.0.1 by this script; no other destination is contacted.
// They need no installed agent: with no native messaging host registered the channel is absent, and
// on a device with the agent installed it connects, and each check holds either way. An unpacked
// extension is never granted webRequestBlocking (only a policy-installed one is), so cancelling a
// request is not observable here; the check confirms the extension reports that it cannot block.
// Prints one PASS or FAIL line per check and exits 1 on a failure. With no browser it exits 3
// (skipped) and prints the reason on its last line.

import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer-core';

import { extensionId } from './extension-id.mjs';

const PKG = resolve(fileURLToPath(import.meta.url), '..', '..');
const CHAT_PATH = '/v1/chat/completions';
const BROWSERS = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
  '/usr/bin/google-chrome',
  '/usr/bin/microsoft-edge',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

const results = [];
function verdict(title, ok, detail) {
  results.push(ok);
  console.log(`${ok ? 'PASS' : 'FAIL'} check ${results.length}: ${title} - ${detail}`);
}

async function waitFor(fn, timeoutMs) {
  const end = Date.now() + timeoutMs;
  for (;;) {
    const value = await fn().catch(() => undefined);
    if (value || Date.now() > end) return value;
    await new Promise((r) => setTimeout(r, 100));
  }
}

const CHAT_BODY = JSON.stringify({
  model: 'in-browser-check',
  messages: [
    { role: 'system', content: 'You are a helpful assistant.' },
    { role: 'user', content: `Summarise the quarterly figures. ${'padding '.repeat(20)}` },
  ],
  temperature: 0.2,
  max_tokens: 512,
});

// The page posts a chat-shaped body when it loads and again on send(), and holds a file input the
// check selects a file into.
const PAGE = `<!doctype html><html><body><input id="attachment" type="file"><script>
window.send = () => {
  window.__result = 'pending';
  return fetch(${JSON.stringify(CHAT_PATH)}, { method: 'POST', headers: { 'content-type': 'application/json' }, body: ${JSON.stringify(CHAT_BODY)} })
    .then((r) => { window.__result = 'completed:' + r.status; })
    .catch((e) => { window.__result = 'failed:' + e.message; });
};
window.send();
</script></body></html>`;

function startServer() {
  return new Promise((done) => {
    const server = createServer((req, res) => {
      req.resume();
      req.on('end', () => {
        res.writeHead(200, { 'content-type': req.url === '/' ? 'text/html' : 'application/json' });
        res.end(req.url === '/' ? PAGE : '{"ok":true}');
      });
    });
    server.listen(0, '127.0.0.1', () => done(server));
  });
}

const executablePath = [process.env.SAC_BROWSER, ...BROWSERS].find((p) => p && existsSync(p));
if (!executablePath) {
  console.log('SKIPPED: no Chrome, Edge or Chromium found; set SAC_BROWSER to the browser executable');
  process.exit(3);
}

const server = await startServer();
const origin = `http://127.0.0.1:${server.address().port}`;
const work = mkdtempSync(join(tmpdir(), 'sac-in-browser-'));
const browser = await puppeteer.launch({ executablePath, headless: true, pipe: true, enableExtensions: [PKG], userDataDir: join(work, 'profile') });
try {
  console.log(`browser: ${executablePath} (${await browser.version()})`);

  // 1. The service worker starts under the pinned id.
  const id = extensionId();
  const swTarget = await browser
    .waitForTarget((t) => t.type() === 'service_worker' && t.url().endsWith('/background/service-worker.js'), { timeout: 20_000 })
    .catch(() => null);
  const swHost = swTarget ? new URL(swTarget.url()).host : null;
  verdict('the extension loads and its MV3 service worker starts under the pinned id', swHost === id, swHost ? `chrome-extension://${swHost}/` : 'no service worker target');
  if (!swTarget) throw new Error('the extension did not start; nothing else can be checked');
  const worker = await swTarget.worker();
  const ready = await waitFor(() => worker.evaluate('Boolean(globalThis.__captureApp && globalThis.__captureApp.lanes)'), 20_000);
  if (!ready) throw new Error('the extension never registered its webRequest listeners');

  // Every payload the pipeline queues, recorded before the page makes its request.
  await worker.evaluate(`(() => {
    const app = globalThis.__captureApp;
    globalThis.__observations = [];
    const enqueue = app.queue.enqueue.bind(app.queue);
    app.queue.enqueue = (kind, payload, size) => { globalThis.__observations.push({ kind, payload }); return enqueue(kind, payload, size); };
  })()`);
  const observedBefore = await worker.evaluate('globalThis.__captureApp.health.report().counters.observed');

  const page = await browser.newPage();
  await page.goto(`${origin}/`);
  const pageResult = await waitFor(() => page.evaluate('window.__result !== "pending" && window.__result'), 15_000);

  // 2. webRequest sees the request, and the page's request completes.
  const observed = await waitFor(async () => {
    const n = await worker.evaluate('globalThis.__captureApp.health.report().counters.observed');
    return n > observedBefore ? n : 0;
  }, 10_000);
  verdict('a chat-shaped request is observed and the page request still completes', Boolean(observed) && pageResult === 'completed:200', `observed=${observed || observedBefore}, page ${pageResult}`);

  // 3. The observation carries content only when its destination's mode reads content.
  const observation = await waitFor(
    () => worker.evaluate(`(globalThis.__observations.find((o) => o.kind === 'observation') || {}).payload || null`),
    10_000,
  );
  if (!observation) {
    verdict('an observation carries content only when the mode reads content', false, 'no observation was queued');
  } else {
    const mode = await worker.evaluate(
      `globalThis.__captureApp.policy.modeFor({ host: ${JSON.stringify(new URL(origin).host)}, tool_fingerprint: ${JSON.stringify(observation.tool_fingerprint)} }).mode`,
    );
    const readsContent = ['m1', 'm2', 'm3'].includes(mode);
    const carries = Boolean(observation.has_content || observation.content || observation.content_digest);
    verdict('an observation carries content only when the mode reads content', readsContent || !carries, `mode ${mode}, has_content=${Boolean(observation.has_content)}`);
  }

  // 4. The native channel's state is reported as it is.
  const report = await waitFor(async () => {
    const r = await worker.evaluate('globalThis.__captureApp.health.report()');
    return r.core === 'connected' || r.state === 'degraded' ? r : null;
  }, 15_000);
  const channelHonest = report && (report.core === 'connected' || (report.core === 'absent' && report.state === 'degraded'));
  verdict('the native channel is reported as it is', Boolean(channelHonest), report ? `core ${report.core}, state ${report.state}` : 'no health report');

  // 5. An install without webRequestBlocking says it can only observe.
  const blocking = await worker.evaluate('globalThis.__captureApp.blockingAvailable');
  const after = await worker.evaluate('globalThis.__captureApp.health.report()');
  verdict(
    'an install without webRequestBlocking reports observation only',
    blocking === false ? after.enforcement === 'observation_only' : after.enforcement === 'blocking',
    `webRequestBlocking ${blocking ? 'granted' : 'not granted'}, enforcement ${after.enforcement}`,
  );

  // 6. The declared content script loads its modules and resolves a file the user selected.
  const file = join(work, 'in-browser-check.txt');
  const bytes = 'IN-BROWSER-CHECK-ATTACHMENT '.repeat(8);
  writeFileSync(file, bytes);
  await (await page.$('#attachment')).uploadFile(file);
  const answer = await waitFor(async () => {
    const a = await worker.evaluate(`(async () => {
      const [tab] = await chrome.tabs.query({ url: ${JSON.stringify(`${origin}/*`)} });
      return tab ? chrome.tabs.sendMessage(tab.id, { type: 'capture_upload_check' }) : null;
    })()`);
    return a && !a.not_ready ? a : null;
  }, 10_000);
  const candidate = answer?.candidates?.[0];
  verdict(
    'the content script loads and resolves the file selected in the page',
    candidate?.name === 'in-browser-check.txt' && candidate?.size_bytes === Buffer.byteLength(bytes),
    candidate ? `${candidate.name}, ${candidate.size_bytes} bytes` : JSON.stringify(answer),
  );

  // 7. At a mode that reads content, the file attached in the page goes with the next submission.
  // With no agent running its bytes cannot be transferred, so the observation names it without a
  // digest; with the agent installed it carries the digest of the bytes capture-core received.
  await worker.evaluate(`globalThis.__captureApp.applyPolicy({ policy_version: 'in-browser-check', bundle: { policy_version: 'in-browser-check', default_mode: 'm2' } })`);
  await page.evaluate('window.send()');
  const withFile = await waitFor(
    () => worker.evaluate(`globalThis.__observations.map((o) => o.payload).find((p) => p && p.attachments) || null`),
    15_000,
  );
  const named = withFile?.attachments?.[0];
  verdict(
    'a file attached in the page goes with the next submission',
    named?.name === 'in-browser-check.txt' && withFile.page_context_attachments === true,
    named ? `${named.name}, ${named.content_digest ? `digest ${named.content_digest.slice(0, 19)}` : 'not transferred: no agent is running'}` : 'no observation named the file',
  );
} catch (err) {
  verdict('the browser run completes', false, err.message);
} finally {
  await browser.close();
  server.close();
  rmSync(work, { recursive: true, force: true });
}

const failed = results.filter((ok) => !ok).length;
console.log(`${results.length - failed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
