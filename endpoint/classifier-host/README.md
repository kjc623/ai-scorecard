# classifier-host

This is the classifier host of [docs/01-collectors.md §9](../../docs/01-collectors.md) and the parent
of the §10 document-parsing child: one Go source built for two targets — the resident native host and
the extension's in-page copy (`GOOS=js GOARCH=wasm`). It exists so rules and model cannot drift
between the two places a verdict is produced, and so the classifier never sees who is being
classified. [ADR 0016](../../docs/adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)
made the language Go, decided when the build host had no network and no Rust toolchain; [TOOLCHAIN-DECISION.md](TOOLCHAIN-DECISION.md) records what
that preserves and what it costs.

The pipeline runs normalise, rules, validators, model, and the output is labels plus a confidence
band — never a boolean. Bytes in, labels out: the request type has no tenant, device, user, tool, host
or route field, and a compile-time guard keeps it that way.

## What is here

| Package | Responsibility |
|---|---|
| `classify` | The pipeline, §9.4's measured budget, §9.7's degraded semantics, §9.6's release states, the §3.4 framing server, the rolling per-stage p95 |
| `rules` | The §9.5 DSL as **data**: strict decoding, load-time caps, whole-file rejection, and an interpreter whose import set cannot reach os/net/exec |
| `validators` | The closed validator set shipped in the binary: Luhn, IBAN mod-97, US SSN, UK NINO, EAN-13, UPC-A, SIN, NPI |
| `norm` | §9.2's normalise stage: decoding, a documented Unicode subset, whitespace collapsing, bounded structured-body extraction, the `content_digest` |
| `model` | The fuzzy-class stage: a signed artefact verified by digest, scored in fixed-point integers so both targets agree bit for bit |
| `release` | Signed releases (§9.5/§9.6): ed25519 over the manifest and artefacts, digest checks, cap checks, atomic swap, retention of the previous release |
| `docparse`, `parser`, `parser/isolation` | The §10 child: one framed document in, extracted text out; the child imports no os/net/exec/syscall, and the parent enforces the job object, residency cap, timeout, hard kill, output cap, bounded fan-out and a per-format breaker |
| `cmd/classifier-host` | One binary: `classify`, `measure`, `release`, `serve`, `parse-child`, `version`; on js/wasm it also registers `shadowAIClassifier` |
| `internal/*` | `hiresclock` (sub-millisecond stage timing on Windows), `testhook` (the documented §10 hooks), `testrig`/`importcheck` (test-only) |

## Build, test, measure

The build is offline by choice: standard library only, `GOPROXY=off`.

```powershell
# from endpoint/classifier-host
$env:GOCACHE="$PWD\..\..\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
go test ./...                                   # builds both targets and runs the suite
go build -o classifier-host.exe ./cmd/classifier-host
$env:GOOS="js"; $env:GOARCH="wasm"; go build -o classifier-host.wasm ./cmd/classifier-host
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

`go test .` runs the equivalence and latency runs for real and writes [`reports/`](reports); the
`release`, `classify` and `measure` invocations behind those reports are in
[MEASUREMENTS.md](MEASUREMENTS.md), which is the full record. `endpoint/protocol` comes in through a
local `replace`, and `tools/wasm_exec.js` is a verbatim copy of Go 1.27's shim.

The resident host is started with `serve`, which takes a signed release directory and the
release-signing public key. The transport defaults to `stdio` on Windows and `unix` elsewhere;
`unix` and `tcp` need `--addr`:

```powershell
.\classifier-host.exe serve --release DIR --pubkey HEX --transport unix --addr PATH
```

## Measured (2026-10-02, Windows, Go 1.27.0, Node 22.23.1)

- **§9.1 equivalence: executed and passing.** 20 corpus cases, 13 with labels, 2 degraded,
  byte-identical output (8,142 bytes) from `windows/amd64` and `js/wasm`.
- **§9.4 budget: within on both targets.** Native stage p95 — normalise 0.003 ms, rules 0.134 ms,
  validators 0.001 ms, model 0.005 ms — against a 95 ms sum of shares; wasm 0.019 / 0.565 / 0.004 /
  0.029 ms against 150 ms. Whole classification p95: 0.144 ms native, 0.608 ms wasm. `reports/` is
  authoritative, and `measure` fails loudly if a stage leaves its share.
- **js/wasm module: 6,345,103 bytes**, compile and instantiate 12.3–12.8 ms — well over ADR 0016's
  ~2.5 MB estimate for a *trivial* program, comfortably inside the 300 ms interactive budget.
- **§10 hostile documents, all bounded, the parent survives every one:** a bomb to
  `parser_output_cap`; 5000-deep JSON to `parser_failed`; 8 MiB and over-cap declarations refused
  before reading or spawning; 512 MB against a 96 MB residency cap killed as `parser_memory`; a 10 s
  hang against a 250 ms timeout killed as `parser_timeout`; exit 3 and non-frame output as `parser_crash`.
- **§10 end to end:** a real docx through the shipped CLI is parsed by a child and labelled
  `payment_card` natively; on js/wasm it degrades with the `parse` stage named.

## `confidence: degraded`, exactly

Every row emits a degraded response with **no labels**, a named failed or truncated stage, and a detail
from `protocol`'s closed vocabulary. `classify/host_test.go` has one subtest per row.

| Cause (§9.7 row) | Detail |
|---|---|
| a stage was skipped because its budget was exhausted | `budget_exhausted` |
| the model artefact was missing, unloadable or failed to verify | `model_unavailable` |
| normalisation truncated the payload so a rule could not see all of it | `normalise_truncated` |
| the document parser failed, timed out or was killed | `parser_failed` / `parser_timeout` / `parser_memory` / `parser_crash` / `parser_output_cap` |
| content the classifier could not process | `content_over_cap` / `undecodable_content` / `content_unprocessable` |
| a release failed to load and labels came from the retained release | `release_load_failed` |
| the host was unreachable | `host_unreachable` |
| content at M0, where reading content is forbidden | `mode_violation` |

Not emitted when the classifier ran and found nothing (empty labels, `confidence: high`), when the
model returned low scores, when a document parsed and contained nothing, or when the payload was large
but fully processed. Each has a subtest. `protocol.ClassifyResponse.Validate()` runs on every response
the tests produce, so a degraded answer can never be unattributable and a confident one can never hide
a failed stage.

## Release states (§9.6)

| State | Classification | Enforcement |
|---|---|---|
| `shadow` | yes | no — `action: logged`, `shadowed: true`, `decided_locally: true` |
| `enforcing` | yes | yes — the policy's thresholds decide `blocked`/`warned`/`logged` |
| `rolled_back` | yes, at the previous version | no |

A device can enforce release N while shadow-evaluating N+1 on the same traffic: the enforcing verdict
is returned and the shadow verdict rides alongside in `Verdict.Shadow`. A release that fails to load
retains the previous one and degrades with `release_load_failed`; there is no path that ends at "no
rules". The enforcement half of a verdict has no home in `endpoint/protocol`, so the server's frame is
`classify.Verdict` with `action`/`rule_id`/`decided_locally`/`shadowed` alongside the response (item 14).

## A digest has exactly one spelling

A digest is `sha256:<64 lowercase hex>` and only that spelling is the same value. Uppercase hex is the
same bytes written differently, which is why it must not be accepted as a second spelling: the
database's digest columns carry the regex `^sha256:[0-9a-f]{64}$`, so a loader accepting uppercase
would admit a release the store would refuse. `release.verifyDigest` and `model.Load` are both exact,
and `TestDigestHasExactlyOneSpelling` pins the negative case with a positive control; it was verified
in the failing direction (restoring `strings.EqualFold` makes it fail).

## Parser child isolation (§10)

One child per document, spawned as `classifier-host parse-child` and reaped after one result. The
parent enforces a Windows job object with a process-memory limit plus a residency sampler that kills
on breach (Linux: `/proc/<pid>/statm`), a wall-clock timeout covering spawn, write, read and wait, a
hard kill (job termination, or process group on Unix), fan-out bounded at 4, a declared-size cap
checked before any spawn, a bounded read of the output, and a per-format breaker that records a
coverage row. A job memory limit alone is not enough: a Go child at its job cap thrashes instead of
dying, so the parent derives a residency cap from any job cap it sets.

## Open decisions, deviations and what is NOT VERIFIED

§9–§10 do not settle these; each is implemented one way and named here rather than left implicit. The
numbering is stable — MEASUREMENTS.md cites items 1, 7, 8 and 15.

1. **Unicode normalisation is a documented subset, not NFKC.** The build is standard-library only with `GOPROXY=off`, so `golang.org/x/text` is not a dependency (it is fetchable when the module proxy is enabled), and `norm` folds width forms, Unicode spaces, zero-width and format characters, and line endings. Because §9.2 makes normalisation part of the dedup contract, ingest must adopt this subset or the two must be reconciled — **open**.
2. **§9.4's "emit labels found" versus the protocol's degraded shape.** A degraded response carries none; the labels found are kept in `Verdict.PartialLabels` plus counters. **Open** if the audit trail is expected to carry them.
3. **Label-set merge.** Two rules producing one class combine keyed by class, highest score wins, and the count suppressed is recorded.
4. **Confidence bands.** Rules+validators is `high`; model-only is `medium` at ≥ 0.85 and `low` below; a completed run that found nothing is `high`.
5. **Stage names.** `rules | validators | model | parse` is documented; the pipeline also records `normalise`, `release` and `mode`. The field is a free string, so nothing breaks, but the set is **open**.
6. **Rule "family".** An optional `family` field defaults to the class; families evaluate in declaration order and a budget stop happens at a rule boundary inside the family.
7. **Windows named-pipe transport is NOT IMPLEMENTED.** Windows serves over stdio, macOS over a Unix-domain socket, tests over loopback TCP; a hand-rolled `CreateNamedPipe` adapter is the remaining work.
8. **macOS residency monitoring is NOT IMPLEMENTED.** No `/proc`, no libproc binding in the standard library, so the child is bounded by timeout and hard kill only and the result says the memory cap is not active.
9. **`go test -race` is NOT VERIFIED**: `-race` needs cgo and there is no C compiler. Concurrency is covered by `TestClassifyIsSafeForConcurrentUse`, which is not a substitute for the race detector.
10. **The model artefact is a development fixture** — a small hand-written linear scorer. What is proven is the mechanism: digest-verified load, integer scoring, deterministic across targets, skippable when missing or unverified.
11. **PDF is not parseable.** The standard library has no PDF parser; `.docx/.xlsx/.pptx/.odt/.ods`, `.gz`, JSON, XML, CSV and text are. A PDF returns `unsupported_media_type`, degrades, and adds a per-format coverage row.
12. **Request digest versus host digest.** The host computes and returns its own digest and *counts* a disagreement without degrading, because that is a dedup-contract defect rather than a classification failure. **Open**: which side is authoritative.
13. **Deferred parses.** For a document over the inline threshold (64 KiB default) the synchronous answer is degraded with `parse_deferred` set; producing the real labels later is `capture-core`'s async path.
14. **The enforcement half of a verdict has no home in `endpoint/protocol`** — see the release-states section. **Open** if the Lead prefers a protocol-level frame type.
15. **The wasm module is 6.35 MB against ADR 0016's ~2.5 MB estimate.** The cost that matters is ≈12–13 ms to compile and instantiate against a 300 ms budget, so the ADR's revisit trigger is not met; trimming the in-page build is available work if the extension measures worse.
16. **A resident shadow release doubles classification work per request**, and the shadow run is not separately budgeted or counted in the p95.
17. **`ClassifyRequest.ReleaseID` is refused, not honoured.** A caller never selects a release; policy does, so a mismatching value is a degraded refusal (`release_load_failed`).
18. **The parser child's test hooks are shipped.** `CLASSIFIER_HOST_TESTHOOK_{ALLOC_MB,SLEEP_MS,EXIT,GARBAGE}` can only make the child *less* well behaved; deleting them would mean the §10 tests no longer exercise the shipped child.

What it deliberately does not do: it never receives identity (not in the request type, not in a
label); it opens no sockets, reads no spool and holds no CA key; it parses no document in the browser
(§9.1 puts parsing on the native side, and the wasm copy degrades with `parse` named); and it makes no
enforcement decision of its own author — labels and confidence are evidence, and thresholds that turn
them into block, warn or log come from signed policy.
