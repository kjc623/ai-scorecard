# `capture-core` — the endpoint agent binary

This is the service [docs/01-collectors.md §3.1](../../../../docs/01-collectors.md) calls `capture-core`:
one binary that hosts `proxy.tls`, `proxy.loopback`, `proc.detect`, the policy engine, the spool, the
classifier link, the browser's native-messaging host and the device-to-cloud drain.

It exists as a `cmd` package because it must wire and drive components without acquiring opinions of
its own. §4.1's provider contract, §11.2's mode gate, §13.2's bundle verification and §3.5's ordering
live in the packages it imports; the binary resolves configuration, builds the graph, drives the
supervisor, and exposes the two edges the rest of the system speaks — the classifier host's local
socket and Chromium's native-messaging channel. Nothing here decides policy.

```powershell
# from endpoint/capture-core
$env:GOCACHE="$PWD\..\..\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
go build -o bin\capture-core.exe ./cmd/capture-core
.\bin\capture-core.exe --selftest        # the end-to-end evidence run; exit 0 on success
.\bin\capture-core.exe --version
```

Or from the repository root:
`powershell -NoProfile -ExecutionPolicy Bypass -File endpoint\capture-core\run.ps1 -Selftest`.

## Modes

| Invocation | What it does |
|---|---|
| no mode flag (default) | The service: load policy, open the spool, start providers in §3.5 order. There is no positional subcommand: any positional argument, including a literal `run`, is refused. |
| `--print-config` | Resolve the bundle and print the effective configuration, including the resolved mode per tool and the axes that produced it, then exit. |
| `--native-host` | The native-messaging host on stdin/stdout, as Chromium launches it. |
| `--native-frames DIR` | Feed the golden frames in `DIR` through the real native-messaging framing and dispatch, then exit. |
| `--selftest` | The end-to-end evidence run: service, frames, health, shutdown; non-zero on any failed assertion. |
| `--service` | Host the process under the Windows Service Control Manager (Windows only); see "Running it as a service". |
| `--version` | Version, the local framing version, and the native-messaging framing. |

Exactly one mode may be selected; the flag set refuses a combination.

## What runs, and in what order

The service drives `core.Supervisor`, which encodes §3.5 literally and records every step it performs:

| §3.5 startup | What the binary does |
|---|---|
| 1 bundle, verified | `policy.Store.Apply` over the file named by `--bundle`, verified under the pinned `--policy-key`. A failure retains the previous bundle, or falls to **M0** with none (§13.3) — it never widens. |
| 2 spool opened | `capture-spool` with a key file **outside** the spool directory, bounded and encrypted at rest. If it cannot open, no provider starts. |
| 2b identity resolved | load the sealed credential if present and adopt its server-minted `tenant_id`/`device_id` (a disagreement with the flags is logged and the credential wins); with no credential and a drain configured, a bounded synchronous enrolment obtains it. An offline device leaves the identity unresolved and the pipeline refuses to mint rather than stamping the flags. |
| 3 `proc.detect` | only with `--proc-detect`; see the enumeration gap below |
| 4 `cli.shim` | with `--cli-shim`: writes the managed CA bundle and shell profile from the bundle's `cli_shim` block and `interception.root_ca_pem`; no ports |
| 5 classifier-host | `classifierlink` connects to `--classifier-address` and completes the version handshake; or, with `--classifier-release` and no address, starts the `classifier-host` beside this binary as a child on stdio and handshakes with that |
| 6 `proxy.tls` | listens on `--proxy-tls-listen` (or the bundle's `interception.proxy_listen`); with `--trust-install` it installs the device CA from `--ca-cert`/`--ca-key` (or the bundle root) into the OS store; the system proxy is pointed at it only if a `SystemProxy` is wired, which this build does not do |
| 7 `proxy.loopback` | binds the bundle's port map **last**, and only after its own upstream preflight succeeds |

Shutdown reverses it: **the loopback port is released first** (E14), then the proxy stops enforcing,
then the remaining providers, then a bounded drain, then the system proxy, the trust root, and the
classifier host last. `--selftest` prints the recorded order and asserts the release-first step.

## The native-messaging host

`capture-core --native-host` is what Chromium launches as a child process. It reads and writes
**Chromium's** framing — a 4-byte little-endian length prefix plus one JSON document — on
stdin/stdout. That is deliberately *not* `protocol`'s local-socket framing (1 version byte +
big-endian length, §3.4): different peers, different transports, so the adapter lives in the binary
and the protocol package is untouched. Nothing but frames is ever written to stdout; logs go to
stderr.

Handled message types: `observation`, `decision_record`, `attachment_manifest`, `attachment_chunk`,
`attachment_complete`, `health`, `policy_sync`, `mode_query`. `policy_sync` answers with the bundle
already verified in this process; `mode_query` answers from the device's own §11.1 resolution.
Anything unknown, malformed, or contradicting itself is answered with a typed `refusal` — never with
silence, and never by failing the submission.

Attachment bytes are refused **before transfer** when the manifest's size is over `--attachment-cap`,
and a chunk without an accepted manifest, or with a gap in its sequence, is refused rather than
assembled into a partial file.

## Flags that matter

| Flag | Meaning |
|---|---|
| `--spool-dir`, `--spool-key`, `--spool-bounds` | the spool, its key file (must be outside the spool directory), and the bound profile |
| `--tenant-id`, `--device-id`, `--user-ref`, `--population` | the identity and the population used for scope resolution. With `--device-endpoint` set, `tenant_id` and `device_id` on every envelope come from the issued credential and these two flags are only checked against it; with no endpoint they are the identity |
| `--retention` | device-side retention for spooled observations (default `720h`) |
| `--bundle`, `--policy-key`, `--policy-key-id` | the signed bundle and the pinned Ed25519 key; omit the bundle to run at M0 |
| `--classifier-release`, `--classifier-pubkey` | with no `--classifier-address`, run the `classifier-host` installed beside this binary as a child on stdio, loading this signed release under this key |
| `--content-dir`, `--content-key` | the M3 local content store and the key file it is sealed under (which must be outside the directory). Empty means the device holds no content and refuses M3 observations |
| `--classifier-address`, `--classifier-budget` | `unix:PATH`, `pipe:NAME`, or `tcp:127.0.0.1:PORT` (loopback only), and the per-classification budget. An empty address with no `--classifier-release` means rules-only, `confidence: degraded` |
| `--proxy-tls`, `--proxy-tls-listen`, `--proxy-tls-canary` | the interceptor; without a canary it reports `degraded detail=tls_probe_failed` rather than healthy |
| `--proxy-loopback`, `--proc-detect` | the other two providers |
| `--trust-install`, `--trust-store`, `--trust-remove-on-stop` | install the per-device CA into the OS trust store (`root` or Windows `enterprise`), and remove it on shutdown. Off by default: the wrong store fails silently, so installing is opt-in |
| `--ca-cert`, `--ca-key` | pin the per-device CA pair so the trusted root is stable across restarts; `--ca-cert` may be omitted when the bundle carries `interception.root_ca_pem`. With neither, `proxy.tls` mints an ephemeral CA |
| `--cli-shim`, `--shim-dir` | run `cli.shim` and choose where it writes the CA bundle, profile and Node bootstrap |
| `--drain-deadline` | the bound on the shutdown drain (§3.5 step 3) |
| `--health-file`, `--health-interval` | the health channel, appended as JSON lines |
| `--attachment-cap` | the policy cap on one attachment manifest, checked before any byte moves |
| `--device-endpoint`, `--auth-mode`, `--credential-file`, `--enrolment-token`, `--ca-file`, `--mdm-id`, `--backoff-base`, `--backoff-cap` | the device-to-cloud drain (ADR 0020): the ingress base URL, `x509`\|`dpop`, the sealed-credential path, the one-shot enrolment token, the pinned CA set, the MDM device id (hardware-identity seed), and the retry backoff bounds. An empty `--device-endpoint` disables the drain |
| `--config-file` | read a `KEY=VALUE` profile in the `SAC_*` vocabulary (`installer/manifest.mjs` is the catalogue); it supplies flags and an explicitly passed flag wins. The platform wrappers and the Windows service use it, so configuration is a file rather than a command line and a secret or a path with spaces needs no quoting |
| `--service`, `--service-name` | host the process under the Windows SCM (`--service`) and the registered service name to host (default `ShadowAICapture`). Windows only; see "Running it as a service" |
| `--print-config`, `--dry-run` | `--print-config` resolves and validates everything, prints it and exits; `--dry-run` builds the service graph, starts nothing (no §3.5 step runs), prints one health snapshot and waits for the stop signal |
| `--work-dir`, `--keep-work-dir` | the selftest work directory (default OS temp, removed on exit, success or failure) |
| `--log-format`, `--log-level` | `json` (default) or `text`; `debug`, `info`, `warn`, `error` |

## Minting the policy bundle and the device CA

`cmd/sac-bundle` is the operator-facing generator. It mints the per-device CA, scopes `proxy.tls`,
embeds the CA public cert and the `cli_shim` block, and signs the bundle:

```sh
go run ./cmd/sac-bundle \
  --out /etc/shadow-ai-capture/bundle \
  --device-id 22222222-2222-4222-8222-222222222222 \
  --hosts api.anthropic.com,api.openai.com \
  --listen 127.0.0.1:8843 --canary api.anthropic.com:443 \
  --runtimes go,node,python --tenant-default m1
```

It writes `bundle.json` (signed), `ca.pem`, `ca.key` (0600) and `policy-key.pub`, and prints the
`--bundle` / `--policy-key` / `--ca-cert` / `--ca-key` / `--trust-install` / `--cli-shim` mapping.
`--ca-key`/`--ca-cert` load an existing key pair instead of generating one; `--policy-priv` signs
with an existing Ed25519 key. The device never signs a bundle it enforces.

On Windows the same tool is a `.exe`, and PowerShell needs backticks (not `\`) to continue a line,
and a quoted value where a placeholder would be read as redirection:

```powershell
go build -o C:\architecture\bin\sac-bundle.exe   ./cmd/sac-bundle
go build -o C:\architecture\bin\capture-core.exe ./cmd/capture-core

C:\architecture\bin\sac-bundle.exe --out C:\ProgramData\ShadowAICapture\bundle `
  --device-id 22222222-2222-4222-8222-222222222222 `
  --hosts api.anthropic.com `
  --listen 127.0.0.1:8843 `
  --canary api.anthropic.com:443 `
  --shim-managed-dir C:\ProgramData\ShadowAICapture\shim
```

## Capturing Claude Code

Capture is **transparent**. With `--cli-shim`, the shim configures the machine/user environment
(`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` in both cases, `NODE_EXTRA_CA_CERTS`, `NODE_USE_ENV_PROXY=1`,
and — when the bundle sets it — the `NODE_OPTIONS=--require node-proxy.cjs` bootstrap) and the OS
trust store. In a real deployment the environment and trust configuration are delivered by the
customer's MDM/Group Policy ([docs/05 §6](../../../../docs/05-platform-delivery.md)); the user opens
a terminal and runs `claude` exactly as before. There is no vendor wrapper to launch and no
per-command change.

Confirm capture in the health file: after a prompt, the `proxy.tls` row's `observed`/`emitted`
counters climb for `api.anthropic.com`. If they do not, the agent did not inherit the environment —
check that the terminal/session was started after the shim ran (`setx /M` and shell profiles apply
to *new* processes), that `api.anthropic.com` is in the bundle's interception scope, and that the
proxy is listening. A process that was already running when the shim started will not pick the
environment up until it restarts.

This was run for real on Windows 11 through the installed service. The Claude Code CLI there is a
native binary, not Node: it honours `HTTPS_PROXY` itself and trusts the device CA through
`NODE_EXTRA_CA_CERTS` or the OS store. One prompt is several requests — the message itself, smaller
side requests, and telemetry batches to the same host — and each is an observation, so one prompt
is several events. The route's extractor finds the user-authored text in some of them and not in
others; the ones it cannot read are emitted `confidence: degraded` and, at M3, hold the request
body as observed.

The machine environment is not scoped to AI tools. Every process started after the shim runs sends
its HTTPS through the proxy, which blind-tunnels whatever is outside the interception scope, and
every runtime that reads `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` or `CURL_CA_BUNDLE` takes its trust
list from the shim's bundle. That is why the bundle on Windows carries the machine's roots as well
as the device root ([cli/README.md](../../cli/README.md)). With the service installed, Node `fetch`
and `https`, `curl.exe`, `git`, Python `urllib` and Python `requests` were each checked against a
host outside the scope and worked.

## Running it as a service

**Windows.** `capture-core --service` hosts the process under the Service Control Manager itself: it
implements the SCM contract directly (`service_windows.go` — a `SERVICE_TABLE_ENTRY` dispatcher and a
control handler), reports `START_PENDING` and then `RUNNING` only once the agent graph is up, and
shuts down in §3.5 order on a stop or shutdown control. The installer registers it as a
**LocalSystem** service (the trust store and system proxy are machine scope) with automatic start,
so no external wrapper (NSSM/WinSW) is required. It is configured by a file, not a command line: the
installer puts the enrolment profile at `%ProgramData%\ShadowAICapture\capture-core.env` and the
service points at it with `--config-file`, so a secret or a path with spaces needs no quoting and
reconfiguration is a file edit plus a service restart. The installer starts the service at the end of
the install and at every boot. Under the SCM there is no console, so the log goes to `service.log`
beside the health file, and the startup health snapshot that a console run prints is not written
(the log says so). The `classifier-host` child, when there is one, is started by this process and
ends with it; `cli.shim`'s `setx /M` and the trust install run as LocalSystem, which is what lets
them write machine scope. Recovery / restart-on-failure is the SCM's action;
no recovery action may restart in a tight loop, because §3.5 requires a crash loop to stop and report
`absent` with the loop count. Without `--service` the binary is a console application that runs in the
foreground and stops on Ctrl-C. `--service` is Windows-only: a non-Windows binary refuses it with a
clear error.

**Linux/macOS.** A systemd unit or a LaunchDaemon with the same argv, `Restart=on-failure`,
`RestartSec` ≥ 5s. Nothing in the binary forks or daemonises: it expects to be the supervised process
and exits non-zero only when it refuses to start.

**Nothing is installed by building or by `--selftest`.** No service, no system proxy, no scheduled
task. Two things change a machine, and both are asked for explicitly. `--trust-install` (or
`SAC_TRUST_INSTALL=true` in the enrolment profile) installs the per-device CA the profile points at
and, with `--trust-remove-on-stop`, removes it again. `--cli-shim` on Windows writes the proxy and
CA variables into the machine environment and deletes them when the provider stops. The installer
under [installer/](../../../../installer/README.md) is what registers a service.

## What is NOT VERIFIED on this host

- **No browser in the selftest.** `--selftest` never launches a browser. What it verifies is the
  byte-level framing and the child-process model: it starts this binary as a separate process with
  real Chromium frames on its stdin, twice — once with the classifier host up (all six frames
  answered with an ack or a typed refusal, at least one ack, and no rules-only fallback in the
  child's log) and once with it down (the same six answers, the rules-only fallback logged, exit 0).
  The launch by a real Chromium browser is evidenced outside this module:
  `extension/tools/in-browser-check.mjs` loads the extension into Edge with this binary registered
  as the native host, and `extension/tools/in-browser-check.evidence.txt` records the round trip.
- **No real system proxy.** It is behind the `core.SystemProxy` interface and this build wires no
  implementation, so `proxy.tls` is not pointed at automatically. The **trust store is wired**:
  `--trust-install` drives `trust/` on linux/darwin/windows. The Linux path is exercised for real by
  `endpoint/testlab/`, and the Windows path by the installed lab MSI on Windows 11 (the root is
  installed when the service starts and gone after an uninstall). The macOS command path is covered
  by unit tests only.
- **No DPAPI/Keychain sealing.** The spool key is a file protected by filesystem ACLs; the platform
  wrapping described in §12 is a `KeyProvider` implementation that does not exist yet, and the pinned
  CA key is a `0600` file that reports unsealed rather than implying protection it does not have.
- **`cli.shim` has no environment probe wired here.** The provider and its three checks exist; the
  "a child inherited it" check is skipped unless an `EnvProbe` is supplied, and the gap is named
  rather than reported as passed.
- **`proc.detect` is partial.** The only enumerator on this host is `tasklist`, which gives an image
  name and a PID: no modules, no listening sockets, no compute signature. It can match the candidate
  rule and emit daily rollups, but never evidence of use, so it never emits a `model_detection`.
- **The drain is opt-in, and the health channel is still file-only.** With `--device-endpoint` unset
  (the default) nothing is sent to a server: the drain step reports what is still spooled and stops
  at its deadline. With it set, the drain is the real ADR 0020 device-to-cloud path (enrol, DPoP
  token or x509 leaf, batched `POST /v1/events`), proven end-to-end against the local auth lab.
  Health is written to a file, not `POST /v1/health`.
- **The M3 content store is opt-in.** With `--content-dir`/`--content-key` set, an M3 prompt's content
  is held in a sealed local store (`contentstore`), and once its event is delivered the drain asks
  `control-api` for a per-event grant, seals the object under the key the grant carries and makes the
  one upload (docs/02 §3, §10) — proven end to end against the local auth lab on Windows. A denial
  leaves the content on the device until local retention removes it. Without the two flags an M3
  observation is still refused rather than emitted without the content it says it holds. What is held
  is the prompt text where the route could identify the user-authored segment, and the request body
  as observed where it could not. Attachments are not held.
  It was run with an `x509` credential; the `dpop` branch of the grant request is written and has not
  been run.
- **The classifier child has no supervisor.** With `--classifier-release` the agent starts
  `classifier-host` on stdio and re-spawns it on the next request after it dies, with no backoff and
  no crash-loop limit. It was run through the installed Windows service with the development rules
  release; nothing was measured about its latency there.
- **Enrolment is opt-in.** With `--device-endpoint` set, the drain enrols on first start (a PKCS#10
  CSR in `x509` mode, the public JWK plus a proof in `dpop` mode) and seals the issued credential
  beside the spool. The envelope identity is the issued one: until a credential exists the pipeline
  refuses to mint (`identity_unresolved`) rather than stamping the flags. Rotation (§2.2's 60/90-day
  overlap) is a control-api concern and is not implemented here.
- **Five tests fail on Windows, and did before the content and installer work.** Three in `cli`
  (`TestStartWritesFilesWithContentAndPermissions`, `TestNodeProxyScript`, `TestCounters`), one in
  `cmd/sac-bundle` (`TestRun_ProducesVerifiableBundle`) and one in `trust`
  (`TestVerifyLinuxUnusableStore`). They assert POSIX file modes and Linux-only behaviour.

## The self test

`--selftest` is the acceptance evidence and exits non-zero on any failed assertion. It signs a
throwaway bundle, starts a relocated "inference server" and a fake classifier host speaking the real
framing, then drives the real service: the six golden frames from
`endpoint/integration/testdata/native/` through the real native-messaging framing, a mode query, a
policy sync, an extension health row, an over-cap observation, and a frame after the classifier host
stops. It prints the recorded startup order and asserts that every health row validates as
`protocol.HealthReport`; that every spooled record passes the contract's mode branch, an M0 record
carries no content-derived field, a degraded record says `confidence: degraded`, and the spool
directory holds no plaintext prompt bytes; and that the loopback port was released **first** and is
free afterwards. It also starts this binary again in `--native-host` mode as a child process, twice —
once while the classifier host is up, before it is stopped, and once after shutdown with it down —
and decodes the frames it answers.

Everything it prints comes from the production code paths. The only stand-ins are the peers the
host cannot provide — the relocated "inference server", the frames a browser would send and the
classifier host a signed release would provide — and the output names them.
