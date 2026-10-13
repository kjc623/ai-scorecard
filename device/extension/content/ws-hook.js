/**
 * ws-hook.js — the only code that runs in the page's own world.
 *
 * `webRequest` hands the extension a WebSocket's opening handshake and never its frames, so a tool
 * that streams its prompt over a socket is otherwise invisible. The manifest declares this file as a
 * content script in the MAIN world at `document_start`, so it runs before any of the page's own
 * scripts and wraps `WebSocket.prototype.send` before the page can open a socket: a prompt the page
 * sends the moment its socket opens is still seen. For the Copilot socket hosts alone it posts each
 * outgoing text frame to the isolated-world content script (content-boot.js) with
 * `window.postMessage`; that script relays it to the service worker, which recognises the frame and
 * applies the mode before anything is read as content. Frames to any other host are never touched.
 *
 * A main-world content script cannot import, so the host list is written out here; the test drives
 * every host in `SOCKET_HOSTS` (src/socket-submission.js) through this wrapper.
 *
 * Each frame carries a nonce, so the content script relays only what this wrapper posted, never a
 * message a page made up. Whichever of the two scripts runs first writes the nonce to an attribute
 * on the root element and the other reads and removes it; both run before the page's scripts, which
 * never see the attribute.
 *
 * It fails open: the wrapper always calls the original `send`, swallows its own errors, and reads
 * only outgoing frames, so a page that uses WebSockets is never slowed or broken by it.
 */

(() => {
  const HOSTS = ['copilot.microsoft.com', 'substrate.office.com'];

  const root = document.documentElement;
  let nonce = root && root.dataset ? root.dataset.sacWsNonce || '' : '';
  if (nonce) {
    delete root.dataset.sacWsNonce;
  } else if (root && root.dataset) {
    nonce = Array.from(crypto.getRandomValues(new Uint32Array(4)), (n) => n.toString(36)).join('');
    root.dataset.sacWsNonce = nonce;
  }

  const WS = globalThis.WebSocket;
  if (!nonce || !WS || !WS.prototype || typeof WS.prototype.send !== 'function' || WS.prototype.send.__sacWrapped) return;

  const original = WS.prototype.send;
  const post = window.postMessage.bind(window);
  const origin = window.location.origin;
  const targetOrigin = origin && origin !== 'null' ? origin : '*';

  function wrapped(data) {
    // The page's own send runs first and unchanged; a send that throws (a socket still connecting)
    // throws to the page as before and nothing is posted.
    const result = original.apply(this, arguments);
    try {
      // Only a text frame that went out on an open socket to a Copilot host is posted: a socket
      // that never opened carried no prompt, and every other socket's frames are never read.
      if (typeof data === 'string' && this && this.readyState === WS.OPEN && captured(this.url)) {
        post({ source: 'sac-ws', nonce, url: this.url, data }, targetOrigin);
      }
    } catch {
      /* observation must never break the page's own send */
    }
    return result;
  }
  wrapped.__sacWrapped = true;
  try {
    WS.prototype.send = wrapped;
  } catch {
    /* a page that froze WebSocket.prototype keeps its own send; nothing is observed there */
  }

  function captured(url) {
    let host;
    try {
      host = new URL(url).hostname.toLowerCase().replace(/\.$/, '');
    } catch {
      return false;
    }
    return HOSTS.some((known) => host === known || host.endsWith(`.${known}`));
  }
})();
