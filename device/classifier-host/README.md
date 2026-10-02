# classifier-host

The classifier host of [docs/01-collectors.md §9](../../docs/01-collectors.md) and the parent of
the §10 document-parsing child. One Go source, two targets: the resident native host and the
extension's in-page copy (`GOOS=js GOARCH=wasm`), per
[ADR 0016](../../docs/adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)
and [TOOLCHAIN-DECISION.md](TOOLCHAIN-DECISION.md).

```
bytes ─► normalise ─► RULES ─────► VALIDATORS ─────► MODEL ─────► labels + confidence + version
          §9.2         candidates   checksums /        fuzzy       (never a boolean, C19)
                       (§9.5 DSL)   structure          classes
```

## What is here

| Package | Responsibility |
|---|---|
| `classify` | The pipeline, §9.4's measured budget, §9.7's degraded semantics, §9.6's release states, the §3.4 framing server, the rolling per-stage p95 |
| `rules` | The §9.5 DSL as **data**: strict decoding, load-time caps, whole-file rejection, and an interpreter whose import set cannot reach os/net/exec (`rules_test.go`) |
| `validators` | The closed validator set that ships in the binary: Luhn, IBAN mod-97, US SSN, UK NINO, EAN-13, UPC-A, SIN, NPI |
| `norm` | §9.2's normalise stage: decoding, a documented Unicode subset, whitespace collapsing, bounded structured-body extraction, the `content_digest` |
| `model` | The fuzzy-class stage: a signed artefact, verified by digest, scored in fixed-point integers so both targets agree bit for bit |
| `release` | Signed releases (§9.5/§9.6): ed25519 over the manifest and artefacts, digest checks, cap checks, atomic swap, retention of the previous release |
| `docparse` | The interface between the host and the §10 child (so the host still compiles for wasm, where parsing does not exist) |
| `parser` | The child: one framed document in, extracted text and a status out. Imports no os/net/exec/syscall |
| `parser/isolation` | The parent side of §10: one child per document, job object + residency cap, wall-clock timeout, hard kill, output cap, bounded fan-out, per-format breaker |
| `cmd/classifier-host` | One binary: `classify`, `measure`, `release`, `serve`, `parse-child`, `version`; on js/wasm it also registers `shadowAIClassifier` for the in-page path |
| `internal/*` | `hiresclock` (sub-millisecond stage timing on Windows), `testhook` (the documented §10 test hooks), `testrig`/`importcheck` (test-only) |

## Build and test

The build host is offline (ADR 0016): standard library only, `GOPROXY=off`, and the Go command
needs this prefix in PowerShell (dot-sourcing `.tools\env.ps1` is blocked by execution policy):

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
cd device/classifier-host
go build ./...
go test ./...
```

`device/protocol` is consumed through a local `replace` (it is the Lead's package; nothing here
redefines its shapes).

Both targets, plus the two acceptance measurements:

```powershell
go build -o classifier-host.exe ./cmd/classifier-host
$env:GOOS="js"; $env:GOARCH="wasm"; go build -o classifier-host.wasm ./cmd/classifier-host
Remove-Item Env:GOOS; Remove-Item Env:GOARCH

# a signed release to classify with (development key, development rules)
.\classifier-host.exe release --dir .\rel --state shadow --version dev-1 --key .\key.hex --rules .\testdata\dev-rules.json
$pub = .\classifier-host.exe release --key .\key.hex --print-pubkey

# §9.1: the same corpus through both targets
.\classifier-host.exe classify --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --out native.json
node tools/run-wasm.mjs .\classifier-host.wasm classify --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --out wasm.json
# diff native.json wasm.json  -> identical

# §9.4: per-stage latency on both targets
.\classifier-host.exe measure --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --iterations 200 --out latency-native.json
node tools/run-wasm.mjs .\classifier-host.wasm measure --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --iterations 200 --out latency-wasm.json
```

`go test .` runs those two runs for real (`equivalence_test.go`, `measure_test.go`) and writes
[`reports/`](reports); it needs `go` and `node` on PATH. `tools/wasm_exec.js` is a verbatim copy
of Go 1.27's `$(go env GOROOT)\lib\wasm\wasm_exec.js` (BSD, header intact) so the Node test host
can load the module the same way a browser page does.

## Measured

See [MEASUREMENTS.md](MEASUREMENTS.md) and `reports/`. Summary of the last full run on this host
(2026-10-02, Windows 10.0.26200, Go 1.27.0, Node 22.23.1):

- **§9.1 equivalence: executed and passing.** 20 corpus cases, 13 with labels, 2 degraded,
  byte-identical output (8142 bytes) from `windows/amd64` and `js/wasm`.
- **§9.4 budget: within on both targets**, e.g. native rules p95 0.136 ms against a 20 ms share;
  wasm rules p95 0.596 ms against 35 ms. Whole-classification p95: 0.15 ms native, 0.65 ms wasm.
- **js/wasm module: 6,344,880 bytes** (ADR 0016 estimated ~2.5 MB for a *trivial* program);
  compile + instantiate 12.5–14.1 ms, which is the cost that matters for the interactive budget.
- **§10 hostile documents, all bounded:** decompression bomb → `parser_output_cap`; 5000-deep
  JSON → `parser_failed` (nesting status); a member declaring 8 MiB → refused before reading;
  512 MB allocation against a 96 MB residency cap → killed, `parser_memory`; a 10 s hang against a
  250 ms timeout → killed, `parser_timeout`; a child exiting 3 → `parser_crash`; a child writing
  non-frame bytes → `parser_crash`. The parent survives every one and parses the next document.
- **§10 end to end:** a real docx through the shipped CLI is parsed by a child process and
  labelled `payment_card` natively, and degrades with the `parse` stage named on js/wasm
  (`reports/document-routing.txt`).

## `confidence: degraded`, exactly

Every path below emits a `degraded` response with **no labels**, a named failed/truncated stage and
a detail from `protocol`'s closed vocabulary. `classify/host_test.go` has one subtest per row.

| Cause (§9.7 row) | Detail | Test |
|---|---|---|
| a stage was skipped because its budget was exhausted | `budget_exhausted` | rules / validators / model / whole-pipeline subtests |
| the model artefact was missing, unloadable or failed to verify | `model_unavailable` | rules-only release |
| normalisation truncated the payload so a rule could not see all of it | `normalise_truncated` | text cap below the body length |
| the document parser failed, timed out or was killed | `parser_failed` / `parser_timeout` / `parser_memory` / `parser_crash` / `parser_output_cap` | five parser subtests + `parser/isolation` |
| content the classifier could not process (over-cap body, undecodable bytes) | `content_over_cap` / `undecodable_content` / `content_unprocessable` | three normalise subtests |
| a release failed to load and rules-only labels came from the retained release | `release_load_failed` | store with a rejected v2 |
| the host was unreachable | `host_unreachable` | server panic recovery |
| content at M0, where reading content is forbidden | `mode_violation` | mode gate |

Not emitted when (§9.7's other column): the classifier ran and found nothing (empty labels +
`confidence: high`), the model returned low scores, the payload was large but fully processed, a
document parsed and contained nothing, the body was decoded and processed. Each has a subtest.

`protocol.ClassifyResponse.Validate()` is called on every response the tests produce, so a
degraded answer can never be unattributable and a confident one can never hide a failed stage.

## Release states (§9.6)

| State | Classification | Labels | Enforcement |
|---|---|---|---|
| `shadow` | yes | yes | **no** — `action: logged`, `shadowed: true`, `decided_locally: true` |
| `enforcing` | yes | yes | yes — the policy's thresholds decide `blocked`/`warned`/`logged` |
| `rolled_back` | yes, at the previous version | yes | **no** |

A device can enforce release N while shadow-evaluating N+1 on the same traffic; the enforcing
verdict is returned and the shadow verdict rides alongside in `Verdict.Shadow`. A release that
fails to load retains the previous one and degrades with `release_load_failed`; there is no path
that ends at "no rules".

## A digest has exactly one spelling

A digest is **`sha256:<64 lowercase hex>`** and only that spelling is the same value. Uppercase hex
is the same bytes written differently, which is precisely why it must not be accepted as a second
spelling: the database's digest columns carry `~ '^sha256:[0-9a-f]{64}$'`
(`ops.content_object.ciphertext_sha256`, `ops.retrieval_grant.raw_digest`), so a loader that accepted
the uppercase form would admit a release the store would refuse.

Both digest comparisons in this component are therefore exact — `release.verifyDigest` (rules and
model artefacts) and `model.Load` (the model artefact against its signed digest) — and
`TestDigestHasExactlyOneSpelling` pins the negative case (a valid signature over a correctly computed
digest written in uppercase is refused) with a positive control (the lowercase spelling loads). The
test was verified in the failing direction: restoring `strings.EqualFold` makes it fail. Nothing else
in the component normalises a digest; the remaining `strings.ToLower` calls are over media types,
rule tokens, filenames and context values, where case folding is the intended comparison.
## Parser child isolation (§10)

One child per document, spawned as `classifier-host parse-child` and reaped after one result.
Parent-enforced: a Windows job object with `JOB_OBJECT_LIMIT_PROCESS_MEMORY` plus a residency
sampler that kills on breach (Linux: `/proc/<pid>/statm`), a wall-clock timeout covering spawn,
write, read and wait, and a hard kill (job termination on Windows, process group on Unix). Bounded
fan-out (4), a declared-size cap checked before any spawn, a bounded read of the child's output,
and a per-format breaker that disables a repeatedly failing format and records a coverage row.
The child imports no `os`, `net`, `os/exec` or `syscall` — asserted by test — and is given exactly
one document buffer on stdin.

## Open decisions, deviations and what is NOT VERIFIED

§9–§10 do not settle these; each is implemented one way and named here rather than left implicit.
Numbered for the report to the Lead.

1. **Unicode normalisation is a documented subset, not NFKC.** `golang.org/x/text` is not
   fetchable offline (ADR 0016), so `norm` folds fullwidth/halfwidth ASCII forms, Unicode spaces,
   zero-width and format characters, and line endings. Full NFC/NFKC needs a Unicode table this
   host cannot obtain. Because §9.2 makes normalisation part of the **dedup contract**
   ([02-ingest-and-transport §4](../..//docs/02-ingest-and-transport.md)), the ingest-side
   normalisation must either adopt this subset or the two must be reconciled — **open**.
2. **§9.4's "emit labels found" versus the protocol's degraded shape.** §9.4 says a rules-stage
   exhaustion emits the labels found and marks degraded; `protocol.ClassifyResponse.Validate`
   (correctly, per §9.7) forbids labels on a degraded response because the envelope omits them.
   Resolution here: the protocol response carries none, and the labels found are preserved in
   `Verdict.PartialLabels` plus counters for the audit path. **Open** if the audit trail is
   expected to carry them.
3. **Label-set merge.** §9.2 says "output is a label set" without saying how two rules producing
   one class combine. Here: keyed by class, highest score wins, the count suppressed is recorded.
4. **Confidence bands.** §9.7 defines `degraded` exactly and leaves `high`/`medium`/`low` to the
   implementation. Here: a rules+validators verdict is `high`; model-only is `medium` at ≥ 0.85
   and `low` below; a completed run that found nothing is `high`.
5. **Stage names.** `protocol.StageResult.Stage` documents `rules | validators | model | parse`;
   the pipeline also records `normalise` (§9.2 names it), `release` and `mode`. The field is a free
   string, so nothing breaks, but the closed set is **open**.
6. **Rule "family".** §9.4 stops a budget-exhausted rules stage "after the current rule family"
   without defining a family. Here: an optional `family` field defaulting to the class, families
   evaluated in declaration order, and the stop happens at a rule boundary inside the family.
7. **Windows named-pipe transport is NOT IMPLEMENTED.** Go's standard library has no named-pipe
   listener and the offline host cannot fetch one, so Windows serves over its own stdin/stdout
   (`serve --transport stdio`, the parser-child pattern), macOS over a Unix-domain socket
   (`serve --transport unix`), and tests over loopback TCP. A hand-rolled `CreateNamedPipe`
   adapter is the remaining work.
8. **macOS residency monitoring is NOT IMPLEMENTED.** There is no `/proc` on Darwin, and the
   libproc binding (`proc_pid_rusage`) is not fetchable offline, so on macOS the parser child is
   bounded by the timeout and the hard kill only, and the result says so
   ("the parent-enforced memory cap is not active") rather than implying a cap exists. §10 also
   asks for an address-space limit there; RLIMIT_AS would kill a healthy Go child, which reserves
   a large virtual address space at startup.
9. **`go test -race` is NOT VERIFIED on this host**: `-race` needs cgo and there is no C compiler
   (`gcc` not found). Concurrency is covered by `TestClassifyIsSafeForConcurrentUse` and the
   bounded-fan-out tests, which are not a substitute for the race detector.
10. **The model artefact is a development fixture.** `model.DevArtefactJSON()` is a small
    hand-written linear scorer; training and shipping the real artefact is a signed content-
    pipeline deliverable. What is proven here is the mechanism (digest-verified load, integer
    scoring, deterministic across targets, skippable when missing or unverified).
11. **PDF is not parseable.** The standard library has no PDF parser. `.docx/.xlsx/.pptx/.odt/.ods`
    (zip), `.gz`, JSON, XML, CSV and text are parsed; a PDF returns
    `unsupported_media_type` → degraded + a per-format coverage row. §10's per-format coverage is
    exactly the place this is meant to be visible.
12. **Request digest versus host digest.** `protocol.ClassifyRequest.ContentDigest`'s comment says
    the caller computes it; §9.2 says the digest computed by normalisation is the one in
    `content_digest`. Here the host computes and returns its own digest and *counts* a
    disagreement without degrading, because a disagreement is a dedup-contract defect rather than
    a classification failure. **Open**: which side is authoritative.
13. **Deferred parses.** §10 (A13) puts a large document's parse off the interactive path. Here the
    synchronous answer for a document over the inline threshold (64 KiB default) is
    `degraded` with `parse_deferred` set, because nothing was inspected synchronously; producing
    the real labels later as a correction is `capture-core`'s async path, not this component's.
14. **The enforcement half of a verdict has no home in `device/protocol`.** `ClassifyResponse`
    carries no `Decision`, so the server's frame is `classify.Verdict`: the classification half is
    `protocol.ClassifyResponse` verbatim, and `action`/`rule_id`/`decided_locally`/`shadowed` ride
    alongside for the core to place in the envelope's `policy_decision`. **Open** if the Lead
    prefers a protocol-level frame type.
15. **The wasm module is 6.34 MB, well over ADR 0016's ~2.5 MB estimate** (which was measured on a
    trivial program). The cost that matters is the load: 12.5–14.1 ms to compile and instantiate on
    this host, against a 300 ms inline budget, so the ADR's revisit trigger ("the 2.5 MB runtime
    plus its load cost threatens the 300 ms budget") is **not** met — but the size is worth
    reporting, and trimming the in-page build (regexp, ed25519 and XML are the bulk) is available
    work if the extension's own load path measures worse.
16. **A resident shadow release doubles classification work per request.** §9.6's coexistence is
    implemented by running the pipeline twice under the same request budget; the shadow run is not
    separately budgeted or counted in the p95. Worth a follow-up if a device ever runs shadow and
    enforcing releases at high volume.
17. **`ClassifyRequest.ReleaseID` is refused, not honoured.** The protocol says "a caller never
    selects a release; policy does", so any non-empty value that does not match the resident
    release is a degraded refusal (`release_load_failed`).
18. **The parser child's test hooks are shipped.** `CLASSIFIER_HOST_TESTHOOK_{ALLOC_MB,SLEEP_MS,EXIT,GARBAGE}`
    are read by `parse-child` (internal/testhook) so §10's limits can be tested against a child
    that misbehaves. They can only make the child *less* well behaved and are documented rather
    than hidden; deleting them from the shipped binary would mean the §10 tests no longer exercise
    the shipped child.
