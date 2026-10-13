// Package dedup computes the two identifiers that let the server recognise one submission seen
// twice: the canonical content digest (protocol/canon, which the server also computes for a
// provider's record) and the dedup key.
//
//	content_digest = "sha256:" + hex(SHA-256(digest_input))
//	dedup_key      = "sha256:" + hex(SHA-256("sac-dedup-1" ␟ ... ␟ tier ␟ material))
//
// Two routes that observe the same submission (a proxy reading messages[], the extension reading
// a compose box) collapse into one fact only when both compute identical keys, so the
// canonicalisation steps, separators and substitutions here are literal and fixed by test. Both
// identifiers are pinned to a canonicalisation version: changing any step would make a device on
// the old version and one on the new disagree about the same submission.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/protocol/canon"
)

const (
	// CanonVersion is the first field of digest_input, so a version change cannot silently
	// collide old and new digests.
	CanonVersion = canon.Version

	// LadderVersion is the first field of the dedup key input.
	LadderVersion = "sac-dedup-1"

	// BucketWidth is the dedup time bucket on the device's own, uncorrected wall clock. A
	// narrower bucket splits one submission seen by two routes; a wider one merges two
	// identical sends.
	BucketWidth = 300 * time.Second

	// Sep is U+001F INFORMATION SEPARATOR ONE, the field separator. Canonicalisation removes it
	// from every field, which makes the layout unambiguous without escaping.
	Sep = canon.Sep

	// Unreadable stands in for an attachment digest the route cannot compute. It is not
	// "absent": "no attachment" and "an attachment whose bytes cannot be read" are different
	// facts and both must survive into the key.
	Unreadable = canon.Unreadable

	// TierT is content-derived material; TierS is the payload-shape surrogate M0 uses.
	TierT = "T"
	TierS = "S"
)

// Attachment is the canonicalisation input for one attachment (canon.Attachment).
type Attachment = canon.Attachment

// ContentDigest is the envelope's content_digest over the canonical text and the canonicalised
// attachments (canon.Digest).
func ContentDigest(text string, atts []Attachment) string { return canon.Digest(text, atts) }

// CanonicalText canonicalises the user-authored segment a route identified (canon.Text).
func CanonicalText(s string) string { return canon.Text(s) }

// CanonicalAttachments canonicalises and sorts attachments (canon.Attachments).
func CanonicalAttachments(atts []Attachment) []Attachment { return canon.Attachments(atts) }

// NamesDigest is the names part of the Tier S material: SHA-256 over the sorted, canonicalised
// attachment names joined with Sep, or Unreadable when none are known.
func NamesDigest(atts []Attachment) string {
	canon := CanonicalAttachments(atts)
	if len(canon) == 0 {
		return Unreadable
	}
	names := make([]string, 0, len(canon))
	for _, a := range canon {
		names = append(names, a.Name)
	}
	return "sha256:" + hex.EncodeToString(hashBytes(strings.Join(names, Sep)))
}

// BucketStart is floor(occurred_at_utc / 300 s) × 300 s from the device's own wall clock,
// uncorrected. Skew shifts every route on one device equally, so correcting it would reintroduce
// the disagreement the key exists to remove.
func BucketStart(occurredAt time.Time) time.Time {
	return occurredAt.UTC().Truncate(BucketWidth)
}

// Key builds a dedup key:
//
//	dedup_input = "sac-dedup-1" ␟ tenant ␟ device ␟ tool ␟ direction ␟ kind ␟ bucket_start
//	              ␟ tier ␟ material
//
// An empty required field is an error rather than an empty component: a key computed from a
// missing dimension would merge two different submissions.
func Key(tenant, device, tool, direction, kind string, bucketStart time.Time, tier, material string) (string, error) {
	switch {
	case tenant == "":
		return "", fmt.Errorf("dedup: no tenant_id; a key without a tenant would merge across customers")
	case device == "":
		return "", fmt.Errorf("dedup: no device_id; the same text from two devices is two submissions")
	case tool == "":
		return "", fmt.Errorf("dedup: no tool_fingerprint")
	case direction == "":
		return "", fmt.Errorf("dedup: no direction")
	case kind == "":
		return "", fmt.Errorf("dedup: no kind")
	case tier == "":
		return "", fmt.Errorf("dedup: no tier")
	}
	input := strings.Join([]string{
		LadderVersion, tenant, device, tool, direction, kind,
		bucketStart.UTC().Format(time.RFC3339), tier, material,
	}, Sep)
	return "sha256:" + hex.EncodeToString(hashBytes(input)), nil
}

// ContentKey is the Tier T key, the strongest material, available at M1 and above.
func ContentKey(tenant, device, tool, kind string, occurredAt time.Time, contentDigest string) (string, error) {
	if contentDigest == "" {
		return "", fmt.Errorf("dedup: Tier T requires a content_digest")
	}
	return Key(tenant, device, tool, "egress", kind, BucketStart(occurredAt), TierT, contentDigest)
}

// SurrogateKey is the Tier S key used at M0 and whenever the content digest is not canonical:
// size, attachment count and the names digest stand in for content the device did not read. It
// is weaker than Tier T, and the record says so.
func SurrogateKey(tenant, device, tool, kind string, occurredAt time.Time, sizeBytes int64, attachments []Attachment) (string, error) {
	material := fmt.Sprintf("%d%s%d%s%s", sizeBytes, Sep, len(attachments), Sep, NamesDigest(attachments))
	return Key(tenant, device, tool, "egress", kind, BucketStart(occurredAt), TierS, material)
}

func hashBytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Decode turns bytes into text for canonicalisation: each run of ill-formed UTF-8 becomes one
// U+FFFD and a leading byte-order mark is dropped. It never fails, because a collector that
// refused a body it could not decode would drop the observation.
func Decode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if !utf8.Valid(b) {
		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	}
	return strings.TrimPrefix(string(b), "\ufeff")
}
