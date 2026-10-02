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
tools/
  emit-frames.mjs          Emits the golden native-messaging frames device/integration consumes,
                           built by this package's own frame code. `buildCases()` is exported so
                           the suite can build them without touching the Lead's golden directory.
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
test/                      The suite: 193 tests, no browser required.
test-support/              Fake chrome, fake capture-core, harness, fixtures. Not tests themselves.
```

## The cross-component seam

`tools/emit-frames.mjs` builds six golden native-messaging frames using this package's own
`observationBody()` and `frame()`, and `device/integration` decodes them through the real
`device/protocol` Go types and drives one into the real spool. That coupling is deliberate: a fixture
that agrees with the Go types by construction is exactly how the content-encoding break survived every
check on both sides, so the frames must come from the code that ships.

`test/golden-frames.test.mjs` keeps that coupling honest from this side. It asserts that the generator
still emits six decodable cases, that importing it writes nothing, that the CLI writes where it is
told, and — a drift check — that the committed golden files still match what the generator produces.
If you change `observationBody()` or `frame()`, that test fails and names the fix:

```powershell
node apps/capture-extension/tools/emit-frames.mjs
```

## What happens to an observation after it leaves here

Nothing in this package mints an envelope. It hands `capture-core` an `ObservationMessage` and stops:
the core mints the envelope (it holds device identity, the effective mode and the spool sequence),
spools it, and the spool drains to `ingest-api`. [`device/integration/device_path_test.go`](../../device/integration/device_path_test.go)
is the readable description of that path, including the two properties this package cannot test from
inside a browser: that M0 never reaches the content reader or the classifier, and that the spooled
payload is a contract-shaped envelope with no `received_at`.

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

## What has been verified in a real browser, and what has not

`tools/in-browser-check.mjs` launches a real Chromium browser with this extension loaded, drives a
request to a `node:http` server it starts on `127.0.0.1`, and reports a per-check verdict. It exits
non-zero on any FAIL:

```powershell
node apps/capture-extension/tools/in-browser-check.mjs
```

Its raw output is committed as [`tools/in-browser-check.evidence.txt`](tools/in-browser-check.evidence.txt).

### Which browser, and why not Chrome

**The verification runs in Microsoft Edge 154, which is Chromium.** Google Chrome Stable 154 refuses
to load an unpacked extension at all — it ignores both flags:

```
WARNING:extension_service.cc:445] --disable-extensions-except is not allowed in Google Chrome, ignoring.
WARNING:extension_service.cc:423] --load-extension is not allowed in Google Chrome, ignoring.
```

So the claim is "verified in Edge 154 (Chromium)", not "verified in Chrome". The extension APIs are
the same, but the engine difference is stated rather than glossed over.

### Verified in the browser (4 of 5 checks)

| # | Property | How it is observed |
|---|---|---|
| 1 | The extension loads and its MV3 service worker registers | `Target.getTargets` over the browser's DevTools socket lists a `service_worker` target at `background/service-worker.js` under the extension's own origin |
| 2 | Its `webRequest` listener observes a real request | a page served from the harness's own loopback server POSTs a chat-shaped body and the extension's §4.3 `observed` counter moves; that counter is incremented inside the listener and nowhere else |
| 3 | A real `chrome.webRequest` body yields an M0 observation with **no content field** | the frame the extension would send is read out of its own queue and shown to carry `has_content: false` and no `content`, `content_digest`, `labels`, `classifier_version` or `confidence` |
| 4 | An absent native channel degrades rather than breaks | the extension reports capture-core `absent` + itself `degraded`, and the page's request still completes with HTTP 200 — §3.4 plus §7.4's fail-open |

Check 3 is the browser-side counterpart of what `device/integration/device_path_test.go` proves in Go:
the same property, now with a body that came from a real browser. A run's counters also show §7.3's "a
negative match is counted, not emitted" happening for real — the page's `main_frame` GET is counted as
`skipped_not_generative` while the chat-shaped POST produces the observation.

### NOT observed, and why

- **Check 5 — a blocked request is cancelled AND recorded — is NOT OBSERVABLE on this host.** An
  unpacked load revokes `webRequestBlocking` whatever the manifest declares, and the browser says so
  itself: *"You do not have permission to use blocking webRequest listeners. ... webRequestBlocking is
  only allowed for extensions that are installed using ExtensionInstallForcelist."* The manifest
  declares it and a policy-installed extension retains it (E1, §7.4); the load path is what removes
  it. **A human must** install the extension by policy (`ExtensionInstallForcelist`, an elevated
  registry write — the real deployment path), load a bundle with a `blocked` rule, and confirm
  `net::ERR_BLOCKED_BY_CLIENT` in `chrome://net-export` together with an observation for the same
  request. Until then no report should claim that blocking works.
- **A content script reading a real user-selected `File`.** No file is attached in this run. **A
  human must** open a page with a file input and a chat-shaped composer, attach a file, submit, and
  confirm the health channel reports an attachment transfer with a digest and no
  `attachment_read_failed`.
- **A connected native channel.** No messaging host is registered, so only the degraded path is
  exercised. **A human must** install the host manifest for
  `com.shadowaicapture.capture_core`, restart the browser, and confirm the health channel reports
  `core: connected` with a policy version.
- **Google Chrome specifically** — Chrome 154 refuses `--load-extension` outright; see above.

### What the browser run found that the unit suite could not

The first in-browser run failed checks 2 and 3, and the cause was a real defect rather than a test
gap. The extension registered **every** `webRequest` lane with `['blocking']`. Only a policy-installed
extension is granted that privilege, and a refused blocking registration is accepted **silently** and
then never invoked — `hasListeners()` still returns `true`. Measured in Edge 154, on the same event in
the same worker: a plain listener received 3 events, a blocking listener received 0.

So on any install without the privilege the extension observed **nothing at all** while its health
report said nothing about it — the failure §15.2 and C21/C22 forbid ("never less inspection,
silently"). `registration.js` now asks for the capability, registers `blocking` only when it is
actually held, and reports `enforcement: observation_only` when it is not. Observation never depended
on blocking; only §7.4's ability to cancel does, and that is now stated rather than silently absent.
The unit suite covers both modes.

**One residual window, recorded rather than claimed as fixed.** Between `connectNative()` returning a
port and the browser delivering the disconnect for a host that does not exist, `native.isConnected()`
is briefly `true`, so an observation emitted in that window is posted into a dead port and counted as
`emitted` instead of being queued. It was seen once (`emitted: 1`, queue depth 0, no observation
anywhere) and did not reproduce once the harness gated on the extension's readiness, so it is an
unreproduced residual rather than a confirmed defect. The robust fix, if wanted, is to stop
special-casing the connected path in `pipeline.emit()`: always enqueue, and remove an entry only when
`capture-core` acks it — which is what §3.4 describes and what `native.drain()` already does.

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
8. **`enforcement: observation_only` has no `Detail` in the protocol's closed vocabulary.** The
   browser can revoke `webRequestBlocking` from an otherwise valid install (it does so for every
   unpacked load), which leaves observation working and §7.4's ability to cancel gone. `device/protocol`
   has no member for "this install cannot enforce", so the extension reports it as an extension-side
   field on the health report and counts it as `enforcement_unavailable` rather than inventing a
   `Detail`. **A protocol decision, not an implementation one** — raised for the Lead.
