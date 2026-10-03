# `extension` — the browser half of Shadow AI Capture

Manifest V3, Chromium only (E23), policy-installed. Zero dependencies, no bundler, no build step:
these are plain ES modules that Chromium loads directly.

Specification: [docs/01-collectors.md](../docs/01-collectors.md) §7 (§7.2 decode, §7.3 shape
classification and attachments, §7.4 inline warn/block and the WebSocket gap, §7.5 modes B and C),
§8.2 (the shape predicate), §11.2–11.3 (mode enforcement). The wire vocabulary is
[endpoint/protocol](../endpoint/protocol) (owned by the Lead); the record contract is
[contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json).

## Running the tests

```powershell
node --test extension          # from the repository root
node --test 'extension/**/*.test.mjs'
cd extension; node --test      # discovery from the package
```

All three run the same suite. Chromium and a native messaging host are **not** required and are
**not** installed here.

## Layout

```
manifest.json              MV3 manifest, with a pinned `key`. Permissions justified in PERMISSIONS.md,
                           not in the JSON.
package.json               "main" points at run-tests.mjs; Chrome ignores package.json entirely.
run-tests.mjs              what `node --test extension` resolves to (see below).
background/
  service-worker.js        MV3 entry. Wiring only: policy, queue, health, lanes, alarms, relay.
content/
  content-boot.js          THE DECLARED CONTENT SCRIPT. A classic script with no static import, because
                           Chromium loads a content script as a classic script and this package's
                           modules are ES modules. Registers its message listener synchronously and
                           reaches the modules with a dynamic import().
  content-script.js        The wiring that boot reaches: the File registry, the transfer, §7.4's
                           confirmation. An ES module so the Node suite can import it.
tools/
  emit-frames.mjs          Emits the golden native-messaging frames endpoint/integration consumes,
                           built by this package's own frame code. `buildCases()` is exported so
                           the suite can build them without touching the Lead's golden directory.
  in-browser-check.mjs     The browser gate. Drives a real Chromium in two modes; see below.
  native-host.mjs          Writes the native-messaging host manifest and computes the pinned extension id.
  native-host.ps1          Writes the two HKCU registry values that point at it. No elevation needed.
  make-extension-key.mjs   Regenerates the identity pair whose public half is the manifest's `key`.
src/
  adapter.js               THE SEAM. Names the whole assumed chrome.* surface. Every other module
                           takes an adapter; nothing else calls chrome.* (`contract.test.mjs` greps).
  chrome-adapter.js        Builds a browser adapter, and a content-script adapter, from `chrome`.
  messages.js              Transcription of endpoint/protocol's vocabulary + the base64 content encoder.
  codec.js                 Bytes, strict-UTF-8 decode, base64, SHA-256.
  request-body.js          chrome's two requestBody shapes (E2) and the over-cap prefix rule (§5.3).
  predicate.js             §8.2 as pure functions. The unit under test.
  mode-policy.js           The policy cache, most-restrictive resolution, the M0 body gate (§11).
  enforce.js               §7.4 inline warn/block, the 300 ms budget, fail-open.
  queue.js                 Bounded in-memory queue, drop-oldest with a counter (§3.4).
  health.js                §4.3's closed seven counters and §3.4's absent-plus-degraded reporting.
  native.js                The native-messaging port, request/response correlation, the drain.
  registration.js          The two webRequest lanes — where the M0 guarantee becomes a filter.
  pipeline.js              Observe, then classify, then emit, and the inline decision.
  tool-fingerprint.js      §8.1's tf1: fingerprint from the route-independent signal vector.
  attachments/files.js     Page-context File registry (metadata at selection, bytes at send).
  attachments/sender.js    Manifest-then-chunks transfer, refusal before any byte.
test/                      The suite, no browser required. Run it with `node --test`, not the
                           directory form — see below.
test-support/              Fake chrome, fake capture-core, harness, fixtures. Not tests themselves.
```

## The cross-component seam

`tools/emit-frames.mjs` builds six golden native-messaging frames using this package's own
`observationBody()` and `frame()`, and `endpoint/integration` decodes them through the real
`endpoint/protocol` Go types and drives one into the real spool. That coupling is deliberate: a fixture
that agrees with the Go types by construction is exactly how the content-encoding break survived every
check on both sides, so the frames must come from the code that ships.

`test/golden-frames.test.mjs` keeps that coupling honest from this side. It asserts that the generator
still emits six decodable cases, that importing it writes nothing, that the CLI writes where it is
told, and — a drift check — that the committed golden files still match what the generator produces.
If you change `observationBody()` or `frame()`, that test fails and names the fix:

```powershell
node extension/tools/emit-frames.mjs
```

## What happens to an observation after it leaves here

Nothing in this package mints an envelope. It hands `capture-core` an `ObservationMessage` and stops:
the core mints the envelope (it holds device identity, the effective mode and the spool sequence),
spools it, and the spool drains to `ingest-api`. [`endpoint/integration/device_path_test.go`](../endpoint/integration/device_path_test.go)
is the readable description of that path, including the two properties this package cannot test from
inside a browser: that M0 never reaches the content reader or the classifier, and that the spooled
payload is a contract-shaped envelope with no `received_at`.

## Why `run-tests.mjs` exists

On Node 22.23.1 a positional argument to `--test` is treated as a *file*, not a directory to search,
so `node --test extension` would try to load the directory as a module and fail with
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
node extension/tools/in-browser-check.mjs
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

### Verified in the browser

Two modes, because they assert opposite halves of the same requirement. With **no host registered**,
§3.4/INV-6 requires `absent` + `degraded` and a page that still works. With a host registered **before
launch** (`--with-native-host`), the same requirement is `connected`, and only then can the real §7.3
attachment path be driven end to end. `tools/accept.mjs` runs both.

| # | Property | How it is observed |
|---|---|---|
| 1 | The extension loads and its MV3 service worker registers | `Target.getTargets` over the browser's DevTools socket lists a `service_worker` target at `background/service-worker.js` under the extension's own origin |
| 2 | Its `webRequest` listener observes a real request | a page served from the harness's own loopback server POSTs a chat-shaped body and the extension's §4.3 `observed` counter moves; that counter is incremented inside the listener and nowhere else |
| 3 | A real `chrome.webRequest` body yields an M0 observation with **no content field** | the payload the pipeline handed to the queue is tapped as it is enqueued and shown to carry `has_content: false` and no `content`, `content_digest`, `labels`, `classifier_version` or `confidence` |
| 4 | The native channel's state is reported honestly, and the page keeps working | absent host to capture-core `absent` + extension `degraded`; connected host to `core: connected`; either way the page's request completes with HTTP 200 (§3.4, §7.4 fail-open, INV-6) |
| 6 | A registered native host produces a **connected** channel | `connected` is emitted only after a message round-trips, so a PASS means the browser reached a real `capture-core` process and the answer came back — not that a port object was handed out |
| 7 | A real user-selected `File` is read, transferred and acknowledged | a real `File` is attached to the page's input; the content script resolves it through its own registry, and the digest `sender.js` computes over the bytes it read matches one computed independently over the bytes the page attached |

Check 3 is the browser-side counterpart of what `endpoint/integration/device_path_test.go` proves in Go:
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
- **The real file picker and a real drop.** Check 7 attaches a real `File` object and drives the real
  transfer, but no OS picker dialog is opened and no drop event is synthesised from outside the page.
  **A human must** open a page with a chat-shaped composer, attach a file through the picker, submit,
  and confirm the health channel reports a transfer with a digest and no `attachment_read_failed`.
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

**The window is closed, and it was worse than a window.** The residual noted in an earlier revision —
an observation emitted between `connectNative()` returning a port and the browser delivering the
disconnect for an absent host being posted into a dead port and counted `emitted` — turned out to be
two defects, and the browser gate caught both.

1. **`connected` meant "a port object was handed out".** The health report therefore claimed
   `core: connected` for a host that does not exist while `state` was `degraded` — a coverage claim
   that was simply wrong, and the §15.2/INV-6 failure the gate exists to catch. The native client now
   distinguishes `port_opened` from `connected`, and only the latter — emitted when a message actually
   round-trips — marks the channel healthy. `emitted` likewise counts **acks**: `pipeline.emit()` now
   always enqueues, and an entry leaves the queue only when `capture-core` acknowledges it. There is
   no connected fast path left to be wrong about.
2. **The dead host produced a retry loop.** Measured in Edge 154 at **~23,700 failed connects in about
   four seconds** — instrumented, not inferred: the stack was the port's `onDisconnect` to `handleDisconnect` to counted as `native_unavailable`; `typeof` was `number` and `window.errors` was
   `0`, so it was a cumulative running count and **not** a signed-16-bit reinterpretation of `32769`.
   Each connect was followed by a disconnect, the port was dropped, and the next send opened another;
   work hung off the port-creation event closed the cycle. On a real endpoint that is sustained CPU
   and log volume for a channel that is simply not there, and §3.5's crash-loop rule is the same rule
   one process down. The client now backs off (`DEFAULT_CONNECT_COOLDOWN_MS`) after a disconnect,
   nothing is hung off `port_opened`, and a refusal during the backoff is not counted as a fresh
   error. The browser check now reports `errors: 2` where it reported `errors: 23739`.

Both are covered by tests that assert the property rather than the sequence: an observation emitted
against a port that will not answer must end up **queued, not counted**; and 20 observations against a
dead host must produce a bounded number of connect attempts, not one per observation.

### The content script never ran in any browser, and no test could see it

This is the most serious defect this component has produced, and it was found by asking a browser to
load the file rather than by asking the module suite whether it worked.

`content/content-script.js` opened with three static `import` statements, and the manifest declared it
as a content script. **Chromium loads a declared content script as a classic script, and a content
script cannot be an ES module** — the `content_scripts` entry has no `type` field, and adding one does
nothing (verified: the file still died with the same error). So in every real browser the file was
killed on its first line:

```
Uncaught SyntaxError: Cannot use import statement outside a module
  source: chrome-extension://<id>/content/content-script.js
```

The consequence is not in that message. No listener was ever registered, so the service worker's
`capture_upload_check` had **no receiver at all** — `"Could not establish connection. Receiving end
does not exist."` — and the entire §7.3 attachment path was dead: a `File` the user selected could
never be resolved, read, chunked or sent. The product's attachment capture had never once executed in
any browser, in any build.

Every one of the suite's 200+ tests passed throughout, because the suite imports the module the
ordinary way and never asked whether a browser could load it. A test that imports a file cannot see a
defect in how the file is loaded. The fix:

- the manifest declares `content/content-boot.js`, a **classic** script with no static import at all;
- it registers `chrome.runtime.onMessage` **synchronously on the first turn**, so a request arriving
  at `document_start` is never dropped — before its modules have loaded it answers `not_ready` with a
  reason, and a caller that gets a reason can retry, whereas a caller whose message vanishes cannot;
- it reaches the modules with a dynamic `import()` of `content/content-script.js`, which is now listed
  in `web_accessible_resources` (Chromium refuses to serve an extension file that is not);
- `content-script.js` stays an ES module with the wiring, so there is one implementation and two entry
  points: a browser loading a classic script, and the Node suite importing a module.

`contract.test.mjs` now asserts the shape directly — the declared content script contains no static
import, declares no `type`, uses `import()`, and every file it loads by URL is web-accessible — because
this failure is silent and the browser gate alone cannot be the only thing standing in front of it.

### The native channel now connects, and the attachment path now runs

Two checks were added, and both were previously reported as *not verified* in this file.

**Check 6 — a registered native host produces a connected channel.** `capture-core --native-host` has
existed since round 6; what was missing was anything that registered it. Two obstacles, both now
closed:

1. **No stable extension id.** An unpacked extension's id is derived from its checkout path, so
   `allowed_origins` had nothing to name. `manifest.json` now carries a pinned `key`, and
   `tools/make-extension-key.mjs` regenerates the pair. The id in this repository is
   `ebdiaplaignnfokkkoekjkdajlopdfkk` for everyone, and the browser confirms it.
2. **Nothing wrote the registration.** `tools/native-host.mjs` writes the host manifest and computes
   the pinned id; `tools/native-host.ps1` writes two `HKCU` values and regenerates the launcher. No
   elevation is needed, and `-Action uninstall` removes exactly what install created — the gate
   installs before launch and unregisters afterwards.

The registration has to happen **before the browser starts**. Chromium resolves the host manifest when
the connection is requested, and `native.js` applies a 5 s cool-down after a failed connect, so a host
registered mid-run is invisible to that run — the first version of this check did exactly that and
reported a correct FAIL.

**A launcher is required, and finding out why was worth the run.** Chromium launches a native host with
**no arguments**, and a host manifest has no field for them. `capture-core` refuses to start without
`--spool-dir` ("a provider with nowhere to write must not start", §3.5 step 2), so a manifest pointing
at the bare binary produced a host that exited immediately — and the browser reported it as
`Can't find manifest for native messaging host`, which names the wrong problem entirely. The manifest's
`path` is now a generated `.cmd` launcher that supplies the flags; the binary is `_executable`. **The
production installer has to do the same thing**, and that is a deployment requirement this round
discovered rather than assumed.

What makes the verdict strong: `connected` is emitted by `native.js` only after a message has actually
round-tripped, so a PASS here means the browser delivered a message to a real `capture-core` process
and the answer came back. It is not "a port object was handed out" — the failure mode this component
already had once.

**Check 7 — a real user-selected `File` is read, transferred and acknowledged.** With the content
script loading again, this check attaches a real `File` to the page's file input, asks the content
script what it can see through the production `capture_upload_check` message, then drives the
production `capture_upload_send` message. The content script reads the bytes from its own registry,
chunks them, relays them to `capture-core` and hashes exactly what it sent; the check compares that
digest against one computed independently over the bytes the page attached. A match means the bytes
came from the user's file, not from a fixture.

Two earlier attempts at this check are worth recording, because both "passed" something that was not
the claim: the first built a **fresh** `File` registry in the page, which holds no `File` and can only
report zero candidates; the second tried to seed a CDP-created isolated world, which is a **blank**
world — Chromium's own content script lives in a different one, and `chrome.runtime` is not even
defined there. The check now drives the extension's own messages and lets the extension do the work.

### What a browser still cannot observe here, and what would change it

- **Check 5 — a blocked request is cancelled AND recorded — remains NOT OBSERVABLE on this host.** An
  unpacked load revokes `webRequestBlocking` whatever the manifest declares, and the browser says so
  itself: *"webRequestBlocking is only allowed for extensions that are installed using
  ExtensionInstallForcelist."* **A human must** install the extension by policy, load a bundle with a
  `blocked` rule, and confirm `net::ERR_BLOCKED_BY_CLIENT` together with an observation for the same
  request. Until then no report should claim blocking works.
- **The real file picker and a real drop are still not exercised.** Check 7 attaches a real `File`
  object and drives the real transfer, but no OS picker dialog is opened and no drop event is
  synthesised from outside the page.
- **Google Chrome specifically** — Chrome 154 refuses `--load-extension` outright; see above.

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
   state is "do not read the body": no bundle means M0, which means no body lane is registered at all. Where the core
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
   unpacked load), which leaves observation working and §7.4's ability to cancel gone. `endpoint/protocol`
   has no member for "this install cannot enforce", so the extension reports it as an extension-side
   field on the health report and counts it as `enforcement_unavailable` rather than inventing a
   `Detail`. **A protocol decision, not an implementation one** — raised for the Lead.
