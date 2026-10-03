# `capture-core` — the endpoint agent binary

This is the service [docs/01-collectors.md §3.1](../../../../docs/01-collectors.md) calls `capture-core`:
one binary that hosts `proxy.tls`, `proxy.loopback`, `proc.detect`, the policy engine, the spool, the
classifier link and the browser's native-messaging host.

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
| `--version` | Version, the local framing version, and the native-messaging framing. |

Exactly one mode may be selected; the flag set refuses a combination.

## What runs, and in what order

The service drives `core.Supervisor`, which encodes §3.5 literally and records every step it performs:

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
| `--tenant-id`, `--device-id`, `--user-ref`, `--population` | the enrolled identity stamped on every envelope and the population used for scope resolution |
| `--retention` | device-side retention for spooled observations (default `720h`) |
| `--bundle`, `--policy-key`, `--policy-key-id` | the signed bundle and the pinned Ed25519 key; omit the bundle to run at M0 |
| `--classifier-address`, `--classifier-budget` | `unix:PATH`, `pipe:NAME`, or `tcp:127.0.0.1:PORT` (loopback only), and the per-classification budget. Empty address means rules-only, `confidence: degraded` |
| `--proxy-tls`, `--proxy-tls-listen`, `--proxy-tls-canary` | the interceptor; without a canary it reports `degraded detail=tls_probe_failed` rather than healthy |
| `--proxy-loopback`, `--proc-detect` | the other two providers |
| `--drain-deadline` | the bound on the shutdown drain (§3.5 step 3) |
| `--health-file`, `--health-interval` | the health channel, appended as JSON lines |
| `--attachment-cap` | the policy cap on one attachment manifest, checked before any byte moves |
| `--device-endpoint`, `--auth-mode`, `--credential-file`, `--enrolment-token`, `--ca-file`, `--mdm-id`, `--backoff-base`, `--backoff-cap` | the device-to-cloud drain (ADR 0020): the ingress base URL, `x509`\|`dpop`, the sealed-credential path, the one-shot enrolment token, the pinned CA set, the MDM device id (hardware-identity seed), and the retry backoff bounds. An empty `--device-endpoint` disables the drain |
| `--print-config`, `--dry-run` | `--print-config` resolves and validates everything, prints it and exits; `--dry-run` builds the service graph, starts nothing (no §3.5 step runs), prints one health snapshot and waits for the stop signal |
| `--work-dir`, `--keep-work-dir` | the selftest work directory (default OS temp, removed on exit, success or failure) |
| `--log-format`, `--log-level` | `json` (default) or `text`; `debug`, `info`, `warn`, `error` |

## Running it as a service

**Windows.** The binary is a console application that runs in the foreground and stops on Ctrl-C or
the service manager's stop. To install it as a service, use a wrapper the operator already trusts
(`sc.exe` with a service host, NSSM, or a WinSW XML) with **LocalSystem** (the trust store and system
proxy are machine-scope), automatic start, and restart-on-failure with backoff — which is §3.5's
crash policy at the service-manager layer. No recovery action may restart in a tight loop: §3.5
requires a crash loop to stop and report `absent` with the loop count.

**Linux/macOS.** A systemd unit or a LaunchDaemon with the same argv, `Restart=on-failure`,
`RestartSec` ≥ 5s. Nothing in the binary forks or daemonises: it expects to be the supervised process
and exits non-zero only when it refuses to start.

**Nothing is installed by this repository or by `--selftest`.** No service, no system proxy, no
trust-store entry, no scheduled task.

## What is NOT VERIFIED on this host

- **No browser in the selftest.** `--selftest` never launches a browser. What it verifies is the
  byte-level framing and the child-process model: it starts this binary as a separate process with
  real Chromium frames on its stdin, twice — once with the classifier host up (all six frames
  answered with an ack or a typed refusal, at least one ack, and no rules-only fallback in the
  child's log) and once with it down (the same six answers, the rules-only fallback logged, exit 0).
  The launch by a real Chromium browser is evidenced outside this module:
  `extension/tools/in-browser-check.mjs` loads the extension into Edge with this binary registered
  as the native host, and `extension/tools/in-browser-check.evidence.txt` records the round trip.
- **No real system proxy and no real trust store.** Those are behind the interfaces in
  `proxy/tlsproxy` and this build wires none of them: `proxy.tls` therefore reports
  `degraded detail=tls_probe_failed`, and is exercised by its own package tests only.
- **No DPAPI/Keychain sealing.** The spool key is a file protected by filesystem ACLs; the platform
  wrapping described in §12 is a `KeyProvider` implementation that does not exist yet.
- **No `cli.shim`.** The route has no provider, so it has no coverage row.
- **`proc.detect` is partial.** The only enumerator on this host is `tasklist`, which gives an image
  name and a PID: no modules, no listening sockets, no compute signature. It can match the candidate
  rule and emit daily rollups, but never evidence of use, so it never emits a `model_detection`.
- **No ingest client.** With `--device-endpoint` unset (the default) nothing is sent to a server: the
  drain step reports what is still spooled and stops at its deadline. With it set, the drain is the
  real ADR 0020 device-to-cloud path (enrol, DPoP token or x509 leaf, batched `POST /v1/events`).
  The health channel is still written to a file, not POSTed.
- **No M3 content store.** An M3 observation is refused rather than emitted without the content it
  says it holds; `content-vault` is a server-side component.
- **Enrolment is opt-in.** With `--device-endpoint` set, the drain enrols on first start (a PKCS#10
  CSR in `x509` mode, the public JWK plus a proof in `dpop` mode) and seals the issued credential
  beside the spool; the envelope identity is still the flags. Rotation (§2.2's 60/90-day overlap) is
  a control-api concern and is not implemented here.
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
