/**
 * content-boot.js — what the manifest actually declares, and the fix for a defect that made the
 * whole §7.3 attachment path dead in every browser.
 *
 * THE DEFECT. Chromium loads a declared content script as a **classic script**, and a content
 * script cannot be an ES module: the `content_scripts` entry has no `type` field, and adding one
 * does nothing. This package declared `content/content-script.js`, which opens with three static
 * `import` statements, so the browser killed it on its first line:
 *
 *   Uncaught SyntaxError: Cannot use import statement outside a module
 *   source: chrome-extension://<id>/content/content-script.js
 *
 * Nothing in that message names the real consequence. The listener was never registered, so the
 * service worker's `capture_upload_check` found no receiver at all — "Could not establish
 * connection. Receiving end does not exist." — and a `File` the user selected could never be
 * resolved or read. Every unit test passed throughout, because the suite imports the module the
 * ordinary way and never asked whether a browser could load it. It took a browser, and a check
 * that actually sends the message.
 *
 * THE SHAPE. This file is a classic script, so it has no static imports at all; it reaches the
 * modules with a dynamic `import()`, which Chromium serves from the extension's own package
 * because `web_accessible_resources` lists exactly those files. The wiring itself lives in
 * `content-script.js`, which stays a testable ES module — one implementation, two entry points:
 * a browser loading a classic script, and the Node suite importing a module.
 *
 * The message listener is registered **synchronously on the first turn**, before anything is
 * awaited, so a request arriving at `document_start` is never dropped. Until the modules arrive, an
 * upload check is answered with `not_ready` and a reason: a caller that gets a reason can retry,
 * whereas a caller whose message vanishes cannot.
 */

// No `import` statement may appear in this file. See the header: one would make the whole file a
// syntax error in the only environment it runs in.
(() => {
  /** The bootstrapped binding, once its modules have arrived. */
  let app = null;
  /** Why it is not there yet, so a premature request is answered rather than dropped. */
  let loadFailure = null;

  chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
    if (!msg || typeof msg !== 'object') return false;
    if (!app) {
      sendResponse({ ok: false, error: loadFailure || 'content script is still loading its modules', not_ready: true });
      return false;
    }
    // `capture_upload_send` is async: keep the channel open for its answer.
    Promise.resolve(app.handler(msg))
      .then((answer) => sendResponse(answer))
      .catch((e) => sendResponse({ ok: false, error: String((e && e.message) || e) }));
    return true;
  });

  import(chrome.runtime.getURL('content/content-script.js'))
    .then((cs) => {
      const adapter = {
        ...cs.createContentScriptAdapterForThisWorld(),
        messages: {
          sendMessage: (m) => chrome.runtime.sendMessage(m),
          onMessage: () => {},
        },
      };
      app = cs.bootstrapContentScript(adapter, { document: document });
      app.handler = async (msg) => {
        switch (msg.type) {
          case 'capture_upload_check': {
            const resolved = app.resolveUpload();
            return { ok: true, candidates: resolved.candidates, reachable: resolved.reachable, reason: resolved.reason };
          }
          case 'capture_upload_send': {
            const result = await app.sender.send({
              observation_id: msg.observation_id,
              descriptor: msg.descriptor,
              readSlice: (refId, offset, length) => app.registry.readSlice(refId, offset, length),
              isCancelled: () => Boolean(msg.cancelled),
            });
            app.registry.forget(msg.descriptor && msg.descriptor.ref_id);
            return { ok: true, result };
          }
          default:
            return { ok: false, error: `unknown message type ${msg.type}` };
        }
      };
    })
    .catch((e) => {
      loadFailure = String((e && e.message) || e);
      // Reported rather than swallowed. This is the seam that failed silently for the whole life of
      // the component, and a console line in the page's own log is what would have ended it sooner.
      console.error('[capture-extension] content script failed to load its modules:', loadFailure);
    });
})();
