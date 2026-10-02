# `apps/capture-extension` — the browser half of Shadow AI Capture

Manifest V3, Chromium only (E23), policy-installed. Zero dependencies, no bundler, no build step:
these are plain ES modules that Chromium loads directly.

Specification: [docs/01-collectors.md](../../docs/01-collectors.md) §7 (§7.2 decode, §7.3 shape
classification and attachments, §7.4 inline warn/block and the WebSocket gap, §7.5 modes B and C),
§8.2 (the shape predicate), §11.2–11.3 (mode enforcement). The wire vocabulary is
[device/protocol](../../device/protocol) (owned by the Lead); the record contract is
[contracts/event-envelope.schema.json](../../contracts/event-envelope.schema.json).

## Running the tests

```powershell
node --test apps/capture-extension          # from the repository root
node --test 'apps/capture-extension/**/*.test.mjs'
cd apps/capture-extension; node --test      # discovery from the package
```

All three run the same suite. Chromium and a native messaging host are **not** required and are
**not** installed here.

## Layout

```
manifest.json              MV3 manifest. Permissions justified in PERMISSIONS.md, not in the JSON.
package.json               "main" points at run-tests.mjs; Chrome ignores package.json entirely.
run-tests.mjs              what `node --test apps/capture-extension` resolves to (see below).
background/
  service-worker.js        MV3 entry. Wiring only: policy, queue, health, lanes, alarms, relay.
content/
  content-script.js        Isolated-world entry. Owns the File; renders §7.4's confirmation.
src/
  adapter.js               THE SEAM. Names the whole assumed chrome.* surface. Every other module
                           takes an adapter; nothing else calls chrome.* (`contract.test.mjs` greps).
  chrome-adapter.js        Builds a browser adapter, and a content-script adapter, from `chrome`.
  messages.js              Transcription of device/protocol's vocabulary + the base64 content encoder.
  codec.js                 Bytes, strict-UTF-8 decode, base64, SHA-256.
  request-body.js          chrome's two requestBody shapes (E2) and the over-cap prefix rule (§5.3).
  predicate.js             §8.2 as pure functions. The unit under test.
  mode-policy.js           The policy cache, most-restrictive resolution, the M0 body gate (§11).
  enforce.js               §7.4 inline warn/block, the 300 ms budget, fail-open.
  queue.js                 Bounded in-memory queue, drop-oldest with a counter (§3.4).
  health.js                §4.3's closed seven counters and §3.4's absent-plus-degraded reporting.
  native.js                The native-messaging port, request/response correlation, the drain.
  registration.js          The two webRequest lanes — where the M0 guarantee becomes a filter.
  pipeline.js              Observe → classify → emit, and the inline decision.
  tool-fingerprint.js      §8.1's tf1: fingerprint from the route-independent signal vector.
  attachments/files.js     Page-context File registry (metadata at selection, bytes at send).
  attachments/sender.js    Manifest-then-chunks transfer, refusal before any byte.
test/                      The suite: 187 tests.
test-support/              Fake chrome, fake capture-core, harness, fixtures. Not tests themselves.
```

## Why `run-tests.mjs` exists

On Node 22.23.1 a positional argument to `--test` is treated as a *file*, not a directory to search,
so `node --test apps/capture-extension` would try to load the directory as a module and fail with
`Cannot find module`. Node resolves a directory argument through `package.json`'s `main`, so
`run-tests.mjs` is what that documented command loads; it imports the real suite and contains no
tests of its own. Verified on this host: the bare-directory form fails identically in an empty
scratch directory, so it is a Node behaviour rather than a property of this package.

## The assumed `chrome.*` surface

The complete list, so a reviewer can check it against a browser without reading the tree:

```
chrome.webRequest.onBeforeRequest.addListener(fn, filter, ["blocking"])              metadata lane
chrome.webRequest.onBeforeRequest.addListener(fn, filter, ["blocking","requestBody"]) body lane
chrome.webRequest.onBeforeRequest.removeListener(fn)                                  M0 lane removal
chrome.webRequest.onCompleted.addListener(fn, filter)
chrome.runtime.connectNative(application)      chrome.runtime.lastError
chrome.runtime.onMessage / sendMessage / getURL
chrome.tabs.sendMessage / chrome.tabs.query
chrome.alarms.create / onAlarm
```

Not used: `declarativeNetRequest`, any storage, any analytics, any remote code.

## What is NOT VERIFIED

This host has no Chromium and no native messaging host, so **in-browser end-to-end behaviour is NOT
VERIFIED**. The suite tests every decision and the two facts observable at the API boundary (which
listener each lane registered on, and with which `extraInfoSpec`), against a fake `chrome` and a fake
`capture-core`. Five things can only be confirmed by a human in a browser:

1. **`webRequestBlocking` actually cancels.** The suite asserts the extension returns
   `{cancel: true}` for a `blocked` rule; that Chromium honours it for a policy-installed extension
   is the E1 assumption itself.
   *To verify:* load the unpacked extension (`chrome://extensions` → Developer mode → Load
   unpacked → `apps/capture-extension`), install a bundle whose `scope` denies one destination and
   whose rules hold a `blocked` rule for it, then navigate to that destination and confirm
   **`chrome://net-export`** shows the request cancelled and DevTools shows
   `net::ERR_BLOCKED_BY_CLIENT`.
2. **A real `requestBody` arrives in the two shapes E2 claims** — parsed `formData` for a form
   encoding, `raw` bytes otherwise.
   *To verify:* visit a page that POSTs `application/x-www-form-urlencoded`, then
   `chrome://extensions` → the extension's service worker → Console, and inspect the observation for
   the form case; then repeat on a JSON POST.
3. **A content script can read a user-selected `File`.** The suite drives a fake document and a fake
   `File`; `File.slice().arrayBuffer()`, drop events, and the isolated world's access to
   `input.files` are all browser behaviour.
   *To verify:* on a page with a file input, attach a file, submit, and confirm the health channel
   reports an attachment transfer with a digest and no `attachment_read_failed`.
4. **The M0 registration guarantee holds against a real policy change.** The suite asserts the
   listener filter and that the lane is removed when every destination resolves to M0; that Chrome
   stops producing `requestBody` after `removeListener` is browser behaviour.
   *To verify:* with the extension loaded, apply a bundle that resolves one host to M0, then
   `chrome://extensions` → service worker → Console: the metadata lane must still fire for that host
   and no body must appear on any observation, before and after the change.
5. **Native messaging against a real host** — the frame shapes, the 1 MiB ceiling and the refusal
   path are tested against `test-support/fake-core.mjs`, which mirrors `device/protocol`, and against
   the **real Go type** in `test/content-roundtrip.test.mjs` (which compiles and runs a Go consumer
   when a toolchain is present). What is not tested is a real registered host process.
   *To verify:* install the native messaging host manifest for
   `com.shadowaicapture.capture_core`, restart the browser, and confirm the health channel reports
   `core: connected` with a policy version.

## Decisions `docs/01-collectors.md` §7 leaves open

Recorded here because a reader should not have to infer them from the code, and because each was a
choice this implementation had to make:

1. **Where the §8.2 weights live.** The document fixes the evidence and the asymmetry (recall over
   precision) but not the numbers. `predicate.js` exports `SIGNAL_WEIGHT` and `DEFAULT_THRESHOLD`
   (0.8) as data, and the numbers are a calibration, not a specification — a bundle can override them.
2. **The 300 ms budget against a human.** §7.4's 300 ms bounds *reaching a verdict*. A `warned` rule
   then waits for a person, who does not answer inside 300 ms, so treating the two as one number
   would make `warned` behaviourally identical to fail-open and the schema's `warned` action
   unreachable. Rule evaluation is 300 ms; the confirmation window is separate
   (`CONFIRMATION_WINDOW_MS`, default 20 s, bundle-overridable). **This is the most consequential
   open decision in §7.**
3. **What the extension knows about a destination's mode.** §3.4 makes `capture-core` authoritative.
   The extension's cache is therefore *unsigned* relative to the core's answer, and the conservative
   state is "do not read the body": no bundle ⇒ M0 ⇒ no body lane registered at all. Where the core
   answers a mode query directly, its answer wins (`modeFor({...}, {override})`).
4. **`default_mode` versus scope entries.** §11.3 says an observation with no matching scope entry
   resolves to the tenant default. Folding the default into the most-restrictive reduction would let
   an M0 default silently cancel every explicit scope entry and make the matrix inert, so the default
   is a **fallback for unmatched hosts only** and most-restrictive applies across matched entries.
5. **Whether an attachment's content requires its own mode decision.** §7.3 puts attachment capture
   under the same mode, and the extension asks permission on the manifest before reading a byte, so
   the refusal is `capture-core`'s. Whether attachment bytes should be governed by the destination's
   mode or by a per-file policy is not stated.
6. **What an unresolved tool fingerprint should be.** §8.1 defines the derivation but not the
   fallback when the body was never read: here the vector carries `body_shape: "unread"`, so the
   fingerprint is honestly weaker rather than silently equal to a read one.
7. **`content` is base64 on the wire, in every case.** `ObservationMessage.Content` is `[]byte`, so
   `encoding/json` base64-decodes it. A raw text value would fail to decode, and text that *is* valid
   base64 would silently decode to different bytes than the user typed while `content_digest`
   described the original — a content-identity break that would propagate into `dedup_key` and into
   what the classifier sees. `observationBody()` is the single encoder, so a call site cannot get it
   wrong; `test/content-roundtrip.test.mjs` proves it against the real Go type.
