package norm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
)

func normalise(t *testing.T, mediaType, body string) norm.Result {
	t.Helper()
	res, err := norm.Normalise(mediaType, []byte(body), norm.DefaultLimits())
	if err != nil {
		t.Fatalf("normalise %q: %v", body, err)
	}
	return res
}

func TestNormalisationFoldsWhatARuleMustSee(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"full-width digits", "４１１１", "4111"},
		{"full-width digits between Japanese text", "カード４１１１ー１１１１", "カード4111ー1111"},
		{"superscript digit", "x²", "x2"},
		{"ligature", "ﬁle", "file"},
		{"ideographic space", "4111\u30001111", "4111 1111"},
		{"non-breaking space", "4111\u00a01111", "4111 1111"},
		{"narrow no-break space", "4111\u202f1111", "4111 1111"},
		{"zero-width space", "4111\u200b1111", "41111111"},
		{"word joiner", "4111\u20601111", "41111111"},
		{"soft hyphen", "4111\u00ad1111", "41111111"},
		{"byte-order mark", "\ufeffhello", "hello"},
		{"composed and decomposed forms agree", "cafe\u0301", "café"},
		{"line endings", "a\r\nb\rc", "a\nb\nc"},
		{"whitespace runs", "a\t\t   b", "a b"},
		{"trailing space before a newline", "a   \nb", "a\nb"},
		{"leading whitespace", "   hello", "hello"},
		{"blank lines are kept", "a\n\nb\n\n", "a\n\nb"},
		{"C0 control", "a\x01b", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalise(t, "text/plain", tc.body).Text; got != tc.want {
				t.Errorf("normalised %q to %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestUTF16WithAByteOrderMarkIsDecoded(t *testing.T) {
	for name, body := range map[string][]byte{
		"little-endian": {0xFF, 0xFE, 'h', 0, 'i', 0},
		"big-endian":    {0xFE, 0xFF, 0, 'h', 0, 'i'},
	} {
		res, err := norm.Normalise("", body, norm.DefaultLimits())
		if err != nil || res.Text != "hi" {
			t.Errorf("%s: %q, %v", name, res.Text, err)
		}
	}
}

func TestRefusals(t *testing.T) {
	lim := norm.DefaultLimits()
	lim.MaxInputBytes = 16
	if _, err := norm.Normalise("text/plain", []byte(strings.Repeat("a", 17)), lim); err != norm.ErrOverCap {
		t.Errorf("an over-limit body returned %v, want ErrOverCap", err)
	}
	if _, err := norm.Normalise("text/plain", []byte("a\xffb"), lim); err != norm.ErrUndecodable {
		t.Errorf("invalid UTF-8 returned %v, want ErrUndecodable", err)
	}
	if _, err := norm.Normalise("application/pdf", []byte("%PDF-1.7"), lim); err != norm.ErrUndecodable {
		t.Errorf("a binary media type returned %v, want ErrUndecodable", err)
	}
}

func TestTruncationIsMarkedOnlyWhenItCouldHideAMatch(t *testing.T) {
	lim := norm.DefaultLimits()
	lim.MaxTextBytes = 64

	res, err := norm.Normalise("text/plain", []byte(strings.Repeat("abc ", 15)+strings.Repeat(" ", 500)), lim)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Errorf("a body whose excess is whitespace was marked truncated: %+v", res)
	}

	res, err = norm.Normalise("text/plain", []byte("hello"+strings.Repeat("x", 200)), lim)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !res.TruncationAffectsRules || len(res.Text) > 64 {
		t.Errorf("a body over the text limit: %+v", res)
	}
}

func TestStructuredBodiesAreExtracted(t *testing.T) {
	res := normalise(t, "application/json; charset=utf-8",
		`{"messages":[{"role":"user","content":"my card is 4111 1111 1111 1111"}],"temperature":0.7,"stream":true}`)
	for _, want := range []string{"4111 1111 1111 1111", "temperature 0.7", "stream true"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("JSON extraction lost %q: %q", want, res.Text)
		}
	}
	res = normalise(t, "application/xml", `<?xml version="1.0"?><doc><p>patient 42</p><p>diagnosis</p></doc>`)
	if res.Text != "patient 42 diagnosis" {
		t.Errorf("XML extraction: %q", res.Text)
	}
	if res := normalise(t, "application/json", `{"unterminated": "my card`); !strings.Contains(res.Text, "my card") {
		t.Errorf("malformed JSON must be read as text: %q", res.Text)
	}
}

func TestStructuralLimitsTruncateAndMark(t *testing.T) {
	res := normalise(t, "application/json", strings.Repeat("[", 200)+`"deep"`+strings.Repeat("]", 200))
	if res.Pathological != "nesting" || !res.Truncated || !res.TruncationAffectsRules {
		t.Errorf("deep nesting: %+v", res)
	}
	lim := norm.DefaultLimits()
	lim.MaxTokens = 10
	res, err := norm.Normalise("application/json", []byte(`["a","b","c","d","e","f","g","h","i","j","k","l"]`), lim)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pathological != "token_budget" || !res.TruncationAffectsRules {
		t.Errorf("token budget: %+v", res)
	}
}

func TestNormalisationIsLinear(t *testing.T) {
	body := []byte(strings.Repeat("word \u00a0 \n", 100000))
	start := time.Now()
	res, err := norm.Normalise("text/plain", body, norm.Limits{MaxInputBytes: 4 << 20, MaxTextBytes: 4 << 20, MaxTokens: 1, MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "  ") || res.Text == "" {
		t.Fatalf("whitespace was not collapsed")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("normalising %d bytes took %v", len(body), took)
	}
}
