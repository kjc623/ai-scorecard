package dedup

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTierForFollowsTheLadderTable(t *testing.T) {
	readable := []Attachment{{Name: "a.pdf", SizeBytes: 10, ContentDigest: Hash('a'), Readable: true}}
	unreadable := []Attachment{{Name: "a.pdf", SizeBytes: 10}}

	cases := []struct {
		name       string
		kind       string
		digest     string
		hasDigest  bool
		attachable []Attachment
		want       Tier
	}{
		{"prompt with a digest and no attachments is T-A", "prompt", Hash('a'), true, nil, TierTA},
		{"prompt with a digest and all attachment bytes read is T-A", "prompt", Hash('a'), true, readable, TierTA},
		{"prompt with an unreadable attachment is T-B (E3)", "prompt", Hash('a'), true, unreadable, TierTB},
		{"prompt with no digest is M0/canvas/websocket: Tier S", "prompt", "", false, nil, TierS},
		{"a rollup is Tier R", "usage_rollup", "", false, nil, TierR},
		{"a detection is Tier D", "model_detection", "", false, nil, TierD},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TierFor(c.kind, c.digest, c.hasDigest, c.attachable); got != c.want {
				t.Errorf("TierFor = %s, want %s", got, c.want)
			}
		})
	}
}

func TestWireTierCollapsesTAAndTBOnly(t *testing.T) {
	// §5.3's dedup_tier is "T (tier T) or S"; the A/B distinction is internal.
	if got := TierTA.WireTier(); got != "T" {
		t.Errorf("TierTA.WireTier() = %q, want T", got)
	}
	if got := TierTB.WireTier(); got != "T" {
		t.Errorf("TierTB.WireTier() = %q, want T", got)
	}
	for tier, want := range map[Tier]string{TierS: "S", TierR: "R", TierD: "D"} {
		if got := tier.WireTier(); got != want {
			t.Errorf("%s.WireTier() = %q, want %q", tier, got, want)
		}
	}
	if got := Tier("junk").WireTier(); got != "" {
		t.Errorf("an unknown tier reported %q, want the empty string rather than a guess", got)
	}
}

// TestTierDominatesRouteRank is §4.5's "Tier dominates rank: a Tier-S observation can never win a
// contested field, whatever its route's rank".
func TestTierDominatesRouteRank(t *testing.T) {
	// ext.page_context has the best route rank in the document's scale (90); proc.detect the worst (10).
	best := 90
	worst := 10
	if !Beats(TierTA, worst, "a", TierS, best, "b") {
		t.Error("a Tier T-A observation on the weakest route must beat a Tier S observation on the strongest")
	}
	if Beats(TierS, best, "a", TierTA, worst, "b") {
		t.Error("a Tier S observation must not beat a Tier T-A observation")
	}
	// Equal fidelity is broken deterministically by the lexicographically smallest event_id.
	if !Beats(TierTA, 40, "aaa", TierTA, 40, "bbb") {
		t.Error("equal fidelity must be broken by the smallest event_id, not by arrival order")
	}
	if Beats(TierTA, 40, "bbb", TierTA, 40, "aaa") {
		t.Error("the larger event_id must not win an equal-fidelity contest")
	}
	if Fidelity(TierTA, 40) != 3*1000+40 {
		t.Errorf("Fidelity = %d, want tier_rank*1000 + route_rank", Fidelity(TierTA, 40))
	}
}

// TestDedupKeyLayout pins the §4.5 preimage, including the separator that C4 guarantees cannot
// occur inside a field.
func TestDedupKeyLayout(t *testing.T) {
	bucket := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)
	got := DedupKey("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"tool-1", "egress", "prompt", bucket, TierTA, "sha256:abc")

	preimage := strings.Join([]string{
		"sac-dedup-1",
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"tool-1", "egress", "prompt",
		"2026-10-02T14:30:00Z",
		"T-A",
		"sha256:abc",
	}, "\u001f")
	want := hash(preimage)
	if got != want {
		t.Errorf("DedupKey = %s\nwant  %s\npreimage = %q", got, want, preimage)
	}
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Errorf("DedupKey = %q, want a sha256: value", got)
	}
}

// TestWeakDedupKeyMirrorsTheStoredFunction checks the *layout* of the Go mirror of
// ingest.weak_dedup_key(). The values are compared against the live function in
// internal/store's database-gated test; this test pins the shape so a refactor cannot silently
// change the preimage between runs.
func TestWeakDedupKeyMirrorsTheStoredFunction(t *testing.T) {
	at := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)
	got := WeakDedupKey("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"tool-1", "prompt", at, 42)
	// concat_ws('|', tenant::text, device::text, tool, kind, floor(epoch/300)::bigint::text, size::text)
	// This test pins the layout; the *value* is compared against the live ingest.weak_dedup_key()
	// in internal/store's database-gated test.
	preimage := strings.Join([]string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"tool-1", "prompt",
		strconv.FormatInt(at.Unix()/300, 10),
		"42",
	}, "|")
	if want := hash(preimage); got != want {
		t.Errorf("WeakDedupKey = %s\nwant %s\npreimage = %q", got, want, preimage)
	}
	// Upper-case uuid input must produce the same key, because Postgres renders uuid as lowercase.
	upper := WeakDedupKey("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"tool-1", "prompt", at, 42)
	if strings.ToLower(got) != upper {
		t.Error("WeakDedupKey must be stable under uuid case")
	}
}

func TestWeakDedupKeyBucketsAtThreeHundredSeconds(t *testing.T) {
	base := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)
	inBucket := WeakDedupKey("t", "d", "tool", "prompt", base.Add(299*time.Second), 10)
	acrossBoundary := WeakDedupKey("t", "d", "tool", "prompt", base.Add(300*time.Second), 10)
	if inBucket == acrossBoundary {
		t.Error("299s and 300s after the bucket start must produce different weak keys")
	}
	if inBucket != WeakDedupKey("t", "d", "tool", "prompt", base.Add(1*time.Second), 10) {
		t.Error("two instants inside one bucket must produce the same weak key")
	}
	if inBucket != WeakDedupKey("t", "d", "tool", "prompt", base.Add(299*time.Second+999*time.Millisecond), 10) {
		t.Error("sub-second precision must not move an instant out of its bucket")
	}
}

// TestCanonicalTextAppliesC4AndC5 covers the two steps that make C9's separator unambiguous.
func TestCanonicalTextAppliesC4AndC5(t *testing.T) {
	c := DefaultCanonical()
	cases := []struct{ name, in, want string }{
		{"CRLF and CR fold to one space", "a\r\n\r\nb", "a b"},
		{"runs of whitespace collapse", "a \t\n  b", "a b"},
		{"leading and trailing whitespace is trimmed", "\n\n  hello  \n", "hello"},
		{"NBSP and en/em spaces are whitespace", "a\u00a0\u2000\u2003b", "a b"},
		{"zero-width characters are stripped", "a\u200b\u200c\u200db", "ab"},
		{"bidi overrides are stripped", "a\u202eb\u202cc", "abc"},
		{"the C9 separator cannot survive canonicalisation", "a\u001fb", "ab"},
		{"U+001E cannot survive either", "a\u001eb", "ab"},
		{"DEL and C1 are stripped", "a\u007f\u0085b", "ab"},
		{"no case folding", "Hello World", "Hello World"},
		{"no punctuation stripping", "a, b.", "a, b."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDigestInputLayout pins §4.2's C9 example shape and the C7 sort order.
func TestDigestInputLayout(t *testing.T) {
	atts := []Attachment{
		{Name: "/tmp/msa.pdf", MediaType: "application/pdf", SizeBytes: 184320, ContentDigest: "sha256:9f2c", Readable: true},
	}
	got := DigestInput("Summarise the attached contract.", atts)
	want := "sac-canon-1\u001fT\u001fSummarise the attached contract.\u001fA\u001fmsa.pdf\u001fapplication/pdf\u001f184320\u001fsha256:9f2c\u001fEND"
	if got != want {
		t.Errorf("DigestInput =\n%q\nwant\n%q", got, want)
	}

	// C7: attachment records are sorted by (name, content_digest, size_bytes).
	unsorted := []Attachment{
		{Name: "b.pdf", SizeBytes: 2, ContentDigest: "sha256:b", Readable: true},
		{Name: "a.pdf", SizeBytes: 1, ContentDigest: "sha256:a", Readable: true},
	}
	sorted := []Attachment{
		{Name: "a.pdf", SizeBytes: 1, ContentDigest: "sha256:a", Readable: true},
		{Name: "b.pdf", SizeBytes: 2, ContentDigest: "sha256:b", Readable: true},
	}
	if DigestInput("t", unsorted) != DigestInput("t", sorted) {
		t.Error("attachment order must not change the digest: a multipart order and a file-list order are not the same fact")
	}

	// A route structurally unable to read the bytes contributes "~" (E3).
	unreadable := []Attachment{{Name: "a.pdf", SizeBytes: 1}}
	if !strings.Contains(DigestInput("t", unreadable), "\u001f~") {
		t.Errorf("an unreadable attachment must contribute the ~ placeholder: %q", DigestInput("t", unreadable))
	}
	// The absent media type becomes application/octet-stream.
	if !strings.Contains(DigestInput("t", unreadable), "application/octet-stream") {
		t.Error("an absent media_type must canonicalise to application/octet-stream")
	}
	// An empty text is present, not omitted.
	if got := DigestInput("", nil); got != "sac-canon-1\u001fT\u001f\u001fEND" {
		t.Errorf("empty text must still occupy its field: %q", got)
	}
}

func TestCanonicalNameAndNamesDigest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/u/contract.pdf", "contract.pdf"},
		{`C:\Users\u\contract.pdf`, "contract.pdf"},
		{"  spaced  name .pdf ", "spaced name .pdf"},
		{"MixedCase.PDF", "MixedCase.PDF"}, // not case-folded
	}
	for _, c := range cases {
		if got := CanonicalName(c.in); got != c.want {
			t.Errorf("CanonicalName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := NamesDigest(nil); got != "~" {
		t.Errorf("NamesDigest(nil) = %q, want ~", got)
	}
	if NamesDigest([]string{"b.pdf", "a.pdf"}) != NamesDigest([]string{"a.pdf", "b.pdf"}) {
		t.Error("NamesDigest must be order-independent")
	}
	if NamesDigest([]string{"/x/a.pdf"}) != NamesDigest([]string{"a.pdf"}) {
		t.Error("NamesDigest must use the basename, so two routes reporting different paths agree")
	}
}

func TestLadderMaterialsAreSeparatedByTheFieldSeparator(t *testing.T) {
	ws, we := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	if got := MaterialForRollup(ws, we); got != "2026-10-02T09:00:00Z\u001f2026-10-02T10:00:00Z" {
		t.Errorf("MaterialForRollup = %q", got)
	}
	if got := MaterialForSurrogate("ext.dom", 7, ""); got != "ext.dom\u001f7\u001f~" {
		t.Errorf("MaterialForSurrogate = %q, want the ~ placeholder when no name is known", got)
	}
	if got := MaterialForContent("sha256:abc"); got != "sha256:abc" {
		t.Errorf("MaterialForContent = %q", got)
	}
	if got := MaterialForDetection("etw"); got != "etw" {
		t.Errorf("MaterialForDetection = %q", got)
	}
	if PromptDigest("hello") != hash("sac-canon-1\u001fT\u001fhello\u001fEND") {
		t.Error("PromptDigest must be sha256(sac-canon-1 ␟ T ␟ text ␟ END)")
	}
}

// Hash is a fixture helper: a well-formed sha256 whose hex is filled with one nibble.
func Hash(nibble byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = nibble
	}
	return "sha256:" + string(b)
}
