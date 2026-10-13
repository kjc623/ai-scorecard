/**
 * ws-hook.js — the only code that runs in the page's own world.
 *
 * `webRequest` hands the extension a WebSocket's opening handshake and never its frames, so a tool
 * that streams its prompt over a socket is otherwise invisible. content-boot injects this file as a
 * `<script type="module">`, which runs in the page's main world, where the page's `WebSocket` lives.
 * It wraps `WebSocket.prototype.send` for the catalogued Copilot socket hosts alone and posts each
 * outgoing frame to the isolated-world content script with `window.postMessage`; the content script
 * relays it to the service worker, which recognises the frame and applies the mode before anything
 * is read as content. Frames to any other host are never touched.
 *
 * It fails open: the wrapper always calls the original `send`, swallows its own errors, and reads
 * only outgoing frames, so a page that uses WebSockets is never slowed or broken by it.
 */

import { isCapturedSocketHost } from '../src/socket-submission.js';

(() => {
  // `document.currentScript` is null inside a module script, so the nonce is read from the injected
  // element by its marker attribute; content-boot removes the element only after this has run.
  const script = document.currentScript || document.querySelector('script[data-sac-nonce]');
  const nonce = script && script.dataset ? script.dataset.sacNonce || '' : '';
  const WS = globalThis.WebSocket;
  if (!WS || !WS.prototype || typeof WS.prototype.send !== 'function' || WS.prototype.send.__sacWrapped) return;

  const original = WS.prototype.send;
  function wrapped(data) {
    try {
      // `this.url` is the socket's URL. Only the Copilot hosts are observed; every other socket is
      // passed straight through, its frames never read.
      if (typeof data === 'string' && hostOf(this && this.url) && isCapturedSocketHost(hostOf(this.url))) {
        window.postMessage({ source: 'sac-ws', nonce, url: this.url, data }, window.location.origin || '*');
      }
    } catch {
      /* observation must never break the page's own send */
    }
    return original.apply(this, arguments);
  }
  wrapped.__sacWrapped = true;
  try {
    WS.prototype.send = wrapped;
  } catch {
    /* a page that froze WebSocket.prototype keeps its own send; nothing is observed there */
  }

  function hostOf(url) {
    try {
      return new URL(url, window.location.href).hostname.toLowerCase().replace(/\.$/, '');
    } catch {
      return '';
    }
  }
})();
