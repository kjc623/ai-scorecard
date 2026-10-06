package norm

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
)

// extract returns the text of a JSON or XML body: its strings, numbers and booleans, or its
// character data, joined by spaces. The token readers keep their own stacks, so nesting cannot
// recurse through this process's stack. When a limit stops extraction, the text so far is
// returned with the limit's name. A body that does not parse as its declared type is returned
// unchanged: malformed JSON is still text a rule can read.
func extract(mt, text string, lim Limits) (string, string) {
	switch {
	case mt == "application/json" || mt == "application/x-ndjson" || strings.HasSuffix(mt, "+json"):
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
	depth, tokens := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s, ""
		}
		if tokens++; tokens > lim.MaxTokens {
			return b.String(), "token_budget"
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				if depth++; depth > lim.MaxDepth {
					return b.String(), "nesting"
				}
			} else {
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
			return b.String(), "text_cap"
		}
	}
	return nonEmptyOr(b.String(), s), ""
}

func extractXML(s string, lim Limits) (string, string) {
	dec := xml.NewDecoder(strings.NewReader(s))
	var b strings.Builder
	depth, tokens := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s, ""
		}
		if tokens++; tokens > lim.MaxTokens {
			return b.String(), "token_budget"
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth++; depth > lim.MaxDepth {
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
	return nonEmptyOr(b.String(), s), ""
}

// nonEmptyOr returns extracted, or raw when nothing was extracted ("{}", "[1,2]" without
// strings), so the body's shape and keys stay visible to the rules.
func nonEmptyOr(extracted, raw string) string {
	if strings.TrimSpace(extracted) == "" {
		return raw
	}
	return extracted
}

func appendWord(b *strings.Builder, s string) {
	if s = strings.TrimSpace(s); s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(s)
}
