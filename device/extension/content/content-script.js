/**
 * content-script.js — the isolated-world wiring, and the only code that touches a `File`.
 * `content-boot.js` loads it in a browser; the Node suite imports it directly.
 *
 *   1. a drop listener, so a dragged-and-dropped file is resolvable;
 *   2. `capture_upload_check`: resolves the page's file input or drop target to `File` objects and
 *      returns metadata only (no byte is read until a transfer is opened);
 *   3. `capture_upload_send`: runs the chunked transfer;
 *   4. `capture_warn`: the warn confirmation, rendered in the page and answered explicitly;
 *   5. `capture_block`: the notice that a request was blocked.
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

  // 2–5. Messages from the service worker.
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
      case 'capture_block': {
        // The request is already cancelled: answer once the notice is up, not when it is dismissed.
        return { ok: showBlocked(document, msg.spec) !== null };
      }
      default:
        return { ok: false, error: `unknown message type ${msg.type}` };
    }
  });

  return { registry, sender, resolveUpload };
}

/**
 * The warn confirmation, rendered before the request proceeds: the rule's message and link, with
 * "Send anyway" and "Cancel request". An unanswered prompt resolves as proceed after `timeout_ms`
 * (fail open).
 */
export function showWarning(document, spec) {
  return new Promise((resolve) => {
    if (!document || typeof document.createElement !== 'function') return resolve(true);
    const timeout = Number.isFinite(spec && spec.timeout_ms) ? spec.timeout_ms : 300;

    const { host, buttons } = overlay(document, 'Request held for confirmation', spec, [
      ['Send anyway', '#374151'],
      ['Cancel request', '#b91c1c'],
    ]);
    const [send, cancel] = buttons;

    function finish(proceeded) {
      dismiss(host);
      clearTimeout(timer);
      resolve(proceeded);
    }
    send.addEventListener('click', () => finish(true));
    cancel.addEventListener('click', () => finish(false));

    // An unanswered prompt fails open, never a silent block or an indefinitely held request.
    const timer = setTimeout(() => finish(true), timeout);
  });
}

/**
 * The notice that a request was blocked: the rule's message and link, shown until dismissed.
 * Returns the overlay, or null when there is no document to render into.
 */
export function showBlocked(document, spec) {
  if (!document || typeof document.createElement !== 'function') return null;
  const { host, buttons } = overlay(document, 'Request blocked', spec, [['Dismiss', '#374151']]);
  buttons[0].addEventListener('click', () => dismiss(host));
  return host;
}

/**
 * The overlay both notices share, with one button per `[label, background]`. Text is set with
 * `textContent` only: a rule's message from a signed bundle is data, not markup, and its link is
 * offered only when it is an https URL.
 */
function overlay(document, titleText, spec, buttonSpecs) {
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
  title.textContent = titleText;
  title.style.cssText = 'font-weight:600;margin-bottom:6px';

  const body = document.createElement('div');
  body.textContent = (spec && spec.message) || '';
  body.style.cssText = 'margin-bottom:6px';

  const parts = [title, body];
  const link = spec && typeof spec.link === 'string' && spec.link.startsWith('https://') ? spec.link : '';
  if (link) {
    const a = document.createElement('a');
    a.textContent = link;
    a.setAttribute('href', link);
    a.setAttribute('target', '_blank');
    a.setAttribute('rel', 'noopener noreferrer');
    a.style.cssText = 'display:block;margin-bottom:6px;color:#93c5fd;word-break:break-all';
    parts.push(a);
  }

  const target = document.createElement('div');
  target.textContent = `${(spec && spec.host) || ''}${(spec && spec.path) || ''}`;
  target.style.cssText = 'opacity:.75;margin-bottom:10px;word-break:break-all';
  const buttons = buttonSpecs.map(([label, background]) => {
    const b = document.createElement('button');
    b.textContent = label;
    b.style.cssText = `margin-right:8px;padding:6px 10px;border:0;border-radius:6px;background:${background};color:#fff;cursor:pointer`;
    return b;
  });
  parts.push(target, ...buttons);

  host.append(...parts);
  (document.body || document.documentElement).appendChild(host);
  return { host, buttons };
}

function dismiss(host) {
  try {
    host.remove();
  } catch (e) {
    /* the page may have torn the subtree down already */
  }
}
