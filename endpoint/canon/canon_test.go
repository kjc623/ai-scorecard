package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- testdata -----------------------------------------------------------------------------

type corpusCase struct {
	Name string `json:"name"`
	Note string `json:"note"`
	In   string `json:"in"`
}

type corpusFile struct {
	Contract string       `json:"contract"`
	Unicode  string       `json:"unicode"`
	Cases    []corpusCase `json:"cases"`
}

type expectedFile struct {
	Unicode  string   `json:"unicode"`
	ICU      string   `json:"icu"`
	Expected []string `json:"expected"`
}

type metaFile struct {
	Contract  string `json:"contract"`
	Node      string `json:"node"`
	ICU       string `json:"icu"`
	Unicode   string `json:"unicode"`
	Generated struct {
		DecompositionMappings int `json:"decompositionMappings"`
		CombiningClassEntries int `json:"combiningClassEntries"`
		CompositionPairs      int `json:"compositionPairs"`
		CompositionExclusions int `json:"compositionExclusions"`
	} `json:"generated"`
	Verification struct {
		CombiningClassPairsChecked int `json:"combiningClassPairsChecked"`
		CorpusCases                int `json:"corpusCases"`
		CorpusCasesChangedByNFC    int `json:"corpusCasesChangedByNFC"`
		PairCrossProductCases      int `json:"pairCrossProductCases"`
	} `json:"verification"`
	SingleCodePointDigest  string   `json:"singleCodePointDigest"`
	PairSet                []uint32 `json:"pairSet"`
	PairCrossProductDigest string   `json:"pairCrossProductDigest"`
	NonStarterSet          []uint32 `json:"nonStarterSet"`
	NonStarterPairDigest   string   `json:"nonStarterPairDigest"`
	DigestChecks           []struct {
		Name     string `json:"name"`
		NFDInput string `json:"nfdInput"`
		NFCInput string `json:"nfcInput"`
		SHA256   string `json:"sha256"`
	} `json:"digestChecks"`
	Coverage struct {
		DistinctCodePoints int `json:"distinctCodePoints"`
		OutsideNamedRanges int `json:"outsideNamedRanges"`
		ByScript           []struct {
			Script     string `json:"script"`
			CodePoints int    `json:"codePoints"`
		} `json:"byScript"`
	} `json:"coverage"`
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parsing testdata/%s: %v", name, err)
	}
}

func loadCorpus(t *testing.T) (corpusFile, expectedFile, metaFile) {
	t.Helper()
	var corpus corpusFile
	var expected expectedFile
	var meta metaFile
	readJSON(t, "corpus.json", &corpus)
	readJSON(t, "corpus.expected.json", &expected)
	readJSON(t, "meta.json", &meta)
	return corpus, expected, meta
}

func codePoints(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, "U+%04X ", r)
	}
	return strings.TrimSpace(b.String())
}

// --- the corpus ---------------------------------------------------------------------------

// TestCorpusAgainstGolden is the conformance test: every case in the corpus must produce
// exactly the bytes Node's String.prototype.normalize('NFC') produces for it.
func TestCorpusAgainstGolden(t *testing.T) {
	corpus, expected, _ := loadCorpus(t)
	if len(corpus.Cases) < 300 {
		t.Fatalf("corpus has %d cases, want at least 300", len(corpus.Cases))
	}
	if len(corpus.Cases) != len(expected.Expected) {
		t.Fatalf("corpus has %d cases but %d expected results", len(corpus.Cases), len(expected.Expected))
	}
	if corpus.Unicode != expected.Unicode || corpus.Unicode != unicodeVersion {
		t.Fatalf("unicode version mismatch: corpus %q, expected %q, tables %q", corpus.Unicode, expected.Unicode, unicodeVersion)
	}

	changed := 0
	for i, c := range corpus.Cases {
		got, err := NFC([]byte(c.In))
		if err != nil {
			t.Fatalf("case %q (%s): NFC: %v", c.Name, codePoints(c.In), err)
		}
		if string(got) != expected.Expected[i] {
			t.Fatalf("case %q (%s) [%s]:\n got %s\nwant %s",
				c.Name, codePoints(c.In), c.Note, codePoints(string(got)), codePoints(expected.Expected[i]))
		}
		if string(got) != c.In {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("no corpus case changed under NFC: the corpus is not exercising the algorithm")
	}
	t.Logf("%d corpus cases, %d of them changed by NFC", len(corpus.Cases), changed)
}

// TestIdempotence: NFC(NFC(x)) == NFC(x) for every case. A normaliser that is not idempotent
// would make a second pass produce a different digest than the first.
func TestIdempotence(t *testing.T) {
	corpus, _, _ := loadCorpus(t)
	for _, c := range corpus.Cases {
		once, err := NFC([]byte(c.In))
		if err != nil {
			t.Fatalf("case %q: %v", c.Name, err)
		}
		twice, err := NFC(once)
		if err != nil {
			t.Fatalf("case %q, second pass: %v", c.Name, err)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("case %q is not idempotent:\n once %s\ntwice %s", c.Name, codePoints(string(once)), codePoints(string(twice)))
		}
	}
}

// TestNodeEquivalenceLive runs the JavaScript side of the contract at test time and compares
// it with Go, byte for byte, over the whole corpus. It also insists that the local Node
// agrees with the committed expectations: a Node with a different Unicode version is a
// finding, not something to skip past.
func TestNodeEquivalenceLive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the live cross-implementation check cannot run here; TestCorpusAgainstGolden still compares against Node's committed output")
	}
	corpus, expected, _ := loadCorpus(t)
	out, err := exec.Command(node, filepath.Join("gen", "nfc_oracle.mjs"), filepath.Join("testdata", "corpus.json")).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("running the node oracle: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("running the node oracle: %v", err)
	}
	var oracle struct {
		Contract string   `json:"contract"`
		Node     string   `json:"node"`
		ICU      string   `json:"icu"`
		Unicode  string   `json:"unicode"`
		Expected []string `json:"expected"`
	}
	if err := json.Unmarshal(out, &oracle); err != nil {
		t.Fatalf("parsing the node oracle output: %v", err)
	}
	if oracle.Contract != Contract {
		t.Fatalf("node oracle speaks contract %q, Go implements %q", oracle.Contract, Contract)
	}
	if oracle.Unicode != unicodeVersion {
		t.Fatalf("the local node reports Unicode %s but the tables were generated from %s: the two implementations are not the same contract",
			oracle.Unicode, unicodeVersion)
	}
	if len(oracle.Expected) != len(corpus.Cases) {
		t.Fatalf("node returned %d results for %d cases", len(oracle.Expected), len(corpus.Cases))
	}

	for i, c := range corpus.Cases {
		// The committed expectation must still be what Node produces.
		if oracle.Expected[i] != expected.Expected[i] {
			t.Fatalf("case %q: node now produces %s but testdata/corpus.expected.json says %s: regenerate the corpus",
				c.Name, codePoints(oracle.Expected[i]), codePoints(expected.Expected[i]))
		}
		got, err := NFC([]byte(c.In))
		if err != nil {
			t.Fatalf("case %q: %v", c.Name, err)
		}
		if string(got) != oracle.Expected[i] {
			t.Fatalf("case %q (%s) [%s]:\n   go %s\nnode %s",
				c.Name, codePoints(c.In), c.Note, codePoints(string(got)), codePoints(oracle.Expected[i]))
		}
	}
	t.Logf("go == node %s (ICU %s, Unicode %s) over %d cases, byte for byte",
		oracle.Node, oracle.ICU, oracle.Unicode, len(corpus.Cases))
}

// TestExhaustiveSingleCodePointDigest compares the two implementations over the entire code
// space in a single hash: both hash NFC of every code point, in the same order, separated by
// U+001F. Agreement means every single code point agrees, not a sample.
func TestExhaustiveSingleCodePointDigest(t *testing.T) {
	_, _, meta := loadCorpus(t)
	h := sha256.New()
	for cp := rune(0); cp <= utf8.MaxRune; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue // surrogates cannot appear in UTF-8
		}
		var buf [4]byte
		n := utf8.EncodeRune(buf[:], cp)
		got, err := NFC(buf[:n])
		if err != nil {
			t.Fatalf("NFC(U+%04X): %v", cp, err)
		}
		h.Write(got)
		h.Write([]byte{0x1f})
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != meta.SingleCodePointDigest {
		t.Fatalf("single-code-point digest\n got %s\nwant %s (node)\nAt least one code point normalises differently from ICU 78.2.",
			got, meta.SingleCodePointDigest)
	}
	t.Logf("all 1,112,064 code points agree with node: %s", got)
}

// TestPairCrossProductDigest is the systematic companion to the single-code-point digest:
// both implementations hash NFC of every ordered pair drawn from a 256-code-point sample of
// everything the algorithm reacts to — non-starters, excluded characters, and both halves of
// every composition pair. Single code points are covered exhaustively; this covers ordered
// pairs by construction rather than by random sampling.
func TestPairCrossProductDigest(t *testing.T) {
	_, _, meta := loadCorpus(t)
	if len(meta.PairSet) != 256 {
		t.Fatalf("pair set has %d code points, want 256", len(meta.PairSet))
	}
	h := sha256.New()
	var pair [2]rune
	buf := make([]byte, 0, 8)
	for _, a := range meta.PairSet {
		for _, b := range meta.PairSet {
			pair[0], pair[1] = rune(a), rune(b)
			buf = buf[:0]
			for _, r := range pair {
				buf = utf8.AppendRune(buf, r)
			}
			got, err := NFC(buf)
			if err != nil {
				t.Fatalf("NFC(U+%04X U+%04X): %v", a, b, err)
			}
			h.Write(got)
			h.Write([]byte{0x1f})
		}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != meta.PairCrossProductDigest {
		t.Fatalf("pair cross-product digest over %d ordered pairs\n got %s\nwant %s (node)",
			len(meta.PairSet)*len(meta.PairSet), got, meta.PairCrossProductDigest)
	}
	t.Logf("%d ordered pairs agree with node", len(meta.PairSet)*len(meta.PairSet))
}

// TestAllNonStarterPairsDigest is the exhaustive check on the one table that is derived
// rather than transcribed. Both implementations hash NFC of every ordered pair of
// non-starters — all 929,296 of them — so a single misclassified combining class shows up
// here even if no corpus case happens to exercise that pair.
func TestAllNonStarterPairsDigest(t *testing.T) {
	_, _, meta := loadCorpus(t)
	if len(meta.NonStarterSet) < 900 {
		t.Fatalf("non-starter set has %d entries, want the ~964 the tables were built from", len(meta.NonStarterSet))
	}
	h := sha256.New()
	buf := make([]byte, 0, 8)
	for _, a := range meta.NonStarterSet {
		for _, b := range meta.NonStarterSet {
			buf = buf[:0]
			buf = utf8.AppendRune(buf, rune(a))
			buf = utf8.AppendRune(buf, rune(b))
			got, err := NFC(buf)
			if err != nil {
				t.Fatalf("NFC(U+%04X U+%04X): %v", a, b, err)
			}
			h.Write(got)
			h.Write([]byte{0x1f})
		}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != meta.NonStarterPairDigest {
		t.Fatalf("non-starter pair digest over %d ordered pairs\n got %s\nwant %s (node)\nAt least one combining class is ranked wrongly.",
			len(meta.NonStarterSet)*len(meta.NonStarterSet), got, meta.NonStarterPairDigest)
	}
	t.Logf("all %d ordered non-starter pairs agree with node", len(meta.NonStarterSet)*len(meta.NonStarterSet))
}

// TestGoldenDigestPrecomposedVsDecomposed is the property dedup_key actually depends on: the
// two spellings of the same text must hash to the same value, and that value must be the one
// Node computes.
func TestGoldenDigestPrecomposedVsDecomposed(t *testing.T) {
	_, _, meta := loadCorpus(t)
	if len(meta.DigestChecks) == 0 {
		t.Fatal("no digest checks in testdata/meta.json")
	}
	for _, dc := range meta.DigestChecks {
		decomposed, err := NFC([]byte(dc.NFDInput))
		if err != nil {
			t.Fatalf("%s: NFC(decomposed): %v", dc.Name, err)
		}
		precomposed, err := NFC([]byte(dc.NFCInput))
		if err != nil {
			t.Fatalf("%s: NFC(precomposed): %v", dc.Name, err)
		}
		if !bytes.Equal(decomposed, precomposed) {
			t.Fatalf("%s: NFC of the two spellings differs:\n%s\n%s", dc.Name, codePoints(string(decomposed)), codePoints(string(precomposed)))
		}
		sum := sha256.Sum256(decomposed)
		if got := hex.EncodeToString(sum[:]); got != dc.SHA256 {
			t.Fatalf("%s: sha256 of the canonical form is %s, node says %s", dc.Name, got, dc.SHA256)
		}
	}
	t.Logf("%d digest pairs agree across spellings and with node", len(meta.DigestChecks))
}

// --- API behaviour -------------------------------------------------------------------------

func TestInvalidUTF8IsAnErrorNotAPassThrough(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"lone continuation byte", []byte{0x80}},
		{"truncated two-byte sequence", []byte{0xC3}},
		{"truncated three-byte sequence", []byte{0xE2, 0x82}},
		{"overlong encoding", []byte{0xC0, 0xAF}},
		{"surrogate half", []byte{0xED, 0xA0, 0x80}},
		{"valid prefix then garbage", append([]byte("hello "), 0xFF, 0xFE)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NFC(tc.in)
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("NFC(% x) = (%q, %v), want ErrInvalidUTF8", tc.in, got, err)
			}
			if got != nil {
				t.Fatalf("NFC returned %q on error, want nil: a partial result must not look usable", got)
			}
			if _, err := IsNFC(tc.in); !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("IsNFC(% x) = %v, want ErrInvalidUTF8", tc.in, err)
			}
			if _, err := NFCString(string(tc.in)); !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("NFCString(% x) = %v, want ErrInvalidUTF8", tc.in, err)
			}
		})
	}
}

// The string-shaped adapter used by capture-core has no error channel. Its documented
// behaviour on invalid UTF-8 is to return the input unchanged: deterministic, identical
// between two routes seeing the same bytes, and never pretending a normalisation happened.
func TestStringAdapterOnInvalidUTF8ReturnsInputUnchanged(t *testing.T) {
	in := string([]byte{0x61, 0xFF, 0x62})
	if got := (Normalizer{}).NFC(in); got != in {
		t.Fatalf("Normalizer.NFC(% x) = % x, want the input unchanged", []byte(in), []byte(got))
	}
}

func TestNormalizerAdapterNormalises(t *testing.T) {
	var n interface{ NFC(string) string } = Normalizer{}
	decomposed := "Tie\u0302\u0301ng"
	if got, want := n.NFC(decomposed), "Ti\u1ebfng"; got != want {
		t.Fatalf("Normalizer.NFC(%s) = %s, want %s", codePoints(decomposed), codePoints(got), codePoints(want))
	}
	// And it agrees with the byte API for valid input.
	viaBytes, err := NFCString(decomposed)
	if err != nil {
		t.Fatalf("NFCString: %v", err)
	}
	if n.NFC(decomposed) != viaBytes {
		t.Fatalf("the two APIs disagree: %q vs %q", n.NFC(decomposed), viaBytes)
	}
}

func TestIsNFC(t *testing.T) {
	precomposed := "Ti\u1ebfng Vi\u1ec7t"
	decomposed := "Tie\u0302\u0301ng Vie\u0323\u0302t"
	if ok, err := IsNFC([]byte(precomposed)); err != nil || !ok {
		t.Fatalf("IsNFC(precomposed) = (%v, %v), want true", ok, err)
	}
	if ok, err := IsNFC([]byte(decomposed)); err != nil || ok {
		t.Fatalf("IsNFC(decomposed) = (%v, %v), want false", ok, err)
	}
	// A tail sequence that the quick check must not wave through: an excluded character.
	if ok, err := IsNFC([]byte("\u0958")); err != nil || ok {
		t.Fatalf("IsNFC(U+0958) = (%v, %v), want false", ok, err)
	}
}

func TestCapabilityIdentifiers(t *testing.T) {
	_, _, meta := loadCorpus(t)
	if !Canonical() {
		t.Fatal("Canonical() is false: a caller would conclude no normaliser is installed")
	}
	if Version() != meta.Unicode {
		t.Fatalf("Version() = %q but the corpus and tables record %q", Version(), meta.Unicode)
	}
	if Contract != "sac-canon-1" {
		t.Fatalf("Contract = %q, want sac-canon-1", Contract)
	}
	if icuVersion != meta.ICU {
		t.Fatalf("tables were built against ICU %s but meta.json records %s", icuVersion, meta.ICU)
	}
	t.Logf("tables: %s, ICU %s, %s; %d decomposition mappings, %d combining-class entries, %d composition pairs, %d exclusions",
		unicodeVersion, icuVersion, nodeBuild, meta.Generated.DecompositionMappings,
		meta.Generated.CombiningClassEntries, meta.Generated.CompositionPairs, meta.Generated.CompositionExclusions)
}

// --- the quick check ------------------------------------------------------------------------

// TestQuickCheckDoesNotSkipWork drives every composition pair through the public API. If the
// quick check did not flag a pair's second character as "may combine", NFC would return the
// input unchanged and this test would catch it.
func TestQuickCheckDoesNotSkipWork(t *testing.T) {
	pairs := 0
	for i, key := range compKeys {
		a := rune(key >> 21)
		b := rune(key & 0x1FFFFF)
		want := rune(compVals[i])
		got, err := NFC([]byte(string([]rune{a, b})))
		if err != nil {
			t.Fatalf("NFC(U+%04X U+%04X): %v", a, b, err)
		}
		if string(got) != string([]rune{want}) {
			t.Fatalf("composition pair U+%04X + U+%04X: got %s, want U+%04X",
				a, b, codePoints(string(got)), want)
		}
		pairs++
	}
	t.Logf("%d canonical composition pairs compose through the public API", pairs)
}

// TestHangulIsAlgorithmic checks the algorithmic path against the script's structure rather
// than against a table: every syllable is NFC, decomposes to the expected jamo, and
// recomposes, and the jamo ranges are not in the generated decomposition table.
func TestHangulIsAlgorithmic(t *testing.T) {
	for cp := rune(sBase); cp < sBase+sCount; cp++ {
		s := string(cp)
		got, err := NFC([]byte(s))
		if err != nil {
			t.Fatalf("NFC(U+%04X): %v", cp, err)
		}
		if string(got) != s {
			t.Fatalf("Hangul syllable U+%04X normalised to %s: an LVT syllable is already NFC", cp, codePoints(string(got)))
		}
		// My own decomposition and composition must round-trip.
		buf := decomposeInto(nil, []byte(s))
		if len(buf) < 2 || len(buf) > 3 {
			t.Fatalf("U+%04X decomposed to %d jamo, want 2 or 3", cp, len(buf))
		}
		if back := composeAll(buf); len(back) != 1 || back[0] != cp {
			t.Fatalf("U+%04X did not recompose: %s", cp, codePoints(string(back)))
		}
	}
	// The table must not carry Hangul: it is the algorithmic path that handles it.
	for _, r := range decompData {
		if r >= sBase && r < sBase+sCount {
			t.Fatalf("decomposition table contains Hangul syllable U+%04X: the split between table and algorithm is broken", r)
		}
	}
}

// TestCompositionExclusionsDoNotRecompose: every code point NFC changes must stay changed,
// and must be stable afterwards.
func TestCompositionExclusionsDoNotRecompose(t *testing.T) {
	count := 0
	for i := 0; i+1 < len(qcNoRanges); i += 2 {
		for cp := rune(qcNoRanges[i]); cp <= rune(qcNoRanges[i+1]); cp++ {
			if cp >= 0xD800 && cp <= 0xDFFF {
				continue
			}
			in := string(cp)
			once, err := NFC([]byte(in))
			if err != nil {
				t.Fatalf("NFC(U+%04X): %v", cp, err)
			}
			if string(once) == in {
				t.Fatalf("U+%04X is in the QC=No table but NFC left it unchanged", cp)
			}
			twice, err := NFC(once)
			if err != nil {
				t.Fatalf("NFC(NFC(U+%04X)): %v", cp, err)
			}
			if !bytes.Equal(once, twice) {
				t.Fatalf("U+%04X is not idempotent: %s -> %s", cp, codePoints(string(once)), codePoints(string(twice)))
			}
			count++
		}
	}
	if count < 1000 {
		t.Fatalf("only %d excluded code points checked; the QC=No ranges look wrong", count)
	}
	t.Logf("%d composition exclusions stay decomposed and are idempotent", count)
}

// TestAdversarialMarkRun: a long run of combining marks in descending class order must be
// reordered correctly and must not take quadratic time. This exercises the counting-sort path
// that the small-run insertion sort normally hides.
func TestAdversarialMarkRun(t *testing.T) {
	const n = 20000
	marks := make([]rune, 0, n)
	for i := 0; i < n; i++ {
		// cycle through ranks in reverse order
		marks = append(marks, rune(cccKeys[len(cccKeys)-1-int(uint(i)%uint(len(cccKeys)))]))
	}
	in := append([]rune{'a'}, marks...)
	got, err := NFC([]byte(string(in)))
	if err != nil {
		t.Fatalf("NFC: %v", err)
	}
	out := []rune(string(got))
	if len(out) != len(in) {
		t.Fatalf("reordering changed the length: %d -> %d", len(in), len(out))
	}
	last := uint8(0)
	for i, r := range out {
		cc := combiningRank(r)
		if i > 0 && cc != 0 && last > cc {
			t.Fatalf("output is not in canonical order at index %d: rank %d after %d", i, cc, last)
		}
		if cc != 0 {
			last = cc
		}
	}
}

// --- the generator itself -------------------------------------------------------------------

// TestGeneratorIsReproducible runs the generator against a temporary copy of the tree and
// requires the committed tables to be exactly what it produces. It is skipped when node is
// unavailable; TestCorpusAgainstGolden and TestExhaustiveSingleCodePointDigest still pin the
// committed data to Node's output in that case.
func TestGeneratorIsReproducible(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: cannot re-run the generator here")
	}
	out, err := exec.Command(node, filepath.Join("gen", "gen_tables.mjs"), "--check").CombinedOutput()
	if err != nil {
		t.Fatalf("the committed tables are not what the generator produces:\n%s", out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}
