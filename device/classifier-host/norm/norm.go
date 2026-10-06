// Package norm turns the bytes handed to the classifier into the text the rules and the model
// read: it decodes them, extracts the text of a JSON or XML body, applies Unicode NFKC
// normalisation, removes invisible format and control characters, and collapses whitespace.
//
// NFKC folds compatibility forms, so a full-width or superscript digit, a ligature or a
// non-breaking space reads as its plain equivalent and cannot hide a match from a rule.
//
// The stage is bounded because its input is attacker-influenced: a body over the input limit is
// refused, a structured body is read with a token budget and a depth limit, and text over the
// text limit is truncated and marked.
package norm

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits bound the stage.
type Limits struct {
	// MaxInputBytes is the largest body normalised at all.
	MaxInputBytes int
	// MaxTextBytes bounds the normalised text.
	MaxTextBytes int
	// MaxTokens bounds the tokens read from a JSON or XML body.
	MaxTokens int
	// MaxDepth bounds nesting in a JSON or XML body.
	MaxDepth int
}

// DefaultLimits are the limits the classifier runs with.
func DefaultLimits() Limits {
	return Limits{MaxInputBytes: 1 << 20, MaxTextBytes: 1 << 20, MaxTokens: 20000, MaxDepth: 64}
}

var (
	// ErrOverCap is a body larger than MaxInputBytes.
	ErrOverCap = errors.New("norm: content is over the classifier's input limit")
	// ErrUndecodable is a body that is not text in a supported encoding.
	ErrUndecodable = errors.New("norm: content is not decodable as text")
)

// Result is the normalised text and what was lost producing it.
type Result struct {
	Text string
	// Truncated is set when input was dropped to fit a limit.
	Truncated bool
	// TruncationAffectsRules is set when the dropped input could have carried a match: it held
	// something other than whitespace, or a structural limit stopped extraction.
	TruncationAffectsRules bool
	// Pathological names the structural limit that stopped extraction: "nesting",
	// "token_budget" or "text_cap".
	Pathological string
}

// Normalise runs the stage. An error means the body could not be processed at all.
func Normalise(mediaType string, content []byte, lim Limits) (Result, error) {
	var res Result
	if len(content) == 0 {
		return res, nil
	}
	if len(content) > lim.MaxInputBytes {
		return res, ErrOverCap
	}
	mt := baseMediaType(mediaType)
	text, err := decode(mt, content)
	if err != nil {
		return res, err
	}
	text, res.Pathological = extract(mt, text, lim)
	text = collapseWhitespace(fold(text))
	if len(text) > lim.MaxTextBytes {
		cut := lim.MaxTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		res.Truncated = true
		res.TruncationAffectsRules = strings.TrimSpace(text[cut:]) != ""
		text = text[:cut]
	}
	if res.Pathological != "" {
		res.Truncated, res.TruncationAffectsRules = true, true
	}
	res.Text = text
	return res, nil
}

// baseMediaType lowercases a media type and drops its parameters.
func baseMediaType(mt string) string {
	mt = strings.ToLower(mt)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.TrimSpace(mt)
}

// decode returns the body as UTF-8. UTF-16 with a byte-order mark is decoded; a binary media type
// or invalid UTF-8 is refused rather than guessed at.
func decode(mt string, content []byte) (string, error) {
	if !textual(mt) {
		return "", ErrUndecodable
	}
	if len(content) >= 2 {
		switch {
		case content[0] == 0xFF && content[1] == 0xFE:
			return decodeUTF16(content[2:], false), nil
		case content[0] == 0xFE && content[1] == 0xFF:
			return decodeUTF16(content[2:], true), nil
		}
	}
	body := content
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		body = body[3:]
	}
	if !utf8.Valid(body) {
		return "", ErrUndecodable
	}
	return string(body), nil
}

// textual reports whether a media type is read as text. An empty media type is: capture-core
// does not always know the type of the prompt bytes it hands over.
func textual(mt string) bool {
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"), strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/x-ndjson", "application/xml",
		"application/x-www-form-urlencoded", "application/javascript", "application/x-javascript",
		"application/x-sh", "application/yaml", "application/x-yaml":
		return true
	}
	return false
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// fold removes format characters (zero-width characters, soft hyphens, byte-order marks) and C0
// controls other than tab and newline, turns CR and CRLF into LF, and applies NFKC. Removing an
// invisible character first means it cannot split a match in two.
func fold(s string) string {
	if !needsFold(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r == '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				continue
			}
			b.WriteByte('\n')
		case r < 0x20 && r != '\n' && r != '\t', unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	return norm.NFKC.String(b.String())
}

// needsFold reports whether s holds anything fold would change: a non-ASCII byte, a CR or a
// control character.
func needsFold(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || c < 0x20 && c != '\n' && c != '\t' {
			return true
		}
	}
	return false
}

// collapseWhitespace collapses runs of horizontal whitespace to one space, removes whitespace at
// the start and end of each line and leading and trailing newlines, and keeps the line structure
// that excerpt offsets refer to. It is a single pass over the input.
func collapseWhitespace(s string) string {
	buf := make([]byte, 0, len(s))
	pendingSpace, atLineStart := false, true
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\n':
			if len(buf) > 0 {
				buf = append(buf, '\n')
			}
			pendingSpace, atLineStart = false, true
		case ' ', '\t', '\v', '\f':
			pendingSpace = !atLineStart
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
