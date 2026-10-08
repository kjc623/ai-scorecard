package parsers

import (
	"errors"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
)

// The generic parser takes the last user-role message, or a top-level prompt.
func TestGenericLastUserTurn(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"text","text":"second"}]}]}`)
	res, err := Generic{}.Parse(body, "application/json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Text != "second" {
		t.Fatalf("text = %q, want the last user turn", res.Text)
	}
	if len(res.Attachments) != 0 {
		t.Fatalf("attachments = %v, want none", res.Attachments)
	}
	if _, err := (Generic{}).Parse([]byte(`{"nope":true}`), "application/json"); !errors.Is(err, ErrNoText) {
		t.Fatalf("err = %v: a body with no identifiable user-authored segment must not be guessed at", err)
	}
}

func TestGenericPromptAndInput(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"p","messages":[{"role":"user","content":"m"}]}`: "p",
		`{"input":"i"}`: "i",
		`{"input":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}`: "a b",
	}
	for body, want := range cases {
		res, err := Generic{}.Parse([]byte(body), "application/json")
		if err != nil || res.Text != want {
			t.Errorf("%s: text = %q, err = %v, want %q", body, res.Text, err, want)
		}
	}
	if _, err := (Generic{}).Parse(nil, ""); !errors.Is(err, ErrNoText) {
		t.Fatalf("empty body: err = %v", err)
	}
}

// The generic parser feeds the same dedup.ContentDigest the pipeline uses, so the two cannot drift.
func TestGenericExtractionFeedsCanonicalDigest(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	text, atts, err := NewRegistry().For("127.0.0.1:11434", "/api/chat").Extract(body, "application/json")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if d1, d2 := dedup.ContentDigest(text, atts), dedup.ContentDigest("hello world", nil); d1 != d2 {
		t.Fatalf("digest through the extractor = %s, want %s", d1, d2)
	}
}

type stubParser struct {
	host  string
	parse func([]byte) (Result, error)
}

func (s stubParser) Match(host, _ string) bool { return host == s.host }
func (s stubParser) Version() string           { return "test" }
func (s stubParser) Parse(b []byte, _ string) (Result, error) {
	return s.parse(b)
}
func (s stubParser) BlockResponse(string, string) (int, string, []byte) { return 403, "", nil }

func TestLookupNormalisesTheHostAndFallsBack(t *testing.T) {
	stub := stubParser{host: "api.example.com"}
	r := NewRegistry(stub)
	for _, h := range []string{"api.example.com", "API.Example.com:443", "api.example.com."} {
		if _, ok := r.Lookup(h, "/").(stubParser); !ok {
			t.Errorf("Lookup(%q) did not choose the matching parser", h)
		}
	}
	if _, ok := r.Lookup("other.example.com", "/").(Generic); !ok {
		t.Error("an unmatched host did not fall back to the generic parser")
	}
}

// A panicking parser is recovered, the request is read by the generic parser, and the
// extraction says so for the route to count.
func TestPanickingParserFallsBackToGeneric(t *testing.T) {
	r := NewRegistry(stubParser{host: "api.example.com", parse: func([]byte) (Result, error) { panic("boom") }})
	x := r.For("api.example.com", "/v1/x")
	text, _, err := x.Extract([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), "application/json")
	if err != nil || text != "hello" {
		t.Fatalf("text = %q, err = %v, want the generic parser's reading", text, err)
	}
	if !x.Panicked() {
		t.Fatal("the panic was not reported")
	}
	if x.UnknownShape() {
		t.Fatal("a panic is not an unknown shape")
	}
}

func TestUnknownShapeIsReported(t *testing.T) {
	r := NewRegistry(stubParser{host: "api.example.com", parse: func([]byte) (Result, error) {
		return Result{}, UnknownShape("stub", "no messages")
	}})
	x := r.For("api.example.com", "/v1/x")
	if _, _, err := x.Extract([]byte(`{}`), "application/json"); !errors.Is(err, ErrUnknownShape) {
		t.Fatalf("err = %v, want ErrUnknownShape", err)
	}
	if !x.UnknownShape() || x.Panicked() {
		t.Fatalf("unknown = %v, panicked = %v", x.UnknownShape(), x.Panicked())
	}
	// The generic parser's failure is not a format change.
	g := r.For("other.example.com", "/")
	if _, _, err := g.Extract([]byte(`{}`), "application/json"); !errors.Is(err, ErrNoText) || g.UnknownShape() {
		t.Fatalf("generic: err = %v, unknown = %v", err, g.UnknownShape())
	}
}

// A destination of no known target is answered in plain text: the message, then the link.
func TestGenericBlockResponseIsPlainText(t *testing.T) {
	status, ctype, body := Generic{}.BlockResponse("Remove the credential and try again.", "https://intranet.example/ai")
	if status != 403 || ctype != "text/plain; charset=utf-8" {
		t.Fatalf("status, Content-Type = %d, %q", status, ctype)
	}
	if string(body) != "Remove the credential and try again. https://intranet.example/ai" {
		t.Fatalf("body = %q", body)
	}
	if _, _, body := (Generic{}).BlockResponse("Not here.", ""); string(body) != "Not here." {
		t.Fatalf("body without a link = %q", body)
	}
}
