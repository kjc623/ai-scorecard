package norm_test

import (
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
)

func TestNormalisationFoldsWhatARuleMustSee(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"fullwidth digits", "４１１１", "4111"},
		{"ideographic space", "4111　1111", "4111 1111"},
		{"non-breaking space", "4111\u00a01111", "4111 1111"},
		{"zero-width space", "4111\u200b1111", "41111111"},
		{"soft hyphen", "4111\u00ad1111", "41111111"},
		{"BOM", "\ufeffhello", "hello"},
		{"line endings", "a\r\nb\rc", "a\nb\nc"},
		{"whitespace collapse", "a\t\t   b", "a b"},
		{"trailing space before newline", "a   \nb", "a\nb"},
		{"leading whitespace", "   hello", "hello"},
		{"C0 control", "a\x01b", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := norm.Normalise("text/plain", []byte(tc.body), norm.DefaultLimits())
			if err != nil {
				t.Fatalf("normalise: %v", err)
			}
			if res.Text != tc.want {
				t.Errorf("normalised %q to %q, want %q", tc.body, res.Text, tc.want)
			}
		})
	}
}

func TestFullwidthFoldingProducesDigitsNotControls(t *testing.T) {
	// Regression: an offset of 0xFF00 instead of 0xFEE0 turns U+FF14 into a C0 control byte, so a
	// pasted full-width card number becomes invisible to every rule.
	res, err := norm.Normalise("text/plain", []byte("カード４１１１ー１１１１"), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Text {
		if r < 0x20 && r != '\n' && r != '\t' {
			t.Fatalf("normalisation emitted a control character %q in %q", r, res.Text)
		}
	}
	if !strings.Contains(res.Text, "4111") {
		t.Fatalf("full-width digits did not fold to ASCII: %q", res.Text)
	}
}

func TestDecoding(t *testing.T) {
	utf16le := []byte{0xFF, 0xFE, 'h', 0, 'i', 0}
	res, err := norm.Normalise("text/plain", utf16le, norm.DefaultLimits())
	if err != nil {
		t.Fatalf("UTF-16LE with a BOM must decode rather than refuse: %v", err)
	}
	if res.Text != "hi" || res.Encoding != norm.EncodingUTF16LE {
		t.Errorf("decoded %q as %q", res.Text, res.Encoding)
	}

	utf16be := []byte{0xFE, 0xFF, 0, 'h', 0, 'i'}
	res, err = norm.Normalise("", utf16be, norm.DefaultLimits())
	if err != nil || res.Text != "hi" || res.Encoding != norm.EncodingUTF16BE {
		t.Errorf("UTF-16BE: %q %q %v", res.Text, res.Encoding, err)
	}
}

// TestRefusalsAreDistinguishable is §9.7's two different rows: an over-cap body is content the
// provider handed over that the classifier cannot process, and undecodable bytes are bytes it
// cannot read. Both degrade, with different details.
func TestRefusalsAreDistinguishable(t *testing.T) {
	limits := norm.DefaultLimits()
	limits.MaxInputBytes = 16

	if _, err := norm.Normalise("text/plain", []byte(strings.Repeat("a", 17)), limits); err != norm.ErrOverCap {
		t.Errorf("over-cap body returned %v, want ErrOverCap", err)
	}
	if _, err := norm.Normalise("text/plain", []byte("a\xffb"), limits); err != norm.ErrUndecodable {
		t.Errorf("undecodable bytes returned %v, want ErrUndecodable", err)
	}
	if _, err := norm.Normalise("application/pdf", []byte("%PDF-1.7"), limits); err != norm.ErrUndecodable {
		t.Errorf("a declared binary media type returned %v, want ErrUndecodable", err)
	}
}

// TestTruncationTrivialVsConsequential is the "not emitted when" row of §9.7: "The payload was
// large but fully processed" must not degrade, while a payload cut such that a rule could not see
// all of it must.
//
// "Fully processed" is expressed as *not truncated at all*: this stage collapses whitespace runs
// and trims trailing whitespace, so a body whose only excess is whitespace normalises to something
// that fits, and there is nothing for a rule to have missed. The truncated-with-a-whitespace-tail
// case therefore cannot arise here, and the implementation's hasRuleVisibleContent guard is
// defensive rather than load-bearing.
func TestTruncationTrivialVsConsequential(t *testing.T) {
	limits := norm.DefaultLimits()
	limits.MaxInputBytes = 4096
	limits.MaxTextBytes = 64

	largeWithinCap := strings.Repeat("abc ", 15) // 60 bytes, just under the 64-byte text cap
	res, err := norm.Normalise("text/plain", []byte(largeWithinCap), limits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.TruncationAffectsRules {
		t.Errorf("a large body that fits the cap was marked truncated: %+v", res)
	}

	contentTail := "hello" + strings.Repeat("x", 200)
	res, err = norm.Normalise("text/plain", []byte(contentTail), limits)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("a body over the text cap was not marked truncated")
	}
	if !res.TruncationAffectsRules {
		t.Error("a tail carrying content must mark the truncation as consequential")
	}
}

func TestStructuredBodyExtraction(t *testing.T) {
	jsonBody := `{"messages":[{"role":"user","content":"my card is 4111 1111 1111 1111"}],"temperature":0.7}`
	res, err := norm.Normalise("application/json", []byte(jsonBody), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"4111 1111 1111 1111", "temperature", "messages"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("JSON extraction lost %q: %q", want, res.Text)
		}
	}

	xmlBody := `<?xml version="1.0"?><doc><p>patient 42</p><p>diagnosis</p></doc>`
	res, err = norm.Normalise("application/xml", []byte(xmlBody), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "patient 42") || !strings.Contains(res.Text, "diagnosis") {
		t.Errorf("XML extraction lost text: %q", res.Text)
	}
}

// TestDeepNestingIsRefusedByTruncating is §9.2's "refuses pathological inputs — deeply nested
// structures, enormous strings — by truncating and marking".
func TestDeepNestingIsRefusedByTruncating(t *testing.T) {
	depth := 200
	body := strings.Repeat("[", depth) + `"deep"` + strings.Repeat("]", depth)
	res, err := norm.Normalise("application/json", []byte(body), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pathological != "nesting" {
		t.Fatalf("deep nesting was not reported as pathological: %+v", res)
	}
	if !res.Truncated || !res.TruncationAffectsRules {
		t.Error("a pathological body must be marked truncated and consequential")
	}
}

func TestDigestIsStableAndOverNormalisedText(t *testing.T) {
	a, err := norm.Normalise("text/plain", []byte("4111  1111"), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	b, err := norm.Normalise("text/plain", []byte("4111 1111"), norm.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Errorf("the same text by two spellings produced two digests: %s vs %s", a.Digest, b.Digest)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") || len(a.Digest) != len("sha256:")+64 {
		t.Errorf("digest %q is not a sha256 in the contract's form", a.Digest)
	}
	empty, err := norm.Normalise("text/plain", nil, norm.DefaultLimits())
	if err != nil || empty.Digest == "" {
		t.Errorf("an empty body must still have a digest: %q %v", empty.Digest, err)
	}
}

func TestNormalisationIsLinearOnAPathologicalBody(t *testing.T) {
	// A quadratic whitespace pass would make this test's runtime blow up; the bounded pass keeps it
	// proportional to the input.
	body := strings.Repeat("word   \n", 20000)
	limits := norm.DefaultLimits()
	limits.MaxInputBytes = 1 << 20
	limits.MaxTextBytes = 1 << 20
	res, err := norm.Normalise("text/plain", []byte(body), limits)
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesOut == 0 {
		t.Fatal("the body normalised to nothing")
	}
	if strings.Contains(res.Text, "  ") {
		t.Error("whitespace runs survived collapsing")
	}
}
