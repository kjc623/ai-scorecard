# endpoint — the device tier

This is the half of the system that runs on a company-managed laptop. It exists because the
customers are companies that have *not* bought enterprise AI: there is no vendor-side API to pull
from, so if the device does not observe a submission, the fact does not exist anywhere. Everything
here is built to make that observation possible without making the device a content-egress path.

The design and its reasons are in [docs/01-collectors.md](../docs/01-collectors.md); the language
choices are in [docs/00-architecture.md §4.1](../docs/00-architecture.md). This file is the map of
the code, not a second copy of the design.

## Next: collecting Claude Code prompts

The delivery path is built (service → enrol → spool → drain → `POST /v1/events`); nothing is captured
yet because no provider is enabled and there is no policy. To collect prompts from Claude Code — a CLI
speaking HTTPS to `api.anthropic.com`:

1. **Build a signed policy bundle** that scopes `proxy.tls` to the generative hosts, carries the local
   root CA, and names the classifier. This is the missing component: with no bundle the device is at
   M0 and the interceptor is unconfigured. (There is no operator-facing bundle generator; `--selftest`
   signs a throwaway one.)
2. **Turn the interceptor on**: `SAC_PROXY_TLS=true` with a fixed `SAC_PROXY_TLS_LISTEN` port.
3. **Route the CLI to it**: point Claude Code's proxy at that port.
4. **Trust the CA**: the bundle's root CA in the machine trust store (or `NODE_EXTRA_CA_CERTS` for the
   Node-based CLI), or the intercepted handshake fails.

Steps 2–4 are configuration; step 1 is the work.

## What is here

| Directory | What it is |
|---|---|
| [protocol/](protocol/README.md) | The device-side wire and IPC contract, owned by the Lead. Envelope, frames, spool records, native messaging, batch shapes. No component redefines these shapes. |
| [capture-core/](capture-core/README.md) | The privileged agent: providers, mode resolution, envelope minting, policy store, spool wiring, the native-messaging host, and the device-to-cloud drain. One static Go binary per platform; on Windows it hosts the service itself (`--service`). |
| [capture-spool/](capture-spool/README.md) | The only durable store on the device: bounded, encrypted at rest, append-only, single-writer. Implements `protocol.Store`. |
| [classifier-host/](classifier-host/README.md) | Rules, validators and model over bytes handed to it, compiled from one Go source to native and `js/wasm`, with document parsing in an isolated child. |
| [canon/](canon/README.md) | Unicode NFC — step C3 of the `sac-canon-1` contract — with tables generated from and checked against Node's ICU. |
| [integration/](integration/README.md) | Test-only module that wires the pieces together for real, because every component suite only proves the component against its own fakes. |

`capture-extension` is the fourth device process and lives in [extension/](../extension), outside
this tree: it is plain JavaScript (ES modules) and Manifest V3, and it is the only component that can see browser
internals. Its seam with `capture-core` is `protocol/native.go`.

Distribution is in [installer/](../installer): a Windows MSI, a macOS PKG and a Linux package, all
driven from one manifest. The Windows MSI registers `capture-core` as a service it hosts itself
(`--service`, the SCM contract implemented in the binary), configured by a file. This tree is the
code those artefacts install.

## How the parts compose

A provider (proxy, broker, detector, or the browser over native messaging) hands the core an
observation: metadata it could obtain **without** reading content, plus a lazy content reader. The
pipeline resolves the effective mode from the signed bundle first, and only then decides whether the
reader is ever called. That ordering is the mechanism behind the product's central promise — at M0
nothing reads the bytes at all — so it is enforced by the type system rather than by a check
someone has to remember.

From there the pipeline computes the route's dedup keys (`dedup`), classifies content if the mode
permits (`classifierlink` to `classifier-host`), mints an envelope that the contract's closed schema
will accept, and appends it to the spool. The spool holds exactly the bytes the device will
eventually send, and nothing in it parses them.

The spool is not the end of the path. [`capture-core/drain/`](capture-core/drain/README.md) reads it
oldest-first and delivers batches to the tenant's ingest API over the ADR 0020 transport — `x509`
mTLS or DPoP — so a device enrols once, holds one revocable credential, and settles every record
from the API's per-event outcome. It is **opt-in** (`--device-endpoint`): with no endpoint the spool
*is* the endpoint and the shutdown drain reports what is still in it. The ingest service itself lives
in [ingestion/](../ingestion), not here; the device never holds a database credential
([ADR 0001](../docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md)).

Policy is data: a signed bundle decides interception scope, loopback port maps, per-tool modes, the
body cap and the kill switch. A bundle that fails verification never changes what the device is
enforcing — the previous one stays in force, or the device runs at M0 with none.

## Build and test

Go commands run with the offline prefix. The modules depend on the standard library and on each
other only, and the repository's gates set `GOPROXY=off` by choice, so no build reaches the network.
With `$PWD` at the repository root (where `.tools\` lives):

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
```

Each module builds and tests independently (`go test ./...` in `protocol/`, `canon/`,
`capture-spool/`, `capture-core/`, `classifier-host/`, `integration/`). From the repository root,
`node tools/verify-all.mjs` runs them all, and `node tools/accept.mjs` is the acceptance gate.
`endpoint/capture-core/run.ps1` is the capture-core entry point, with `-Selftest` for the assembled
end-to-end run.

## What is deliberately not here

- **The browser half.** Chromium APIs are unavailable outside Chromium; see [extension/](../extension).
- **`cli.shim`.** The route exists in the closed route vocabulary and in §3.5's startup order, but
  no provider implements it, so it has no coverage row rather than a healthy-looking empty one.
- **The server tier.** `ingest-api`, `control-api` and `content-vault` live in
  [ingestion/](../ingestion), [control/](../control) and [vault/](../vault). What *is* here is the
  device half of the write path — [`capture-core/drain/`](capture-core/drain/README.md) — which is
  opt-in and delivers to `POST /v1/events`; the device holds no database credential.
- **Platform facilities.** The system proxy, the OS trust store, DPAPI/Keychain key sealing and a
  full process enumerator are interfaces with no wired implementation in this build. Where a
  capability is missing the component reports `degraded` with a named detail instead of claiming
  health.
- **SQLite.** [ADR 0002](../docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md)
  names SQLite in WAL mode for the device spool. The spool was written without network access and
  the builds run with `GOPROXY=off`, so no SQLite driver is a dependency, and cgo is unavailable
  (the build host has no C compiler); the spool is an append-only segment log behind the same
  `protocol.Store` interface. A pure-Go driver is fetchable when the module proxy is enabled and
  could satisfy that interface later. The deviation is stated in
  [capture-spool/doc.go](capture-spool/doc.go) and is not presented as SQLite.
- **Rust, and the language change that replaced it.** [ADR 0016](../docs/adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)
  makes the classifier host one Go source compiled to native and `js/wasm`, because §9.1's requirement is
  byte-identical labels from one source rather than a particular language — and when it was decided the build
  host had no Rust toolchain and no network to fetch one. The decision is recorded; the files that stated the original
  choice ([docs/00 §4.1](../docs/00-architecture.md), [docs/01 §3.1 and §9.1](../docs/01-collectors.md))
  were corrected to point at it.
