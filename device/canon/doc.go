// Package canon is the Go half of the `sac-canon-1` canonicalisation contract
// (docs/02-ingest-and-transport.md §4.2). It implements step C3: Unicode NFC.
//
// # Why this package exists
//
// C3 is NFC, explicitly not NFKC or NFKD, and §4 requires the derivation to be identical in
// a Go implementation and a JavaScript one. Skipping it fails silently: the same text in
// precomposed and decomposed form produces two different content_digest values, which feeds
// dedup_key, which means either two rows for one submission or a Tier-T key two routes can
// never agree on. Go's standard library has no normalisation and golang.org/x/text cannot be
// fetched on this offline host, so the tables here are generated from the JavaScript side of
// that same contract — Node with full ICU — and then checked against it.
//
// # Provenance, and why the tables are trustworthy
//
// gen/gen_tables.mjs derives every table by *observing* String.prototype.normalize on this
// host's ICU and re-checks each derivation against the same oracle before writing it out:
//
//   - canonical decompositions: NFD of every code point in the code space, stored fully
//     decomposed so decomposition needs no recursion;
//   - canonical combining classes: Node exposes no CCC property, so the class order is
//     observed through canonical ordering itself, and the resulting 964 entries in 55 classes
//     are verified exhaustively — all 464,166 pairs — against ICU;
//   - composition pairs and exclusions: derived from the decomposition chains, so an
//     exclusion (a singleton like U+2126, a script-specific case like U+0958, a non-starter
//     decomposition) contributes no pair rather than being transcribed from a list;
//   - the quick-check properties, cross-checked so that the two independent derivations of
//     "characters NFC changes" agree exactly.
//
// The committed tables record the Unicode version they came from (17.0, ICU 78.2). A table
// generated from a different version is not interchangeable with this one, which is why the
// version is part of the API and why the test suite fails if the local Node disagrees with
// the recorded oracle.
//
// # What the tests prove
//
// The corpus in testdata/ carries 3,376 cases and the expected output of Node for every one
// of them; TestCorpusAgainstGolden compares byte for byte, and TestNodeEquivalenceLive
// re-runs Node at test time to prove the committed expectations are still what the JavaScript
// side of the contract produces. TestExhaustiveSingleCodePointDigest compares Go and Node
// over *every* code point in the code space in one hash. Idempotence and the digest golden
// values are covered too, because those are the properties dedup_key actually depends on.
//
// # Scope, stated honestly
//
// The corpus covers every canonical decomposition mapping and every composition pair in the
// tables, the exhaustive pairwise combining-class check, all Hangul boundary shapes, and
// seeded multi-mark sequences on top of starters from Latin, Greek, Cyrillic, Hebrew, Arabic,
// Devanagari, Bengali, Thai, Tibetan, Kana, CJK and Hangul. What it does not cover is text
// that is canonically indecomposable and has no combining marks — CJK, emoji, most of the
// supplementary planes — because those are NFC-invariant; they are covered only as "already
// NFC", which the exhaustive code-point digest does check. The full coverage table is
// generated into testdata/meta.json rather than asserted by hand.
//
// # Invalid input
//
// Bytes that are not valid UTF-8 are an error, never a silent pass-through. §4.2 step C2
// replaces ill-formed sequences with U+FFFD, and that is deliberately the caller's decision:
// this package refuses to guess which of two different character sequences the caller meant.
package canon
