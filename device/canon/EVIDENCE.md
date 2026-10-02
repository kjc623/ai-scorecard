# E5 — Pure-Go NFC normaliser: evidence

Task: `task-15` (docs/02-ingest-and-transport.md §4.2 step C3, contract `sac-canon-1`).
Owner: spool-dev. Write scope: `device/canon/` only. `docs/` was not touched.

Everything below was run on this host (Windows, Go 1.27.0, Node v22.23.1, `GOPROXY=off`).

---

## 1. What was delivered

A pure-Go NFC normaliser, one module, stdlib only, generated from and verified against the
JavaScript side of the same contract.

| File | What it is |
|---|---|
| `nfc.go` | The algorithm: strict UTF-8 decode, full canonical decomposition, canonical ordering, canonical composition, Hangul algorithmic composition, and a conservative quick check. Public API: `NFC`, `IsNFC`, `NFCString`, `Normalizer`, `Canonical`, `Version`, `Contract`, `ErrInvalidUTF8`. |
| `tables.go` | Generated: decompositions, combining-class ranks, composition pairs, quick-check ranges. `gofmt`-clean as emitted. |
| `doc.go` | Package documentation: why it exists, provenance, what the tests prove, honest scope. |
| `gen/gen_tables.mjs` | The generator. Derives every table from Node's ICU and re-checks each derivation before writing. `--check` fails if the committed files are stale. |
| `gen/nfc_oracle.mjs` | The JavaScript half of the equivalence check, called at test time. |
| `gen/README.md` | How to regenerate, and how each table is derived. |
| `testdata/corpus.json` | 3,376 named conformance cases. |
| `testdata/corpus.expected.json` | Node's NFC for each case, so the corpus is checkable without Node. |
| `testdata/meta.json` | Versions, table sizes, coverage, and the digests the Go tests compare against. |
| `canon_test.go`, `bench_test.go` | 17 test functions and 6 benchmarks. |

**Unicode version: 17.0** (ICU 78.2, node 22.23.1). Recorded in the tables, in
`testdata/meta.json`, exposed as `Version()`, and asserted in tests — a table built from a
different version is not interchangeable with this one, and the suite fails if the local Node
disagrees with the recorded oracle.

## 2. How the tables were obtained without network access

Nothing is transcribed from a Unicode data file. Every table is derived by *observing*
`String.prototype.normalize` on this host, and every derivation is re-checked against the
same oracle before it is written; the generator exits non-zero if any check fails.

- **Decompositions** — `NFD` of all 1,112,064 code points; the full decomposition is stored,
  so the Go side needs one lookup and no recursion. 13,253 mappings, of which 11,172 are
  Hangul syllables and are deliberately absent because Hangul is algorithmic.
- **Combining classes** — Node exposes no CCC property, and the Python on this host has
  Unicode 14.0 while Node has 17.0, so importing CCC values would silently mix versions.
  Instead the class *order* is observed through canonical ordering, which is the only thing
  UAX #15 uses it for: `c` is a non-starter exactly when `NFD(H + c + L) !== H + c + L` for
  two non-starters `H`, `L` of different class (a starter would terminate the combining
  sequence and block all reordering). Two non-starters then compare by `NFD(a+b) === a+b`.
  Result: **964 code points in 55 classes**, with **all 464,166 pairs** re-verified against
  ICU. The UCD defines exactly 55 distinct non-zero CCC values, so a miscount would be
  visible. The Go table stores the class *rank*, not the UCD number, which is sufficient and
  not an approximation: canonical ordering is a stable sort by class and the blocking rule
  compares two classes, so only order and equality are ever consulted.
- **Composition pairs and exclusions** — derived from the decomposition chains: a character
  whose chain composes back contributes pairs, and one whose chain does not is excluded.
  Singletons (U+2126), script-specific exclusions (U+0958) and non-starter decompositions
  therefore fall out of the data instead of being transcribed. 961 pairs, 1,120 exclusions,
  and the two independent derivations of "characters NFC changes" are required to agree
  exactly.
- **Quick-check properties** — `No` = `NFC(c) !== c` (1,120 code points in 73 ranges);
  `Maybe` = second elements of composition pairs plus the Hangul V and T jamo (49 ranges).
  They are asserted disjoint.

## 3. Exact commands and raw output

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
```

Run in `device/canon`; raw captures in `build-output.txt`, `test-output.txt`,
`bench-output.txt`.

```
=== gofmt -l . (empty means formatted) ===
=== go build ./... ===
build exit=0
=== go vet ./... ===
vet exit=0
=== node gen/gen_tables.mjs ===
node 22.23.1, ICU 78.2, Unicode 17.0
decomposition mappings : 13253 (of which Hangul excluded: 11172)
combining-class entries: 964 in 55 classes
composition pairs      : 961
composition exclusions : 1120
quick-check ranges     : No=73 Maybe=49
pairwise CCC checks    : 464166
corpus cases           : 3376 (2061 change under NFC)
single-code-point hash : 1234f515bdf9ca19158ad72c132ca78d4629facc5674a7953d1db33fe11a9e23
coverage               : 3162 distinct code points, 33 named ranges
=== node gen/gen_tables.mjs --check ===
generated files are current
```

```
> go test -v -count=1 ./...      →  ok  github.com/shadow-ai-capture/device/canon  1.749s
```

```
=== RUN   TestCorpusAgainstGolden
    canon_test.go:139: 3376 corpus cases, 2061 of them changed by NFC
--- PASS: TestCorpusAgainstGolden (0.01s)
=== RUN   TestIdempotence
--- PASS: TestIdempotence (0.01s)
=== RUN   TestNodeEquivalenceLive
    canon_test.go:215: go == node 22.23.1 (ICU 78.2, Unicode 17.0) over 3376 cases, byte for byte
--- PASS: TestNodeEquivalenceLive (0.07s)
=== RUN   TestExhaustiveSingleCodePointDigest
    canon_test.go:243: all 1,112,064 code points agree with node: 1234f515bdf9ca19158ad72c132ca78d4629facc5674a7953d1db33fe11a9e23
--- PASS: TestExhaustiveSingleCodePointDigest (0.05s)
=== RUN   TestPairCrossProductDigest
    canon_test.go:279: 65536 ordered pairs agree with node
--- PASS: TestPairCrossProductDigest (0.02s)
=== RUN   TestAllNonStarterPairsDigest
    canon_test.go:311: all 929296 ordered non-starter pairs agree with node
--- PASS: TestAllNonStarterPairsDigest (0.19s)
=== RUN   TestGoldenDigestPrecomposedVsDecomposed
    canon_test.go:339: 5 digest pairs agree across spellings and with node
--- PASS: TestGoldenDigestPrecomposedVsDecomposed (0.00s)
=== RUN   TestInvalidUTF8IsAnErrorNotAPassThrough
    --- PASS: TestInvalidUTF8IsAnErrorNotAPassThrough (0.00s)   [6 subtests]
=== RUN   TestStringAdapterOnInvalidUTF8ReturnsInputUnchanged
--- PASS: TestStringAdapterOnInvalidUTF8ReturnsInputUnchanged (0.00s)
=== RUN   TestNormalizerAdapterNormalises
--- PASS: TestNormalizerAdapterNormalises (0.00s)
=== RUN   TestIsNFC
--- PASS: TestIsNFC (0.00s)
=== RUN   TestCapabilityIdentifiers
    canon_test.go:430: tables: 17.0, ICU 78.2, node 22.23.1; 13253 decomposition mappings, 964 combining-class entries, 961 composition pairs, 1120 exclusions
--- PASS: TestCapabilityIdentifiers (0.00s)
=== RUN   TestQuickCheckDoesNotSkipWork
    canon_test.go:456: 961 canonical composition pairs compose through the public API
--- PASS: TestQuickCheckDoesNotSkipWork (0.00s)
=== RUN   TestHangulIsAlgorithmic
--- PASS: TestHangulIsAlgorithmic (0.00s)
=== RUN   TestCompositionExclusionsDoNotRecompose
    canon_test.go:519: 1120 composition exclusions stay decomposed and are idempotent
--- PASS: TestCompositionExclusionsDoNotRecompose (0.00s)
=== RUN   TestAdversarialMarkRun
--- PASS: TestAdversarialMarkRun (0.00s)
=== RUN   TestGeneratorIsReproducible
    canon_test.go:568: generated files are current
--- PASS: TestGeneratorIsReproducible (1.04s)
PASS
ok  	github.com/shadow-ai-capture/device/canon	1.749s
```

## 4. The equivalence evidence, and why it can fail

Four independent comparisons against Node, all of them executable here:

1. **Every corpus case, byte for byte, live** (`TestNodeEquivalenceLive`): Node is invoked at
   test time through `gen/nfc_oracle.mjs`; Go and Node must agree on all 3,376 cases, and the
   committed expectations must still be what this Node produces. A Node with a different
   Unicode version fails the test rather than being skipped past.
2. **Every code point, exhaustively** (`TestExhaustiveSingleCodePointDigest`): both
   implementations hash NFC of all 1,112,064 code points (surrogates excepted, since they
   cannot appear in UTF-8) and the hashes must match.
3. **Every ordered pair of non-starters** (`TestAllNonStarterPairsDigest`): 929,296 pairs,
   hashed by both sides. This is the exhaustive check on the one table that is derived rather
   than transcribed — a single misclassified combining class fails here even if no corpus case
   happens to exercise that pair.
4. **A 256-code-point cross-product** (`TestPairCrossProductDigest`): 65,536 ordered pairs
   drawn by deterministic stride from every code point the algorithm can react to, so ordered
   pairs are covered by construction rather than by random sampling.

Plus table-level checks that do not need Node: all 961 composition pairs compose through the
public API (which also proves the quick check never waves them through), all 1,120 excluded
code points stay decomposed and are idempotent, all 11,172 Hangul syllables are NFC and
round-trip through the algorithmic path, and a 20,000-mark adversarial run comes out in
canonical order.

**The tests can fail.** Three mutations were applied and reverted on this host:

| Mutation | Tests that caught it |
|---|---|
| Composition blocking rule removed (`blocked := false`) | `TestCorpusAgainstGolden`, `TestNodeEquivalenceLive`, `TestAdversarialMarkRun` |
| Quick check disabled (`needsWork` returns false) | 8 tests, including all four equivalence checks |
| Combining classes all report as starters (`combiningRank` returns 0) | `TestCorpusAgainstGolden`, `TestNodeEquivalenceLive`, `TestPairCrossProductDigest` |

Each mutation was reverted and the suite re-run green, with `gofmt -l` empty.

## 5. Corpus and coverage

3,376 cases: every canonical decomposition mapping in the tables (2,081 non-Hangul), a
deterministic sweep of the 11,172 algorithmic Hangul syllables, every Hangul boundary shape
including the ones that must **not** compose (two trailing jamo; two vowels; vowel then
trailing; two leading jamo; trailing then vowel; L+V+T+trailing; blocked by an intervening
mark; L+mark+V+T), combining-class reordering including equal classes that must stay in input
order, all composition exclusions, 1,200 seeded multi-mark sequences, and realistic text.

| Covered by named script ranges | Code points |
|---|---|
| Latin (incl. Extended Additional) | 515 |
| Greek (incl. Extended) | 267 |
| Arabic | 65 |
| Combining Diacritical Supplement | 64 |
| Kana | 63 |
| Cyrillic | 57 |
| Hebrew | 56 |
| Hangul (syllables + jamo) | 51 |
| Musical Symbols | 42 |
| Tibetan | 37 |
| Syriac | 27 |
| Combining Diacritical Marks for Symbols | 26 |
| Devanagari | 19 |
| Thai | 16 |
| Bengali, Tamil | 9 each |
| Gurmukhi | 8 |
| Oriya, Kannada, Malayalam, Lao, Adlam | 7 each |
| Telugu, Sinhala, Myanmar | 5 each |
| CJK | 4 |
| Ethiopic | 3 |
| Gujarati, Khmer | 2 each |
| Mongolian | 1 |

**What is not covered.** 1,769 of the 3,162 distinct code points in the corpus fall outside
the named ranges above; they are still present in the corpus (every decomposition mapping is
included, named or not), but the table above names blocks rather than claiming script
knowledge. Beyond that, the corpus is deliberately not a general Unicode text corpus: text
that is canonically indecomposable and carries no combining marks — CJK, emoji, most of the
supplementary planes — is **not** exercised as a transformation, because it is NFC-invariant.
It is covered only at the "already NFC" level, which the exhaustive code-point digest does
check. There is also no claim about unpaired UTF-16 surrogates: they cannot be represented in
Go's UTF-8 input, and the byte API rejects the CESU-8/WTF-8 encodings of them as invalid.

## 6. Performance

`go test -run xxx -bench . -benchmem -benchtime 300ms`, raw output in `bench-output.txt`:

| Benchmark | ns/op | Throughput | Allocs |
|---|---|---|---|
| **1 KB prompt, already NFC** | **2,894** (2.9 µs) | 354 MB/s | 0 |
| 1 KB prompt, decomposed (marks throughout) | 44,487 (44 µs) | 23 MB/s | 2 |
| **64 KB document, already NFC** | **344,456** (0.34 ms) | 190 MB/s | 0 |
| 64 KB document, decomposed | 2,751,192 (2.75 ms) | 24 MB/s | 2 |
| 64 KB of combining marks in descending class order (adversarial) | 2,296,277 (2.3 ms) | 43 MB/s | 3 |
| 1 KB pure ASCII | 740 | 1.4 GB/s | 0 |

The worst case measured — a 64 KB document where every few characters carry a combining
mark — is 2.75 ms, **0.9% of the classifier's 300 ms interactive budget**. The common case,
text that is already NFC, allocates nothing because the quick check returns the input slice
unchanged (documented: callers that intend to mutate the result must copy). The adversarial
mark run uses the counting-sort path, so reordering cannot be turned into quadratic work.

Note the honest caveat: these numbers are for one machine (Ryzen 9 5900XT, windows/amd64) and
`go test` microbenchmarks. Nothing here has been measured on an endpoint under a real proxy
load.

## 7. Integration note for capture-core

`device/capture-core/dedup.Normalizer` is declared as:

```go
type Normalizer interface { NFC(string) string }
```

That is a **string-shaped interface with no error channel**, whereas this task specifies
`NFC([]byte) ([]byte, error)` with invalid UTF-8 as an error. Both are provided:

- `canon.NFC([]byte) ([]byte, error)` — the strict API, returns `ErrInvalidUTF8` and a nil
  result for ill-formed input. §4.2 step C2 (replace ill-formed sequences with U+FFFD) is the
  caller's decision, and this package deliberately refuses to guess which of two character
  sequences was meant.
- `canon.Normalizer` — implements `NFC(string) string`, so `p.Normalizer = canon.Normalizer{}`
  satisfies core's field directly. Because that interface cannot report an error, its
  documented behaviour on ill-formed input is to return the input unchanged: deterministic,
  identical between two routes seeing the same bytes, and never pretending a normalisation
  happened. This is the one place where the strictness rule cannot be honoured, and it is
  documented on the type rather than hidden.

`Canonical() bool` returns true and `Version()` returns `"17.0"`, so a caller can tell that a
normaliser is really installed and which Unicode version it hashed with.

## 8. What is NOT verified

- **Any script the corpus does not exercise as a transformation.** See §5: canonically
  indecomposable text (CJK, emoji, most supplementary planes) is only checked as
  NFC-invariant, not as a transformation.
- **Text with unpaired surrogates / WTF-8.** Not representable in Go's UTF-8 input; the byte
  API rejects the encodings. Node's behaviour on lone surrogates was therefore not compared —
  there is no valid UTF-8 input that reaches it.
- **Unicode versions other than 17.0.** The tables are pinned to 17.0/ICU 78.2. If the
  JavaScript implementation in the fleet ships a different ICU, the two sides are not the same
  contract; the test suite detects this locally but a fleet-wide version pin is a release
  decision, not something this package can enforce.
- **The exact NFC/NFD behaviour of a browser's ICU versus Node's.** §4 requires agreement
  between "a Go implementation and a JavaScript one". The JavaScript side verified here is
  Node 22.23.1 with ICU 78.2; Chrome, Edge and Safari ship their own ICU builds. The mechanism
  to check a browser is the same corpus (`testdata/corpus.json` plus
  `gen/nfc_oracle.mjs`), but it has not been run in a browser on this host — no browser is
  installed here.
- **End-to-end digest agreement with the extension.** This package proves NFC agrees with the
  JavaScript implementation on this host; it does not prove that the extension's C1/C2/C4–C9
  steps produce the same canonical string. That is the verifier's cross-component check, not
  this module's.
