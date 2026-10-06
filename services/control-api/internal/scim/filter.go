package scim

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// The RFC 7644 §3.4.2.2 filter language, the part identity providers use: comparisons (eq ne co sw
// ew gt ge lt le pr), and, or, not and parentheses. It serves two places: a GET's `filter=` (where
// an indexed term narrows the rows and the whole expression is then evaluated on each resource) and
// a PATCH value path such as `emails[type eq "work"]` or Entra's `members[value eq "…"]`.
//
// String comparison is case-insensitive. The RFC makes that depend on each attribute's caseExact,
// but every attribute a provider filters on here is either case-insensitive (userName, displayName,
// type) or an identifier whose case variants never name two different things in practice.

var errBadFilter = errors.New("scim: malformed filter")

type expr interface {
	match(target map[string]any) bool
}

type comparison struct {
	attr string
	op   string // lower-case
	val  any    // string, bool, nil, or json.Number
}

type logical struct {
	and  bool
	l, r expr
}

type negation struct{ e expr }

func (l logical) match(t map[string]any) bool {
	if l.and {
		return l.l.match(t) && l.r.match(t)
	}
	return l.l.match(t) || l.r.match(t)
}

func (n negation) match(t map[string]any) bool { return !n.e.match(t) }

func (c comparison) match(t map[string]any) bool {
	vals := values(t, c.attr)
	if c.op == "pr" {
		for _, v := range vals {
			if s, ok := v.(string); !ok || s != "" {
				return true
			}
		}
		return false
	}
	if c.val == nil {
		// `eq null` is true for an absent attribute; `ne null` for a present one.
		switch c.op {
		case "eq":
			return len(vals) == 0
		case "ne":
			return len(vals) > 0
		}
		return false
	}
	if c.op == "ne" {
		for _, v := range vals {
			if compare(v, "eq", c.val) {
				return false
			}
		}
		return true
	}
	for _, v := range vals {
		if compare(v, c.op, c.val) {
			return true
		}
	}
	return false
}

func compare(have any, op string, want any) bool {
	switch w := want.(type) {
	case bool:
		h, ok := boolValue(have)
		if !ok {
			return false
		}
		return op == "eq" && h == w
	case json.Number:
		hf, err1 := strconv.ParseFloat(stringOf(have), 64)
		wf, err2 := w.Float64()
		if err1 != nil || err2 != nil {
			return false
		}
		switch op {
		case "eq":
			return hf == wf
		case "gt":
			return hf > wf
		case "ge":
			return hf >= wf
		case "lt":
			return hf < wf
		case "le":
			return hf <= wf
		}
		return false
	case string:
		h, ok := have.(string)
		if !ok {
			return false
		}
		hl, wl := strings.ToLower(h), strings.ToLower(w)
		switch op {
		case "eq":
			return hl == wl
		case "co":
			return strings.Contains(hl, wl)
		case "sw":
			return strings.HasPrefix(hl, wl)
		case "ew":
			return strings.HasSuffix(hl, wl)
		case "gt":
			return hl > wl
		case "ge":
			return hl >= wl
		case "lt":
			return hl < wl
		case "le":
			return hl <= wl
		}
	}
	return false
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

// boolValue reads a SCIM boolean, including Entra's "True"/"False" strings.
func boolValue(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// seed is the value a filter describes, for creating the value a PATCH addresses when none matches:
// `emails[type eq "work"].value` on a user with no work email creates {"type":"work","value":…}.
// Only a conjunction of equalities on plain attributes describes one value.
func seed(e expr) map[string]any {
	switch t := e.(type) {
	case comparison:
		if t.op != "eq" || t.val == nil || !validName(t.attr) || strings.Contains(t.attr, ":") {
			return nil
		}
		return map[string]any{t.attr: t.val}
	case logical:
		if !t.and {
			return nil
		}
		l, r := seed(t.l), seed(t.r)
		if l == nil || r == nil {
			return nil
		}
		for k, v := range r {
			l[k] = v
		}
		return l
	}
	return nil
}

// indexTerm finds, in the top-level conjunction of a GET filter, an `eq` on one of the attributes a
// store can look up directly, so the rows are narrowed before any resource is opened.
func indexTerm(e expr, attrs ...string) (attr, value string, ok bool) {
	switch t := e.(type) {
	case comparison:
		if t.op != "eq" {
			return "", "", false
		}
		s, isString := t.val.(string)
		if !isString {
			return "", "", false
		}
		p, err := parsePath(t.attr)
		if err != nil || p.schema != "" || p.filter != nil || p.sub != "" {
			return "", "", false
		}
		for _, a := range attrs {
			if strings.EqualFold(p.attr, a) {
				return a, s, true
			}
		}
	case logical:
		if !t.and {
			return "", "", false
		}
		if a, v, ok := indexTerm(t.l, attrs...); ok {
			return a, v, true
		}
		return indexTerm(t.r, attrs...)
	}
	return "", "", false
}

// parseFilter parses a filter expression.
func parseFilter(s string) (expr, error) {
	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	p := &filterParser{toks: toks}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, errBadFilter
	}
	return e, nil
}

type token struct {
	text   string
	quoted bool
}

func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')':
			toks = append(toks, token{text: string(c)})
			i++
		case c == '"':
			j := i + 1
			for ; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '"' {
					break
				}
			}
			if j >= len(s) {
				return nil, errBadFilter
			}
			var str string
			if err := json.Unmarshal([]byte(s[i:j+1]), &str); err != nil {
				return nil, errBadFilter
			}
			toks = append(toks, token{text: str, quoted: true})
			i = j + 1
		default:
			j := i
			depth := 0
		scan:
			for ; j < len(s); j++ {
				switch c := s[j]; {
				case c == '[':
					depth++
				case c == ']':
					depth--
				case c == '"' && depth > 0:
					// A quoted value inside a value path may hold a space or a bracket.
					for j++; j < len(s) && s[j] != '"'; j++ {
						if s[j] == '\\' {
							j++
						}
					}
				case depth == 0 && (c == ' ' || c == '\t' || c == '(' || c == ')'):
					break scan
				}
			}
			if j > len(s) {
				j = len(s)
			}
			toks = append(toks, token{text: s[i:j]})
			i = j
		}
	}
	return toks, nil
}

type filterParser struct {
	toks []token
	pos  int
}

func (p *filterParser) peekWord(w string) bool {
	return p.pos < len(p.toks) && !p.toks[p.pos].quoted && strings.EqualFold(p.toks[p.pos].text, w)
}

func (p *filterParser) or() (expr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peekWord("or") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = logical{and: false, l: l, r: r}
	}
	return l, nil
}

func (p *filterParser) and() (expr, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.peekWord("and") {
		p.pos++
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = logical{and: true, l: l, r: r}
	}
	return l, nil
}

func (p *filterParser) unary() (expr, error) {
	if p.peekWord("not") {
		p.pos++
		if !p.peekWord("(") {
			return nil, errBadFilter
		}
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return negation{e: e}, nil
	}
	if p.peekWord("(") {
		p.pos++
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.peekWord(")") {
			return nil, errBadFilter
		}
		p.pos++
		return e, nil
	}
	return p.comparison()
}

func (p *filterParser) comparison() (expr, error) {
	if p.pos >= len(p.toks) {
		return nil, errBadFilter
	}
	attr := p.toks[p.pos]
	if attr.quoted || attr.text == "(" || attr.text == ")" {
		return nil, errBadFilter
	}
	if _, err := parsePath(attr.text); err != nil {
		return nil, errBadFilter
	}
	p.pos++
	// A value path standing alone, emails[type eq "work"], is true when some value matches.
	if strings.HasSuffix(attr.text, "]") && (p.pos >= len(p.toks) || p.peekWord("and") || p.peekWord("or") || p.peekWord(")")) {
		return comparison{attr: attr.text, op: "pr"}, nil
	}
	if p.pos >= len(p.toks) || p.toks[p.pos].quoted {
		return nil, errBadFilter
	}
	op := strings.ToLower(p.toks[p.pos].text)
	p.pos++
	switch op {
	case "pr":
		return comparison{attr: attr.text, op: op}, nil
	case "eq", "ne", "co", "sw", "ew", "gt", "ge", "lt", "le":
	default:
		return nil, errBadFilter
	}
	if p.pos >= len(p.toks) {
		return nil, errBadFilter
	}
	vt := p.toks[p.pos]
	p.pos++
	var val any
	switch {
	case vt.quoted:
		val = vt.text
	case strings.EqualFold(vt.text, "true"):
		val = true
	case strings.EqualFold(vt.text, "false"):
		val = false
	case strings.EqualFold(vt.text, "null"):
		val = nil
	default:
		if _, err := strconv.ParseFloat(vt.text, 64); err != nil {
			return nil, errBadFilter
		}
		val = json.Number(vt.text)
	}
	return comparison{attr: attr.text, op: op, val: val}, nil
}
