/**
 * test/ws-relay.test.mjs — the isolated-world end of the WebSocket hook: content-boot relays a frame
 * to the service worker only when it carries the nonce it shares with the hook and came from this
 * window.
 *
 * The page-world hook (content/ws-hook.js) cannot reach the service worker, so it posts each Copilot
 * frame with `window.postMessage` and content-boot forwards it. The nonce is what keeps a page from
 * making one up: a message without it, or from another window, must never become a
 * `capture_ws_frame`. The relay is registered when content-boot runs, at `document_start`, not once
 * its modules load, so a frame posted at once is not lost; and the two scripts agree on the nonce
 * whichever the browser runs first. Each case loads the real scripts into a fresh fake world.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const URL = 'wss://copilot.microsoft.com/c/api/chat';
const FRAME = JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'hello' }] });
// content-boot's module import resolves to a stand-in: this suite is about the relay, which must not wait for it.
const MODULES = 'data:text/javascript,export const createContentScriptAdapterForThisWorld=()=>({});export const bootstrapContentScript=()=>{};';

let loads = 0;

/** A fresh page world: the root element, a window whose `message` listeners can be fired, a recorded worker bridge. */
function makeWorld({ nonce = '', sendMessage, modules = MODULES } = {}) {
  const listeners = [];
  const sent = [];
  const dataset = nonce ? { sacWsNonce: nonce } : {};
  const window = {
    addEventListener: (type, fn) => {
      if (type === 'message') listeners.push(fn);
    },
    postMessage: (data) => listeners.forEach((fn) => fn({ source: window, data })),
    location: { origin: 'https://copilot.microsoft.com', href: 'https://copilot.microsoft.com/' },
  };
  class FakeWebSocket {
    constructor(url) {
      this.url = url;
      this.readyState = 1;
    }
    send() {}
  }
  FakeWebSocket.OPEN = 1;
  globalThis.window = window;
  globalThis.document = { documentElement: { dataset } };
  globalThis.WebSocket = FakeWebSocket;
  globalThis.chrome = {
    runtime: {
      onMessage: { addListener: () => {} },
      getURL: () => modules,
      sendMessage: sendMessage || ((m) => (sent.push(m), Promise.resolve({ ok: true }))),
    },
  };
  return {
    sent,
    dataset,
    window,
    fire: (event) => listeners.forEach((fn) => fn(event)),
    load: async (script) => {
      loads += 1;
      await import(`../content/${script}?load=${loads}`);
    },
  };
}

test('a frame from this window carrying the nonce is relayed as capture_ws_frame, url and data intact, before the modules load', async () => {
  // Modules that never finish loading: the relay must not depend on them.
  const w = makeWorld({ nonce: 'left-by-the-hook', modules: 'data:text/javascript,await new Promise(() => {});' });
  await w.load('content-boot.js');
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: URL, data: FRAME } });
  assert.deepEqual(w.sent, [{ type: 'capture_ws_frame', url: URL, data: FRAME }]);
  assert.equal(w.dataset.sacWsNonce, undefined, 'the nonce is taken off the page');
});

test('a frame carrying the wrong nonce, or none, is dropped', async () => {
  const w = makeWorld({ nonce: 'left-by-the-hook' });
  await w.load('content-boot.js');
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: 'guessed', url: URL, data: FRAME } });
  w.fire({ source: w.window, data: { source: 'sac-ws', url: URL, data: FRAME } });
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: '', url: URL, data: FRAME } });
  assert.deepEqual(w.sent, []);
});

test('a message from another window is dropped, whatever it carries', async () => {
  const w = makeWorld({ nonce: 'left-by-the-hook' });
  await w.load('content-boot.js');
  w.fire({ source: {}, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: URL, data: FRAME } });
  assert.deepEqual(w.sent, []);
});

test('an unrelated page postMessage is not mistaken for a frame', async () => {
  const w = makeWorld({ nonce: 'left-by-the-hook' });
  await w.load('content-boot.js');
  for (const data of [null, undefined, 'a string', { source: 'other', nonce: 'left-by-the-hook', url: URL, data: FRAME }, { nonce: 'left-by-the-hook' }]) {
    w.fire({ source: w.window, data });
  }
  assert.deepEqual(w.sent, []);
});

test('a frame whose url or data is not a string is dropped before it reaches the worker', async () => {
  const w = makeWorld({ nonce: 'left-by-the-hook' });
  await w.load('content-boot.js');
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: 42, data: FRAME } });
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: URL, data: { not: 'a string' } } });
  assert.deepEqual(w.sent, []);
});

test('a worker that cannot be reached, or an extension context that is gone, loses the frame quietly', async () => {
  const unhandled = [];
  const onUnhandled = (reason) => unhandled.push(reason);
  process.on('unhandledRejection', onUnhandled);
  try {
    const unreachable = makeWorld({ nonce: 'left-by-the-hook', sendMessage: () => Promise.reject(new Error('Receiving end does not exist.')) });
    await unreachable.load('content-boot.js');
    unreachable.fire({ source: unreachable.window, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: URL, data: FRAME } });
    const orphaned = makeWorld({
      nonce: 'left-by-the-hook',
      sendMessage: () => {
        throw new Error('Extension context invalidated.');
      },
    });
    await orphaned.load('content-boot.js');
    assert.doesNotThrow(() => orphaned.fire({ source: orphaned.window, data: { source: 'sac-ws', nonce: 'left-by-the-hook', url: URL, data: FRAME } }));
    await new Promise((r) => setTimeout(r, 10));
    assert.deepEqual(unhandled, []);
  } finally {
    process.off('unhandledRejection', onUnhandled);
  }
});

for (const order of [['content-boot.js', 'ws-hook.js'], ['ws-hook.js', 'content-boot.js']]) {
  test(`the hook and the relay agree on the nonce when ${order[0]} runs first, and leave nothing on the page`, async () => {
    const w = makeWorld();
    for (const script of order) await w.load(script);
    assert.equal(w.dataset.sacWsNonce, undefined);
    new WebSocket(URL).send(FRAME);
    assert.deepEqual(w.sent, [{ type: 'capture_ws_frame', url: URL, data: FRAME }]);
  });
}
