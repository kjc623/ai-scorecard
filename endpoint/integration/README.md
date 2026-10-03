# Device integration harness

**Owner:** the Lead. **Scope:** test files only — nothing here is production code, and nothing here
should become production code. If a piece of logic belongs in a component, it belongs in that
component; this module exists to wire components together and observe what happens.

```
cd endpoint/integration
$env:GOCACHE="$PWD\..\..\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
go test ./... -count=1
```

Or from the repo root: `node tools/verify-all.mjs` (this module is one of its packages).

## Why it exists

Every component in the device tier has its own suite, and every one of those suites proves the
component **against its own fakes**. None of them can show that the pieces compose. That gap is not
theoretical here: the extension sent observation content as raw text while `endpoint/protocol`
declared it `[]byte`, so `encoding/json` base64-decoded it — text payloads failed outright, and text
that happened to *be* valid base64 decoded silently to different bytes than the user typed while the
digest covered the original. Both sides' suites were green, because each side tested its own
assumption. This module is the check that would have caught it.

## What is here

| File | Drives | Proves |
|---|---|---|
| `native_seam_test.go` | The golden frames in `testdata/native/` through the real `protocol` types | The extension-to-capture-core seam: framing, the frame version, and that `content` is base64 whose decoded bytes are the bytes the digest describes |
| `device_path_test.go` | One gold frame through `core.Pipeline` and the real `capture-spool` | The endpoint stack composes: the spooled payload is a contract-shaped envelope; M0 never calls the content reader or the classifier; a refused spool write is attributable; a missing canonicaliser degrades and never claims a Tier-T key |

The golden frames are **not** hand-written fixtures. `extension/tools/emit-frames.mjs`
builds them with the extension's own `observationBody()` and `frame()`, so what this module reads is
what the extension actually produces:

```
node extension/tools/emit-frames.mjs
```

Six cases, chosen for what they can falsify rather than for coverage: plain ASCII; a payload that is
*itself* valid base64 (the silent-corruption case a decode-error test cannot see); multi-byte UTF-8;
binary that is not text; M0 with no content; and an over-cap body. Regenerate after changing the
extension — and if a case fails, read the case's `why` before assuming the extension is wrong, since
the whole reason this seam broke once is that both sides were individually confident.

## These tests were verified to fail

A check that has never failed proves nothing. Both directions were run by mutating a golden frame to
send its content **raw**, the way the pre-fix extension did:

| Mutation | Result |
|---|---|
| Raw text where the payload is ASCII | `content is not valid base64 (illegal base64 data at input byte 9)` |
| Raw text where the payload *is* valid base64 | `content decodes to "hello world", but this case's payload is "aGVsbG8gd29ybGQ="` — the silent-corruption control, caught at the wire level too |

Frames were restored and the suite re-run green afterwards in both cases.

## What is deliberately NOT covered

- **No browser.** Nothing here launches one, so this module says nothing about the `chrome.*`
  surface, native-messaging host registration, or the inline warn/block path. The extension's own
  suite covers their logic against a fake adapter, and `extension/tools/in-browser-check.mjs` drives
  a real Chromium browser (Edge) with `capture-core` registered as the native host — its record is
  `extension/tools/in-browser-check.evidence.txt`, where the blocking path is still marked not
  observable.
- **No server.** Draining the spool to `ingest-api` is a separate seam with its own evidence;
  nothing here asserts that a batch the device would send is accepted.
- **No real platform facilities.** Process enumeration, the system proxy, the OS trust store and
  DPAPI/Keychain are behind interfaces with fakes in the components. The classifier is a recorder
  here, not the real host, so this module says nothing about classification correctness — only about
  what the pipeline does with its answer.
- **Not an end-to-end endpoint run.** Nothing here starts a process, installs anything, or binds a
  real port: the tests drive the packages in-process, and the golden frames are files. The
  `capture-core` binary now exists (`cmd/capture-core`, with `--selftest` starting it as a child
  process), but that is its own evidence, not this module's.
