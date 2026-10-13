package dedup

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeIllFormedBytesBecomeReplacement(t *testing.T) {
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

func TestNamesDigestUnreadableWhenNone(t *testing.T) {
	if got := NamesDigest(nil); got != Unreadable {
		t.Fatalf("names digest with no attachments = %q, want %q", got, Unreadable)
	}
	got := NamesDigest([]Attachment{{Name: "a.txt"}})
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("names digest = %q, want a sha256: value", got)
	}
}

// The bucket is floor(occurred_at / 300 s) on the device wall clock, uncorrected.
func TestBucketStart300Seconds(t *testing.T) {
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

func TestKeyTiersDifferAndRequireIdentity(t *testing.T) {
	at := time.Date(2026, 10, 2, 13, 7, 31, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("ab", 32)

	tierT, err := ContentKey("t1", "d1", "tool", "prompt", at, digest)
	if err != nil {
		t.Fatalf("ContentKey: %v", err)
	}
	tierS, err := SurrogateKey("t1", "d1", "tool", "prompt", at, 4096, nil)
	if err != nil {
		t.Fatalf("SurrogateKey: %v", err)
	}
	for name, k := range map[string]string{"T": tierT, "S": tierS} {
		if !strings.HasPrefix(k, "sha256:") || len(k) != len("sha256:")+64 {
			t.Fatalf("tier %s key = %q, want a sha256: value", name, k)
		}
	}
	if tierT == tierS {
		t.Fatal("tiers T and S produced the same key; the tier must be part of the input")
	}

	// Tenant, device and tool are in the key: the same text from two people or two devices is two
	// submissions.
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
	} {
		if _, err := c.fn(); err == nil {
			t.Errorf("%s: key was computed with a missing dimension", c.name)
		}
	}
}

func TestContentDigestLeavesThePathOut(t *testing.T) {
	named := []Attachment{{Name: "msa.pdf", MediaType: "application/pdf", SizeBytes: 10}}
	located := []Attachment{{Name: "msa.pdf", Path: `C:\Users\u\Contracts\msa.pdf`, MediaType: "application/pdf", SizeBytes: 10}}
	if ContentDigest("hello", named) != ContentDigest("hello", located) {
		t.Fatal("a route that saw where the file came from computed another digest than one that did not")
	}
	if NamesDigest(named) != NamesDigest(located) {
		t.Fatal("the path changed the names digest")
	}
}
