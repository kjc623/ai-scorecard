# device

Everything that runs on or is installed onto a managed device: the capture agent, the browser
extension that hands it submissions, and the installers that put both in place. The agent observes
AI submissions (the local TLS proxy for CLI tools, the loopback broker for local inference servers,
the browser extension through native messaging), classifies them on the device, applies the tenant's
signed collection policy, spools them encrypted and delivers them to the device edge over mutual TLS.

| Component | What it is |
|---|---|
| `protocol` | The shapes the device components and the device edge agree on: enrolment, policy, events, health, content upload, native messaging, classifier frames, spool records. |
| `capture-core` | The agent binary (`cmd/capture-core`) and its packages: `core` (pipeline, mode gate, envelope, supervisor), `policy`, `enforce` (evaluates the bundle's enforcement rules), `proxy/tlsproxy`, `proxy/loopback`, `cli` (CLI trust shim), `trust`, `drain` (enrolment, delivery, policy fetch, health, content upload), `credential`, `contentstore`, `state`, `hostinfo`, `dedup`, `classifierlink`, `attachments`, `localipc` (the local endpoint the browser relay and the user-session helper connect to), `userhelper` (the user-session helper), `toolconfig` (writes AI tools' managed configuration), `discovery` (de-duplicates and budgets discovery records), `inventory` (the installed-app scanner), `component` (supervises the child processes capture-core runs, such as classifier-host: restarts with backoff, at most 5 times in 10 minutes, and kills them with the service). |

| `capture-core` | The agent binary (`cmd/capture-core`) and its packages: `core` (pipeline, mode gate, envelope, supervisor), `policy`, `enforce` (evaluates the bundle's enforcement rules), `proxy/tlsproxy`, `proxy/loopback`, `cli` (CLI trust shim), `trust`, `drain` (enrolment, delivery, policy fetch, health, content upload), `credential`, `contentstore`, `state`, `hostinfo`, `dedup`, `classifierlink`, `attachments`, `localipc` (the local endpoint the browser relay and the user-session helper connect to), `userhelper` (the user-session helper), `toolconfig` (writes AI tools' managed configuration), `etwsession` (real-time ETW sessions), `procmon` (the process monitor), `component` (supervises the child processes capture-core runs, such as classifier-host: restarts with backoff, at most 5 times in 10 minutes, and kills them with the service). |
| `capture-spool` | The encrypted, bounded, crash-safe single-writer spool. |
| `classifier-host` | The on-device classifier, run by capture-core as a child process on stdio. |
| `extension` | The Chrome/Edge extension that observes browser submissions to AI tools and hands them, with their attachments, to capture-core through the native messaging host. |
| `installer` | The Windows MSI, macOS package and Linux package that install the agent and register the native messaging host. |
| `integration` | Cross-component tests, including contract acceptance of every envelope the device emits. |

## Running in production

The installer registers one service (Windows service `ShadowAICapture`, launchd
`com.shadowaicapture.capture-core`, systemd `shadow-ai-capture.service`) that runs:

```
capture-core --config-file <vendor capture-core.env> --config-file <tenant.env>
```

Each file is `KEY=VALUE` lines; a later file wins, and a command-line flag wins over both. The
tenant file comes from the customer's MDM with the deployment package.

| Key | Flag | Meaning |
|---|---|---|
| `SAC_STATE_DIR` | `--state-dir` | Protected state directory (required). Holds the spool, `spool.key`, `credential.sealed`, `content/`, `content.key`, `policy/`, `device-ca/`, `health.json` and, for the Windows service, `capture-core.log`. capture-core gives it a protected DACL (SYSTEM, Administrators) on Windows and mode 0700 owned by root elsewhere. |
| `SAC_TENANT_ID` | `--tenant-id` | Tenant id (required; tenant file). |
| `SAC_DEVICE_ENDPOINT` | `--device-endpoint` | Device edge base URL, https with no path (required; tenant file). |
| `SAC_DEPLOYMENT_KEY` | `--deployment-key` | Tenant deployment key; enrols the device (tenant file). |
| `SAC_CA_FILE` | `--ca-file` | PEM CA set trusted for the device endpoint in addition to the system roots. |
| `SAC_POLICY_KEY` | `--policy-key` | Hex Ed25519 key the policy bundle must verify under. Without it the device runs at M0. |
| `SAC_POLICY_KEY_ID` | `--policy-key-id` | Key id the bundle must name (default `policy-key-1`). |
| `SAC_CLASSIFIER_RELEASE` | `--classifier-release` | Signed classifier release directory; set with the key below. |
| `SAC_CLASSIFIER_PUBKEY` | `--classifier-pubkey` | Hex Ed25519 key the classifier release must verify under. |
| `SAC_DEVICE_IDENTITY` | `--device-identity` | `clear` or `hashed` until the server states the tenant's setting (default `clear`). |
| `SAC_LOG_LEVEL` | `--log-level` | `debug`, `info`, `warn`, `error` (default `info`). Logs are JSON on standard error. |

The CLI trust shim writes the CA bundle and environment profile that command-line runtimes read to
a directory users can read, outside the state directory: `C:\ProgramData\ShadowAICapture\cli`
(Windows), `/var/db/shadow-ai-capture` (macOS, with `state/` inside it, root only) and
`/etc/shadow-ai-capture` plus `/etc/profile.d/shadow-ai-capture.sh` (Linux). The directory is the
agent's own default, never taken from the policy bundle.

On first start the agent enrols with the deployment key (a CSR; the edge returns a device
certificate), fetches the signed policy bundle, mints its per-device interception CA, and starts the
providers. The CA's certificate is kept in `device-ca/`; on Windows its private key is a
non-exportable CNG machine key, `ShadowAICapture-DeviceRoot` in the Microsoft Software Key Storage
Provider, and elsewhere a file beside the certificate. TLS inspection is a tenant setting, off by default: only while the bundle's
`interception.enabled` is true does the agent run the TLS proxy, write the CLI trust shim, set the
Windows desktop-app PAC and keep its CA in the trust store, and a policy change starts or removes
them without a restart. It rotates the certificate, presenting the current
one, two thirds of the way through its validity. `capture-core --print-config` shows the resolved
configuration and the enrolment; `--version` the build.

## The browser's native messaging host

Chrome and Edge start the registered host with the extension's origin (`chrome-extension://<id>/`)
as the first argument; the installers register `capture-core` itself. In that mode it is a relay:
it connects to the running service at `\\.\pipe\ShadowAICapture.native` (Windows; SYSTEM and
Administrators full control, interactive users read and write, remote clients refused) or
`/var/run/shadow-ai-capture/native.sock` (macOS and Linux; directory 0755 root, socket 0666), checks
that the endpoint belongs to the service (pipe owned by SYSTEM or Administrators, socket peer uid
0), and copies Chromium native-messaging frames (4-byte little-endian length, then one JSON
message, at most 1 MiB) between its stdin/stdout and the endpoint. The service names the connecting
process's account (the pipe's client process token, `SO_PEERCRED`, `LOCAL_PEERCRED`) and attributes
that browser user's observations to them.

Attachment bytes arrive ahead of their observation (manifest, contiguous chunks, completion). The
service refuses the manifest with `mode_forbids_read` when policy reads no content, holds at most
32 MiB per attachment and 64 MiB per connection in memory, and drops bytes whose observation does
not name them by digest within two minutes. The observation's bytes are classified under its mode
(documents go to classifier-host's isolated parser child), the labels join the event's, and the
bytes are discarded: the envelope carries only the name, size and digest, and nothing is written
to the spool or the content store.

## The user-session helper

On Windows the service starts `capture-core --user-helper` in every session with a signed-in user
(active or disconnected), as that user, on `winsta0\default` with no window, and checks every 15
seconds that each still has one; a helper that exits is started again at most 5 times in 10 minutes
per session. The helper connects to the same endpoint, with the same check that the service owns
it, and opens with `helper_hello` naming its session; the service accepts it only from the user
signed in to that session. It shows the notifications the service sends as Windows toasts under the
AppUserModelID `ShadowAICapture.Agent`, which the MSI's Start-menu shortcut carries. Its health row
is `user_helper`: healthy when every signed-in session has a connected helper, else `degraded` with
`helper_unavailable`; on macOS and Linux it is `absent` with `helper_unavailable`.

## Tool configuration

While the bundle switches the OTLP receiver and Claude Code's OTel export on, the service merges
Claude Code's telemetry variables (the receiver's address and token, and prompt logging on when the
mode for `app:claude_code` is `m1` or higher) into the `env` object of
`C:\Program Files\ClaudeCode\managed-settings.json`, which users cannot override. While the hook
relay and Claude Code's hooks are on, it also declares its own hooks there: `UserPromptSubmit`, and
`PreToolUse` for Bash, PowerShell, WebFetch and MCP tools, each running the installed
`capture-core.exe --hook claude_code <event>` with a 1-second timeout, beside any hooks the
customer declares; with `endpoint.hooks.managed_only` it sets `allowManagedHooksOnly`. It touches no
other key. Before its first write it backs the file up to `toolconfig\claude_code\original` in the
state directory; switching the export or the hooks off removes those keys and restores the values
they replaced.
The file keeps its access control unless users could write it; a new file is readable by users and
writable by administrators only. Its health row is `tool_config_claude_code`: `healthy` while the file
holds the agent's keys, `absent` with `tool_not_installed` when the inventory's CLI scan finds no
Claude Code in any user profile, `degraded` with
`config_write_failed` otherwise; on macOS and Linux it is `absent` with `tool_version_unsupported`.
With TLS inspection on, the proxy blind-tunnels a connection from a tool whose native collector is
enabled.

## Tool hooks

A tool's prompt hook runs `capture-core --hook <tool> <event>` as the user, before the tool sends
the prompt. It reads the tool's JSON from stdin (at most 1 MiB), turns it into one `hook_evaluate`
(the prompt text up to 256 KiB; a longer one as its length with `over_cap`) through the tool's
adapter in `capture-core/hooks`, sends it on the native endpoint with the same check that the service
owns it, and prints the adapter's rendering of the `hook_decision`. Within 400 ms of starting, or on
any failure, it prints the tool's allow output instead; it writes nothing to stderr and logs
nothing. The service's `hook_relay` collector, on while the bundle's `endpoint.hooks.enabled` is,
answers a tool whose `endpoint.tools.<key>.hooks` is on from the bundle's rules (classifying with a
30 ms budget at `m1` and above), then records the prompt on route `tool.hook` with that decision.
Anything else is answered `allow` and not recorded. Claude Code's adapter sends a `UserPromptSubmit`
prompt as written and a `PreToolUse` call's `tool_input` as compact JSON, and answers in Claude
Code's JSON output with exit code 0: a block with the rule's message and link, a warning as a
`systemMessage`, and no output to allow.

## Installed apps

While the bundle switches `endpoint.inventory` on, the service scans the installed applications at
start and then every `interval_minutes`, reading the registry only: the `Uninstall` entries of HKLM
(both views) and of each signed-in user's hive, the machine's AppX/MSIX packages
(`Appx\AppxAllUserStore\Applications`) and each user's package repository. It matches them
against the bundle's app catalog (uninstall name, executable, package family, and the publisher for
an app the catalog names no other way) and emits each match as a `discovery` record of type
`app_installed` on `inv.scan`, attributed to the user whose hive holds it or `unattributed` for a
machine-wide install, at most once per day. Its health row is `inventory_scanner`: `healthy` after a
complete scan, `degraded` with `enumeration_partial` when a key could not be read; on macOS and
Linux it is `absent` with `tool_version_unsupported`.

The same scan finds the catalog's command-line tools in each user profile under `C:\Users` that has
a loaded hive or an `NTUSER.DAT`: global npm packages (`%APPDATA%\npm\node_modules` and the prefix
the user's `.npmrc` sets), the Claude Code native install (`%USERPROFILE%\.local\bin\claude.exe`,
versioned by `.local\share\claude\versions`), pipx venvs, and command files on the user's and the
machine's PATH (with the PE file version of an `.exe`). Each is a `discovery` record of type
`cli_installed`, attributed to the profile's owner. It reads files and metadata only and never runs a
discovered program; each file checked counts `observed`, each place it cannot read `errors`.

## The process monitor

While the bundle's `endpoint.processes.enabled` is true, the agent watches process start and stop in
real time through an ETW session, `ShadowAICapture-process`, on `Microsoft-Windows-Kernel-Process`
(keyword `WINEVENT_KEYWORD_PROCESS`, events 1 and 2), and lists the running processes once when it
starts. A process whose executable name is a catalog app's `windows_exe` is attributed to the account
it runs as and recorded once per app, user and UTC day as an `app_running` discovery with the
image's file version and its verified signer, even when the signer is not the catalog's publisher.
Processes of the same app that it starts are part of the same running app. The service log has one
line, naming the app and the process id, when an app starts and one when it stops. Its health row is
`process_detector`: `healthy` while the session delivers events, `degraded` with
`etw_session_failed` while it cannot be opened (it is retried every minute); on macOS and Linux it is
`absent` with `etw_session_failed`.

## The flow monitor

While the bundle's `endpoint.flows.enabled` is true, the agent reads connection metadata, never a
payload, through an ETW session, `ShadowAICapture-flow`, on `Microsoft-Windows-DNS-Client` (event
3008, a completed query and its answers, under the process that asked) and
`Microsoft-Windows-Kernel-Network` (events 12 and 28, a TCP connect over IPv4 or IPv6). It keeps,
for 10 minutes, the addresses each process resolved from a catalog `inference_domain`. A connect to
one of them, by that process or failing that by any process, is attributed to the domain's app and
recorded once per app, host, user and UTC day as an `inference_connection` discovery on `net.flow`,
with the host name, the connecting process's signer and the account it runs as. Any other connect is
ignored. Its health row is `flow_monitor`: `healthy` while the session delivers events, `degraded`
with `etw_session_failed` while it cannot be opened (it is retried every minute); on macOS and Linux
it is `absent` with `etw_session_failed`.

## Build and test

```
cd device/capture-core && go build -ldflags "-X main.version=1.2.3" ./cmd/capture-core
for m in protocol capture-spool capture-core integration; do (cd device/$m && gofmt -l . && go vet ./... && go test ./...); done
```
