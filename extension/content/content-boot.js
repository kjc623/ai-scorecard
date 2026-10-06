/**
 * content-boot.js — the declared content script.
 *
 * Chromium loads a declared content script as a classic script, so this file has no static
 * import: it registers its message listener synchronously and reaches the wiring in
 * `content-script.js` (an ES module, listed in `web_accessible_resources`) with a dynamic
 * `import()`. One implementation, two entry points: this classic script in a browser, and the
 * Node suite importing the module.
 *
 * The listener exists from the first turn, so a message arriving at `document_start` is answered
 * `not_ready` with a reason instead of being dropped.
 */

// No `import` statement may appear in this file: one would make it a syntax error as a classic script.
(() => {
  /** The content script's message handler, once its modules have loaded. */
  let handler = null;
  /** Why it has not loaded, so a premature request is answered rather than dropped. */
  let loadFailure = null;

  chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    if (!msg || typeof msg !== 'object') return false;
    if (!handler) {
      sendResponse({ ok: false, error: loadFailure || 'content script is still loading its modules', not_ready: true });
      return false;
    }
    // Answers are async: keep the channel open.
    Promise.resolve(handler(msg, sender))
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
          // The listener above is the only one registered; it forwards to the module's handler.
          onMessage: (fn) => {
            handler = fn;
          },
        },
      };
      cs.bootstrapContentScript(adapter, { document });
    })
    .catch((e) => {
      loadFailure = String((e && e.message) || e);
      console.error('[capture-extension] content script failed to load its modules:', loadFailure);
    });
})();
