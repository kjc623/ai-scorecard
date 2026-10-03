# Permission justification — `extension`

§7.1 states that the broad host permission is "a real privacy surface" and requires it to appear
"in the deployment's permission justification and the customer's notice (§13.4) rather than being
discovered in a review". This file is that justification. The manifest carries no comments, because
JSON does not admit them and Chrome rejects a manifest that carries extra keys.

Every entry below names where it is used. `contract.test.mjs` asserts that the manifest's
`permissions` are exactly the six listed here, against a list written in the test; it does not read
this file, so keeping the table in step with the manifest is a manual edit.

---

## `permissions`

| Permission | Used by | Why it is needed, and what it does not permit |
|---|---|---|
| `webRequest` | `src/registration.js`, `src/chrome-adapter.js` | The observation layer of §7.1. The extension must see every request the browser makes, on every host, because a curated destination list cannot find a tool nobody enumerated (E5, C7). It is read-only: the extension observes, it does not rewrite. |
| `webRequestBlocking` | `src/registration.js`, `src/enforce.js` | §7.4/E1. Chromium removed this from ordinary extensions; a **policy-installed** extension retains it, which is what makes inline warn/block possible while a request is still pending. It is used for exactly one thing: returning `{cancel: true}` after a locally decided `blocked` rule has matched. A request that is not blocked is never delayed beyond the inline decision, and no request is ever modified. |
| `nativeMessaging` | `src/native.js` | §3.4/A2. Native messaging is the only supported channel from an MV3 extension to a privileged process, and it is bidirectional — which is what lets the inline decision use policy the extension already holds instead of a round trip. Observations leave through this channel and nowhere else. |
| `scripting` | No module calls `chrome.scripting` | §7.3 needs a content script in the page's isolated world, because a `File` object exists nowhere else. That script is declared statically in `manifest.json` (`content_scripts`) rather than injected, so the permission is declared but has no call site in this package. |
| `tabs` | `src/chrome-adapter.js` (`warnUser`) | §7.4's `warned` decision must be *rendered before the request proceeds*, which means reaching the tab that made the request. Used to send the confirmation and to resolve the active tab when the request carries none. The extension reads no tab content through it. |
| `alarms` | `background/service-worker.js` | An MV3 service worker is killed aggressively. The health report (§4.3, §15.2) and the policy poll (§11.3) must happen on a schedule rather than only when traffic happens, or a device that is idle would report nothing and a mode change would not take effect until the next request. |

**Deliberately absent:** `storage` (local, sync or session) — §7.1: "the extension holds nothing
durable"; nothing here persists. `src/chrome-adapter.js` does contain a guarded accessor over
`chrome.storage.session`, but no module calls it, and the manifest does not request the permission.
`cookies`, `history`, `bookmarks`, `downloads`, `management`,
`clipboardRead`, `debugger` — no module uses them and no requirement asks for them.
`declarativeNetRequest` — it cannot express a decision that depends on the body, which is what §7.4
requires.

## `host_permissions`

| Entry | Why |
|---|---|
| `<all_urls>` | §7.1's observation layer: "Every host the browser visits, under a deployment-controlled host permission". Modes B and C exist precisely because the interesting traffic is on domains nobody has enumerated, so narrowing this to a list would break the product's core discovery claim (E5, C7). |

**The compensating controls, stated here rather than left implicit**, because the breadth is real:

1. **No request is reported unless it matched.** The §8.2 predicate runs locally and a negative
   match is counted, never emitted (§7.3). What leaves the process goes to `capture-core` over the
   native channel as `endpoint/protocol` messages: matched observations, attachment frames for
   them, the health report and the policy sync. The extension does not mint envelopes;
   `capture-core` builds the contract envelope from the observation.
2. **No body is ever written to extension storage**, because the extension has no durable storage
   (see above). Undeliverable observations sit in a bounded in-memory queue (§3.4).
3. **At M0 a destination's body is never read at all.** The body-bearing `webRequest` listener is not
   registered for hosts the policy resolves to a non-reading mode, and with no bundle it is not
   registered at all (§11.2, §11.3). This is enforced by the listener filter and asserted by a test
   on the registered filter — not by a handler that promises to look away.
4. **No request is modified, and none is delayed beyond the blocking decision** (§7.1). The extension
   does not request `DISABLE_OPTIMIZATION`, which exists for extensions that mutate requests.
5. **No remote code.** The manifest carries no external URL, no `content_security_policy` override
   and no `externally_connectable`; the test suite asserts all three.

## `web_accessible_resources`

The content script's own ES modules are listed so that its dynamic imports resolve. Without the
declaration the content script fails to load in Chromium. The list is `content/content-script.js` —
the module `content-boot.js` imports, which holds the content-script wiring — and the `src/` modules
it reaches; no manifest and no test file. `contract.test.mjs` asserts that every file
`content-boot.js` loads by URL, and each of the six `src/` modules, is in the list. It checks
inclusion, not that the list contains nothing else.

## `content_scripts`

`matches: ["<all_urls>"]`, `all_frames: true`, `run_at: document_start`, isolated world (the default,
asserted by a test).

- **`<all_urls>`** for the same reason as above, and because a file input may be inside an iframe.
- **`document_start`** so the drop listener is installed before the page's own scripts can run.
- **`all_frames`** because the composer may be in a frame.
- **Isolated world** because §7.3 requires reading the file object *in page context* without giving
  the page any access to the extension, and because a page that could call into the extension could
  forge a submission.
