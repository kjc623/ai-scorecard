// Package dedup implements docs/02-ingest-and-transport.md §4, the normative deduplication
// specification, in the parts that are reachable from the ingest service.
//
// What is reachable and what is not, stated plainly so nothing here is mistaken for more than it is:
//
//   - §4.3 (time bucket), §4.4 (material tier and route ranking) and §4.5 (the key ladder) are
//     fully implemented and tested.
//   - §4.2's canonicalisation of *text* (sac-canon-1) is implemented except for the NFC step, which
//     needs golang.org/x/text/unicode/norm; this build is offline (GOPROXY=off) and the standard
//     library has no normaliser. See Canonical: the limitation is explicit and the default
//     normaliser is the identity.
//   - The result of §4.2 never appears on the wire: the envelope carries the finished
//     `content_digest`, not the text it was computed from, so the service cannot and does not
//     recompute it. What the service does with this package is recompute the *tier* (which §4.5
//     requires ingest to recompute because it is not a wire field), derive the ladder keys, and
//     offer a diagnostic comparison against the device-supplied dedup_key.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Version prefixes. §4.2 and §4.5 make these versioned contracts: changing either splits the fleet
// into two digest populations, so a bump is a schema change with a conformance test.
const (
	CanonVersion = "sac-canon-1"
	KeyVersion   = "sac-dedup-1"
	WeakVersion  = "sac-l1"
)

// BucketWidth is the §4.3 time bucket width.
const BucketWidth = 300 * time.Second

// BucketStart returns floor(occurred_at_utc / 300s) × 300s, in UTC. The clock is the *device's*
// own wall clock, uncorrected: the bucket is about one device's observation of one event, so a
// skewed device still collides its own routes together (§4.3).
func BucketStart(occurredAt time.Time) time.Time {
	idx := BucketIndex(occurredAt)
	return time.Unix(idx*int64(BucketWidth/time.Second), 0).UTC()
}

// BucketIndex is the bucket ordinal, including for pre-epoch instants (floor, not truncation).
func BucketIndex(occurredAt time.Time) int64 {
	secs := occurredAt.UTC().Unix()
	return int64(math.Floor(float64(secs) / BucketWidth.Seconds()))
}

// Tier is the material tier of §4.5. The tier is recomputed at ingest because it is not a wire
// field: the envelope is frozen, so a device cannot assert it.
type Tier string

const (
	TierTA Tier = "T-A" // text + all attachment content digests, or no attachments
	TierTB Tier = "T-B" // text, one or more attachments unreadable ("~")
	TierS  Tier = "S"   // no canonical text; surrogate material only
	TierR  Tier = "R"   // rollup window
	TierD  Tier = "D"   // detection: no payload at all
)

// WireTier is the single-letter form the batch response reports (§5.3 `dedup_tier`: "T (tier T) or
// S"), which cannot distinguish T-A from T-B. The distinction is kept internally because it is the
// difference between an exact and a text-ladder merge (§4.5).
func (t Tier) WireTier() string {
	switch t {
	case TierTA, TierTB:
		return "T"
	case TierS:
		return "S"
	case TierR:
		return "R"
	case TierD:
		return "D"
	}
	return ""
}

// TierRank is §4.4's material order. Tier dominates route rank: a Tier-S observation can never win
// a contested field, whatever its route's rank.
func (t Tier) Rank() int {
	switch t {
	case TierR:
		return 4
	case TierTA:
		return 3
	case TierTB:
		return 2
	case TierS:
		return 1
	case TierD:
		return 0
	}
	return -1
}

// Attachment is an attachment descriptor as it appears on the wire. Readable is false when the
// route could not obtain the bytes; §4.2 C7 then uses the literal "~" as its content digest, which
// is what separates Tier T-A from Tier T-B.
type Attachment struct {
	Name          string
	MediaType     string
	SizeBytes     int64
	ContentDigest string
	Readable      bool
}

// TierFor recomputes the tier of one observation, per the §4.5 table.
func TierFor(kind string, contentDigest string, hasContentDigest bool, attachments []Attachment) Tier {
	switch kind {
	case "usage_rollup":
		return TierR
	case "model_detection":
		return TierD
	case "prompt":
		if !hasContentDigest || contentDigest == "" {
			return TierS
		}
		for _, a := range attachments {
			if !a.Readable || a.ContentDigest == "" || a.ContentDigest == "~" {
				return TierTB
			}
		}
		return TierTA
	}
	return TierS
}

// Fidelity is §4.4's single stored integer: tier_rank × 1000 + route_rank. It exists so that a
// route which cannot read attachment bytes never outranks one that can, whatever their ranks.
//
// routeRank is the *document's* §4.4 rank (higher is better), not ref.route_fidelity's stored rank
// (lower is better, seeded 10..70). Both are kept apart deliberately: the stored rank is what
// ingest.record_event() compares, and this composite is the specification's ordering.
func Fidelity(t Tier, routeRank int) int { return t.Rank()*1000 + routeRank }

// Beats reports whether observation a should win a contested field against b, per §4.4 and §4.5
// ("tier dominates rank"), with equal fidelity broken deterministically by the lexicographically
// smallest event_id.
//
// This is the *specified* ordering. The authoritative merge decision is the store's: nothing in
// the request path uses Beats to decide a merge, and the service reports what the store decided.
func Beats(aTier Tier, aRank int, aEventID string, bTier Tier, bRank int, bEventID string) bool {
	af, bf := Fidelity(aTier, aRank), Fidelity(bTier, bRank)
	if af != bf {
		return af > bf
	}
	return aEventID < bEventID
}

// DedupKey is §4.5's key ladder: "sac-dedup-1" ␟ tenant ␟ device ␟ tool ␟ direction ␟ kind ␟
// bucket_start ␟ tier ␟ material. §4.2 C9's separator guarantee applies: U+001F cannot occur in
// any field, because the canonicaliser strips control characters.
func DedupKey(tenant, device, tool, direction, kind string, bucketStart time.Time, t Tier, material string) string {
	parts := []string{
		KeyVersion, strings.ToLower(tenant), strings.ToLower(device), tool, direction, kind,
		RFC3339UTC(bucketStart), string(t), material,
	}
	return hash(strings.Join(parts, sep))
}

// WeakDedupKey mirrors db/schema.sql's ingest.weak_dedup_key() exactly: sha256 over the tenant,
// device, tool, kind, the 300-second bucket ordinal and the payload size, joined by '|'.
//
// It is duplicated in Go only so the in-memory store used by tests decides the ladder the way the
// database does. The integration test compares this function against the live
// ingest.weak_dedup_key() so the mirror is *checked*, not assumed. Nothing in the request path
// derives a weak key that the store would use for a write: the stored procedure computes its own.
func WeakDedupKey(tenant, device, tool, kind string, occurredAt time.Time, sizeBytes int64) string {
	bucket := strconv.FormatInt(BucketIndex(occurredAt), 10)
	joined := strings.Join([]string{
		strings.ToLower(tenant), strings.ToLower(device), tool, kind, bucket, strconv.FormatInt(sizeBytes, 10),
	}, "|")
	sum := sha256.Sum256([]byte(joined))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MaterialForContent is Tier T's ladder material: the normalised content digest itself.
func MaterialForContent(contentDigest string) string { return contentDigest }

// MaterialForSurrogate is Tier S's ladder material. "~" stands in for the names digest when no
// attachment name is known, per §4.5.
func MaterialForSurrogate(route string, sizeBytes int64, namesDigest string) string {
	if namesDigest == "" {
		namesDigest = "~"
	}
	return strings.Join([]string{route, strconv.FormatInt(sizeBytes, 10), namesDigest}, sep)
}

// MaterialForRollup is Tier R's ladder material.
func MaterialForRollup(windowStart, windowEnd time.Time) string {
	return strings.Join([]string{RFC3339UTC(windowStart), RFC3339UTC(windowEnd)}, sep)
}

// MaterialForDetection is Tier D's ladder material.
func MaterialForDetection(detectionBasis string) string { return detectionBasis }

// NamesDigest is sha256 over the sorted, canonicalised attachment names, or "~" when none are
// known (§4.5).
func NamesDigest(names []string) string {
	if len(names) == 0 {
		return "~"
	}
	canon := make([]string, 0, len(names))
	for _, n := range names {
		canon = append(canon, CanonicalName(n))
	}
	sort.Strings(canon)
	sum := sha256.Sum256([]byte(strings.Join(canon, sep)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PromptDigest is the Tier T ladder material used by the coarse key: sha256("sac-canon-1" ␟ "T" ␟
// text ␟ "END"). It takes an already-canonicalised text.
func PromptDigest(canonicalText string) string {
	return hash(strings.Join([]string{CanonVersion, "T", canonicalText, "END"}, sep))
}

const sep = "\u001f"

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RFC3339UTC renders an instant for a key. Key material must not depend on the local zone or on
// sub-second precision: two routes stamp milliseconds apart and must still agree.
func RFC3339UTC(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// --- §4.2 canonicalisation (sac-canon-1) ---------------------------------------------------

// Normalizer is the C3 step: Unicode normalisation to NFC, and *not* NFKC/NFKD.
//
// The default is the identity, because NFC needs golang.org/x/text/unicode/norm and this build is
// offline. That makes the canonicaliser correct only for input that is already NFC, which is
// exactly why it is not used to compute anything the service writes: the wire carries the finished
// `content_digest`, so the server has no text to canonicalise. Supplying a real normaliser is a
// one-line change for a build that may fetch the module, and nothing else in this package moves.
type Normalizer func(string) string

// Canonical holds the versioned canonicalisation behaviour.
type Canonical struct {
	Normalize Normalizer
}

// DefaultCanonical returns the canonicaliser with the identity normaliser. See Normalizer.
func DefaultCanonical() Canonical { return Canonical{Normalize: func(s string) string { return s }} }

// CanonicalName applies C7's name rules: basename only, NFC (see Normalizer), controls stripped,
// whitespace collapsed, and deliberately *not* case-folded.
func CanonicalName(name string) string {
	base := basename(name)
	return DefaultCanonical().Text(base)
}

// basename takes the last path component under both separators, because one route reports a POSIX
// path and another a Windows one.
func basename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Text applies C3–C6: normalise, strip controls and invisibles (C4), collapse whitespace (C5),
// trim; no case folding, no stemming, no punctuation stripping (C6).
func (c Canonical) Text(in string) string {
	if c.Normalize != nil {
		in = c.Normalize(in)
	}
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		if isStrippedControl(r) {
			continue
		}
		if isCollapsibleSpace(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), " ")
}

// DigestInput builds C9's layout:
//
//	digest_input = "sac-canon-1" ␟ "T" ␟ text ␟ att* ␟ "END"
//	att          = "A" ␟ name ␟ media_type ␟ decimal(size_bytes) ␟ content_digest
func DigestInput(canonicalText string, attachments []Attachment) string {
	parts := []string{CanonVersion, "T", canonicalText}
	sorted := make([]Attachment, len(attachments))
	copy(sorted, attachments)
	// C7: attachment records are sorted by (name, content_digest, size_bytes), because a multipart
	// body's order and a page's file list order are not the same fact.
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		if sorted[i].ContentDigest != sorted[j].ContentDigest {
			return sorted[i].ContentDigest < sorted[j].ContentDigest
		}
		return sorted[i].SizeBytes < sorted[j].SizeBytes
	})
	for _, a := range sorted {
		mediaType := a.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		digest := a.ContentDigest
		if !a.Readable || digest == "" {
			digest = "~" // "structurally unable to read the bytes" (E3)
		}
		parts = append(parts, "A", CanonicalName(a.Name), strings.ToLower(mediaType),
			strconv.FormatInt(a.SizeBytes, 10), digest)
	}
	parts = append(parts, "END")
	return strings.Join(parts, sep)
}

// ContentDigest is §4.2's output: "sha256:" + hex(SHA-256(UTF-8(digest_input))).
func ContentDigest(canonicalText string, attachments []Attachment) string {
	return hash(DigestInput(canonicalText, attachments))
}

// isStrippedControl is C4. TAB, LF and CR are *not* stripped: C5 folds them. Stripping everything
// else here is what guarantees U+001E and U+001F cannot occur in any field, which is what makes
// C9's separator unambiguous without escaping.
func isStrippedControl(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return false
	case r < 0x20, r == 0x7F:
		return true
	case r >= 0x80 && r <= 0x9F:
		return true
	case r >= 0x200B && r <= 0x200D:
		return true
	case r == 0x200E || r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2060 && r <= 0x2064:
		return true
	case r == 0xFEFF:
		return true
	}
	return false
}

// isCollapsibleSpace is C5's set. CRLF/CR become LF first, then every run of whitespace becomes a
// single U+0020 — so the caller never sees the distinction between the two.
func isCollapsibleSpace(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' || r == ' ' {
		return true
	}
	switch r {
	case 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// ValidUTF8 reports whether a byte slice decodes as UTF-8, which is C2's precondition.
func ValidUTF8(b []byte) bool { return utf8.Valid(b) }
