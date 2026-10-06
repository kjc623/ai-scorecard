/**
 * content-script.js — the isolated-world wiring, and the only code that touches a `File`.
 * `content-boot.js` loads it in a browser; the Node suite imports it directly.
 *
 *   1. a drop listener, so a dragged-and-dropped file is resolvable;
 *   2. `capture_upload_check`: resolves the page's file input or drop target to `File` objects and
 *      returns metadata only (no byte is read until a transfer is opened);
 *   3. `capture_upload_send`: runs the chunked transfer;
 *   4. `capture_warn`: the warn confirmation, rendered in the page and answered explicitly.
 */

import { createContentScriptAdapter } from '../src/chrome-adapter.js';
import { createFileRegistry } from '../src/attachments/files.js';
import { createAttachmentSender } from '../src/attachments/sender.js';

/** The adapter for the world this module is loaded into; `content-boot.js` cannot import it statically. */
export function createContentScriptAdapterForThisWorld() {
  return createContentScriptAdapter(globalThis);
}

export function bootstrapContentScript(adapter, { document = adapter.document } = {}) {
  const registry = createFileRegistry({ document });
  const sender = createAttachmentSender({
    bridge: adapter.messages,
    crypto: adapter.crypto,
    newIds: () => adapter.randomUUIDs(1),
  });

  // 1. Files that arrive by drop never appear in an `<input type=file>`.
  if (document && typeof document.addEventListener === 'function') {
    document.addEventListener(
      'drop',
      (event) => {
        try {
          const dt = event.dataTransfer;
          if (dt && dt.files && dt.files.length) registry.noteDrop(dt.files);
        } catch (e) {
          /* a page that blocks the read leaves no reachable file, which is reported as such */
        }
      },
      true,
    );
  }

  function resolveUpload() {
    return registry.collectCandidates();
  }

  // 2/3/4. Messages from the service worker.
  adapter.messages.onMessage(async (msg) => {
    if (!msg || typeof msg !== 'object') return { ok: false };
    switch (msg.type) {
      case 'capture_upload_check': {
        const resolved = resolveUpload();
        return {
          ok: true,
          candidates: resolved.candidates,
          // False when the composer built the upload in a worker or canvas and no `File` handle
          // is reachable.
          reachable: resolved.reachable,
          reason: resolved.reason,
        };
      }
      case 'capture_upload_send': {
        const result = await sender.send({
          observation_id: msg.observation_id,
          descriptor: msg.descriptor,
          readSlice: (refId, offset, length) => registry.readSlice(refId, offset, length),
          isCancelled: () => Boolean(msg.cancelled),
        });
        // The transfer has ended: the reference is dropped and the file is not offered again.
        registry.forget(msg.descriptor && msg.descriptor.ref_id);
        return { ok: true, result };
      }
      case 'capture_warn': {
        const proceeded = await showWarning(document, msg.spec);
        return { ok: true, answered: true, proceeded };
      }
      default:
        return { ok: false, error: `unknown message type ${msg.type}` };
    }
  });

  return { registry, sender, resolveUpload };
}

/**
 * The warn confirmation, rendered before the request proceeds: an overlay with "Send anyway" and
 * "Cancel request". An unanswered prompt resolves as proceed after `timeout_ms` (fail open).
 *
 * Text is set with `textContent` only: a rule message from a signed bundle is data, not markup.
 */
export function showWarning(document, spec) {
  return new Promise((resolve) => {
    if (!document || typeof document.createElement !== 'function') return resolve(true);
    const timeout = Number.isFinite(spec && spec.timeout_ms) ? spec.timeout_ms : 300;

    const host = document.createElement('div');
    host.setAttribute('role', 'dialog');
    host.setAttribute('aria-modal', 'true');
    host.style.cssText = [
      'position:fixed', 'z-index:2147483647', 'inset:auto 16px 16px auto',
      'max-width:420px', 'padding:16px', 'border-radius:8px',
      'background:#111827', 'color:#f9fafb', 'font:13px/1.45 system-ui,sans-serif',
      'box-shadow:0 8px 32px rgba(0,0,0,.4)',
    ].join(';');

    const title = document.createElement('div');
    title.textContent = 'Request held for confirmation';
    title.style.cssText = 'font-weight:600;margin-bottom:6px';

    const body = document.createElement('div');
    body.textContent = (spec && spec.message) || 'Your organisation\u2019s policy requires confirmation before this request is sent.';
    body.style.cssText = 'margin-bottom:6px';

    const target = document.createElement('div');
    target.textContent = `${(spec && spec.host) || ''}${(spec && spec.path) || ''}`;
    target.style.cssText = 'opacity:.75;margin-bottom:10px;word-break:break-all';

    const send = document.createElement('button');
    send.textContent = 'Send anyway';
    send.style.cssText = 'margin-right:8px;padding:6px 10px;border:0;border-radius:6px;background:#374151;color:#f9fafb;cursor:pointer';

    const cancel = document.createElement('button');
    cancel.textContent = 'Cancel request';
    cancel.style.cssText = 'padding:6px 10px;border:0;border-radius:6px;background:#b91c1c;color:#fff;cursor:pointer';

    function finish(proceeded) {
      try {
        host.remove();
      } catch (e) {
        /* the page may have torn the subtree down already */
      }
      clearTimeout(timer);
      resolve(proceeded);
    }
    send.addEventListener('click', () => finish(true));
    cancel.addEventListener('click', () => finish(false));

    host.append(title, body, target, send, cancel);
    (document.body || document.documentElement).appendChild(host);

    // An unanswered prompt fails open, never a silent block or an indefinitely held request.
    const timer = setTimeout(() => finish(true), timeout);
  });
}
