// Package norm is the normalise stage of docs/01-collectors.md §9.2: "decoding, Unicode
// normalisation, whitespace collapsing and text extraction from structured bodies", with the
// digest that becomes `content_digest` computed here because normalisation is part of the dedup
// contract.
//
// §9.2 also makes the stage *bounded*, because it runs over attacker-influenced input on the
// interactive path: "linear, allocation-bounded, and refuses pathological inputs — deeply
// nested structures, enormous strings — by truncating and marking, with `confidence: degraded`
// if the truncation changed what a rule could see."
//
// Two different caps, two different outcomes, and the difference matters to §9.7:
//
//   - A body over the classifier's *input* cap is content the provider handed over that the
//     classifier could not process: ErrOverCap, degraded, detail `content_over_cap`.
//   - A body that decodes but whose text exceeds the *text* cap is truncated and marked. It
//     degrades only when the dropped tail could have carried a match; a payload that was large
//     but fully processed does not degrade (§9.7's "not emitted when" column).
//
// Unicode normalisation is a documented subset this build round, not full NFKC: golang.org/x/text
// is not fetchable on the offline build host (ADR 0016). The subset — compatibility folding for
// fullwidth/halfwidth ASCII forms, Unicode space folding, zero-width and format-character
// removal, and line-ending canonicalisation — is deterministic and target-independent, which is
// what §9.1's equivalence property requires. It is reported as an open decision in README.md.
package norm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Limits bounds the stage. Values are parameters (brief §8's resource budget), not design.
type Limits struct {
	// MaxInputBytes is the largest body the classifier will normalise at all.
	MaxInputBytes int
	// MaxTextBytes bounds the normalised text handed to rules and the model.
	MaxTextBytes int
	// MaxJSONTokens bounds the number of tokens read from a structured body.
	MaxJSONTokens int
	// MaxDepth bounds nesting in a structured body.
	MaxDepth int
}

// DefaultLimits is the shipped bound. It is small on purpose: a prompt that needs more than a
// megabyte of normalised text is an attachment, and attachments take the parser-child path
// (§10) off the interactive path.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes: 1 << 20,
		MaxTextBytes:  1 << 20,
		MaxJSONTokens: 20000,
		MaxDepth:      64,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = d.MaxInputBytes
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = d.MaxTextBytes
	}
	if l.MaxJSONTokens <= 0 {
		l.MaxJSONTokens = d.MaxJSONTokens
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	return l
}

// Encoding is what the bytes turned out to be.
type Encoding string

const (
	EncodingUTF8    Encoding = "utf-8"
	EncodingUTF16LE Encoding = "utf-16le"
	EncodingUTF16BE Encoding = "utf-16be"
	EncodingEmpty   Encoding = ""
)

var (
	// ErrOverCap is the §9.7 "over-cap body" case: content the classifier refuses to process.
	ErrOverCap = errors.New("norm: content is over the classifier's input cap")
	// ErrUndecodable is the §9.7 "undecodable bytes" case.
	ErrUndecodable = errors.New("norm: content is not decodable as the declared media type")
)

// Result is the normalised text plus the facts a caller needs to decide about degradation.
type Result struct {
	Text     string
	Digest   string // sha256:<hex> of Text — the digest that becomes content_digest
	Encoding Encoding

	// Truncated is true when input was dropped to fit a cap.
	Truncated bool
	// TruncationAffectsRules is true when the dropped input could have carried a match. It is
	// the difference between §9.7's two rows: a truncated payload that a rule could not fully
	// see degrades, and one that was large but fully processed does not.
	TruncationAffectsRules bool
	// Pathological names the structure that tripped a structural cap (nesting, token budget),
	// for the stage's error text.
	Pathological string

	BytesIn  int
	BytesOut int
}

// Normalise runs the stage. A non-nil error means the classifier could not process the body at
// all (degraded, detail per the error); a nil error with TruncationAffectsRules means the
// pipeline degrades the normalise stage while still running rules over the truncated text.
func Normalise(mediaType string, content []byte, lim Limits) (Result, error) {
	lim = lim.withDefaults()
	res := Result{BytesIn: len(content), Encoding: EncodingEmpty}
	if len(content) == 0 {
		res.Digest = digestOf("")
		return res, nil
	}
	if len(content) > lim.MaxInputBytes {
		return res, ErrOverCap
	}

	text, enc, err := decode(mediaType, content)
	if err != nil {
		return res, err
	}
	res.Encoding = enc

	text, patho := extract(mediaType, text, lim)
	if patho != "" {
		res.Pathological = patho
	}

	text = foldUnicode(text)
	text = collapseWhitespace(text)

	if len(text) > lim.MaxTextBytes {
		cut := utf8Boundary(text, lim.MaxTextBytes)
		dropped := text[cut:]
		res.Truncated = true
		res.TruncationAffectsRules = hasRuleVisibleContent(dropped)
		text = text[:cut]
	}
	if res.Pathological != "" {
		res.Truncated = true
		res.TruncationAffectsRules = true
	}

	res.Text = text
	res.BytesOut = len(text)
	res.Digest = digestOf(text)
	return res, nil
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decode resolves the bytes to a UTF-8 string, or refuses. A declared binary media type is
// refused rather than guessed at: the classifier classifies text and hands documents to the
// parser child (§10), and guessing would turn "we could not read this" into "we found nothing".
func decode(mediaType string, content []byte) (string, Encoding, error) {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if !textual(mt) {
		return "", EncodingEmpty, ErrUndecodable
	}

	// UTF-16 with a byte-order mark is decoded rather than refused: the bytes are readable and
	// refusing them would lose content the product exists to classify.
	if len(content) >= 2 {
		switch {
		case content[0] == 0xFF && content[1] == 0xFE:
			return decodeUTF16(content[2:], false), EncodingUTF16LE, nil
		case content[0] == 0xFE && content[1] == 0xFF:
			return decodeUTF16(content[2:], true), EncodingUTF16BE, nil
		}
	}
	body := content
	enc := EncodingUTF8
	if len(content) >= 3 && content[0] == 0xEF && content[1] == 0xBB && content[2] == 0xBF {
		body = content[3:]
	}
	if !utf8.Valid(body) {
		return "", enc, ErrUndecodable
	}
	return string(body), enc, nil
}

// textual reports whether the media type is one the classifier reads as text or extracts text
// from. An empty media type is treated as text: capture-core hands over prompt bytes whose type
// it may not know, and §9.2's normalisation is exactly the stage that copes with that.
func textual(mt string) bool {
	if mt == "" {
		return true
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/ld+json", "application/x-ndjson", "application/xml",
		"application/x-www-form-urlencoded", "application/javascript", "application/x-javascript",
		"application/x-sh", "application/yaml", "application/x-yaml":
		return true
	}
	// A +json / +xml structured suffix is text by definition.
	if strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") {
		return true
	}
	return false
}

func decodeUTF16(b []byte, bigEndian bool) string {
	n := len(b) / 2
	u := make([]uint16, 0, n)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// foldUnicode is the documented compatibility subset. It is a single pass, allocation-bounded
// by the input length, and identical on every target (no locale, no tables from outside the
// standard library).
func foldUnicode(s string) string {
	// Fast path: pure ASCII without the characters below needs no rewrite.
	if isPlainASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == 0xFEFF || (r >= 0x200B && r <= 0x200F) || r == 0x2060 || r == 0x00AD || r == 0x180E:
			// Byte-order mark, zero-width and format characters: not content, and keeping them
			// would let a rule's match be split by an invisible character.
			continue
		case r == 0x00A0 || (r >= 0x2000 && r <= 0x200A) || r == 0x202F || r == 0x205F || r == 0x3000:
			b.WriteByte(' ')
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteByte(byte(r - 0xFF00)) // fullwidth ASCII -> ASCII
		case r == '\r':
			// canonicalised with \n in collapseWhitespace
			b.WriteByte('\n')
		case r < 0x20 && r != '\n' && r != '\t':
			// C0 controls other than tab and newline: dropped rather than mapped, so they
			// cannot break a token into two.
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isPlainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c == '\r' || c < 0x20 && c != '\n' && c != '\t' {
			return false
		}
	}
	return true
}

// collapseWhitespace canonicalises line endings, collapses horizontal whitespace runs, and
// trims. Newlines are preserved: §9.2's whitespace collapsing is about the interactive path's
// cost and about a rule seeing "4111  1111" and "4111 1111" the same way, not about destroying
// line structure that offsets and excerpts depend on.
//
// It writes into a byte buffer rather than a strings.Builder because trimming the space run
// before a newline has to be O(1): a builder cannot truncate, and rebuilding it per line would
// make this stage quadratic in exactly the attacker-controlled input §9.2 says must stay linear.
func collapseWhitespace(s string) string {
	buf := make([]byte, 0, len(s))
	pendingSpace := false
	atLineStart := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\n':
			for len(buf) > 0 && (buf[len(buf)-1] == ' ' || buf[len(buf)-1] == '\t') {
				buf = buf[:len(buf)-1]
			}
			if len(buf) > 0 {
				buf = append(buf, '\n')
			}
			pendingSpace = false
			atLineStart = true
		case ' ', '\t', '\v', '\f':
			if atLineStart {
				continue
			}
			pendingSpace = true
		default:
			if pendingSpace {
				buf = append(buf, ' ')
				pendingSpace = false
			}
			buf = append(buf, c)
			atLineStart = false
		}
	}
	for len(buf) > 0 && buf[len(buf)-1] == '\n' {
		buf = buf[:len(buf)-1]
	}
	return string(buf)
}

// hasRuleVisibleContent reports whether a dropped tail could have carried a match: any
// non-whitespace byte. A tail of pure whitespace is a payload that was large but fully
// processed, which §9.7 says must not degrade.
func hasRuleVisibleContent(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			return true
		}
	}
	return false
}

func utf8Boundary(s string, max int) int {
	if max >= len(s) {
		return len(s)
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}
