// Package dedup computes the two identifiers that let the server recognise one submission seen
// twice: the canonical content digest and the dedup key.
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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// CanonVersion is the first field of digest_input, so a version change cannot silently
	// collide old and new digests.
	CanonVersion = "sac-canon-1"

	// LadderVersion is the first field of the dedup key input.
	LadderVersion = "sac-dedup-1"

	// BucketWidth is the dedup time bucket on the device's own, uncorrected wall clock. A
	// narrower bucket splits one submission seen by two routes; a wider one merges two
	// identical sends.
	BucketWidth = 300 * time.Second

	// Sep is U+001F INFORMATION SEPARATOR ONE, the field separator. Canonicalisation removes it
	// from every field, which makes the layout unambiguous without escaping.
	Sep = "\x1f"

	// Unreadable stands in for an attachment digest the route cannot compute. It is not
	// "absent": "no attachment" and "an attachment whose bytes cannot be read" are different
	// facts and both must survive into the key.
	Unreadable = "~"

	// TierT is content-derived material; TierS is the payload-shape surrogate M0 uses.
	TierT = "T"
	TierS = "S"
)

// Attachment is the canonicalisation input for one attachment. ContentDigest is
// "sha256:"+hex over the raw attached octets, or Unreadable when the route cannot read them.
type Attachment struct {
	Name          string
	MediaType     string
	SizeBytes     int64
	ContentDigest string
}

// digestInput builds the digest layout:
//
//	"sac-canon-1" ␟ "T" ␟ text ␟ att* ␟ "END"
//	att = "A" ␟ name ␟ media_type ␟ decimal(size_bytes) ␟ content_digest
func digestInput(text string, atts []Attachment) string {
	var b strings.Builder
	b.WriteString(CanonVersion)
	b.WriteString(Sep)
	b.WriteString("T")
	b.WriteString(Sep)
	b.WriteString(text)
	b.WriteString(Sep)
	for _, a := range atts {
		b.WriteString("A")
		b.WriteString(Sep)
		b.WriteString(a.Name)
		b.WriteString(Sep)
		b.WriteString(a.MediaType)
		b.WriteString(Sep)
		fmt.Fprintf(&b, "%d", a.SizeBytes)
		b.WriteString(Sep)
		b.WriteString(a.ContentDigest)
		b.WriteString(Sep)
	}
	b.WriteString("END")
	return b.String()
}

// ContentDigest is the envelope's content_digest over the canonical text and the canonicalised
// attachments, prefixed with the algorithm so a future change is visible in the value itself.
func ContentDigest(text string, atts []Attachment) string {
	return "sha256:" + hex.EncodeToString(hashBytes(digestInput(CanonicalText(text), CanonicalAttachments(atts))))
}

// CanonicalText canonicalises the user-authored segment a route identified:
//
//   - a leading byte-order mark is removed (it belongs to the encoding, not the content);
//   - the text is normalised to Unicode NFC;
//   - control and invisible characters are removed, keeping TAB, LF and CR for the next step;
//   - every run of whitespace becomes one U+0020, and the ends are trimmed;
//   - nothing else: no case folding, stemming or punctuation stripping.
func CanonicalText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = norm.NFC.String(s)
	s = stripControlsAndInvisibles(s)
	return collapseWhitespace(s)
}

// CanonicalAttachments canonicalises each attachment (base name, NFC, lower-case media type
// without parameters, Unreadable for a missing digest) and sorts them by (name, content_digest,
// size_bytes), because multipart order and file-list order are not the same fact.
func CanonicalAttachments(atts []Attachment) []Attachment {
	out := make([]Attachment, 0, len(atts))
	for _, a := range atts {
		name := collapseWhitespace(stripControlsAndInvisibles(norm.NFC.String(basename(a.Name))))
		mt := strings.ToLower(strings.TrimSpace(a.MediaType))
		if i := strings.IndexByte(mt, ';'); i >= 0 { // parameters (charset, boundary) are framing
			mt = strings.TrimSpace(mt[:i])
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		dig := a.ContentDigest
		if dig == "" {
			dig = Unreadable
		}
		size := a.SizeBytes
		if size < 0 {
			size = 0
		}
		out = append(out, Attachment{Name: name, MediaType: mt, SizeBytes: size, ContentDigest: dig})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].ContentDigest != out[j].ContentDigest {
			return out[i].ContentDigest < out[j].ContentDigest
		}
		return out[i].SizeBytes < out[j].SizeBytes
	})
	return out
}

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

func basename(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// stripControlsAndInvisibles removes C0 controls (except TAB, LF and CR, which
// collapseWhitespace folds), DEL and C1, the zero-width and joiner characters, bidi marks and
// overrides, the word joiner and invisible operators, and U+FEFF. It is what guarantees U+001F
// cannot occur inside a field.
func stripControlsAndInvisibles(s string) string {
	if !strings.ContainsFunc(s, isControlOrInvisible) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControlOrInvisible(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControlOrInvisible(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return false
	case r < 0x20: // C0 controls
		return true
	case r == 0x7f: // DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r >= 0x200b && r <= 0x200d: // zero-width space, non-joiner, joiner
		return true
	case r == 0x200e || r == 0x200f: // LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embedding and override
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r == 0xfeff: // BOM
		return true
	}
	return false
}

// collapseWhitespace turns every run of whitespace into a single U+0020 and trims both ends.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	started := false
	for _, r := range s {
		if isWhitespace(r) {
			if started {
				space = true
			}
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
		started = true
	}
	return b.String()
}

func isWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\r', 0x20, 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
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
