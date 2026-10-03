package canon

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

// The classifier's interactive budget is 300 ms and the proxy path sits on the user's request
// path, so the numbers that matter are a typical prompt and a full-size document, in both
// spellings: text that is already NFC (the common case, which must not allocate) and text
// carrying combining marks (which runs the whole algorithm).
const (
	benchCleanUnit = "Résumé the Tiếng Việt contract, café, naïve, coöperate, and flag anything unusual. "
	// The same sentence written the way a macOS filename or a decomposed provider body
	// arrives: precomposed characters replaced by base plus combining marks.
	benchDirtyUnit = "Re\u0301sume\u0301 the Tie\u0302\u0301ng Vie\u0323\u0302t contract, cafe\u0301, nai\u0308ve, coo\u0308perate, and flag anything unusual. "
	benchCJKUnit   = "添付された契約書を要約し、異常があれば報告してください。"
)

func benchInput(size int, unit string) []byte {
	var b bytes.Buffer
	b.Grow(size + len(unit))
	for b.Len() < size {
		b.WriteString(unit)
	}
	out := b.Bytes()[:size]
	for len(out) > 0 && !utf8.Valid(out) {
		out = out[:len(out)-1]
	}
	return out
}

// benchInputMarks builds a pathological input: a run of combining marks in descending
// combining-class order, which the reordering pass must sort. It exists to show the reorder
// is linear rather than quadratic on hostile input.
func benchInputMarks(size int) []byte {
	marks := make([]rune, 0, size/2)
	for i := 0; len(marks) < size/2; i++ {
		marks = append(marks, rune(cccKeys[len(cccKeys)-1-i%len(cccKeys)]))
	}
	in := append([]rune{'a'}, marks...)
	return []byte(string(in))
}

func benchmarkNFC(b *testing.B, in []byte) {
	b.Helper()
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	out, err := NFC(in)
	if err != nil {
		b.Fatalf("NFC: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NFC(in); err != nil {
			b.Fatalf("NFC: %v", err)
		}
	}
	b.StopTimer()
	_ = out
}

func BenchmarkNFC1KBPrompt(b *testing.B) {
	benchmarkNFC(b, benchInput(1<<10, benchCleanUnit))
}

func BenchmarkNFC1KBPromptDecomposed(b *testing.B) {
	benchmarkNFC(b, benchInput(1<<10, benchDirtyUnit))
}

func BenchmarkNFC64KBDocument(b *testing.B) {
	benchmarkNFC(b, benchInput(64<<10, benchCJKUnit+benchCleanUnit))
}

func BenchmarkNFC64KBDocumentDecomposed(b *testing.B) {
	benchmarkNFC(b, benchInput(64<<10, benchDirtyUnit))
}

// BenchmarkNFC64KBCombiningMarks is the adversarial shape: 32,768 combining marks in
// descending class order. It exercises the counting sort rather than the insertion sort and
// proves the reordering cannot be turned into quadratic work.
func BenchmarkNFC64KBCombiningMarks(b *testing.B) {
	in := benchInputMarks(64 << 10)
	// The input is not valid UTF-8 only if the rune slicing above produced lone bytes; it
	// cannot, but the benchmark should fail loudly rather than measure garbage.
	if !utf8.Valid(in) {
		b.Fatal("adversarial input is not valid UTF-8")
	}
	benchmarkNFC(b, in)
}

// BenchmarkNFCASCII is the floor: pure ASCII, which the quick check must reject in one pass
// with no allocation at all.
func BenchmarkNFCASCII(b *testing.B) {
	benchmarkNFC(b, benchInput(1<<10, "Summarise the attached contract and flag anything unusual. "))
}
