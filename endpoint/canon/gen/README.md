# Generating the NFC tables

`gen_tables.mjs` writes `tables.go` and everything under `testdata/`. Nothing in the package
is transcribed from a Unicode data file: the host is offline and `golang.org/x/text` cannot be
fetched, so every table is **derived by observing `String.prototype.normalize`** on this
host's Node (which has full ICU) and then **re-checked against the same oracle** before it is
written out.

```sh
# from endpoint/canon
node gen/gen_tables.mjs           # regenerate tables.go and testdata/
node gen/gen_tables.mjs --check   # fail if the committed files are not what the generator produces
node gen/nfc_oracle.mjs testdata/corpus.json   # the JavaScript half of the equivalence check
```

`go test ./...` runs the generator's `--check` mode itself (`TestGeneratorIsReproducible`),
and calls `nfc_oracle.mjs` live (`TestNodeEquivalenceLive`), so a stale table or a Node whose
Unicode version has moved is a test failure rather than a silent drift.

## What it produced

| | |
|---|---|
| Node / ICU / Unicode | `node 22.23.1`, ICU `78.2`, Unicode `17.0` |
| canonical decompositions | 13,253 (11,172 Hangul syllables excluded — algorithmic) |
| combining classes | 964 code points in **55 classes** |
| composition pairs | 961 |
| composition exclusions | 1,120 |
| quick-check ranges | 73 `No`, 49 `Maybe` |
| corpus cases | 3,376 (2,061 of which change under NFC) |

The 55 classes are worth noting: the UCD defines exactly 55 distinct non-zero canonical
combining class values, so a derivation that produced a different count would be visibly
wrong.

## How each table is derived

**Decompositions.** `NFD` of every code point in the code space. The *full* decomposition is
stored, so the Go side needs one lookup and no recursion.

**Combining classes.** Node exposes no CCC property, and importing one from elsewhere would
mix Unicode versions (Python's `unicodedata` on this host is Unicode 14.0). Instead the class
*order* is observed through canonical ordering, which is the only thing UAX #15 uses it for:

- a code point `c` is a non-starter exactly when `NFD(H + c + L) !== H + c + L`, where `H` and
  `L` are two non-starters of different class: a starter would terminate the combining
  sequence and block all reordering, a non-starter cannot;
- two non-starters compare by canonical order: `ccc(a) < ccc(b)` iff `NFD(a+b) === a+b` and
  `NFD(b+a) !== b+a`;
- sorting with that comparator yields the classes, and the generator then verifies **all
  464,166 pairs** against ICU before writing anything.

The Go table stores a *rank* (1..55) rather than the UCD's numeric value. That is sufficient
and is not an approximation: canonical ordering is a stable sort by class, and the blocking
rule compares two classes, so only order and equality are ever consulted.

**Composition pairs and exclusions.** For a character whose full decomposition is `d1..dk`,
the chain `NFC(d1+d2)`, `NFC(c2+d3)`, … either ends at the character — in which case each step
is a composition pair — or it does not, in which case the character is excluded from
composition. That is how singletons (U+2126), script-specific exclusions (U+0958) and
non-starter decompositions fall out of the data instead of being transcribed from a list. The
two independent derivations of "characters NFC changes" (this one, and `NFC(c) !== c` from the
oracle) are required to agree exactly.

**Quick-check properties.** `NFC_QC=No` is `NFC(c) !== c`. `NFC_QC=Maybe` is the set of
characters that can combine with a preceding one: the second element of every composition
pair, plus the Hangul V and T jamo. The Go quick check combines these with a canonical-order
test; it is conservative, so a false result means "definitely already NFC".

## What it generates besides the tables

- `testdata/corpus.json` — 3,376 named cases: every decomposition mapping, the Hangul
  boundary shapes including the ones that must not compose, combining-class reordering
  (including equal classes, which must stay in input order), composition exclusions, 1,200
  seeded multi-mark sequences, a deterministic sweep of the 11,172 Hangul syllables, and
  realistic text in several scripts.
- `testdata/corpus.expected.json` — Node's NFC for each case, so the corpus is checkable
  without Node.
- `testdata/meta.json` — versions, table sizes, coverage, the exhaustive single-code-point
  digest, the 256-code-point pair set and its 65,536-pair digest, and the golden digests that
  prove the two spellings of a text hash to the same value.

The generator is deterministic: the same Node and ICU produce the same files byte for byte,
which is what `--check` relies on. `tables.go` is emitted already `gofmt`-clean, so it can be
compared without a formatter in the loop.
