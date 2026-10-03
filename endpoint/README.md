# endpoint — the device tier

This is the half of the system that runs on a company-managed laptop. It exists because the
customers are companies that have *not* bought enterprise AI: there is no vendor-side API to pull
from, so if the device does not observe a submission, the fact does not exist anywhere. Everything
here is built to make that observation possible without making the device a content-egress path.

The design and its reasons are in [docs/01-collectors.md](../docs/01-collectors.md); the language
choices are in [docs/00-architecture.md §4.1](../docs/00-architecture.md). This file is the map of
the code, not a second copy of the design.

## What is here

| Directory | What it is |
|---|---|
| [protocol/](protocol/README.md) | The device-side wire and IPC contract, owned by the Lead. Envelope, frames, spool records, native messaging, batch shapes. No component redefines these shapes. |
| [capture-core/](capture-core/README.md) | The privileged agent: providers, mode resolution, envelope minting, policy store, spool wiring, and the native-messaging host. One static Go binary per platform. |
| [capture-spool/](capture-spool/README.md) | The only durable store on the device: bounded, encrypted at rest, append-only, single-writer. Implements `protocol.Store`. |
| [classifier-host/](classifier-host/README.md) | Rules, validators and model over bytes handed to it, compiled from one Go source to native and `js/wasm`, with document parsing in an isolated child. |
| [canon/](canon/README.md) | Unicode NFC — step C3 of the `sac-canon-1` contract — with tables generated from and checked against Node's ICU. |
| [integration/](integration/README.md) | Test-only module that wires the pieces together for real, because every component suite only proves the component against its own fakes. |

`capture-extension` is the fourth device process and lives in [extension/](../extension), outside
this tree: it is TypeScript and Manifest V3, and it is the only component that can see browser
internals. Its seam with `capture-core` is `protocol/native.go`.

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

Policy is data: a signed bundle decides interception scope, loopback port maps, per-tool modes, the
body cap and the kill switch. A bundle that fails verification never changes what the device is
enforcing — the previous one stays in force, or the device runs at M0 with none.

## Build and test

Go commands on this host need the offline prefix and no network:

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
- **`ingest-api`, `control-api` and `content-vault`.** Nothing in this tree sends to a server yet:
  the spool fills and the drain step reports what is still in it. The cloud tier is elsewhere.
- **Platform facilities.** The system proxy, the OS trust store, DPAPI/Keychain key sealing and a
  full process enumerator are interfaces with no wired implementation in this build. Where a
  capability is missing the component reports `degraded` with a named detail instead of claiming
  health.
- **SQLite.** [ADR 0002](../docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md)
  names SQLite in WAL mode for the device spool; no SQLite driver is fetchable on this offline host,
  so the spool is an append-only segment log behind the same `protocol.Store` interface. The
  deviation is stated in [capture-spool/doc.go](capture-spool/doc.go) and is not presented as SQLite.
- **Rust, and the language change that replaced it.** [ADR 0016](../docs/adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)
  makes the classifier host one Go source compiled to native and `js/wasm`, because §9.1's requirement is
  byte-identical labels from one source rather than a particular language — and there is no Rust toolchain
  on this host and no network to fetch one. The decision is recorded; the files that stated the original
  choice ([docs/00 §4.1](../docs/00-architecture.md), [docs/01 §3.1 and §9.1](../docs/01-collectors.md))
  were corrected to point at it.
