package canon

import (
	"errors"
	"unicode/utf8"
)

// ErrInvalidUTF8 is returned for input that is not valid UTF-8. It is never silently
// passed through: a digest computed over ill-formed bytes is a digest two routes cannot
// agree on, which is the failure this package exists to prevent.
var ErrInvalidUTF8 = errors.New("canon: input is not valid UTF-8")

// Contract is the canonicalisation contract these tables serve
// (docs/02-ingest-and-transport.md §4.2). C3 is the step this package implements.
const Contract = "sac-canon-1"

// Hangul constants, UAX #15 §10. Hangul is algorithmic: its 11,172 syllables are not in
// the tables, and both their decomposition and their composition are computed.
const (
	sBase  = 0xAC00
	lBase  = 0x1100
	vBase  = 0x1161
	tBase  = 0x11A7
	lCount = 19
	vCount = 21
	tCount = 28
	nCount = vCount * tCount
	sCount = lCount * nCount
)

// Canonical reports whether a conforming NFC normaliser is installed. It exists so a caller
// can tell "nothing was normalised" from "normalisation ran and changed nothing": this
// package always returns true, and a build without it must say so rather than defaulting to
// an identity that silently produces a non-conforming digest.
func Canonical() bool { return true }

// Version is the Unicode version of the generated tables, e.g. "17.0". It is part of the
// package's honest surface: an implementation built from a different Unicode version is not
// interchangeable with this one, and a caller that records the version it hashed with can
// tell.
func Version() string { return unicodeVersion }

// NFC returns the Unicode NFC normalisation of s (UAX #15, and step C3 of §4.2).
//
// Not NFKC, not NFKD: compatibility folding would merge distinct content, which §4.2 rejects
// explicitly.
//
// The returned slice may be s itself when s is already in NFC; callers that intend to modify
// the result must copy it.
func NFC(s []byte) ([]byte, error) {
	if !utf8.Valid(s) {
		return nil, ErrInvalidUTF8
	}
	if !needsWork(s) {
		return s, nil
	}
	return normalize(s), nil
}

// IsNFC reports whether s is already in NFC. It answers without allocating for the common
// case, and by comparing against the normalised form otherwise, so it is never optimistic.
func IsNFC(s []byte) (bool, error) {
	if !utf8.Valid(s) {
		return false, ErrInvalidUTF8
	}
	if !needsWork(s) {
		return true, nil
	}
	out := normalize(s)
	return string(out) == string(s), nil
}

// NFCString is NFC for callers that hold a string.
func NFCString(s string) (string, error) {
	out, err := NFC([]byte(s))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Normalizer adapts this package to the string-shaped normaliser interface declared by
// device/capture-core (`dedup.Normalizer`: NFC(string) string).
//
// That interface has no error channel, so this method cannot report ill-formed input, and it
// deliberately does not guess: it returns the input unchanged. Returning the input keeps the
// digest deterministic and identical between two routes seeing the same bytes, and it never
// pretends a normalisation happened. A caller that cannot guarantee valid UTF-8 must decode
// first — which is step C2 of §4.2, where ill-formed sequences become U+FFFD — and then call
// the byte API, which reports ErrInvalidUTF8 rather than quietly passing bytes through.
type Normalizer struct{}

// NFC implements the string-shaped normaliser. The precondition is that s is valid UTF-8;
// see the type documentation for what happens when it is not.
func (Normalizer) NFC(s string) string {
	out, err := NFC([]byte(s))
	if err != nil {
		return s
	}
	return string(out)
}

// needsWork reports whether s can possibly not be in NFC. It is the UAX #15 quick check in
// conservative form: it returns false only when the string is provably already normalised
// (no character that NFC changes, no character that can combine with a predecessor, and no
// combining class out of canonical order). A true result means "run the algorithm", not
// "this string changes".
func needsWork(s []byte) bool {
	var lastCCC uint8
	for i := 0; i < len(s); {
		c, size := utf8.DecodeRune(s[i:])
		i += size
		if c < utf8.RuneSelf {
			lastCCC = 0
			continue
		}
		cc := combiningRank(c)
		if cc != 0 && lastCCC > cc {
			return true // canonical order is violated: something must move
		}
		lastCCC = cc
		if !quickYes(c) {
			return true // NFC changes this character, or it can combine with a starter
		}
	}
	return false
}

// quickYes reports whether a code point is definitely unchanged by NFC and definitely
// cannot combine with a preceding starter. Everything else takes the full path.
func quickYes(c rune) bool {
	if c >= sBase && c < sBase+sCount {
		return true // an LV or LVT syllable is already composed; only a following T jamo matters
	}
	if inRanges(qcMaybeRanges[:], c) {
		return false // can be the second element of a composition, or a Hangul V/T jamo
	}
	if inRanges(qcNoRanges[:], c) {
		return false // NFC decomposes or recomposes this character
	}
	return true
}

// normalize is the full algorithm: decompose, order canonically, compose.
func normalize(s []byte) []byte {
	buf := make([]rune, 0, len(s))
	buf = decomposeInto(buf, s)
	orderCanonically(buf)
	buf = composeAll(buf)
	return encodeRunes(buf)
}

// decomposeInto appends the full canonical decomposition of every code point of s. The
// generated table stores full decompositions, so this is one lookup per code point and
// needs no recursion; Hangul is computed.
func decomposeInto(dst []rune, s []byte) []rune {
	for i := 0; i < len(s); {
		c, size := utf8.DecodeRune(s[i:])
		i += size
		if c >= sBase && c < sBase+sCount {
			si := c - sBase
			dst = append(dst, lBase+si/nCount, vBase+(si%nCount)/tCount)
			if ti := si % tCount; ti != 0 {
				dst = append(dst, tBase+ti)
			}
			continue
		}
		if idx, ok := lookupDecomp(c); ok {
			dst = append(dst, decompData[decompOffsets[idx]:decompOffsets[idx+1]]...)
			continue
		}
		dst = append(dst, c)
	}
	return dst
}

// orderCanonically puts every maximal run of non-starters into canonical order. The sort is
// stable, so characters of equal class keep their relative order, which is what makes the
// result canonical rather than merely sorted.
//
// Small runs — essentially all real text — use insertion sort. Longer runs use a counting
// sort by rank, so an adversarial document consisting of nothing but combining marks cannot
// turn the reordering into quadratic work on the request path. The scratch buffer the
// counting sort needs is allocated only if such a run actually occurs.
func orderCanonically(buf []rune) {
	var scratch []rune
	for i := 0; i < len(buf); {
		if combiningRank(buf[i]) == 0 {
			i++
			continue
		}
		j := i
		for j < len(buf) && combiningRank(buf[j]) != 0 {
			j++
		}
		if run := buf[i:j]; len(run) > 1 {
			if len(run) <= insertionSortLimit {
				insertionSortRun(run)
			} else {
				if scratch == nil {
					scratch = make([]rune, len(buf))
				}
				countingSortRun(run, scratch[:len(run)])
			}
		}
		i = j
	}
}

const insertionSortLimit = 16

func insertionSortRun(run []rune) {
	for i := 1; i < len(run); i++ {
		c := run[i]
		cc := combiningRank(c)
		j := i - 1
		for j >= 0 && combiningRank(run[j]) > cc {
			run[j+1] = run[j]
			j--
		}
		run[j+1] = c
	}
}

// countingSortRun is a stable sort by rank. maxCCCRank is 55, so the count table is tiny and
// the sort is linear in the run.
func countingSortRun(run []rune, scratch []rune) {
	var counts [maxCCCRank + 1]int
	for _, r := range run {
		counts[combiningRank(r)]++
	}
	var start [maxCCCRank + 1]int
	at := 0
	for rank := 1; rank <= maxCCCRank; rank++ {
		start[rank] = at
		at += counts[rank]
	}
	for _, r := range run {
		rank := combiningRank(r)
		scratch[start[rank]] = r
		start[rank]++
	}
	copy(run, scratch)
}

// composeAll applies canonical composition to a decomposed, canonically ordered sequence.
//
// The blocking rule is evaluated against the character immediately before the current one.
// That is exactly right here and not an approximation: everything between the last starter
// and the current character is a non-starter (a starter would itself have become the last
// starter), and canonical ordering has already made their classes non-decreasing, so the
// immediately preceding character carries the maximum class in that range.
func composeAll(buf []rune) []rune {
	out := buf[:0]
	starter := -1
	var lastCCC uint8
	for _, c := range buf {
		cc := combiningRank(c)
		blocked := starter >= 0 && len(out) > starter+1 && lastCCC >= cc
		if !blocked {
			if starter >= 0 {
				if composed, ok := composeRune(out[starter], c); ok {
					out[starter] = composed
					continue // c was consumed; the starter stands in its place
				}
			}
		}
		if cc == 0 {
			starter = len(out)
		}
		out = append(out, c)
		lastCCC = cc
	}
	return out
}

// composeRune returns the canonical composition of a starter and a following character, if
// one exists. Hangul is algorithmic; everything else is a table lookup.
func composeRune(a, b rune) (rune, bool) {
	if a >= lBase && a < lBase+lCount && b >= vBase && b < vBase+vCount {
		return sBase + (a-lBase)*nCount + (b-vBase)*tCount, true
	}
	if a >= sBase && a < sBase+sCount && (a-sBase)%tCount == 0 && b > tBase && b < tBase+tCount {
		return a + (b - tBase), true
	}
	if idx, ok := lookupComp(a, b); ok {
		return rune(compVals[idx]), true
	}
	return 0, false
}

func encodeRunes(buf []rune) []byte {
	n := 0
	for _, r := range buf {
		n += utf8.RuneLen(r)
	}
	out := make([]byte, 0, n)
	for _, r := range buf {
		out = utf8.AppendRune(out, r)
	}
	return out
}

// --- table lookups ------------------------------------------------------------------------

func lookupDecomp(c rune) (int, bool) {
	lo, hi := 0, len(decompKeys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if decompKeys[mid] < uint32(c) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(decompKeys) && decompKeys[lo] == uint32(c) {
		return lo, true
	}
	return 0, false
}

func combiningRank(c rune) uint8 {
	lo, hi := 0, len(cccKeys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if cccKeys[mid] < uint32(c) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(cccKeys) && cccKeys[lo] == uint32(c) {
		return cccRanks[lo]
	}
	return 0
}

func lookupComp(a, b rune) (int, bool) {
	key := uint64(a)<<21 | uint64(b)
	lo, hi := 0, len(compKeys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if compKeys[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(compKeys) && compKeys[lo] == key {
		return lo, true
	}
	return 0, false
}

// inRanges reports whether c falls in one of the inclusive [lo,hi] spans stored flat.
func inRanges(ranges []uint32, c rune) bool {
	lo, hi := 0, len(ranges)/2
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if ranges[2*mid+1] < uint32(c) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(ranges)/2 && ranges[2*lo] <= uint32(c)
}
