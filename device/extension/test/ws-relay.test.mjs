/**
 * test/ws-relay.test.mjs — the isolated-world end of the WebSocket hook: the content script relays a
 * frame to the service worker only when it carries this load's nonce and came from this window.
 *
 * The main-world hook (content/ws-hook.js) cannot reach the service worker, so it posts each Copilot
 * frame with `window.postMessage` and the content script forwards it. The nonce is the whole of the
 * trust here: the page can post anything, so a message without this load's nonce, or from another
 * window, must never become a `capture_ws_frame`. This checks that boundary directly, which the
 * socket replay (driving the worker handler) does not exercise.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { bootstrapContentScript } from '../content/content-script.js';
import { fakeCrypto } from '../test-support/harness.mjs';

const NONCE = '1700000000000-abc123';
const URL = 'wss://copilot.microsoft.com/c/api/chat';
const FRAME = JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'hello' }] });

/** A content-script world with a fake window whose `message` handler we can fire, and a recorded bridge. */
function makeWorld({ nonce = NONCE } = {}) {
  let handler = null;
  const window = {
    addEventListener: (type, fn) => {
      if (type === 'message') handler = fn;
    },
  };
  const sent = [];
  const crypto = fakeCrypto();
  const adapter = {
    crypto,
    randomUUIDs: (n) => Array.from({ length: n }, () => crypto.randomUUID()),
    messages: { sendMessage: (m) => (sent.push(m), Promise.resolve({ ok: true })), onMessage: () => {} },
  };
  const document = { querySelectorAll: () => [], addEventListener: () => {} };
  bootstrapContentScript(adapter, { document, window, wsNonce: nonce });
  return { fire: (event) => handler(event), sent, window };
}

test('a frame from this window carrying the nonce is relayed as capture_ws_frame, url and data intact', () => {
  const w = makeWorld();
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: NONCE, url: URL, data: FRAME } });
  assert.deepEqual(w.sent, [{ type: 'capture_ws_frame', url: URL, data: FRAME }]);
});

test('a frame carrying the wrong nonce, or none, is dropped', () => {
  const w = makeWorld();
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: 'guessed', url: URL, data: FRAME } });
  w.fire({ source: w.window, data: { source: 'sac-ws', url: URL, data: FRAME } });
  assert.deepEqual(w.sent, []);
});

test('a message from another window is dropped, whatever it carries', () => {
  const w = makeWorld();
  w.fire({ source: {}, data: { source: 'sac-ws', nonce: NONCE, url: URL, data: FRAME } });
  assert.deepEqual(w.sent, []);
});

test('an unrelated page postMessage is not mistaken for a frame', () => {
  const w = makeWorld();
  for (const data of [null, undefined, 'a string', { source: 'other', nonce: NONCE, url: URL, data: FRAME }, { nonce: NONCE }]) {
    w.fire({ source: w.window, data });
  }
  assert.deepEqual(w.sent, []);
});

test('a frame whose url or data is not a string is dropped before it reaches the worker', () => {
  const w = makeWorld();
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: NONCE, url: 42, data: FRAME } });
  w.fire({ source: w.window, data: { source: 'sac-ws', nonce: NONCE, url: URL, data: { not: 'a string' } } });
  assert.deepEqual(w.sent, []);
});

test('without a window (the service worker, or a page that denied one) nothing is wired and nothing throws', () => {
  const sent = [];
  const crypto = fakeCrypto();
  const adapter = {
    crypto,
    randomUUIDs: (n) => Array.from({ length: n }, () => crypto.randomUUID()),
    messages: { sendMessage: (m) => (sent.push(m), Promise.resolve({ ok: true })), onMessage: () => {} },
  };
  const document = { querySelectorAll: () => [], addEventListener: () => {} };
  assert.doesNotThrow(() => bootstrapContentScript(adapter, { document, window: null, wsNonce: NONCE }));
  assert.deepEqual(sent, []);
});
