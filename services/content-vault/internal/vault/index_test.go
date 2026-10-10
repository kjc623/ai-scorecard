package vault

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shadow-ai-capture/content-vault/internal/store"
)

func TestIndexUnits(t *testing.T) {
	cases := []struct {
		name, content, prompt string
		names                 []string
	}{
		{"plain text", "  what is our refund policy?  ", "what is our refund policy?", nil},
		{"injected context removed", "<system-reminder>\nrules\n</system-reminder>\nfix the build", "fix the build", nil},
		{"latest user message of a body", `{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"a"},{"role":"user","content":"new"}]}`, "new", nil},
		{"text blocks only", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"},{"type":"text","text":"typed"}]}]}`, "typed", nil},
		{"a ChatGPT web conversation", `{"action":"next","messages":[{"id":"m1","author":{"role":"user"},"content":{"content_type":"text","parts":["what is our refund policy?"]},"metadata":{}}],"parent_message_id":"client-created-root","model":"auto"}`, "what is our refund policy?", nil},
		{"a ChatGPT message with an image", `{"messages":[{"author":{"role":"user"},"content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://f"},"describe this chart"]}}]}`, "describe this chart", nil},
		{"a ChatGPT message from the assistant", `{"messages":[{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["hi"]}}]}`, "", nil},
		{"a body nobody typed", `{"events":[{"name":"telemetry"}]}`, "", nil},
		{"an assistant-only body", `{"messages":[{"role":"assistant","content":"hi"}]}`, "", nil},
		{"structured object", `{"prompt":"review this","attachments":[{"name":" a.pdf "},{"name":""},{"size_bytes":3}]}`, "review this", []string{"a.pdf"}},
		{"structured object with a body prompt", `{"prompt":"{\"messages\":[{\"role\":\"user\",\"content\":\"inner\"}]}","attachments":[]}`, "inner", nil},
		{"invalid JSON is text", `{"messages": [`, `{"messages": [`, nil},
		{"NUL and invalid UTF-8", "a\x00b\xffc", "a b�c", nil},
	}
	for _, tc := range cases {
		prompt, names := indexUnits([]byte(tc.content))
		if prompt != tc.prompt || strings.Join(names, "|") != strings.Join(tc.names, "|") {
			t.Errorf("%s: indexUnits = %q, %q; want %q, %q", tc.name, prompt, names, tc.prompt, tc.names)
		}
	}
}

func TestIndexedTextIsBounded(t *testing.T) {
	long := strings.Repeat("é", maxIndexedRunes+10)
	prompt, _ := indexUnits([]byte(long))
	if n := utf8.RuneCountInString(prompt); n != maxIndexedRunes || !utf8.ValidString(prompt) {
		t.Fatalf("prompt is %d runes (valid %v), want %d", n, utf8.ValidString(prompt), maxIndexedRunes)
	}
	var b strings.Builder
	b.WriteString(`{"attachments":[`)
	for i := range maxAttachmentNames + 5 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"f.txt"}`)
	}
	b.WriteString(`]}`)
	if _, names := indexUnits([]byte(b.String())); len(names) != maxAttachmentNames {
		t.Fatalf("%d names indexed, want at most %d", len(names), maxAttachmentNames)
	}
}

func TestSearchText(t *testing.T) {
	cases := []struct {
		form        store.SearchForm
		query, want string
	}{
		{store.FormTerms, "Contract renewal", "contract & renewal"},
		{store.FormTerms, `"board pack" Q3`, "board <-> pack & q3"},
		{store.FormTerms, "a AND b or NOT c", "a & b & c"},
		{store.FormTerms, "x:* | !y & (z)", "x & y & z"},
		{store.FormSubstring, "Q3_50%", `Q3\_50\%`},
		{store.FormSubstring, `a\b`, `a\\b`},
		{store.FormFuzzy, "contrct.pdf", "contrct.pdf"},
	}
	for _, tc := range cases {
		got, err := searchText(tc.form, tc.query)
		if err != nil || got != tc.want {
			t.Errorf("searchText(%s, %q) = %q, %v; want %q", tc.form, tc.query, got, err, tc.want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := store.SearchCursor{SubmissionID: "a0000000-0000-4000-8000-000000000001", UnitKind: store.UnitPromptBody, UnitIndex: 2}
	c.ReceivedAt = c.ReceivedAt.AddDate(2026, 0, 0)
	got, err := decodeCursor(encodeCursor(c))
	if err != nil || *got != c {
		t.Fatalf("cursor round trip = %+v, %v", got, err)
	}
	for _, bad := range []string{"%%%", "e30", encodeCursor(store.SearchCursor{SubmissionID: "x", UnitKind: "prompt_body"})} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) accepted a bad cursor", bad)
		}
	}
}
