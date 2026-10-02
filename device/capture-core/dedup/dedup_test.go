package dedup

import (
	"strings"
	"testing"
	"time"
)

// The C9 layout is normative and shared with ingest, so it is asserted as a literal string
// rather than through its hash: a hash mismatch would say "something changed", a layout
// mismatch says which separator moved.
func TestDigestInput_C9LayoutIsLiteral(t *testing.T) {
	atts := []Attachment{{
		Name:          "/tmp/msa.pdf",
		MediaType:     "Application/PDF; charset=binary",
		SizeBytes:     184320,
		ContentDigest: "sha256:" + strings.Repeat("9f2c", 16),
	}}
	text := CanonicalText("Summarise   the attached contract.\r\n", IdentityNFC{})
	canon := CanonicalAttachments(atts, IdentityNFC{})
	got := digestInput(text, canon)
	want := "sac-canon-1\x1f" + "T\x1f" +
		"Summarise the attached contract.\x1f" +
		"A\x1fmsa.pdf\x1fapplication/pdf\x1f184320\x1fsha256:" + strings.Repeat("9f2c", 16) + "\x1f" +
		"END"
	if got != want {
		t.Fatalf("digest input layout changed:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestCanonicalText_C2ToC6(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"C2 leading BOM is encoding, not content", "\ufeffhello", "hello"},
		{"C4 strips C0 controls", "a\x00b\x1fc", "abc"},
		{"C4 strips C1 and DEL", "a\u007f\u0085b", "ab"},
		{"C4 strips zero-width and bidi", "a\u200b\u200e\u202e\u2060b", "ab"},
		{"C4 keeps TAB/LF/CR for C5 to fold", "a\tb\nc\rd", "a b c d"},
		{"C5 collapses runs of whitespace", "a  \t\n   b", "a b"},
		{"C5 handles NBSP and ideographic space", "a\u00a0\u3000b", "a b"},
		{"C5 trims", "   padded   ", "padded"},
		{"C6 does not case-fold", "Hello World", "Hello World"},
		{"C6 does not strip punctuation", "a,b;c!", "a,b;c!"},
		{"empty stays empty", "", ""},
	}
	for _, c := range cases {
		if got := CanonicalText(c.in, IdentityNFC{}); got != c.want {
			t.Errorf("%s: CanonicalText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// The separator guarantee C4 provides: after canonicalisation no field can contain U+001F, which
// is what makes the C9 layout unambiguous without escaping.
func TestCanonicalText_SeparatorCannotSurvive(t *testing.T) {
	in := "a\x1fb\x1fc"
	if got := CanonicalText(in, IdentityNFC{}); strings.ContainsRune(got, 0x1f) {
		t.Fatalf("canonical text still contains U+001F: %q", got)
	}
}

func TestDecode_IllFormedBytesBecomeReplacement(t *testing.T) {
	got := Decode([]byte{0xff, 0xfe, 'a'})
	if !strings.Contains(got, "a") {
		t.Fatalf("Decode dropped the valid rune: %q", got)
	}
	if got == "" {
		t.Fatal("Decode returned an empty string for non-empty input")
	}
	// Two different ill-formed inputs must not collapse onto two different results for the same
	// bytes: the replacement is deterministic.
	if Decode([]byte{0xff}) != Decode([]byte{0xff}) {
		t.Fatal("Decode is not deterministic")
	}
}

func TestCanonicalAttachments_C7(t *testing.T) {
	atts := []Attachment{
		{Name: `C:\Users\kyle\msa.pdf`, MediaType: "APPLICATION/PDF; charset=utf-8", SizeBytes: 10, ContentDigest: "sha256:a"},
		{Name: "b.txt", MediaType: "", SizeBytes: 5},                                      // no media type -> octet-stream
		{Name: "a.txt", MediaType: "text/plain", SizeBytes: 7, ContentDigest: Unreadable}, // bytes unreadable
	}
	got := CanonicalAttachments(atts, IdentityNFC{})
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

func TestNamesDigest_UnreadableWhenNone(t *testing.T) {
	if got := NamesDigest(nil, IdentityNFC{}); got != Unreadable {
		t.Fatalf("names digest with no attachments = %q, want %q", got, Unreadable)
	}
	got := NamesDigest([]Attachment{{Name: "a.txt"}}, IdentityNFC{})
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("names digest = %q, want a sha256: value", got)
	}
}

// The 300 s bucket is floor(occurred_at / 300 s) on the device wall clock, uncorrected (§4.3).
func TestBucketStart_300Seconds(t *testing.T) {
	at := time.Date(2026, 10, 2, 13, 7, 31, 0, time.UTC)
	got := BucketStart(at)
	want := time.Date(2026, 10, 2, 13, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("bucket = %v, want %v", got, want)
	}
	// Two observations 2 s apart in the same bucket collide; one crossing the boundary does not.
	if BucketStart(at.Add(2*time.Second)) != got {
		t.Fatal("observations 2 s apart landed in different buckets")
	}
	if BucketStart(at.Add(5*time.Minute)) == got {
		t.Fatal("observations 5 minutes apart landed in the same bucket")
	}
}

func TestKeyLadder_TiersDifferAndRequireIdentity(t *testing.T) {
	at := time.Date(2026, 10, 2, 13, 7, 31, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("ab", 32)

	tierT, err := ContentKey("t1", "d1", "tool", "prompt", at, digest)
	if err != nil {
		t.Fatalf("ContentKey: %v", err)
	}
	tierS, err := SurrogateKey("t1", "d1", "tool", "prompt", at, 4096, nil, IdentityNFC{})
	if err != nil {
		t.Fatalf("SurrogateKey: %v", err)
	}
	start := BucketStart(at)
	tierR, err := RollupKey("t1", "d1", "tool", "usage_rollup", start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("RollupKey: %v", err)
	}
	tierD, err := DetectionKey("t1", "d1", "tool", "model_detection", at, "process_scan")
	if err != nil {
		t.Fatalf("DetectionKey: %v", err)
	}
	seen := map[string]string{}
	for name, k := range map[string]string{"T": tierT, "S": tierS, "R": tierR, "D": tierD} {
		if !strings.HasPrefix(k, "sha256:") || len(k) != len("sha256:")+64 {
			t.Fatalf("tier %s key = %q, want a sha256: value", name, k)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("tiers %s and %s produced the same key; the tier must be part of the input", prev, name)
		}
		seen[k] = name
	}

	// Tenant, device and tool are in the key: the same text from two people or two devices is two
	// submissions (brief §4.1).
	other, _ := ContentKey("t2", "d1", "tool", "prompt", at, digest)
	if other == tierT {
		t.Fatal("two tenants produced the same key")
	}
	other, _ = ContentKey("t1", "d2", "tool", "prompt", at, digest)
	if other == tierT {
		t.Fatal("two devices produced the same key")
	}

	// A key with a missing dimension is refused rather than computed: it would merge submissions.
	for _, c := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"no tenant", func() (string, error) { return ContentKey("", "d1", "tool", "prompt", at, digest) }},
		{"no device", func() (string, error) { return ContentKey("t1", "", "tool", "prompt", at, digest) }},
		{"no tool", func() (string, error) { return ContentKey("t1", "d1", "", "prompt", at, digest) }},
		{"no digest", func() (string, error) { return ContentKey("t1", "d1", "tool", "prompt", at, "") }},
		{"no basis", func() (string, error) { return DetectionKey("t1", "d1", "tool", "model_detection", at, "") }},
	} {
		if _, err := c.fn(); err == nil {
			t.Errorf("%s: key was computed with a missing dimension", c.name)
		}
	}
}

// Same submission, two spellings: the digest must be identical, because that is what makes two
// routes collapse into one row (R9) rather than double-counting.
func TestContentDigest_SameContentDifferentSpellingIsOneDigest(t *testing.T) {
	a := CanonicalText("Summarise   the attached contract.\r\n", IdentityNFC{})
	b := CanonicalText("Summarise the attached contract.", IdentityNFC{})
	if a != b {
		t.Fatalf("canonical forms differ: %q vs %q", a, b)
	}
	if ContentDigest(a, nil, IdentityNFC{}) != ContentDigest(b, nil, IdentityNFC{}) {
		t.Fatal("the same content produced two digests")
	}
	if ContentDigest(a, nil, IdentityNFC{}) == ContentDigest("Summarise the attached contract!", nil, IdentityNFC{}) {
		t.Fatal("different content produced the same digest")
	}
}

func TestContentDigest_AttachmentTierIsPartOfTheDigest(t *testing.T) {
	read := []Attachment{{Name: "msa.pdf", MediaType: "application/pdf", SizeBytes: 10, ContentDigest: "sha256:" + strings.Repeat("11", 32)}}
	unread := []Attachment{{Name: "msa.pdf", MediaType: "application/pdf", SizeBytes: 10, ContentDigest: Unreadable}}
	d1 := ContentDigest("hello", read, IdentityNFC{})
	d2 := ContentDigest("hello", unread, IdentityNFC{})
	if d1 == d2 {
		t.Fatal("readable and unreadable attachment bytes produced the same digest; ingest could not tell T-A from T-B")
	}
}
