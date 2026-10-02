package norm

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
)

// extract pulls text out of a structured body (§9.2: "text extraction from structured bodies").
//
// It is iterative — the JSON and XML token readers keep their own explicit stacks, so a
// deeply nested body cannot recurse through this process's stack — and it is bounded twice:
// by a token budget and by a depth cap. Tripping either returns the text extracted so far plus
// the name of the structure that tripped it, which the caller marks as truncation that a rule
// could not see past.
//
// A body that is declared structured but does not parse is treated as plain text rather than
// refused: a truncated or malformed JSON prompt is still prompt text, and refusing it would
// report "no sensitive data" for content the product can read.
func extract(mediaType, text string, lim Limits) (string, string) {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case mt == "application/json" || mt == "application/ld+json" || mt == "application/x-ndjson" || strings.HasSuffix(mt, "+json"):
		return extractJSON(text, lim)
	case mt == "application/xml" || strings.HasSuffix(mt, "+xml"):
		return extractXML(text, lim)
	default:
		return text, ""
	}
}

func extractJSON(s string, lim Limits) (string, string) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var b strings.Builder
	depth := 0
	tokens := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Not JSON after all: the raw text is the best available text.
			return s, ""
		}
		tokens++
		if tokens > lim.MaxJSONTokens {
			return b.String(), "token_budget"
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
				if depth > lim.MaxDepth {
					return b.String(), "nesting"
				}
			default:
				depth--
			}
		case string:
			appendWord(&b, t)
		case json.Number:
			appendWord(&b, t.String())
		case bool:
			if t {
				appendWord(&b, "true")
			} else {
				appendWord(&b, "false")
			}
		}
		if b.Len() > lim.MaxTextBytes {
			// The text cap is enforced by the caller too; stopping here keeps the extraction
			// itself bounded rather than building a string nobody will use.
			return b.String(), "text_cap"
		}
	}
	out := b.String()
	if strings.TrimSpace(out) == "" {
		// A JSON body with no strings at all ("{}", "[1,2]") carries no text: fall back to the
		// raw bytes so shape and keys are still visible to rules.
		return s, ""
	}
	return out, ""
}

func extractXML(s string, lim Limits) (string, string) {
	dec := xml.NewDecoder(strings.NewReader(s))
	var b strings.Builder
	depth := 0
	tokens := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s, ""
		}
		tokens++
		if tokens > lim.MaxJSONTokens {
			return b.String(), "token_budget"
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > lim.MaxDepth {
				return b.String(), "nesting"
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			appendWord(&b, string(t))
		}
		if b.Len() > lim.MaxTextBytes {
			return b.String(), "text_cap"
		}
	}
	out := b.String()
	if strings.TrimSpace(out) == "" {
		return s, ""
	}
	return out, ""
}

func appendWord(b *strings.Builder, s string) {
	t := strings.TrimSpace(s)
	if t == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(t)
}
