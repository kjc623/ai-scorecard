# MEASUREMENTS — classifier-host

Measured, not asserted. Every number here comes from a command in this file, run on the build host
described below; the machine-readable originals are in [`reports/`](reports) and are regenerated
by `go test .`.

## Environment

| Fact | Value |
|---|---|
| Host | Windows, `windows/amd64` (`[System.Environment]::OSVersion` 10.0.26200) |
| Go | `go1.27.0` (run offline: `GOPROXY=off`, `GOTOOLCHAIN=local`, stdlib only) |
| Node | `v22.23.1` |
| wasm runtime shim | `$(go env GOROOT)\lib\wasm\wasm_exec.js`, Go 1.27.0 |
| Corpus | [`testdata/corpus.json`](testdata/corpus.json) — 20 cases, 200 iterations per case |
| Rules | [`testdata/dev-rules.json`](testdata/dev-rules.json) (development rule set) |
| Release | `dev-1`, state `shadow`, ed25519-signed, model artefact `dev-artefact-1` |

Commands (from `endpoint/classifier-host`, after the offline prefix
`$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"`):

```powershell
go build -o classifier-host.exe ./cmd/classifier-host
$env:GOOS="js"; $env:GOARCH="wasm"; go build -o classifier-host.wasm ./cmd/classifier-host
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
.\classifier-host.exe release --dir .\rel --state shadow --version dev-1 --key .\key.hex --rules .\testdata\dev-rules.json
$pub = .\classifier-host.exe release --key .\key.hex --print-pubkey
.\classifier-host.exe classify --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --out native.json
node tools/run-wasm.mjs .\classifier-host.wasm classify --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --out wasm.json
.\classifier-host.exe measure --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --iterations 200 --out latency-native.json
node tools/run-wasm.mjs .\classifier-host.wasm measure --release .\rel --pubkey $pub --corpus .\testdata\corpus.json --iterations 200 --out latency-wasm.json
```

## §9.1 — one source, two targets, byte-identical labels

**Executed.** `go test . -run TestNativeAndWasmProduceByteIdenticalLabels` builds both targets
from the same source, runs the fixed corpus through both (`js/wasm` under Node through Go's own
shim), and diffs the canonical records byte for byte.

```
corpus=classifier-equivalence-v1 cases=20
native=windows/amd64 bytes=8142
wasm=js/wasm        bytes=8142
identical=true
corpus: 20 cases, 13 with labels, 2 degraded
```

The records exclude stage durations and counters by construction, so an identical diff is a
statement about labels, not about two machines agreeing on a clock. The corpus is also checked
against its declared expectations (classes present, classes absent after a failed checksum,
confidence, degraded), and the test fails if either target produced no labels at all — otherwise
"both empty" would pass.

Document/parser cases are excluded from this corpus by design: [§9.1's own
table](../../docs/01-collectors.md) puts document parsing on the native side only. That routing is
measured separately below.

## §9.4 — the latency budget, measured on both targets

Nearest-rank p95 over every stage observation of the whole run (20 cases × 200 iterations).

| Stage | §9.4 native share | measured native p95 | §9.4 wasm share | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | **0.003 ms** | 10 ms | **0.019 ms** |
| rules | 20 ms | **0.134 ms** | 35 ms | **0.565 ms** |
| validators | 10 ms | **0.001 ms** | 15 ms | **0.004 ms** |
| model | 60 ms | **0.005 ms** | 90 ms | **0.029 ms** |
| **sum of stage p95** | 95 ms / 150 ms target | **0.143 ms** | 150 ms / 150 ms target | **0.617 ms** |
| whole classification | — | p50 0.017 / p95 0.144 / max 0.754 ms | — | p50 0.079 / p95 0.608 / max 5.89 ms |

(Run-to-run variation between full runs is a few percent: other runs measured native rules
p95 0.136 ms and wasm 0.548 and 0.596 ms. The figures above are the ones committed in `reports/`,
which holds the machine-readable originals and is authoritative; `go test .` rewrites those files
on every run, so a fresh run will differ from this table by the same few percent.)

Every stage is inside its share and every sum is inside the interactive target. `measure` fails
loudly if a stage leaves its share, and `measure_test.go` re-asserts it from the reports, so this
table cannot silently rot.

Two notes on honesty of the clock: Windows' `time.Now()` is too coarse to see sub-millisecond
stages (the first measurement run reported every native stage as 0.000 ms), so the host and the
harness use `QueryPerformanceCounter` through `internal/hiresclock`; and these figures are for
prompt-sized bodies under the shipped caps (1 MiB input, 1 MiB text, 20 000 model tokens).

## js/wasm module size and load cost

```
module_bytes=6345103
compile_and_instantiate_ms=12.1 .. 12.5
run_ms=36.1 .. 37.6  (all 20 corpus cases, inside the module)
target=js/wasm
go=go1.27.0
```

ADR 0016 estimated "roughly 2.5 MB of wasm for a trivial program" and made the interactive budget
(300 ms for the inline warn/block path) the revisit trigger. The shipped module is **6.35 MB** —
repo's own note is that a real program is bigger than a trivial one — and the load cost that
matters is **≈12–13 ms to compile and instantiate**, which does not threaten the budget on this host.
Reported as a finding rather than a footnote (README open decision 15).

## §10 — hostile documents, end to end

`go test ./parser/isolation/ -v` spawns *real* child processes (the test binary re-executed as the
parser child) and drives them at the parent's limits:

| Hostile input | Result | Detail | Parent survives |
|---|---|---|---|
| gzip bomb: 128 MiB of zeros in ~150 KB | `output_capped`, text bounded | `parser_output_cap` | yes, parses the next document |
| JSON nested 5000 deep | `nesting_over_cap` | `parser_failed` | yes |
| zip member declaring 8 MiB with a 1 MiB cap | refused **before** reading the member | `content_over_cap` | yes |
| document declaring 2 KiB against a 1 KiB cap | refused **before spawning** (< 100 ms) | `content_over_cap` | yes |
| child allocating 512 MB against a 96 MB residency cap | killed | `parser_memory` | yes |
| child allocating 2 GB against a 192 MB job cap | killed (job + derived residency cap) | `parser_memory` / `parser_crash` | yes |
| child hanging 10 s against a 250 ms timeout | killed | `parser_timeout` | yes, parses the next document |
| child exiting 3 | crash | `parser_crash` | yes |
| child writing non-frame bytes | malformed result | `parser_crash` | yes |
| result larger than the parent's 4 KiB read cap | killed, bounded read | `parser_output_cap` | yes |

The job-object + residency pair matters: a job memory limit *alone* makes the child's allocations
fail, and a Go runtime answers that by thrashing — measured here as a child sitting at exactly its
192 MB cap for 15 s without dying. The parent therefore derives a residency cap from any job cap it
sets, so a limit the parent imposes is a limit the parent enforces.

## §10 — document routing, native only

`go test . -run TestDocumentParsingIsNativeOnly` runs the shipped CLI (so a real `parse-child` is
spawned) over a two-case corpus: a real docx containing a card number, and the same text as
`text/plain`.

```
corpus=documents-native-only-v1 cases=2
native docx: degraded=false labels=payment_card
wasm docx:   degraded=true stages=parse
text path identical=true
```

Native parses the document, classifies its text and labels `payment_card`; the wasm copy degrades
with the `parse` stage named, which is what §9.1's table requires ("the extension may not parse a
document"); and the text path is still byte-identical across the two targets.

## Test suite

```
go build ./...   exit 0
go vet ./...     exit 0
gofmt -l .       (no output)
go test ./...
ok  github.com/shadow-ai-capture/device/classifier-host              6.1s
ok  .../classify          1.0s
ok  .../model             0.3s
ok  .../norm              0.2s
ok  .../parser            0.5s
ok  .../parser/isolation  1.5s
ok  .../release           0.6s
ok  .../rules             0.3s
ok  .../validators        0.2s
```

Cross-compilation (ADR 0016's target list), all exit 0: `windows/amd64`, `windows/arm64`,
`darwin/amd64`, `darwin/arm64`, `linux/amd64`, plus `js/wasm`.

### Not verified

- **`go test -race`**: `-race` requires cgo and this host has no C compiler (`gcc` not found).
  Concurrency is covered by `TestClassifyIsSafeForConcurrentUse` (16 goroutines × 50
  classifications, every response validated) and by the bounded-fan-out test, but that is not the
  race detector.
- **macOS residency monitoring**: no `/proc`, no libproc binding in the standard library; the parser child there is
  bounded by timeout and hard kill only, and the result says the memory cap is not active
  (README open decision 8).
- **Windows named-pipe transport**: not implemented; Windows serves over stdio
  (README open decision 7).
- **Full NFKC normalisation**: not in the stdlib-only, `GOPROXY=off` build (README open decision 1).
