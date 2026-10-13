package canon

import (
	"strings"
	"testing"
)

// The digest layout is asserted as a literal string rather than through its hash: a hash mismatch
// would say "something changed", a layout mismatch says which separator moved.
func TestDigestInputLayoutIsLiteral(t *testing.T) {
	atts := []Attachment{{
		Name:          "/tmp/msa.pdf",
		MediaType:     "Application/PDF; charset=binary",
		SizeBytes:     184320,
		ContentDigest: "sha256:" + strings.Repeat("9f2c", 16),
	}}
	text := Text("Summarise   the attached contract.\r\n")
	canon := Attachments(atts)
	got := digestInput(text, canon)
	want := "sac-canon-1\x1f" + "T\x1f" +
		"Summarise the attached contract.\x1f" +
		"A\x1fmsa.pdf\x1fapplication/pdf\x1f184320\x1fsha256:" + strings.Repeat("9f2c", 16) + "\x1f" +
		"END"
	if got != want {
		t.Fatalf("digest input layout changed:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestCanonicalTextSteps(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"leading BOM is encoding, not content", "\ufeffhello", "hello"},
		{"NFC composes a decomposed sequence", "cafe\u0301", "caf\u00e9"},
		{"strips C0 controls", "a\x00b\x1fc", "abc"},
		{"strips C1 and DEL", "a\u007f\u0085b", "ab"},
		{"strips zero-width and bidi", "a\u200b\u200e\u202e\u2060b", "ab"},
		{"folds TAB, LF and CR", "a\tb\nc\rd", "a b c d"},
		{"collapses runs of whitespace", "a  \t\n   b", "a b"},
		{"handles NBSP and ideographic space", "a\u00a0\u3000b", "a b"},
		{"trims", "   padded   ", "padded"},
		{"does not case-fold", "Hello World", "Hello World"},
		{"does not strip punctuation", "a,b;c!", "a,b;c!"},
		{"empty stays empty", "", ""},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("%s: Text(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// After canonicalisation no field can contain U+001F, which is what makes the layout unambiguous
// without escaping.
func TestCanonicalTextSeparatorCannotSurvive(t *testing.T) {
	in := "a\x1fb\x1fc"
	if got := Text(in); strings.ContainsRune(got, 0x1f) {
		t.Fatalf("canonical text still contains U+001F: %q", got)
	}
}

func TestAttachments(t *testing.T) {
	atts := []Attachment{
		{Name: `C:\Users\kyle\msa.pdf`, MediaType: "APPLICATION/PDF; charset=utf-8", SizeBytes: 10, ContentDigest: "sha256:a"},
		{Name: "b.txt", MediaType: "", SizeBytes: 5},                                      // no media type -> octet-stream
		{Name: "a.txt", MediaType: "text/plain", SizeBytes: 7, ContentDigest: Unreadable}, // bytes unreadable
	}
	got := Attachments(atts)
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	// Sorted by (name, content_digest, size_bytes).
	if got[0].Name != "a.txt" || got[1].Name != "b.txt" || got[2].Name != "msa.pdf" {
		t.Fatalf("sort order = %q, %q, %q", got[0].Name, got[1].Name, got[2].Name)
	}
	if got[0].ContentDigest != Unreadable {
		t.Fatalf("unreadable bytes = %q, want %q", got[0].ContentDigest, Unreadable)
	}
	if got[1].MediaType != "application/octet-stream" {
		t.Fatalf("missing media type = %q, want application/octet-stream", got[1].MediaType)
	}
	if got[2].MediaType != "application/pdf" {
		t.Fatalf("media type parameters were not stripped: %q", got[2].MediaType)
	}
	if got[2].Name != "msa.pdf" {
		t.Fatalf("basename = %q (a Windows path must be basename'd too)", got[2].Name)
	}
}

// Same submission, two spellings: the digest must be identical, because that is what makes two
// observers collapse into one row rather than double-counting.
func TestDigestSameContentDifferentSpellingIsOneDigest(t *testing.T) {
	a := Text("Summarise   the attached contract.\r\n")
	b := Text("Summarise the attached contract.")
	if a != b {
		t.Fatalf("canonical forms differ: %q vs %q", a, b)
	}
	if Digest(a, nil) != Digest(b, nil) {
		t.Fatal("the same content produced two digests")
	}
	if Digest(a, nil) == Digest("Summarise the attached contract!", nil) {
		t.Fatal("different content produced the same digest")
	}
}

func TestDigestAttachmentReadabilityIsPartOfTheDigest(t *testing.T) {
	read := []Attachment{{Name: "msa.pdf", MediaType: "application/pdf", SizeBytes: 10, ContentDigest: "sha256:" + strings.Repeat("11", 32)}}
	unread := []Attachment{{Name: "msa.pdf", MediaType: "application/pdf", SizeBytes: 10, ContentDigest: Unreadable}}
	d1 := Digest("hello", read)
	d2 := Digest("hello", unread)
	if d1 == d2 {
		t.Fatal("readable and unreadable attachment bytes produced the same digest; the server could not tell a read attachment from an unread one")
	}
}
