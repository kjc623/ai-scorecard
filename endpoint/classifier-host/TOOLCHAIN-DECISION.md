# O1 — Toolchain and key-backend decision (offline build host)

**Status:** decided by the Lead, 2026-10-02 · **Supersedes nothing** · the classifier-language decision is
recorded as [ADR 0016](../../docs/adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md),
and `docs/00-architecture.md` §4.1 and `docs/01-collectors.md` §9.1 state Go and point at it.

This file exists because two decisions in the design package assumed a machine that can fetch things, and
on 2026-10-02 this build host could not. The host has outbound network now; it still has no C compiler, so
cgo is unavailable, and the repository's gates still set `GOPROXY=off` by choice.

## 1. What the host was when this was decided (measured, not assumed)

| Fact | Evidence |
|---|---|
| No outbound network | `curl https://registry.npmjs.org/` → `curl: (35) schannel: AcquireCredentialsHandle failed`; `proxy.golang.org` → connection closed. No npm install, no `go get`, no toolchain download. |
| Go 1.27.0 present | `go version` → `go version go1.27.0 windows/amd64`; native build and `GOOS=js GOARCH=wasm` build both succeed offline (2.5 MB wasm, verified). |
| Node 22.23.1 present | `node --version`; `node --test` is the test runner; no global TypeScript, no bundler, no `node_modules` cache. |
| Rust absent | `rustc`, `cargo`, `%USERPROFILE%\.cargo`, `%USERPROFILE%\.rustup`, `C:\Program Files\Rust*` all absent; `rustup` cannot be downloaded. |
| PostgreSQL absent | `.tools/pg/pgroot/postgresql-18.6.0-x86_64-pc-windows-msvc/` exists but is **empty**; no service, no `psql` on PATH, nothing at the usual install roots. |
| Azure absent | no `az` CLI, no subscription, no network. |

## 2. Decision — classifier host language

`docs/00-architecture.md` §4.1 and `docs/01-collectors.md` §9.1 originally chose **Rust, one source compiled
to native and `wasm32`**. That toolchain did not exist on the host and could not be installed.

**Decided: build the classifier host in Go, one source compiled to two targets** —
`GOOS=windows|darwin GOARCH=amd64|arm64` for the native host, and `GOOS=js GOARCH=wasm` for the in-page
copy, which is the target Go supports and for which it ships its own runtime shim
(`$(go env GOROOT)\lib\wasm\wasm_exec.js`).

What this preserves from §9.1, which is why it is an acceptable substitute rather than a retreat:

- **One source, two targets, byte-identical labels.** The equivalence test in
  `endpoint/classifier-host` compares label output across the two builds; that property is what §9.1 exists
  for, and it survives the language change.
- The rules DSL (9.5) is data, evaluated by an interpreter with a closed operator set, so the rule corpus
  and its semantics are unchanged.
- Rules → validators → model (9.2), the latency budget (9.4), `confidence: degraded` semantics (9.7),
  release states (9.6) and the parser-child isolation of §10 are language-independent and unchanged.
- The vault's key backend decision below does **not** depend on this choice.

What it costs, stated plainly:

- ADR-worthy deviation: the design package named Rust in two documents. The classifier is kept in Go, so
  the deviation is recorded as ADR 0016 and both documents now state Go; the ADR's revisit trigger is a
  build host with `rustup` together with a wasm load cost that threatens the interactive budget.
- Go's `js/wasm` runtime is roughly 2.5 MB of wasm for a trivial program; the extension's inline path
  loads it once. The 300 ms interactive budget must be measured against the real module, and
  `endpoint/classifier-host` is required to publish that measurement rather than assume it.
- `wasip1` is the other candidate target and is deliberately **not** used: it cannot be loaded by a
  Chromium content script without a WASI shim, and the in-page copy exists precisely to avoid a round trip.

## 3. Decision — key backend, spool storage, and database

| Question | Decision | Consequence |
|---|---|---|
| Content-vault key backend | `KeyWrapper` interface with a **local AES-256-GCM software implementation** (tests and local dev) and an **explicitly unimplemented** Azure Key Vault / Managed HSM backend whose interface documents what the cloud must provide. | Any claim that the cloud path works is false on this host. `vault/content-vault` must report it as NOT VERIFIED. |
| Device spool storage | Append-only segment log with a storage abstraction. SQLite (ADR 0002) is the documented choice and remains the target; no SQLite driver could be fetched when this was decided, the builds run with `GOPROXY=off`, and cgo is unavailable (a pure-Go driver is fetchable when the module proxy is enabled). | The deviation is recorded in the package doc and must be reported, never silently presented as SQLite. |
| Database schema proof | Real-server execution is the acceptance criterion and was **blocked** when this was decided (no server, no image, no network). Fallback is a static structural checker plus a runnable harness (`database/tools/run-invariants.ps1`). | The 27 assertions are **NOT VERIFIED against a real server** until the harness is run where a server exists. A static checker is not a substitute and must not be reported as one. |
| Infrastructure | Bicep plus a static checker; never deployed. | Deployment is NOT VERIFIED; the exact human command and preconditions are named in `azure/`. |

## 4. What every agent on this host must do

1. **Zero external dependencies.** Go stdlib only, Node stdlib only. `GOPROXY=off`, by choice.
2. **Go commands** use this prefix, with `$PWD` at the repository root where `.tools\` lives (dot-sourcing
   `.tools\env.ps1` is blocked by execution policy):
   `$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"`
3. **Tests run offline**: `node --test <dir>` for JS, `go test ./...` for Go.
4. **Never claim a check passed that was not run**, and never present a stand-in as the real thing
   (a local HTTP server is not a vendor tool; a static checker is not a database).
