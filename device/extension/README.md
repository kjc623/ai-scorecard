# extension

The Shadow AI Capture browser extension: Manifest V3, Chrome and Edge, force-installed by policy. It
observes requests, decides locally whether one is a submission to a generative-AI service, and hands
matched observations (and the files attached to them) to `capture-core` over native messaging; the
agent builds, spools and sends the records. A request to a web app the catalog names whose own
prompt request is known (Claude, Gemini and Perplexity, `src/submission.js`; ChatGPT's form sends a
`prompt` field) is a submission whatever its shape, and only the typed text is taken from it; any
other request is judged by its shape (`src/predicate.js`). At M0 no request body is read: the body listener is not
registered for a destination whose mode does not read content. At a mode that reads content, the
files the user chose, dropped or pasted in the frame that sent the request, and has not sent with
an earlier prompt, are transferred to `capture-core` (manifest, chunks, completion) before the
observation that names them, so the agent can classify them on the device; the observation carries
each file's name, media type, size and digest. When the page uploaded the file in a multipart form,
the browser shows the body lane the file's path in place of its bytes; the worker holds that path
for the tab (at most 64, for 30 minutes) and gives it to the descriptor of the same name. Plain ES
modules, no build step.

## How it runs in production

The release build (`device/installer/release-msi.mjs`) packages `shadow-ai-capture.crx` beside the MSI and
records it in `release.json`; control-api serves the CRX and its update manifest, which browsers reach
through the dashboard on the analyst hostname at
`https://<analyst-fqdn>/v1/extension/updates.xml` (not the device hostname, whose client
certificate request the extension downloader cannot answer), and the browsers install it from the
`ExtensionInstallForcelist` policy (see `device/installer/README.md`). The packaged manifest's
`update_url` names the same update manifest, and the `ExtensionSettings` policy's
`override_update_url` makes the browser update from it whatever an installed copy's `update_url`
says. The browser reads the manifest every few hours on its own; the extension reads it too, on
start and every five minutes, and when it lists a newer version asks the browser to fetch it now
(`runtime.requestUpdateCheck`). `capture-core` names its release version in each `policy_bundle`
answer, and a newer one starts the same read at once; a policy sync runs each time the native
channel comes back, so that is within a minute of `capture-core` restarting into a new release. The
browser refuses update checks for hours after one that does not install an update, so the extension
asks only when the manifest lists a newer version. A downloaded update installs only when the
service worker stops, which the open native port prevents, so on `runtime.onUpdateAvailable` the
extension hands its queue to `capture-core` and reloads. The installers register the native
messaging host `com.shadowaicapture.capture_core` for the extension's id, which `manifest.json`'s
`key` pins (`jnjjgjlbhfleknjpoiiopcaogphghodk`). With no host the extension keeps observing, holds
observations in a bounded memory queue and reports the channel absent.

Policy comes from `capture-core`: a `policy_sync` answers with the decoded payload of the signed
bundle in force, which the extension holds in memory, with its rules as they apply to the person the
browser runs as: a rule that names other people is left out, and one that names this person no
longer names anyone. The mode is `tenant_default_mode` or the tool's `tool_modes` entry, whichever
is more restrictive; when the bundle also scopes by population, device, notice or class prior, the
extension asks `capture-core` (`mode_query`). The bundle's `rules` and `sanctioned_tools` are
matched as `capture-core/enforce` matches them, with no labels (the extension classifies nothing); a
`warn` holds the request for the user's confirmation and a `block` cancels it, each showing the
rule's message and link in the page. A `redirect` sends a tab opening a web app the catalog names to
the extension's Sundial splash page (`splash/`), which shows the rule's message and a button to its
link; the visit is recorded as the rule's `blocked` decision, and a prompt it matches is cancelled
and its tab sent to the same page. The page's address carries only the rule id and the host, and it
reads the message and link from the bundle in force.

| Permission | Why |
|---|---|
| `webRequest`, host `<all_urls>` | see every request, including to services nobody listed; requests are never modified. The service worker also reads its own update manifest, without credentials |
| `webRequestBlocking` | cancel a request a `block` rule matches, or send a page a `redirect` rule matches to the splash page; granted only to a policy-installed extension, and its absence is reported |
| `nativeMessaging` | the only channel to `capture-core`; observations leave through it and nowhere else |
| `tabs` | show a `warn` rule's confirmation, or a `block` rule's notice, in the tab that made the request, and open a `redirect` rule's splash page there |
| `alarms` | send the health report, refresh policy and read the update manifest while the browser is idle |

The content script runs in every frame's isolated world from `document_start`, because a file the
user attaches exists only in the page. The extension stores nothing and loads no remote code.

## Build and test

```
npm ci
npm test                                   # unit suite, Node 22, no browser
npm run check:browser                      # the extension in a real Chrome, Edge or Chromium (SAC_BROWSER)
node tools/build-crx.mjs --key extension.pem --version 1.4.0 --update-url https://<analyst-fqdn>/v1/extension/updates.xml --out dist
node tools/emit-frames.mjs                 # regenerate device/integration's golden native frames
```

The signing key is the RSA private key whose public half is `manifest.json`'s `key` (Key Vault in CI,
never committed); a different key is refused because the CRX would install under another id. A new
key changes the id, the policy value and the host registration: `openssl genpkey -algorithm RSA
-pkeyopt rsa_keygen_bits:2048 -out extension.pem`, then put
`openssl pkey -in extension.pem -pubout -outform DER | base64 -w0` in `key`.
