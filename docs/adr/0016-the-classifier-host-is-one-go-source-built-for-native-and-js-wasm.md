# 0016. The classifier host is one Go source built for native and `js/wasm`

Status: proposed · Amends the toolchain choice in [00-architecture.md §4.1](../00-architecture.md) and
[01-collectors.md §9.1](../01-collectors.md) · Related: [0002](0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md) (device store)
Date: 2026-10-02

## Context

`docs/00-architecture.md` §4.1 and `docs/01-collectors.md` §9.1 choose **Rust** for the classifier host,
compiled from one source to a native target and to `wasm32`. §9.1's real requirement is not the language —
it is that **one source produces byte-identical labels in two places**, so the synchronous in-page
decision and the native, authoritative decision cannot drift.

Building it surfaced constraints that were not in play when the choice was made. All four are verified on
the build host, not assumed:

1. **There is no outbound network.** `https://registry.npmjs.org/` fails at the TLS layer
   (`schannel: AcquireCredentialsHandle failed`), as does `proxy.golang.org`. Nothing can be fetched:
   no npm package, no Go module, no toolchain installer.
2. **There is no Rust toolchain and it cannot be installed.** `rustc`, `cargo`, `%USERPROFILE%\.cargo`,
   `%USERPROFILE%\.rustup` and `C:\Program Files\Rust*` are all absent; `rustup` is a download.
3. **Go is present and builds both targets offline.** Go 1.27.0 produced a native binary and, with
   `GOOS=js GOARCH=wasm`, a 2.5 MB wasm module, both from the same source, with no network.
4. **The requirement §9.1 exists to protect is still live.** The extension's WASM copy makes the
   synchronous inline warn/block decision inside the 300 ms interactive budget
   ([01-collectors.md §7.4](../01-collectors.md)), and the native host is authoritative for the envelope's
   labels. Two hand-maintained implementations of the same rules were already rejected as the alternative
   §9.1 rules out.

## Decision

**The classifier host is Go, one source, compiled twice:** `GOOS=windows|darwin GOARCH=amd64|arm64` for
the resident native host, and `GOOS=js GOARCH=wasm` for the in-page copy, loaded through Go's own
`wasm_exec.js` runtime shim.

Everything §9.1 and §9.2 require of the component is unchanged and remains the acceptance criteria:

- **Byte-identical labels across the two targets** over a fixed corpus, asserted by a test in
  `device/classifier-host`, not by convention.
- Rules → validators → model, with each stage's budget measured rather than asserted, and a stage that
  exceeds it producing `confidence: degraded` rather than a failed submission (C21).
- The rules DSL stays **data** evaluated by an interpreter with a closed operator set; a rule cannot
  execute code, reach the network, or read the spool.
- The parser child keeps its parent-enforced cap, timeout and hard kill, and still receives exactly one
  document buffer.
- The classifier still receives **bytes and nothing else**: no tool identity, no `user_ref`, no
  destination ([01-collectors.md §3.3](../01-collectors.md)). `device/protocol` makes that structural — the
  request type has no field that can carry identity, and a compile-time guard fails the build if one is
  added.

`wasip1` is explicitly **not** the wasm target: a Chromium content script cannot load a WASI module without
a shim, and the shim is exactly the round trip the in-page copy exists to avoid.

## Alternatives considered

- **Keep Rust, and accept that the component cannot be built or tested here.** Rejected for this build
  round: it would leave the component unverified while the rest of the device tier is testable. It is the
  right answer the moment a networked build host with `rustup` exists, which is why the revisit trigger
  below is written as a toolchain fact rather than a preference.
- **Two implementations, one per target.** Rejected by §9.1 itself: the divergence test exists because
  hand-maintained parallel copies of a security-relevant classifier drift, and the drift is invisible
  until a customer asks why the inline decision and the recorded verdict disagree.
- **Native only, with a plain-JS predicate for the in-page path.** Rejected: it makes the inline decision
  a second, weaker classifier whose labels are not comparable to the recorded ones, which is the same
  divergence with worse attribution.
- **Move classification to the native host for every request.** Rejected: it puts a cross-process round
  trip on the browser's blocking path inside a 300 ms budget, and it is unavailable exactly when the
  channel is down — the case §3.4 says must degrade to `degraded` rather than to a slow browser.

## Consequences

**Easier.** The classifier is buildable and testable on this host, so §9.1's equivalence property is
demonstrable rather than aspirational. One language covers the whole device tier (capture-core, spool,
classifier), which removes a toolchain from the build and release path and from the endpoint's signing
surface.

**Harder.** Go's `js/wasm` runtime is roughly 2.5 MB for a trivial program, and the extension loads it
once before the first inline decision; the 300 ms budget must therefore be measured against the real
module with the real rules corpus, and that measurement is part of the component's acceptance rather than a
footnote. Go's wasm target also has no direct Chrome-API access, so the classifier's inputs and outputs
cross the JS boundary as copied bytes, which is an interface the Rust design would have had in a different
form.

**We now maintain.** A second target's build in CI, and the equivalence test that keeps the two honest —
which is the same obligation §9.1 already created, now with a toolchain that exists here.

**Revisit if:** a build host with `rustup` is available and the 2.5 MB wasm runtime plus its load cost
threatens the 300 ms interactive budget — in that case Rust may be revisited on measurement rather than on
preference, and this record is superseded rather than quietly ignored. This record does not change
`device/protocol`, the rules DSL, the release states, or the parser-child isolation: none of them depends
on the language.
