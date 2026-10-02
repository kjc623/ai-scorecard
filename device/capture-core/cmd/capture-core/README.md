# `capture-core` — the endpoint agent binary

The service that docs/01-collectors.md §3.1 calls `capture-core`: one binary that hosts
`proxy.tls`, `proxy.loopback`, `proc.detect`, the policy engine, the spool, the classifier-host
link and the browser's native-messaging host. It wires components; it contains no policy of its own.

```powershell
# from device/capture-core
$env:GOCACHE="$PWD\..\..\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
go build -o bin\capture-core.exe ./cmd/capture-core
.\bin\capture-core.exe --selftest        # the end-to-end evidence run; exit 0 on success
.\bin\capture-core.exe --version
```

Or from the repository root: `powershell -ExecutionPolicy Bypass -File device\capture-core\run.ps1 -Selftest`.

## What runs, and in what order

`run` drives `core.Supervisor`, which encodes §3.5 literally and records every step:

| §3.5 startup | What the binary does |
|---|---|
| 1 bundle, verified | `policy.Store.Apply` over the file named by `--bundle`, verified under the pinned `--policy-key`. A failure retains the previous bundle, or falls to **M0** with none (§13.3) — it never widens. |
| 2 spool opened | `capture-spool` with a key file **outside** the spool directory, bounded and encrypted at rest. If it cannot open, no provider starts. |
| 3 `proc.detect` | only with `--proc-detect`; see the enumeration gap below |
| 4 `cli.shim` | not implemented (see gaps) |
| 5 classifier-host | `classifierlink` connects to `--classifier-address` and completes the version handshake |
| 6 `proxy.tls` | listens on `--proxy-tls-listen`; the system proxy is pointed at it only if a `SystemProxy` is wired, which this build does not do |
| 7 `proxy.loopback` | binds the bundle's port map **last**, and only after its own upstream preflight succeeds |

Shutdown reverses it: **the loopback port is released first** (E14), then the proxy stops enforcing,
then the remaining providers, then a bounded drain, then the system proxy, the trust root, and the
classifier host last. `--selftest` prints the recorded order and asserts the release-first step.

## The native-messaging host

```
capture-core --native-host     # Chromium launches this as a child process
```

Reads and writes **Chromium's** framing — a 4-byte little-endian length prefix plus one JSON
document — on stdin/stdout. That is deliberately *not* `device/protocol`'s local-socket framing
(1 version byte + big-endian length, §3.4): different peers, different transports, so the adapter
lives in the binary and the protocol package is untouched. Nothing but frames is ever written to
stdout; logs go to stderr.

Handled message types: `observation`, `decision_record`, `attachment_manifest`, `attachment_chunk`,
`attachment_complete`, `health`, `policy_sync`, `mode_query`. `policy_sync` answers with the bundle
already verified in this process; `mode_query` answers from the device's own §11.1 resolution.
Anything unknown, malformed, or contradicting itself is answered with a typed `refusal` — never with
silence, and never by failing the submission.

Attachment bytes are refused **before transfer** when the manifest's size is over `--attachment-cap`,
and a chunk without an accepted manifest (or with a gap in its sequence) is refused rather than
assembled into a partial file.

## Flags that matter

| Flag | Meaning |
|---|---|
| `--spool-dir`, `--spool-key` | the spool and its key file; the key must be outside the spool directory |
| `--bundle`, `--policy-key`, `--policy-key-id` | the signed bundle and the pinned Ed25519 key; omit both to run at M0 |
| `--tenant-id`, `--device-id`, `--user-ref` | the enrolled identity stamped on every envelope |
| `--classifier-address` | `unix:PATH`, `pipe:NAME`, or `tcp:127.0.0.1:PORT` (loopback only). Empty ⇒ rules-only, `confidence: degraded` |
| `--proxy-tls`, `--proxy-tls-listen`, `--proxy-tls-canary` | the interceptor; without a canary it reports `degraded detail=tls_probe_failed` rather than healthy |
| `--proxy-loopback`, `--proc-detect` | the other two providers |
| `--health-file`, `--health-interval` | the health channel, appended as JSON lines |
| `--print-config` | resolve the bundle and print the effective configuration, including the resolved mode per tool, then exit |
| `--selftest`, `--work-dir`, `--keep-work-dir` | the evidence run; its work directory defaults to the OS temp directory and is removed on exit, success or failure |
| `--dry-run` | resolve and validate everything, start nothing |

## Running it as a service

**Windows.** The binary is a console application that runs in the foreground and stops on Ctrl-C or
`SIGTERM`-equivalent (the service manager's stop). To install it as a service, use a wrapper the
operator already trusts (`sc.exe create` with a service host, NSSM, or a WinSW XML) with:

- `bin\capture-core.exe run --spool-dir C:\ProgramData\ShadowAICapture\spool --spool-key C:\ProgramData\ShadowAICapture\spool.key ...`
- **LocalSystem** (the trust store and the system proxy are machine-scope), automatic start,
  restart-on-failure with backoff — which is §3.5's crash policy at the service-manager layer,
- no recovery action that restarts in a tight loop: §3.5 requires a crash loop to stop and report
  `absent` with the loop count, not to retry forever.

**Linux/macOS.** A systemd unit (or a LaunchDaemon) with the same argv, `Restart=on-failure`,
`RestartSec` ≥ 5s, and `LimitNOFILE` at the default. Nothing in the binary forks or daemonises: it
expects to be the supervised process, and it exits non-zero only when it refuses to start.

**Nothing is installed by this repository or by `--selftest`.** No service, no system proxy, no
trust-store entry, no scheduled task.

## What is NOT VERIFIED on this host

- **No browser.** Chromium is not installed, so the native-messaging host has never been launched by
  a real browser. What *is* verified is the byte-level framing and the child-process model: the
  selftest starts this binary as a separate process with real Chromium frames on its stdin, twice —
  once with the classifier host up (six acks, `confidence: medium`) and once with it down (six acks,
  `confidence: degraded`, exit 0).
- **No real system proxy and no real trust store.** Those are behind the interfaces in
  `proxy/tlsproxy` and this build wires none of them: `proxy.tls` therefore reports
  `degraded detail=tls_probe_failed` (`not_effective_proxy` once a system proxy is wired). The
  interceptor itself is exercised by its own package tests with in-process root pools.
- **No DPAPI/Keychain sealing.** The spool key is a file protected by filesystem ACLs; the platform
  wrapping described in §12 is a `KeyProvider` implementation that does not exist yet.
- **No `cli.shim`.** The route has no provider, so it has no coverage row.
- **`proc.detect` is partial.** The only enumerator on this host is `tasklist` (Windows), which gives
  an image name and a PID: no modules, no listening sockets, no compute signature. It can match the
  candidate rule and emit daily rollups, but it can never produce evidence of use, so it never emits
  a `model_detection`. It is off by default; `--proc-detect` turns on the partial version.
- **No ingest client.** Nothing in this binary sends anything to a server: the drain step reports what
  is still spooled and stops at its deadline. The health channel is written to a file, not POSTed to
  `/v1/health`.
- **No M3 content store.** An M3 observation is refused rather than emitted without the content it
  says it holds; `content-vault` is a server-side component.
- **The docs' `capture-core` is a privileged service with a per-device certificate (§13.1).** This
  binary takes its identity from flags; enrolment, credential storage and rotation are not
  implemented here.

## The self test

`--selftest` is the acceptance evidence, and it exits non-zero on any failed assertion. It:

1. mints a throwaway Ed25519 key and signs a bundle (tenant default M3, four tool modes, one loopback
   port pair, a `proc.detect` seed set);
2. starts a relocated "inference server" and a fake classifier host **speaking the real framing**;
3. starts the real service and prints the recorded startup order;
4. drives the six golden frames from `device/integration/testdata/native/` through the real
   native-messaging framing, then a mode query, a policy sync, an extension health row, an over-cap
   observation, and a frame after the classifier host is stopped;
5. prints the health channel and asserts every row validates as `protocol.HealthReport`;
6. prints the spool contents, asserts every record passes the contract's mode branch, that an M0
   record carries no content-derived field, that a degraded record says `confidence: degraded`, and
   that the spool directory contains no plaintext prompt bytes;
7. shuts down and asserts the loopback port was released **first** and is free afterwards;
8. starts this binary again in `--native-host` mode as a child process, twice (classifier up, then
   down), and decodes the frames it answers.

Everything it prints comes from the production code paths. The only stand-ins are the two peers a
browser and a signed classifier release would provide, and the output names them.
