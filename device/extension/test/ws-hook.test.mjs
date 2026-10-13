/**
 * test/ws-hook.test.mjs — the page-world hook wraps `WebSocket.prototype.send` and posts a text frame
 * to a Copilot socket, in exactly the shape the content script relays, only once the page's own send
 * has put it on an open socket.
 *
 * The hook is a classic main-world content script, so it cannot import the host list from
 * src/socket-submission.js; every host in `SOCKET_HOSTS` is driven through it here. The page's own
 * `send` must always run, for every socket, and a send the browser refuses must throw to the page as
 * it would without the hook. Each case loads the hook into a fresh fake page world: a WebSocket, the
 * root element the nonce is exchanged on, and a window.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { SOCKET_HOSTS } from '../src/socket-submission.js';

const CONNECTING = 0;
const OPEN = 1;
const CLOSED = 3;

let loads = 0;

/** A fresh page world with the hook loaded into it; `nonce` is what the content script left, if it ran first. */
async function loadHook({ nonce = 'left-by-the-content-script' } = {}) {
  const posted = [];
  const sent = [];
  class FakeWebSocket {
    constructor(url, readyState = OPEN) {
      this.url = url;
      this.readyState = readyState;
    }
    send(data) {
      if (this.readyState === CONNECTING) throw new Error('InvalidStateError: still in CONNECTING state');
      sent.push({ url: this.url, data });
    }
  }
  FakeWebSocket.OPEN = OPEN;
  const dataset = nonce ? { sacWsNonce: nonce } : {};
  globalThis.WebSocket = FakeWebSocket;
  globalThis.document = { documentElement: { dataset } };
  globalThis.window = {
    postMessage: (message, targetOrigin) => posted.push({ message, targetOrigin }),
    location: { origin: 'https://copilot.microsoft.com', href: 'https://copilot.microsoft.com/' },
  };
  loads += 1;
  await import(`../content/ws-hook.js?load=${loads}`);
  return {
    posted,
    sent,
    dataset,
    send(url, data, readyState = OPEN) {
      posted.length = 0;
      sent.length = 0;
      return new FakeWebSocket(url, readyState).send(data);
    },
  };
}

const FRAME = JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'hi' }] });

test('the hook wraps send exactly once, however often it is loaded into a page', async () => {
  const page = await loadHook();
  assert.equal(WebSocket.prototype.send.__sacWrapped, true);
  loads += 1;
  await import(`../content/ws-hook.js?load=${loads}`);
  page.send('wss://copilot.microsoft.com/c/api/chat', FRAME);
  assert.equal(page.posted.length, 1);
});

test('a Copilot frame on an open socket is posted with the nonce, after the page\'s own send', async () => {
  const page = await loadHook();
  page.send('wss://copilot.microsoft.com/c/api/chat', FRAME);
  assert.deepEqual(page.sent, [{ url: 'wss://copilot.microsoft.com/c/api/chat', data: FRAME }], 'the original send always runs');
  assert.deepEqual(page.posted, [
    {
      message: { source: 'sac-ws', nonce: 'left-by-the-content-script', url: 'wss://copilot.microsoft.com/c/api/chat', data: FRAME },
      targetOrigin: 'https://copilot.microsoft.com',
    },
  ]);
});

test('the nonce the content script left is taken off the page, so the page never reads it', async () => {
  const page = await loadHook();
  assert.equal(page.dataset.sacWsNonce, undefined);
});

test('loaded before the content script, the hook leaves a fresh nonce for it and posts with that', async () => {
  const page = await loadHook({ nonce: '' });
  const left = page.dataset.sacWsNonce;
  assert.match(left, /^[0-9a-z]{16,}$/);
  page.send('wss://copilot.microsoft.com/c/api/chat', FRAME);
  assert.equal(page.posted[0].message.nonce, left);
});

test('every socket host the recipes read is posted, as is a subdomain of one, in any case, with a trailing dot', async () => {
  const page = await loadHook();
  for (const host of SOCKET_HOSTS) {
    for (const name of [host, `eu.${host}`, host.toUpperCase(), `${host}.`]) {
      page.send(`wss://${name}/any`, FRAME);
      assert.equal(page.posted.length, 1, `${name} must be posted: the hook's host list must cover SOCKET_HOSTS`);
    }
  }
});

test('a frame to any other socket, a look-alike host included, is passed through, never posted', async () => {
  const page = await loadHook();
  for (const url of [
    'wss://chat.example.invalid/socket',
    'wss://notcopilot.microsoft.com/c/api/chat',
    'wss://copilot.microsoft.com.example.invalid/c/api/chat',
    'not a url',
  ]) {
    page.send(url, FRAME);
    assert.equal(page.sent.length, 1, `${url}: the page's own send still runs`);
    assert.deepEqual(page.posted, [], `${url} must not be posted`);
  }
});

test('a non-string (binary) frame to a Copilot socket is passed through, never posted', async () => {
  const page = await loadHook();
  page.send('wss://substrate.office.com/m365Copilot/Chathub', new Uint8Array([1, 2, 3]));
  assert.equal(page.sent.length, 1);
  assert.deepEqual(page.posted, []);
});

test('a send the browser refuses throws to the page as before, and nothing is posted', async () => {
  // Any page can open a socket to a Copilot host and call send at once; a frame that never went out
  // on an open socket is not a prompt anyone sent, so it must not become one.
  const page = await loadHook();
  assert.throws(() => page.send('wss://copilot.microsoft.com/c/api/chat', FRAME, CONNECTING), /CONNECTING/);
  assert.deepEqual(page.posted, []);
});

test('a frame given to a closed socket is not posted', async () => {
  const page = await loadHook();
  page.send('wss://copilot.microsoft.com/c/api/chat', FRAME, CLOSED);
  assert.deepEqual(page.posted, []);
});
