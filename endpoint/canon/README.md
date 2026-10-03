# canon — Unicode NFC for the `sac-canon-1` contract

This package implements step **C3** of the dedup contract ([docs/02 §4.2](../../docs/02-ingest-and-transport.md)):
Unicode **NFC**, explicitly not NFKC or NFKD.

It exists because C3 fails silently when it is missing. The same text in precomposed and decomposed
form produces two different `content_digest` values, the digest feeds `dedup_key`, and the result is
either two rows for one submission or a Tier-T key two collection routes can never agree on. Go's
standard library has no Unicode normalisation and `golang.org/x/text` cannot be fetched on this
offline host, so the tables are generated from the JavaScript side of the same contract — Node with
full ICU — and then checked against it.

## What it is

`nfc.go` holds the algorithm: strict UTF-8 decode, full canonical decomposition, canonical ordering,
canonical composition, algorithmic Hangul composition, and a conservative quick check. The public
API is `NFC`, `IsNFC`, `NFCString`, `Normalizer`, `Canonical`, `Version`, `Contract` and
`ErrInvalidUTF8`. `tables.go` is generated and emitted `gofmt`-clean.

`capture-core` wires it in as the pipeline's `dedup.Normalizer` (`cmd/capture-core/service.go`). The
pipeline has no default: with no normaliser installed it takes the degraded path rather than passing
an unnormalised digest off as canonical.

## Provenance of the tables

Nothing here is transcribed from a Unicode data file. Every table is derived by *observing*
`String.prototype.normalize` on this host and re-checked against the same oracle before it is
written; the generator exits non-zero if a check fails. The full method is in
[gen/README.md](gen/README.md).

| Table | How it was obtained | Size |
|---|---|---|
| canonical decompositions | `NFD` of every code point; stored fully decomposed, so the Go side needs one lookup and no recursion | 13,253 mappings |
| combining classes | class *order* observed through canonical ordering, because Node exposes no CCC property; all 464,166 pairs re-verified against ICU | 964 code points in 55 classes |
| composition pairs and exclusions | derived from the decomposition chains, so singletons and script-specific exclusions fall out of the data rather than being transcribed | 961 pairs, 1,120 exclusions |
| quick-check properties | `No` = `NFC(c) !== c`; `Maybe` = composition-pair seconds plus Hangul V and T | 73 `No`, 49 `Maybe` ranges |

The committed tables record the Unicode version they came from — **17.0**, ICU 78.2 — and that
version is part of the API. A table from a different version is not interchangeable with this one.

## Regenerating

```sh
# from endpoint/canon
node gen/gen_tables.mjs           # regenerate tables.go and testdata/
node gen/gen_tables.mjs --check   # fail if the committed files are stale
go test ./...
```

`go test ./...` runs the generator's `--check` mode itself and calls `gen/nfc_oracle.mjs` live, so a
stale table or a Node whose Unicode version has moved is a test failure rather than silent drift.

## What the tests prove

The corpus in `testdata/` carries 3,376 named cases with Node's expected output for every one.
`TestCorpusAgainstGolden` compares byte for byte, `TestNodeEquivalenceLive` re-runs Node at test time,
and `TestExhaustiveSingleCodePointDigest` compares Go and Node over every code point in the code
space in one hash. Idempotence and the digest golden values are covered too, because those are the
properties `dedup_key` actually depends on. Full evidence: [EVIDENCE.md](EVIDENCE.md)
(17 test functions, 6 benchmarks).

## What it deliberately does not do

- **Invalid UTF-8 is an error, never a silent pass-through.** C2 replaces ill-formed sequences with
  U+FFFD and that is deliberately the caller's decision; this package refuses to guess which of two
  character sequences was meant.
- **No NFKC or NFKD.** C3 is NFC; case folding, width folding and compatibility mappings belong to
  the classifier's own normalisation, not to the dedup contract.
- **No coverage claim it cannot back.** Text that is canonically indecomposable and carries no
  combining marks — CJK, emoji, most supplementary planes — is NFC-invariant and is covered only as
  "already NFC", which the exhaustive code-point digest does check. The honest coverage table is
  generated into `testdata/meta.json` rather than asserted by hand.
