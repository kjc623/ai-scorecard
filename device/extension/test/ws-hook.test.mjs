/**
 * test/ws-hook.test.mjs — the main-world hook wraps `WebSocket.prototype.send`, reads this load's
 * nonce from its own injected element, and posts a Copilot frame in exactly the shape the content
 * script relays.
 *
 * The hook runs as a module, where `document.currentScript` is null, so the nonce has to be read from
 * the element by its marker attribute; a nonce read wrong here means the content script drops every
 * frame and nothing is ever captured. The page's own `send` must always run, for every socket. This
 * sets up a fake WebSocket, document and window, imports the hook so its wrapper installs, and drives
 * `send` for a Copilot socket and for an unrelated one.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const NONCE = 'load-nonce-xyz';
const posted = [];
const sent = [];

// The hook reads these module globals at import time, so they are set before the dynamic import.
class FakeWebSocket {
  send(data) {
    sent.push({ url: this.url, data });
  }
}
globalThis.WebSocket = FakeWebSocket;
globalThis.document = {
  currentScript: null, // a module script, as the browser leaves it
  querySelector: (sel) => (sel === 'script[data-sac-nonce]' ? { dataset: { sacNonce: NONCE } } : null),
};
globalThis.window = {
  postMessage: (message, targetOrigin) => posted.push({ message, targetOrigin }),
  location: { origin: 'https://copilot.microsoft.com', href: 'https://copilot.microsoft.com/' },
};

await import('../content/ws-hook.js');

function send(url, data) {
  posted.length = 0;
  sent.length = 0;
  const socket = { url };
  WebSocket.prototype.send.call(socket, data);
}

test('the hook wraps send exactly once', () => {
  assert.equal(WebSocket.prototype.send.__sacWrapped, true);
});

test('a Copilot frame is posted with this load\'s nonce, and the page\'s own send still runs', () => {
  const frame = JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'hi' }] });
  send('wss://copilot.microsoft.com/c/api/chat', frame);
  assert.equal(sent.length, 1, 'the original send always runs');
  assert.deepEqual(posted, [
    { message: { source: 'sac-ws', nonce: NONCE, url: 'wss://copilot.microsoft.com/c/api/chat', data: frame }, targetOrigin: 'https://copilot.microsoft.com' },
  ]);
});

test('a frame to an unrelated socket is passed through, never posted', () => {
  send('wss://chat.example.invalid/socket', 'anything');
  assert.equal(sent.length, 1);
  assert.deepEqual(posted, []);
});

test('a non-string (binary) frame to a Copilot socket is passed through, never posted', () => {
  send('wss://substrate.office.com/m365Copilot/Chathub', new Uint8Array([1, 2, 3]));
  assert.equal(sent.length, 1);
  assert.deepEqual(posted, []);
});
