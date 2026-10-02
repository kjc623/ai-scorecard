/**
 * content-script.js — the isolated-world half of §7.3, and the only code that ever touches a
 * `File`.
 *
 * Wiring only: the two pieces of logic it uses (`files.js`, `sender.js`) are plain modules with
 * no chrome.* and are unit-tested in Node. What lives here is the three listeners that make them
 * reachable from a page:
 *
 *   1. a drop listener, so a drag-and-dropped file is resolvable (the drop target is an element
 *      the user chose, which is the strongest available signal);
 *   2. the `capture_upload_check` call from the service worker, which resolves the page's file
 *      input or drop target to `File` objects and returns **metadata only** — a filename is not
 *      attachment capture (§7.3), and reading a byte here would violate snapshot-on-send;
 *   3. the `capture_upload_send` call, which opens the chunked transfer;
 *   4. the §7.4 warning overlay, which is rendered in the page and answered explicitly.
 */

import { createContentScriptAdapter } from '../src/chrome-adapter.js';
import { createFileRegistry } from '../src/attachments/files.js';
import { createAttachmentSender } from '../src/attachments/sender.js';

export function bootstrapContentScript(adapter, { document = adapter.document } = {}) {
  const registry = createFileRegistry({ document });
  const sender = createAttachmentSender({
    bridge: adapter.messages,
    crypto: adapter.crypto,
    newIds: () => adapter.randomUUIDs(1),
  });
  let lastResolution = { candidates: [], reachable: false, reason: 'not_asked' };

  // 1. Files that arrive by drop never appear in an `<input type=file>`.
  if (document && typeof document.addEventListener === 'function') {
    document.addEventListener(
      'drop',
      (event) => {
        try {
          const dt = event.dataTransfer;
          if (dt && dt.files && dt.files.length) registry.noteDrop(dt.files);
        } catch (e) {
          /* a page that blocks the read is exactly the `content_no_attachments` case */
        }
      },
      true,
    );
  }

  function resolveUpload() {
    lastResolution = registry.collectCandidates();
    return lastResolution;
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
          // §7.3: where the composer builds the upload in a worker or canvas and no `File`
          // handle is reachable, this is what the pipeline records as `content_no_attachments`
          // and counts in the coverage row.
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
        // Snapshot-on-send: the transfer is complete, so the reference is dropped. Nothing was
        // written to extension storage at any point.
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

  return { registry, sender, resolveUpload, get lastResolution() { return lastResolution; } };
}

/**
 * §7.4's confirmation, rendered before the request proceeds. Deliberately blunt: a modal
 * overlay, a countdown matching the 300 ms decision budget, and the default answer being
 * "cancel" — the request only proceeds on an explicit click.
 *
 * Injected with `textContent` only: a rule message comes from a signed bundle, and a signed
 * bundle is still data, not markup.
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

    // Bounded by §7.4's budget: an unanswered prompt is fail-open `logged` with
    // `confidence: degraded`, never a silent block and never an indefinitely held request.
    const timer = setTimeout(() => finish(true), timeout);
  });
}

// ── MV3 entry: only runs in a browser, never under `node --test` ─────────────────────────────
if (typeof chrome !== 'undefined' && chrome.runtime && chrome.runtime.id) {
  const adapter = createContentScriptAdapter(globalThis);
  bootstrapContentScript(adapter, { document: globalThis.document });
}
