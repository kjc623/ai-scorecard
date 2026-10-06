// Package rules compiles and evaluates the classifier's rules document. A rule is data, never
// code: a single linear-time regular expression, optionally followed by validators from the
// closed set in package validators and by context predicates over the text around the match.
//
// The document is decoded strictly (unknown fields are an error), every limit is checked, and
// the whole document is rejected on the first defect, so a partially applied rule set cannot
// exist. Patterns are compiled with Go's regexp package (RE2), which has no backtracking
// constructs: a lookaround or a backreference is a compile error.
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/validators"
)

// Limits a rules document must stay within.
const (
	MaxDocumentBytes  = 1 << 20
	MaxRules          = 512
	MaxSignalsPerRule = 8
	MaxPatternBytes   = 512
	MaxMatchesPerRule = 64
	MaxValueBytes     = 128
	MaxIDBytes        = 64
	MaxContextWindow  = 256

	defaultContextWindow = 24
)

// SignalKind is the closed set of clauses a rule's `when` list may contain.
type SignalKind string

const (
	SignalRegex     SignalKind = "regex"
	SignalValidator SignalKind = "validator"
	SignalContext   SignalKind = "context"
)

// Predicate is the closed set of context operators.
type Predicate string

const (
	NotPrecededBy Predicate = "not_preceded_by"
	NotFollowedBy Predicate = "not_followed_by"
	PrecededBy    Predicate = "preceded_by"
	FollowedBy    Predicate = "followed_by"
)

// LinearDialect is the only regex dialect a rule may declare.
const LinearDialect = "linear"

// Signal is one clause of a rule. Fields that belong to another kind must be absent.
type Signal struct {
	Signal SignalKind `json:"signal"`

	Dialect    string `json:"dialect,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`

	Validator string `json:"validator,omitempty"`

	Predicate Predicate `json:"predicate,omitempty"`
	Value     string    `json:"value,omitempty"`
	Window    int       `json:"window,omitempty"`
}

// Rule is one rule. Its RuleID is emitted as the label's rule_id.
type Rule struct {
	RuleID string   `json:"rule_id"`
	Class  string   `json:"class"`
	Score  float64  `json:"score"`
	When   []Signal `json:"when"`
}

// Document is the rules file.
type Document struct {
	Version string `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Set is a compiled, immutable rule set, safe for concurrent use.
type Set struct {
	version string
	rules   []compiledRule
}

type compiledRule struct {
	id         string
	class      string
	score      float64
	pattern    *regexp.Regexp
	maxMatches int
	validators []string
	contexts   []context
}

type context struct {
	predicate Predicate
	value     string
	window    int
}

// Version is the rules document's version.
func (s *Set) Version() string { return s.version }

// Len is the number of compiled rules.
func (s *Set) Len() int { return len(s.rules) }

// ErrRejected wraps every compile-time rejection.
var ErrRejected = errors.New("rules: document rejected")

func rejectf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRejected, fmt.Sprintf(format, args...))
}

var (
	ruleIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	classPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Compile validates a rules document as a whole and returns the compiled set, or an error naming
// the first defect.
func Compile(raw []byte) (*Set, error) {
	if len(raw) == 0 {
		return nil, rejectf("empty document")
	}
	if len(raw) > MaxDocumentBytes {
		return nil, rejectf("document is %d bytes, over the %d-byte limit", len(raw), MaxDocumentBytes)
	}
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, rejectf("document does not match the schema: %v", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, rejectf("document has trailing content after the first JSON value")
	}
	if doc.Version == "" || len(doc.Version) > MaxIDBytes {
		return nil, rejectf("version is empty or over %d bytes", MaxIDBytes)
	}
	if len(doc.Rules) == 0 {
		return nil, rejectf("document has no rules")
	}
	if len(doc.Rules) > MaxRules {
		return nil, rejectf("document has %d rules, over the %d-rule limit", len(doc.Rules), MaxRules)
	}
	set := &Set{version: doc.Version}
	seen := map[string]bool{}
	for i, r := range doc.Rules {
		cr, err := compileRule(r, i)
		if err != nil {
			return nil, err
		}
		if seen[cr.id] {
			return nil, rejectf("rule %q is declared twice", cr.id)
		}
		seen[cr.id] = true
		set.rules = append(set.rules, cr)
	}
	return set, nil
}

func compileRule(r Rule, index int) (compiledRule, error) {
	where := fmt.Sprintf("rule %d", index)
	if r.RuleID != "" {
		where = fmt.Sprintf("rule %q", r.RuleID)
	}
	switch {
	case r.RuleID == "" || len(r.RuleID) > MaxIDBytes || !ruleIDPattern.MatchString(r.RuleID):
		return compiledRule{}, rejectf("%s: rule_id must match %s and be at most %d bytes", where, ruleIDPattern, MaxIDBytes)
	case r.Class == "" || len(r.Class) > MaxIDBytes || !classPattern.MatchString(r.Class):
		return compiledRule{}, rejectf("%s: class %q must match %s and be at most %d bytes", where, r.Class, classPattern, MaxIDBytes)
	case r.Score < 0 || r.Score > 1:
		return compiledRule{}, rejectf("%s: score %v is outside [0,1]", where, r.Score)
	case len(r.When) == 0:
		return compiledRule{}, rejectf("%s: `when` is empty", where)
	case len(r.When) > MaxSignalsPerRule:
		return compiledRule{}, rejectf("%s: %d signals, over the %d-signal limit", where, len(r.When), MaxSignalsPerRule)
	}

	cr := compiledRule{id: r.RuleID, class: r.Class, score: r.Score, maxMatches: MaxMatchesPerRule}
	for i, s := range r.When {
		if (i == 0) != (s.Signal == SignalRegex) {
			return compiledRule{}, rejectf("%s: the first signal, and only the first, must be the regex", where)
		}
		switch s.Signal {
		case SignalRegex:
			if err := onlyFields(where, i, s, "dialect", "pattern", "max_matches"); err != nil {
				return compiledRule{}, err
			}
			if s.Dialect != LinearDialect {
				return compiledRule{}, rejectf("%s: regex dialect %q is not %q", where, s.Dialect, LinearDialect)
			}
			if s.Pattern == "" || len(s.Pattern) > MaxPatternBytes {
				return compiledRule{}, rejectf("%s: pattern is empty or over the %d-byte limit", where, MaxPatternBytes)
			}
			re, err := regexp.Compile(s.Pattern)
			if err != nil {
				return compiledRule{}, rejectf("%s: pattern does not compile in the linear dialect: %v", where, err)
			}
			cr.pattern = re
			if s.MaxMatches != 0 {
				if s.MaxMatches < 1 || s.MaxMatches > MaxMatchesPerRule {
					return compiledRule{}, rejectf("%s: max_matches %d is outside 1..%d", where, s.MaxMatches, MaxMatchesPerRule)
				}
				cr.maxMatches = s.MaxMatches
			}
		case SignalValidator:
			if err := onlyFields(where, i, s, "validator"); err != nil {
				return compiledRule{}, err
			}
			if _, ok := validators.Lookup(s.Validator); !ok {
				return compiledRule{}, rejectf("%s: validator %q is not in the closed validator set %v", where, s.Validator, validators.Names())
			}
			cr.validators = append(cr.validators, s.Validator)
		case SignalContext:
			if err := onlyFields(where, i, s, "predicate", "value", "window"); err != nil {
				return compiledRule{}, err
			}
			switch s.Predicate {
			case NotPrecededBy, NotFollowedBy, PrecededBy, FollowedBy:
			default:
				return compiledRule{}, rejectf("%s: context predicate %q is outside the closed set", where, s.Predicate)
			}
			if s.Value == "" || len(s.Value) > MaxValueBytes {
				return compiledRule{}, rejectf("%s: context value is empty or over the %d-byte limit", where, MaxValueBytes)
			}
			w := s.Window
			if w == 0 {
				w = defaultContextWindow
			}
			if w < 0 || w > MaxContextWindow {
				return compiledRule{}, rejectf("%s: context window %d is outside 1..%d", where, s.Window, MaxContextWindow)
			}
			cr.contexts = append(cr.contexts, context{predicate: s.Predicate, value: strings.ToLower(s.Value), window: w})
		default:
			return compiledRule{}, rejectf("%s: signal kind %q is outside the closed set {regex, validator, context}", where, s.Signal)
		}
	}
	return cr, nil
}

// onlyFields rejects a signal carrying a field of another kind, which would otherwise be
// silently ignored.
func onlyFields(where string, index int, s Signal, allowed ...string) error {
	present := map[string]bool{
		"dialect":     s.Dialect != "",
		"pattern":     s.Pattern != "",
		"max_matches": s.MaxMatches != 0,
		"validator":   s.Validator != "",
		"predicate":   s.Predicate != "",
		"value":       s.Value != "",
		"window":      s.Window != 0,
	}
	for _, a := range allowed {
		delete(present, a)
	}
	for field, set := range present {
		if set {
			return rejectf("%s: signal %d is kind %q but carries %q, which belongs to another kind", where, index, s.Signal, field)
		}
	}
	return nil
}
