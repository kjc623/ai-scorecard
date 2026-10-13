// Package canon is the canonical form of a prompt and the content digest over it: the envelope's
// content_digest, which a device computes from what it captured and the server from a provider's
// record of the same prompt.
//
//	content_digest = "sha256:" + hex(SHA-256(digest_input))
//
// Two observations of one prompt agree on the digest only when both canonicalise identically, so
// the steps, separators and substitutions here are literal and fixed by test. The digest is pinned
// to a canonicalisation version: changing any step would make two parties on different versions
// disagree about the same prompt.
package canon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const (
	// Version is the first field of digest_input, so a version change cannot silently collide old
	// and new digests.
	Version = "sac-canon-1"

	// Sep is U+001F INFORMATION SEPARATOR ONE, the field separator. Canonicalisation removes it
	// from every field, which makes the layout unambiguous without escaping.
	Sep = "\x1f"

	// Unreadable stands in for an attachment digest the observer cannot compute. It is not
	// "absent": "no attachment" and "an attachment whose bytes cannot be read" are different
	// facts and both must survive into the digest.
	Unreadable = "~"
)

// Attachment is the canonicalisation input for one attachment. ContentDigest is
// "sha256:"+hex over the raw attached octets, or Unreadable when the observer cannot read them.
// Path, where the file was seen on the device, is recorded and never canonicalised: the digest
// leaves it out, because another observer of the same file may not see where it came from.
type Attachment struct {
	Name          string
	Path          string
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
	b.WriteString(Version)
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

// Digest is the envelope's content_digest over the canonical text and the canonicalised
// attachments, prefixed with the algorithm so a future change is visible in the value itself.
func Digest(text string, atts []Attachment) string {
	return "sha256:" + hex.EncodeToString(hashBytes(digestInput(Text(text), Attachments(atts))))
}

// Text canonicalises the user-authored text of a prompt:
//
//   - a leading byte-order mark is removed (it belongs to the encoding, not the content);
//   - the text is normalised to Unicode NFC;
//   - control and invisible characters are removed, keeping TAB, LF and CR for the next step;
//   - every run of whitespace becomes one U+0020, and the ends are trimmed;
//   - nothing else: no case folding, stemming or punctuation stripping.
func Text(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = norm.NFC.String(s)
	s = stripControlsAndInvisibles(s)
	return collapseWhitespace(s)
}

// Attachments canonicalises each attachment (base name, NFC, lower-case media type
// without parameters, Unreadable for a missing digest) and sorts them by (name, content_digest,
// size_bytes), because multipart order and file-list order are not the same fact.
func Attachments(atts []Attachment) []Attachment {
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
