#!/usr/bin/env node
// Generates device/canon/tables.go, the conformance corpus and the Node-side expected
// outputs, from Node's own ICU-backed Unicode data.
//
// Why this exists: docs/02-ingest-and-transport.md §4 defines `sac-canon-1` and requires the
// derivation to be identical in a Go implementation and a JavaScript one. Node 22 on this
// host has full ICU, so the JavaScript side of that contract is the oracle, and the Go side
// is generated from it rather than from a UCD download the offline host cannot perform.
//
// Nothing here is magic: every table is derived by *observing* normalize('NFC'|'NFD') on
// this host's ICU, and every derivation is re-checked against the same oracle before it is
// written out. The checks are asserts, not comments — a table that cannot be reproduced
// fails the generator.
//
// Usage:  node gen/gen_tables.mjs          (writes tables.go, testdata/*.json)
//         node gen/gen_tables.mjs --check  (verifies the committed files are current)
//
// Unicode version is whatever this Node reports (see process.versions.unicode); the
// committed tables record it, and the Go test suite fails loudly if a different Node is used
// to regenerate them.

import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, '..');
const CHECK = process.argv.includes('--check');

const nfc = (s) => s.normalize('NFC');
const nfd = (s) => s.normalize('NFD');
const cps = (s) => Array.from(s, (c) => c.codePointAt(0));
const fromCp = (cp) => String.fromCodePoint(cp);

const MAX_CP = 0x10ffff;
const isSurrogate = (cp) => cp >= 0xd800 && cp <= 0xdfff;

// Hangul, algorithmic per UAX #15 §10 (the tables must not carry 11,172 syllables).
const S_BASE = 0xac00;
const L_BASE = 0x1100;
const V_BASE = 0x1161;
const T_BASE = 0x11a7;
const L_COUNT = 19;
const V_COUNT = 21;
const T_COUNT = 28;
const N_COUNT = V_COUNT * T_COUNT; // 588
const S_COUNT = L_COUNT * N_COUNT; // 11172
const isHangulSyllable = (cp) => cp >= S_BASE && cp < S_BASE + S_COUNT;

const failures = [];
function check(cond, message) {
  if (!cond) failures.push(message);
}

// ---------------------------------------------------------------------------------------
// Pass 1 — decomposition, NFC self-stability, NFD stability.
// ---------------------------------------------------------------------------------------
function passDecompositions() {
  const decomp = new Map(); // cp -> NFD string, only when NFD(s) !== s
  const nfdStable = new Uint8Array(MAX_CP + 1);
  const nfcSelf = new Uint8Array(MAX_CP + 1); // 1 when NFC(s) === s
  for (let cp = 0; cp <= MAX_CP; cp++) {
    if (isSurrogate(cp)) continue;
    const s = fromCp(cp);
    const d = nfd(s);
    const n = nfc(s);
    if (d !== s) decomp.set(cp, d);
    else nfdStable[cp] = 1;
    if (n === s) nfcSelf[cp] = 1;
  }
  return { decomp, nfdStable, nfcSelf };
}

// ---------------------------------------------------------------------------------------
// Pass 2 — canonical combining classes, by observation.
//
// Node exposes no CCC property, and Python's unicodedata on this host is Unicode 14.0 while
// Node is 17.0, so importing CCC values would silently mix versions. The class *order* is
// therefore observed directly from canonical ordering, which is the only thing UAX #15 uses
// the classes for.
//
// Two probes, both non-starters with different classes:
//   PROBE_HIGH = U+0345 COMBINING GREEK YPOGEGRAMMENI
//   PROBE_LOW  = U+0334 COMBINING TILDE OVERLAY
// The generator asserts against ICU that they really are ordered (NFD(H+L) === L+H), so a
// Unicode version that changed them stops the generator rather than silently misclassifying.
//
// A code point c is a non-starter exactly when NFD(H + c + L) !== H + c + L:
//   * if c is a starter it terminates the combining sequence, so all three runs have one
//     element and nothing can reorder — the string is unchanged;
//   * if c is a non-starter the three form one run, and since ccc(H) > ccc(L) no input
//     order [H, c, L] can already be sorted, so something moves.
// That is exact for every class value, including c equal to one of the probes.
//
// Only the ordering and equality of classes matter to the algorithm: canonical ordering is a
// stable sort by class, and the blocking rule compares two classes. The table therefore
// stores a *rank*, and pass 4 proves exhaustively that the ranks reproduce ICU's ordering
// for every pair of non-starters.
// ---------------------------------------------------------------------------------------
const PROBE_HIGH = 0x0345;
const PROBE_LOW = 0x0334;

function passCombiningClasses(nfdStable) {
  const H = fromCp(PROBE_HIGH);
  const L = fromCp(PROBE_LOW);
  // The probes must be ordered, or the sandwich test classifies nothing correctly.
  check(nfd(H + L) === L + H, `probe U+${PROBE_HIGH.toString(16)} does not sort after U+${PROBE_LOW.toString(16)} under ICU`);
  check(nfd(L + H) === L + H, `probe U+${PROBE_LOW.toString(16)} is not already ordered before U+${PROBE_HIGH.toString(16)} under ICU`);
  check(nfd(H) === H && nfd(L) === L, 'a probe is not NFD-stable');

  const candidates = [];
  for (let cp = 0; cp <= MAX_CP; cp++) {
    if (isSurrogate(cp) || !nfdStable[cp]) continue;
    const s = fromCp(cp);
    if (nfd(H + s + L) !== H + s + L) candidates.push(cp);
  }
  check(
    candidates.includes(PROBE_LOW) && candidates.includes(PROBE_HIGH),
    'probe characters were not classified as non-starters',
  );

  // Comparator from the oracle: ccc(a) < ccc(b) iff a + b is already in canonical order and
  // b + a is not.
  const cmp = (a, b) => {
    if (a === b) return 0;
    const ab = fromCp(a) + fromCp(b);
    const ba = fromCp(b) + fromCp(a);
    const abSorted = nfd(ab) === ab;
    const baSorted = nfd(ba) === ba;
    if (abSorted && baSorted) return 0; // equal class: neither order is swapped
    if (abSorted && !baSorted) return -1;
    if (!abSorted && baSorted) return 1;
    check(false, `inconsistent ordering for U+${a.toString(16)} / U+${b.toString(16)}`);
    return 0;
  };

  const sorted = [...candidates].sort(cmp);
  const ranks = new Map();
  let rank = 0;
  for (let i = 0; i < sorted.length; i++) {
    if (i > 0 && cmp(sorted[i - 1], sorted[i]) !== 0) rank++;
    ranks.set(sorted[i], rank + 1); // 1-based; 0 means "starter"
  }
  return { ranks, sorted, cmp };
}

// Pass 4 — exhaustive pairwise verification of the ranks against ICU.
function verifyCombiningClasses(sorted, cmp, ranks) {
  let pairs = 0;
  for (let i = 0; i < sorted.length; i++) {
    for (let j = i + 1; j < sorted.length; j++) {
      const a = sorted[i];
      const b = sorted[j];
      const order = cmp(a, b);
      check(order <= 0, `rank order disagrees with ICU for U+${a.toString(16)} / U+${b.toString(16)}`);
      const ab = fromCp(a) + fromCp(b);
      if (ranks.get(a) < ranks.get(b)) {
        check(nfd(ab) === ab, `SWAP: lower-rank U+${a.toString(16)} was reordered against U+${b.toString(16)}`);
      } else {
        check(nfd(ab) === ab && nfd(fromCp(b) + fromCp(a)) === fromCp(b) + fromCp(a),
          `EQUAL-RANK pair U+${a.toString(16)} / U+${b.toString(16)} was reordered`);
      }
      pairs++;
    }
  }
  return pairs;
}

// ---------------------------------------------------------------------------------------
// Pass 5 — canonical composition pairs, from the decomposition chains.
//
// A canonical composition pair exists exactly where a character's canonical decomposition
// composes back. For a character p whose full decomposition is d1..dk, the chain is
// NFC(d1 + d2) -> c2, NFC(c2 + d3) -> c3, ... and p is composable iff the chain ends at p.
// A character in the composition-exclusion list (singletons like U+2126, script-specific
// exclusions like U+0958, non-starter decompositions) breaks the chain and contributes no
// pairs — which is how exclusions are derived rather than transcribed.
// ---------------------------------------------------------------------------------------
function passCompositionPairs(decomp) {
  const pairs = new Map(); // (first<<21|second) -> composed
  const excluded = [];
  for (const [cp, d] of decomp) {
    if (isHangulSyllable(cp)) continue; // algorithmic
    const parts = cps(d);
    if (parts.length < 2) {
      excluded.push(cp); // singleton decomposition
      continue;
    }
    let cur = parts[0];
    let broken = false;
    const chain = [];
    for (let i = 1; i < parts.length; i++) {
      const next = nfc(fromCp(cur) + fromCp(parts[i]));
      const nextCps = cps(next);
      if (nextCps.length !== 1) {
        broken = true;
        break;
      }
      chain.push([cur, parts[i], nextCps[0]]);
      cur = nextCps[0];
    }
    if (broken || cur !== cp) {
      excluded.push(cp);
      continue;
    }
    for (const [a, b, c] of chain) {
      // A pair must be reachable: composing the two pieces in isolation yields the composed
      // character. This is asserted against ICU, not assumed.
      check(nfc(fromCp(a) + fromCp(b)) === fromCp(c),
        `pair U+${a.toString(16)} + U+${b.toString(16)} does not compose to U+${c.toString(16)} under ICU`);
      pairs.set((a << 21) | b, c);
    }
  }
  return { pairs, excluded };
}

// ---------------------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------------------
const SCRIPT_RANGES = [
  ['Latin', 0x0041, 0x024f],
  ['Latin-Extended-Additional', 0x1e00, 0x1eff],
  ['Greek', 0x0370, 0x03ff],
  ['Greek-Extended', 0x1f00, 0x1fff],
  ['Cyrillic', 0x0400, 0x04ff],
  ['Hebrew', 0x0590, 0x05ff],
  ['Arabic', 0x0600, 0x06ff],
  ['Syriac', 0x0700, 0x074f],
  ['Devanagari', 0x0900, 0x097f],
  ['Bengali', 0x0980, 0x09ff],
  ['Gurmukhi', 0x0a00, 0x0a7f],
  ['Gujarati', 0x0a80, 0x0aff],
  ['Oriya', 0x0b00, 0x0b7f],
  ['Tamil', 0x0b80, 0x0bff],
  ['Telugu', 0x0c00, 0x0c7f],
  ['Kannada', 0x0c80, 0x0cff],
  ['Malayalam', 0x0d00, 0x0d7f],
  ['Sinhala', 0x0d80, 0x0dff],
  ['Thai', 0x0e00, 0x0e7f],
  ['Lao', 0x0e80, 0x0eff],
  ['Tibetan', 0x0f00, 0x0fff],
  ['Myanmar', 0x1000, 0x109f],
  ['Georgian', 0x10a0, 0x10ff],
  ['Hangul-Jamo', 0x1100, 0x11ff],
  ['Ethiopic', 0x1200, 0x137f],
  ['Khmer', 0x1780, 0x17ff],
  ['Mongolian', 0x1800, 0x18af],
  ['Latin-Extended-Additional2', 0x1e00, 0x1eff],
  ['Combining-Diacritical-Supplement', 0x1dc0, 0x1dff],
  ['Combining-Diacritical-Marks-for-Symbols', 0x20d0, 0x20ff],
  ['Kana', 0x3040, 0x30ff],
  ['CJK', 0x4e00, 0x9fff],
  ['Hangul-Syllables', 0xac00, 0xd7a3],
  ['Musical-Symbols', 0x1d100, 0x1d1ff],
  ['Math-Alphanumeric', 0x1d400, 0x1d7ff],
  ['Adlam', 0x1e900, 0x1e95f],
];

function scriptOf(cp) {
  for (const [name, lo, hi] of SCRIPT_RANGES) {
    if (cp >= lo && cp <= hi) return name;
  }
  return null;
}

function buildCorpus({ decomp, nfdStable, ranks, pairs, excluded }) {
  const cases = [];
  const add = (name, note, input) => cases.push({ name, note, in: input });

  // (a) Every canonical decomposition mapping. For non-excluded characters NFC is the
  // identity here (decompose then recompose); for excluded ones it is not.
  let decompCases = 0;
  for (const [cp] of decomp) {
    if (isHangulSyllable(cp)) continue;
    const n = nfc(fromCp(cp));
    const note = n === fromCp(cp) ? 'decomposition mapping, recomposes' : 'decomposition mapping, excluded from composition';
    add(`decomp-U+${cp.toString(16).toUpperCase().padStart(4, '0')}`, note, fromCp(cp));
    decompCases++;
  }

  // (b) Hangul boundaries: composition, all three non-composing shapes, and blocked cases.
  const L = 0x1100, V = 0x1161, T = 0x11a8; // 가, ᅡ, ᆨ
  const LVT = 0xac01;
  const hangul = [
    ['hangul-LV', 'L + V composes to an LV syllable', fromCp(L) + fromCp(V)],
    ['hangul-LVT', 'L + V + T composes to an LVT syllable', fromCp(L) + fromCp(V) + fromCp(T)],
    ['hangul-LV-precomposed-plus-T', 'precomposed LV syllable + T composes', fromCp(0xac00) + fromCp(T)],
    ['hangul-LVT-full-range', 'last LVT syllable', fromCp(0xd7a3)],
    ['hangul-first-syllable', 'first syllable, L + V', fromCp(0xac00)],
    ['hangul-not-two-trailing', 'two trailing jamo must not compose', fromCp(T) + fromCp(T)],
    ['hangul-not-two-vowels', 'two vowels must not compose', fromCp(V) + fromCp(V)],
    ['hangul-not-vowel-then-trailing', 'a vowel and a trailing jamo must not compose', fromCp(V) + fromCp(T)],
    ['hangul-not-two-leading', 'two leading jamo must not compose', fromCp(L) + fromCp(L)],
    ['hangul-not-trailing-then-vowel', 'trailing then vowel must not compose', fromCp(T) + fromCp(V)],
    ['hangul-LVT-plus-trailing', 'L+V+T+trailing: the second trailing jamo must not compose', fromCp(L) + fromCp(V) + fromCp(T) + fromCp(T)],
    ['hangul-blocked-by-mark', 'a mark between LV and T blocks the trailing composition', fromCp(0xac00) + fromCp(0x0300) + fromCp(T)],
    ['hangul-L-V-T-across-marks', 'L + mark + V + T: the mark blocks L+V', fromCp(L) + fromCp(0x0300) + fromCp(V) + fromCp(T)],
    ['hangul-compat-jamo-not-composed', 'compatibility jamo are not canonical and do not compose', fromCp(0x3131) + fromCp(0x314f)],
    ['hangul-jamo-then-syllable', 'a jamo followed by a syllable does not recompose', fromCp(T) + fromCp(LVT)],
  ];
  for (const [name, note, input] of hangul) add(name, note, input);
  // A deterministic sweep of the 11,172 algorithmic syllables, so the algorithmic path is
  // exercised across the whole range rather than at its edges only.
  for (let i = 0; i < 40; i++) {
    const cp = S_BASE + Math.floor((i * S_COUNT) / 40);
    add(`hangul-sweep-${String(i).padStart(2, '0')}`, `algorithmic syllable U+${cp.toString(16).toUpperCase()}`, fromCp(cp));
  }

  // (c) Combining-class reordering with multiple marks, including equal classes (which must
  // stay in input order) and the classic 230/220/1 ladders.
  const marks = [
    { cp: 0x0301, ccc: '230' }, { cp: 0x0300, ccc: '230' }, { cp: 0x0308, ccc: '230' },
    { cp: 0x0323, ccc: '220' }, { cp: 0x0324, ccc: '220' }, { cp: 0x0316, ccc: '220' },
    { cp: 0x0334, ccc: '1' }, { cp: 0x0335, ccc: '1' }, { cp: 0x031b, ccc: '216' },
    { cp: 0x05b0, ccc: '10' }, { cp: 0x05b7, ccc: '17' }, { cp: 0x093c, ccc: '7' },
    { cp: 0x0e38, ccc: '103' }, { cp: 0x0e48, ccc: '107' }, { cp: 0x0f71, ccc: '129' },
  ];
  const reorderCases = [
    ['reorder-acute-below', '230 before 220: input out of order, output sorted', 'a' + fromCp(0x0301) + fromCp(0x0323)],
    ['reorder-below-acute', '220 before 230: already ordered, unchanged', 'a' + fromCp(0x0323) + fromCp(0x0301)],
    ['reorder-three-ladder', '230, 220, 1 -> 1, 220, 230', 'a' + fromCp(0x0301) + fromCp(0x0323) + fromCp(0x0334)],
    ['reorder-equal-classes-are-stable', 'two 230 marks keep input order', 'a' + fromCp(0x0301) + fromCp(0x0300)],
    ['reorder-equal-classes-reversed', 'two 230 marks keep input order, reversed input', 'a' + fromCp(0x0300) + fromCp(0x0301)],
    ['reorder-hebrew', 'Hebrew points, distinct classes', fromCp(0x05d0) + fromCp(0x05b7) + fromCp(0x05b0)],
    ['reorder-thai', 'Thai vowels below/above', fromCp(0x0e01) + fromCp(0x0e48) + fromCp(0x0e38)],
    ['reorder-tibetan', 'Tibetan vowel signs 129/130', fromCp(0x0f40) + fromCp(0x0f72) + fromCp(0x0f71)],
    ['reorder-device-adversarial', 'a long run of marks in descending class order', 'a' + marks.map((m) => fromCp(m.cp)).reverse().join('')],
    ['reorder-marks-only-no-starter', 'marks with no starter still order canonically', marks.map((m) => fromCp(m.cp)).join('')],
    ['reorder-nonstarter-decomposition', 'U+0344 decomposes to two marks and then orders', 'a' + fromCp(0x0344)],
  ];
  for (const [name, note, input] of reorderCases) add(name, note, input);

  // (d) Composition exclusions.
  const exclusions = [
    ['exclusion-singleton-ohm', 'U+2126 OHM SIGN decomposes to U+03A9 and does not recompose', fromCp(0x2126)],
    ['exclusion-singleton-angstrom', 'U+212B ANGSTROM SIGN decomposes to U+00C5 and does not recompose', fromCp(0x212b)],
    ['exclusion-singleton-kelvin', 'U+212A KELVIN SIGN decomposes to U+004B', fromCp(0x212a)],
    ['exclusion-devanagari-qa', 'U+0958 is a script-specific composition exclusion', fromCp(0x0958)],
    ['exclusion-devanagari-qa-parts', 'U+0958 decomposition does not recompose', fromCp(0x0915) + fromCp(0x093c)],
    ['exclusion-bengali-rra', 'U+09DC exclusion', fromCp(0x09dc)],
    ['exclusion-bengali-condensed', 'U+09DF exclusion', fromCp(0x09df)],
    ['exclusion-oriya-dda', 'U+0B5C exclusion', fromCp(0x0b5c)],
    ['exclusion-tamil-nnna', 'U+0B94 exclusion', fromCp(0x0b94)],
    ['exclusion-malayalam', 'U+0D3A exclusion', fromCp(0x0d3a)],
    ['exclusion-hebrew-presentation', 'U+FB1D, non-starter decomposition', fromCp(0xfb1d)],
    ['exclusion-hebrew-presentation2', 'U+FB2A, non-starter decomposition', fromCp(0xfb2a)],
  ];
  for (const [name, note, input] of exclusions) add(name, note, input);

  // (e) Deterministic random multi-mark sequences: the part of the corpus that would catch a
  // wrong combining-class ordering. Seeded, so the corpus is reproducible.
  let seed = 0x5ac0a170 ^ 0x9e3779b9;
  const rnd = () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = seed;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const marksOnly = [...ranks.keys()];
  const starters = [0x0061, 0x0041, 0x0915, 0x05d0, 0x0e01, 0x0f40, 0x1100, 0xac00, 0x09c7, 0x1e0a, 0x304b];
  const randomCount = Number(process.env.CANON_RANDOM_CASES || 1200);
  for (let i = 0; i < randomCount; i++) {
    const n = 2 + Math.floor(rnd() * 5);
    let s = rnd() < 0.8 ? fromCp(starters[Math.floor(rnd() * starters.length)]) : '';
    for (let k = 0; k < n; k++) s += fromCp(marksOnly[Math.floor(rnd() * marksOnly.length)]);
    if (rnd() < 0.15) s += fromCp(L) + fromCp(V) + (rnd() < 0.5 ? fromCp(T) : '');
    add(`random-marks-${String(i).padStart(4, '0')}`, `${n} combining marks, seeded sequence`, s);
  }

  // (f) Realistic text, including the canonicalisation fixtures from docs/02 §4.
  const realistic = [
    ['text-doc-example', 'the §4.2 worked example', 'Summarise the attached contract.'],
    ['text-vietnamese-nfd', 'Vietnamese, decomposed', 'Tie\u0302\u0301ng Vie\u0323\u0302t'],
    ['text-vietnamese-nfc', 'Vietnamese, precomposed', 'Ti\u1ebfng Vi\u1ec7t'],
    ['text-greek-nfd', 'Greek with tonos and dialytika, decomposed', '\u03b1\u03b9\u0301\u03c3\u03b8\u03b7\u03c3\u03b7 \u03bc\u03b5 \u03b4\u03b9\u03b1\u03bb\u03c5\u03c4\u03b9\u03ba\u03ac'],
    ['text-greek-nfc', 'Greek, precomposed', '\u03b1\u03af\u03c3\u03b8\u03b7\u03c3\u03b7 \u03bc\u03b5 \u03b4\u03b9\u03b1\u03bb\u03c5\u03c4\u03b9\u03ba\u03ac'],
    ['text-hangul-sentence', 'Korean sentence, decomposed jamo', '\u1112\u1161\u11ab\u1100\u1173\u11af'],
    ['text-hangul-precomposed', 'Korean sentence, precomposed syllables', '\ud55c\uae00'],
    ['text-japanese-dakuten-nfd', 'Japanese with combining dakuten', '\u304b\u3099\u304f\u3099'],
    ['text-japanese-dakuten-nfc', 'Japanese with precomposed dakuten', '\u304c\u3050'],
    ['text-devanagari', 'Devanagari with a nukta', '\u0915\u093c\u093e'],
    ['text-hebrew-pointed', 'Pointed Hebrew', '\u05e9\u05c1\u05b8\u05dc\u05d5\u05b9\u05dd'],
    ['text-arabic', 'Arabic with a shadda and a fatha', '\u0645\u064e\u0631\u0652\u062d\u064e\u0628\u064b\u0627'],
    ['text-thai', 'Thai with tone marks', '\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35\u0e04\u0e23\u0e31\u0e1a'],
    ['text-tamil', 'Tamil with a combining vowel sign', '\u0ba4\u0bae\u0bbf\u0bb4\u0bcd'],
    ['text-cjk', 'CJK, unaffected by NFC', '\u4eca\u65e5\u306f\u4e16\u754c'],
    ['text-emoji', 'emoji and ZWJ, unaffected by NFC', '\ud83d\udc69\u200d\ud83d\udcbb \ud83c\uddfa\ud83c\uddf8'],
    ['text-ascii-1kb', 'a 1 KB ASCII prompt', 'Summarise the attached contract. '.repeat(32).slice(0, 1024)],
  ];
  for (const [name, note, input] of realistic) add(name, note, input);

  return cases;
}

// ---------------------------------------------------------------------------------------
// Coverage, so the corpus scope statement is generated rather than asserted by hand.
// ---------------------------------------------------------------------------------------
function coverageOf(cases) {
  const seen = new Set();
  for (const c of cases) for (const cp of cps(c.in)) seen.add(cp);
  const byScript = new Map();
  let unassignedRange = 0;
  for (const cp of seen) {
    const s = scriptOf(cp);
    if (s === null) unassignedRange++;
    else byScript.set(s, (byScript.get(s) || 0) + 1);
  }
  return {
    distinctCodePoints: seen.size,
    outsideNamedRanges: unassignedRange,
    byScript: [...byScript.entries()].sort((a, b) => b[1] - a[1]).map(([script, codePoints]) => ({ script, codePoints })),
  };
}

// ---------------------------------------------------------------------------------------
// Emit
// ---------------------------------------------------------------------------------------
function goTable(name, values, type, fmt, perLine = 12) {
  const lines = [];
  for (let i = 0; i < values.length; i += perLine) {
    lines.push('\t' + values.slice(i, i + perLine).map(fmt).join(', ') + ',');
  }
  return `var ${name} = [...]${type}{\n${lines.join('\n')}\n}\n`;
}

function sha256Hex(s) {
  return createHash('sha256').update(s, 'utf8').digest('hex');
}

function buildTables({ decomp, ranks, pairs, quickRanges }) {
  // Hangul syllables are algorithmic, so they are deliberately absent from the table.
  const decompKeys = [...decomp.keys()].filter((cp) => !isHangulSyllable(cp)).sort((a, b) => a - b);
  const offsets = [0];
  const data = [];
  for (const cp of decompKeys) {
    for (const c of cps(decomp.get(cp))) data.push(c);
    offsets.push(data.length);
  }
  const cccKeys = [...ranks.keys()].sort((a, b) => a - b);
  const cccRanks = cccKeys.map((cp) => ranks.get(cp));
  const pairKeys = [...pairs.keys()].sort((a, b) => a - b);
  const pairVals = pairKeys.map((k) => pairs.get(k));
  const maxRank = Math.max(...cccRanks);

  const header = `// Code generated by gen/gen_tables.mjs; DO NOT EDIT.
//
// Source: Node ${process.versions.node} with ICU ${process.versions.icu}, Unicode
// ${process.versions.unicode}. Every table below was derived by observing
// String.prototype.normalize on that implementation and re-checked against it before being
// written (see gen/README.md); regenerate with \`node gen/gen_tables.mjs\`.
//
// Hangul syllables are NOT in these tables: they are handled algorithmically (UAX #15 §10).

package canon

// unicodeVersion is the Unicode version of the host ICU the tables were derived from.
const unicodeVersion = "${process.versions.unicode}"

// icuVersion and nodeBuild record the oracle, so a table regenerated on a different
// implementation is visible in review rather than implied.
const (
\ticuVersion  = "${process.versions.icu}"
\tnodeBuild   = "node ${process.versions.node}"
\tmaxCCCRank = ${maxRank}
)

// decompKeys is the ascending list of code points that have a canonical decomposition.
// decompData[decompOffsets[i]:decompOffsets[i+1]] is the *full* canonical decomposition of
// decompKeys[i], so decomposition is one lookup and needs no recursion.
`;

  let out = header;
  out += goTable('decompKeys', decompKeys, 'uint32', (v) => '0x' + v.toString(16).toUpperCase());
  out += goTable('decompOffsets', offsets, 'uint32', (v) => String(v), 16);
  out += goTable('decompData', data, 'rune', (v) => '0x' + v.toString(16).toUpperCase(), 10);

  out += `
// cccKeys/cccRanks give the canonical combining class *rank* of every NFD-stable
// non-starter. Rank 0 is a starter (absent from the table). The rank preserves both the
// order and the equality of the UCD's classes, which is all UAX #15 uses them for:
// canonical ordering is a stable sort by class, and the blocking rule compares two classes.
// gen/gen_tables.mjs proves the ranks reproduce ICU's ordering for every pair.
`;
  out += goTable('cccKeys', cccKeys, 'uint32', (v) => '0x' + v.toString(16).toUpperCase());
  out += goTable('cccRanks', cccRanks, 'uint8', (v) => String(v), 24);

  out += `
// compKeys packs a canonical composition pair as (first<<21)|second, ascending; compVals is
// the composed code point. Excluded characters (singletons such as U+2126, script-specific
// exclusions such as U+0958, non-starter decompositions) contributed no pair, which is how
// the exclusion list is derived rather than transcribed.
`;
  out += goTable('compKeys', pairKeys.map((k) => '0x' + k.toString(16).toUpperCase()), 'uint64', (v) => v);

  out += goTable('compVals', pairVals, 'uint32', (v) => '0x' + v.toString(16).toUpperCase());

  out += `
// quickRanges are inclusive [lo,hi] spans of code points that can require work: they
// decompose, they carry a non-zero combining class, they can be the first element of a
// composition pair, or they are Hangul. An input containing none of them is already NFC,
// which is the fast path for ordinary text.
`;
  const flat = [];
  for (const [lo, hi] of quickRanges) flat.push(lo, hi);
  out += goTable('quickRanges', flat, 'uint32', (v) => '0x' + v.toString(16).toUpperCase());
  return out;
}

function mergeRanges(points) {
  const sorted = [...points].sort((a, b) => a - b);
  const ranges = [];
  for (const cp of sorted) {
    const last = ranges[ranges.length - 1];
    if (last && cp <= last[1] + 1) last[1] = cp;
    else ranges.push([cp, cp]);
  }
  return ranges;
}

// ---------------------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------------------
const { decomp, nfdStable } = passDecompositions();
const { ranks, sorted, cmp } = passCombiningClasses(nfdStable);
const pairCount = verifyCombiningClasses(sorted, cmp, ranks);
const { pairs, excluded } = passCompositionPairs(decomp);

const quickPoints = new Set();
for (const cp of decomp.keys()) quickPoints.add(cp);
for (const cp of ranks.keys()) quickPoints.add(cp);
for (const k of pairs.keys()) quickPoints.add(k >> 21);
for (let cp = L_BASE; cp < L_BASE + L_COUNT; cp++) quickPoints.add(cp); // Hangul leading jamo
const quickRanges = mergeRanges(quickPoints);

const cases = buildCorpus({ decomp, nfdStable, ranks, pairs, excluded });
const expected = cases.map((c) => nfc(c.in));

// The corpus must contain at least one case that actually changes under NFC, or it proves
// nothing about normalisation.
const changed = cases.filter((c, i) => expected[i] !== c.in).length;
check(changed >= 100, `only ${changed} corpus cases change under NFC; the corpus is not exercising the algorithm`);
for (let i = 0; i < cases.length; i++) {
  check(nfc(expected[i]) === expected[i], `corpus case ${cases[i].name} is not NFC-stable (oracle disagrees with itself)`);
}

// Exhaustive single-code-point digest: both implementations hash NFC of every code point in
// the same order, so one hash compares the whole code space.
const singleDigest = createHash('sha256');
for (let cp = 0; cp <= MAX_CP; cp++) {
  if (isSurrogate(cp)) continue;
  singleDigest.update(nfc(fromCp(cp)), 'utf8');
  singleDigest.update('\u001f', 'utf8');
}

// Golden digest checks: the decomposed and the precomposed spelling of the same text must
// produce the same sha256, and Node says what that value is.
const digestPairs = [
  ['vietnamese', 'Tie\u0302\u0301ng Vie\u0323\u0302t', 'Ti\u1ebfng Vi\u1ec7t'],
  ['greek', '\u03b1\u03b9\u0301\u03c3\u03b8\u03b7\u03c3\u03b7', '\u03b1\u03af\u03c3\u03b8\u03b7\u03c3\u03b7'],
  ['japanese', '\u304b\u3099\u304f\u3099', '\u304c\u3050'],
  ['contract-sentence', 'Summarise the attached contract.', 'Summarise the attached contract.'],
  ['hangul', '\u1112\u1161\u11ab\u1100\u1173\u11af', '\ud55c\uae00'],
];
const digestChecks = digestPairs.map(([name, nfdForm, nfcForm]) => {
  const a = nfc(nfdForm);
  const b = nfc(nfcForm);
  check(a === b, `golden digest pair ${name}: NFC(decomposed) !== NFC(precomposed)`);
  return { name, nfdInput: nfdForm, nfcInput: nfcForm, sha256: sha256Hex(a) };
});

const meta = {
  contract: 'sac-canon-1',
  node: process.versions.node,
  icu: process.versions.icu,
  unicode: process.versions.unicode,
  generated: { decompositionMappings: decomp.size, combiningClassEntries: ranks.size, compositionPairs: pairs.size, compositionExclusions: excluded.length },
  verification: { combiningClassPairsChecked: pairCount, corpusCases: cases.length, corpusCasesChangedByNFC: changed },
  singleCodePointDigest: singleDigest.digest('hex'),
  digestChecks,
  coverage: coverageOf(cases),
  scope: {
    covers: 'every canonical decomposition mapping and every composition pair in the tables, every combination of two in the pairwise check, all Hangul boundary shapes, combining-class reordering, composition exclusions, and seeded multi-mark sequences',
    doesNotCover: 'Unicode text that is not canonically decomposable or combining, i.e. scripts with no canonical decomposition (CJK, emoji, most of the SMP). Those are NFC-invariant and are covered only at the "already NFC" level.',
  },
};

if (failures.length) {
  console.error('generator self-check failed:');
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}

const files = new Map();
files.set(join(ROOT, 'tables.go'), buildTables({ decomp, ranks, pairs, quickRanges }));
files.set(join(ROOT, 'testdata', 'corpus.json'), JSON.stringify({ contract: 'sac-canon-1', unicode: process.versions.unicode, cases }, null, 0) + '\n');
files.set(join(ROOT, 'testdata', 'corpus.expected.json'), JSON.stringify({ unicode: process.versions.unicode, icu: process.versions.icu, expected }, null, 0) + '\n');
files.set(join(ROOT, 'testdata', 'meta.json'), JSON.stringify(meta, null, 2) + '\n');

if (CHECK) {
  let stale = 0;
  for (const [path, body] of files) {
    let current = null;
    try {
      current = readFileSync(path, 'utf8');
    } catch {
      current = null;
    }
    if (current !== body) {
      console.error(`stale: ${path}`);
      stale++;
    }
  }
  if (stale) process.exit(1);
  console.log('generated files are current');
  process.exit(0);
}

mkdirSync(join(ROOT, 'testdata'), { recursive: true });
for (const [path, body] of files) writeFileSync(path, body);

console.log(`node ${process.versions.node}, ICU ${process.versions.icu}, Unicode ${process.versions.unicode}`);
console.log(`decomposition mappings : ${decomp.size} (of which Hangul excluded: ${[...decomp.keys()].filter(isHangulSyllable).length})`);
console.log(`combining-class entries: ${ranks.size} in ${maxRankOf(ranks)} classes`);
console.log(`composition pairs      : ${pairs.size}`);
console.log(`composition exclusions : ${excluded.length}`);
console.log(`quick-check ranges     : ${quickRanges.length}`);
console.log(`pairwise CCC checks    : ${pairCount}`);
console.log(`corpus cases           : ${cases.length} (${changed} change under NFC)`);
console.log(`single-code-point hash : ${meta.singleCodePointDigest}`);
console.log(`coverage               : ${meta.coverage.distinctCodePoints} distinct code points, ${meta.coverage.byScript.length} named ranges`);

function maxRankOf(r) {
  let m = 0;
  for (const v of r.values()) m = Math.max(m, v);
  return m;
}
