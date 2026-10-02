// Package dedup implements the device half of docs/02-ingest-and-transport.md §4 —
// "Deduplication — normative". It is deliberately a separate package from the collectors:
// the digest a route computes and the digest ingest recomputes must be the same function,
// so there is one implementation and one test, and the device never invents its own.
//
// The two keys:
//
//	content_digest = "sha256:" + hex(SHA-256(digest_input))     (§4.2 C9)
//	dedup_key      = sha256("sac-dedup-1" ␟ ... ␟ tier ␟ material) (§4.5 key ladder)
//
// Both are pinned to a canonicalisation version (`sac-canon-1`), because changing any step
// splits the fleet: a device on the old version and one on the new compute different keys for
// the same submission.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// CanonVersion is the canonicalisation version of docs/02 §4.2. It is the first field of
	// digest_input, so a version change cannot silently collide old and new digests.
	CanonVersion = "sac-canon-1"

	// LadderVersion is the dedup_key ladder version of docs/02 §4.5.
	LadderVersion = "sac-dedup-1"

	// BucketWidth is the dedup time bucket, docs/02 §4.3: 300 s, on the device's own wall
	// clock, uncorrected. A narrower bucket splits one submission seen by two routes; a wider
	// one merges two identical sends.
	BucketWidth = 300 * time.Second

	// Sep is U+001F INFORMATION SEPARATOR ONE, the field separator of C9. C4 guarantees it
	// cannot occur inside any field, which is what makes the layout unambiguous without
	// escaping or length prefixes.
	Sep = "\x1f"

	// Unreadable is the literal a canonicalisation step substitutes when a route is
	// structurally unable to read the bytes (C7, E3). It is not "absent": the difference
	// between "no attachment" and "an attachment whose bytes we cannot read" is a coverage
	// fact and must survive into the key.
	Unreadable = "~"

	// TierT is content-derived material; TierS is the payload-shape surrogate M0 is forced
	// to use; TierR is a rollup window; TierD is a detection basis.
	TierT = "T"
	TierS = "S"
	TierR = "R"
	TierD = "D"
)

// Normalizer is Unicode NFC (C3). It is an interface for one honest reason: Go's standard
// library has no Unicode normalisation, and this host is offline, so golang.org/x/text is
// not available. The default is IdentityNFC, which is correct for already-composed text
// (the overwhelming majority of UTF-8 in the wild) and wrong for decomposed sequences; the
// difference is reported rather than hidden (see Normalizer doc on IdentityNFC).
type Normalizer interface {
	NFC(string) string
}

// IdentityNFC performs no normalisation. It is the default so the package compiles and its
// arithmetic is testable offline, and it is a *known* deviation from C3: text containing
// decomposed combining sequences (for example "e" + U+0301) will digest differently here
// than at a conforming implementation. It is named Identity, not Default, so a caller
// cannot adopt it by accident.
type IdentityNFC struct{}

// NFC returns s unchanged.
func (IdentityNFC) NFC(s string) string { return s }

// Attachment is the canonicalisation input for one attachment (C7). ContentDigest is
// "sha256:"+hex over the raw attached octets, or Unreadable when the route cannot read them.
type Attachment struct {
	Name          string
	MediaType     string
	SizeBytes     int64
	ContentDigest string
}

// digestInput builds the C9 layout:
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

// ContentDigest is the envelope's `content_digest`: C9's digest over the canonical text and
// canonicalised attachments, prefixed with the algorithm so a future change is visible in
// the value and not just in a version field.
func ContentDigest(text string, atts []Attachment, n Normalizer) string {
	return "sha256:" + hex.EncodeToString(hashBytes(digestInput(CanonicalText(text, n), CanonicalAttachments(atts, n))))
}

// CanonicalText runs C2–C6 over a caller-supplied segment (C1 segment selection is
// route-specific and is not this package's business):
//
//	C2 decode: ill-formed UTF-8 becomes U+FFFD, a leading BOM is stripped (Go's conversion
//	           yields U+FFFD per invalid byte, which is the specified replacement)
//	C3 NFC
//	C4 strip controls and invisibles, keeping TAB/LF/CR for C5 to fold
//	C5 collapse whitespace to single U+0020, trim
//	C6 nothing else: no case folding, no stemming, no punctuation stripping
func CanonicalText(s string, n Normalizer) string {
	if n == nil {
		n = IdentityNFC{}
	}
	s = strings.TrimPrefix(s, "\ufeff") // C2: a leading BOM belongs to the encoding, not the content
	s = n.NFC(s)                       // C3
	s = stripControlsAndInvisibles(s)  // C4
	s = collapseWhitespace(s)          // C5
	return s
}

// CanonicalAttachments runs C7 and the ordering rule that follows it: records are sorted by
// (name, content_digest, size_bytes), because multipart order and file-list order are not
// the same fact.
func CanonicalAttachments(atts []Attachment, n Normalizer) []Attachment {
	if n == nil {
		n = IdentityNFC{}
	}
	out := make([]Attachment, 0, len(atts))
	for _, a := range atts {
		name := basename(a.Name)
		name = n.NFC(name)
		name = collapseWhitespace(stripControlsAndInvisibles(name))
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
		if a.SizeBytes < 0 {
			a.SizeBytes = 0
		}
		out = append(out, Attachment{Name: name, MediaType: mt, SizeBytes: a.SizeBytes, ContentDigest: dig})
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

// NamesDigest is the `names_digest` of the Tier S material: sha256 over the sorted,
// canonicalised attachment names, or Unreadable when none are known. The document does not
// fix the join character for names; U+001F is used here for the same reason C9 uses it, and
// the choice is recorded as an open item in the endpoint report.
func NamesDigest(atts []Attachment, n Normalizer) string {
	canon := CanonicalAttachments(atts, n)
	if len(canon) == 0 {
		return Unreadable
	}
	names := make([]string, 0, len(canon))
	for _, a := range canon {
		names = append(names, a.Name)
	}
	return "sha256:" + hex.EncodeToString(hashBytes(strings.Join(names, Sep)))
}

// BucketStart is §4.3: floor(occurred_at_utc / 300s) × 300s, from the device's own wall
// clock, uncorrected. Device skew shifts both routes on one device equally, so correcting it
// would reintroduce the disagreement the key exists to remove.
func BucketStart(occurredAt time.Time) time.Time {
	return occurredAt.UTC().Truncate(BucketWidth)
}

// Key is the §4.5 ladder:
//
//	dedup_input = "sac-dedup-1" ␟ tenant ␟ device ␟ tool ␟ direction ␟ kind ␟ bucket_start
//	              ␟ tier ␟ material
//
// Returning "" for any empty required field is deliberate: a key computed from a missing
// dimension would merge two different submissions, so the caller gets an error instead.
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

// ContentKey is the Tier T key: the strongest material, available at M1 and above.
func ContentKey(tenant, device, tool, kind string, occurredAt time.Time, contentDigest string) (string, error) {
	if contentDigest == "" {
		return "", fmt.Errorf("dedup: Tier T requires a content_digest")
	}
	return Key(tenant, device, tool, "egress", kind, BucketStart(occurredAt), TierT, contentDigest)
}

// SurrogateKey is the Tier S key, the M0 path: size, attachment count and names digest stand
// in for content the device is not permitted to read. §4.5 is explicit that this is weaker
// and that the weakness is reported, never hidden.
func SurrogateKey(tenant, device, tool, kind string, occurredAt time.Time, sizeBytes int64, attachments []Attachment, n Normalizer) (string, error) {
	material := fmt.Sprintf("%d%s%d%s%s", sizeBytes, Sep, len(attachments), Sep, NamesDigest(attachments, n))
	return Key(tenant, device, tool, "egress", kind, BucketStart(occurredAt), TierS, material)
}

// RollupKey is Tier R. For a rollup the window *is* the bucket: the record describes a
// period, not an instant.
func RollupKey(tenant, device, tool, kind string, windowStart, windowEnd time.Time) (string, error) {
	material := windowStart.UTC().Format(time.RFC3339) + Sep + windowEnd.UTC().Format(time.RFC3339)
	return Key(tenant, device, tool, "none", kind, BucketStart(windowStart), TierR, material)
}

// DetectionKey is Tier D: the detection basis is the only material a model_detection has.
func DetectionKey(tenant, device, tool, kind string, occurredAt time.Time, detectionBasis string) (string, error) {
	if detectionBasis == "" {
		return "", fmt.Errorf("dedup: Tier D requires a detection_basis")
	}
	return Key(tenant, device, tool, "none", kind, BucketStart(occurredAt), TierD, detectionBasis)
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

// stripControlsAndInvisibles is C4. It keeps TAB, LF and CR because C5 folds them; it
// removes DEL and C1, the zero-width and joiner block, bidi marks and overrides, the word
// joiner and invisible operators block, and U+FEFF. This step is what guarantees U+001F
// cannot occur inside a field.
func stripControlsAndInvisibles(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return isControlOrInvisible(r) }) {
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
		return false // folded by C5
	case r < 0x20: // C0 controls
		return true
	case r == 0x7f: // DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r >= 0x200b && r <= 0x200d: // zero-width space/non-joiner/joiner
		return true
	case r == 0x200e || r == 0x200f: // LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embedding/override
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r == 0xfeff: // BOM
		return true
	}
	return false
}

// collapseWhitespace is C5: CRLF and CR become LF, every run of whitespace becomes a single
// U+0020, and leading/trailing whitespace is trimmed.
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

// Decode is C2 for callers that hold bytes rather than a string: ill-formed sequences become
// U+FFFD and a leading BOM is dropped. It never fails, because a collector that refused a
// body it could not decode would be a collector that drops observations (C22).
func Decode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if !utf8.Valid(b) {
		// Go's []rune conversion substitutes U+FFFD for each ill-formed byte, which is the
		// specified replacement and keeps the digest deterministic.
		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	}
	return strings.TrimPrefix(string(b), "\ufeff")
}

