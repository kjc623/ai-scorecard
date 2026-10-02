// Package rules implements the rules DSL of docs/01-collectors.md §9.5 as **data evaluated by an
// interpreter**, never as code.
//
// The two properties this package exists to make structural:
//
//  1. **A rule cannot do anything but match.** The file is decoded with unknown fields
//     rejected, every signal is one of a closed set of kinds, every validator name must exist in
//     the binary's closed validator set, and the whole file is validated before any part of it
//     is used. There is no expression, loop, callback, host function or reference that could
//     reach code, the network, or the spool; the interpreter's own import set is asserted by
//     test (rules_imports_test.go) so a future edit that adds os/net/exec fails the build's
//     tests rather than quietly widening what a signed rule file can do.
//  2. **A malformed or hostile file is rejected whole.** Compile either returns a fully built
//     Set or an error, and the caller (package release) only swaps a Set in after that error is
//     nil. There is no partial application: §9.6's "a release that fails to load ... never falls
//     back to 'no rules'".
//
// Matching is linear time by construction: patterns are compiled by Go's regexp package, which
// is RE2 — a linear-time engine with no backtracking constructs. A pattern using a lookaround,
// a backreference or any other backtracking feature is a *compile error*, which is §9.5's
// "the engine cannot backtrack, so the attack does not exist" arriving structurally rather than
// by review.
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/validators"
)

// Caps are the load-time bounds of §9.5: "Rule count, matches per rule and pattern length are
// capped at load time; a signed release that violates them is rejected, reported, and the
// previous release retained."
type Caps struct {
	MaxRules           int
	MaxFamilies        int
	MaxSignalsPerRule  int
	MaxPatternBytes    int
	MaxMatchesPerRule  int
	MaxValueBytes      int
	MaxRuleIDBytes     int
	MaxClassBytes      int
	MaxReleaseBytes    int
	MaxContextWindow   int
	MaxVersionBytes    int
}

// DefaultCaps is the shipped bound. Values are deliberately small: a signed release that needs
// more than this is a release that should have been split, and the cost of being wrong is a
// rejected release rather than a slow interactive path.
func DefaultCaps() Caps {
	return Caps{
		MaxRules:          512,
		MaxFamilies:       64,
		MaxSignalsPerRule: 8,
		MaxPatternBytes:   512,
		MaxMatchesPerRule: 64,
		MaxValueBytes:     128,
		MaxRuleIDBytes:    64,
		MaxClassBytes:     64,
		MaxReleaseBytes:   1 << 20,
		MaxContextWindow:  256,
		MaxVersionBytes:   64,
	}
}

// SignalKind is the closed set of signals a rule may reference (§9.5).
type SignalKind string

const (
	SignalRegex     SignalKind = "regex"
	SignalValidator SignalKind = "validator"
	SignalContext   SignalKind = "context"
)

// ContextPredicate is the closed set of context operators. The names are the vocabulary of
// §9.5's example (`not_preceded_by`); nothing else is accepted.
type ContextPredicate string

const (
	PredicateNotPrecededBy ContextPredicate = "not_preceded_by"
	PredicateNotFollowedBy ContextPredicate = "not_followed_by"
	PredicatePrecededBy    ContextPredicate = "preceded_by"
	PredicateFollowedBy    ContextPredicate = "followed_by"
)

// LinearDialect is the only regex dialect. Naming it in the data keeps a future non-linear
// engine from being selectable by a rule file.
const LinearDialect = "linear"

// Signal is one clause of a rule's `when` array. Fields not belonging to the signal's kind must
// be absent: a `regex` signal carrying a `validator` is a malformed file, not a signal to ignore.
type Signal struct {
	Signal SignalKind `json:"signal"`

	// regex
	Dialect    string `json:"dialect,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`

	// validator
	Validator string `json:"validator,omitempty"`

	// context
	Predicate ContextPredicate `json:"predicate,omitempty"`
	Value     string           `json:"value,omitempty"`
	Window    int              `json:"window,omitempty"`
}

// Emits is the excerpt policy of a rule. `excerpt_kind` is the contract's closed excerpt set
// (protocol.ExcerptMatchSpan / protocol.ExcerptRedactedWindow).
type Emits struct {
	ExcerptKind string `json:"excerpt_kind,omitempty"`
}

// Rule is one rule of the release. The struct is the *whole* schema: unknown keys are rejected
// at decode time, so a file attempting to smuggle an operator it did not declare as a field
// fails to load.
type Rule struct {
	RuleID string   `json:"rule_id"`
	Class  string   `json:"class"`
	Family string   `json:"family,omitempty"`
	Score  float64  `json:"score"`
	When   []Signal `json:"when"`
	Emits  Emits    `json:"emits,omitempty"`
}

// File is the release's rules document.
type File struct {
	Version string `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Set is a compiled, immutable rule set. It is safe for concurrent use: patterns are compiled
// once and *regexp.Regexp is goroutine-safe.
type Set struct {
	version  string
	caps     Caps
	rules    []compiledRule
	families []string
	byFamily map[string][]int
	ruleIDs  []string
}

type compiledRule struct {
	Rule
	pattern    *regexp.Regexp
	maxMatches int
	validators []string
	contexts   []compiledContext
}

type compiledContext struct {
	predicate ContextPredicate
	value     string
	window    int
}

// Version is the release version the rules were compiled from.
func (s *Set) Version() string { return s.version }

// RuleCount is the number of compiled rules.
func (s *Set) RuleCount() int { return len(s.rules) }

// Families lists the rule families in evaluation order.
func (s *Set) Families() []string {
	out := make([]string, len(s.families))
	copy(out, s.families)
	return out
}

// RuleIDs lists every rule id in the set, sorted.
func (s *Set) RuleIDs() []string {
	out := make([]string, len(s.ruleIDs))
	copy(out, s.ruleIDs)
	return out
}

// ErrRejected wraps every load-time rejection. The message always names the field or rule, so a
// rejected release is actionable rather than "invalid rules".
var ErrRejected = errors.New("rules: release rejected")

func rejectf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRejected, fmt.Sprintf(format, args...))
}

// Compile validates a rules document whole and returns the compiled set, or an error naming the
// first defect. Nothing is returned on error.
func Compile(raw []byte, caps Caps) (*Set, error) {
	if caps.MaxRules <= 0 {
		caps = DefaultCaps()
	}
	if len(raw) == 0 {
		return nil, rejectf("empty rules document")
	}
	if len(raw) > caps.MaxReleaseBytes {
		return nil, rejectf("rules document is %d bytes, over the %d-byte cap", len(raw), caps.MaxReleaseBytes)
	}

	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, rejectf("rules document is not the declared schema: %v", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, rejectf("rules document has trailing content after the first JSON value")
	}
	if len(f.Version) == 0 {
		return nil, rejectf("rules document has no version")
	}
	if len(f.Version) > caps.MaxVersionBytes {
		return nil, rejectf("rules version is %d bytes, over the %d-byte cap", len(f.Version), caps.MaxVersionBytes)
	}
	if len(f.Rules) == 0 {
		return nil, rejectf("rules document has no rules")
	}
	if len(f.Rules) > caps.MaxRules {
		return nil, rejectf("rules document has %d rules, over the %d-rule cap", len(f.Rules), caps.MaxRules)
	}

	set := &Set{version: f.Version, caps: caps, byFamily: map[string][]int{}}
	seenIDs := map[string]bool{}
	for i, r := range f.Rules {
		cr, err := compileRule(r, i, caps, seenIDs)
		if err != nil {
			return nil, err
		}
		idx := len(set.rules)
		set.rules = append(set.rules, cr)
		if _, ok := set.byFamily[cr.Family]; !ok {
			set.families = append(set.families, cr.Family)
		}
		set.byFamily[cr.Family] = append(set.byFamily[cr.Family], idx)
		set.ruleIDs = append(set.ruleIDs, cr.RuleID)
	}
	if len(set.families) > caps.MaxFamilies {
		return nil, rejectf("rules document has %d families, over the %d-family cap", len(set.families), caps.MaxFamilies)
	}
	sort.Strings(set.ruleIDs)
	return set, nil
}

var (
	ruleIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	classPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

func compileRule(r Rule, index int, caps Caps, seenIDs map[string]bool) (compiledRule, error) {
	where := fmt.Sprintf("rule %d", index)
	if r.RuleID != "" {
		where = fmt.Sprintf("rule %q", r.RuleID)
	}
	if r.RuleID == "" {
		return compiledRule{}, rejectf("%s has no rule_id", where)
	}
	if len(r.RuleID) > caps.MaxRuleIDBytes {
		return compiledRule{}, rejectf("%s rule_id is %d bytes, over the %d-byte cap", where, len(r.RuleID), caps.MaxRuleIDBytes)
	}
	if !ruleIDPattern.MatchString(r.RuleID) {
		return compiledRule{}, rejectf("%s rule_id must match %s (it is emitted as label.rule_id)", where, ruleIDPattern)
	}
	if seenIDs[r.RuleID] {
		return compiledRule{}, rejectf("%s is declared twice; label.rule_id would be ambiguous", where)
	}
	seenIDs[r.RuleID] = true
	if r.Class == "" {
		return compiledRule{}, rejectf("%s has no class", where)
	}
	if len(r.Class) > caps.MaxClassBytes {
		return compiledRule{}, rejectf("%s class is %d bytes, over the %d-byte cap", where, len(r.Class), caps.MaxClassBytes)
	}
	if !classPattern.MatchString(r.Class) {
		return compiledRule{}, rejectf("%s class %q must match %s", where, r.Class, classPattern)
	}
	if r.Score < 0 || r.Score > 1 {
		return compiledRule{}, rejectf("%s score %v is outside [0,1]", where, r.Score)
	}
	if len(r.When) == 0 {
		return compiledRule{}, rejectf("%s has an empty `when`; a rule that matches everything is not data, it is a policy default", where)
	}
	if len(r.When) > caps.MaxSignalsPerRule {
		return compiledRule{}, rejectf("%s has %d signals, over the %d cap", where, len(r.When), caps.MaxSignalsPerRule)
	}
	if r.Emits.ExcerptKind != "" && r.Emits.ExcerptKind != "match_span" && r.Emits.ExcerptKind != "redacted_window" {
		return compiledRule{}, rejectf("%s declares excerpt_kind %q outside the closed set", where, r.Emits.ExcerptKind)
	}

	cr := compiledRule{Rule: r}
	cr.maxMatches = caps.MaxMatchesPerRule
	regexSeen := false
	for si, s := range r.When {
		switch s.Signal {
		case SignalRegex:
			if regexSeen {
				return compiledRule{}, rejectf("%s has a second regex signal at index %d; one pattern per rule, so candidates have one origin", where, si)
			}
			if si != 0 {
				return compiledRule{}, rejectf("%s has its regex signal at index %d; it must come first so validators and context filter its candidates", where, si)
			}
			regexSeen = true
			if s.Dialect != LinearDialect {
				return compiledRule{}, rejectf("%s regex dialect %q is not %q", where, s.Dialect, LinearDialect)
			}
			if err := onlyFields(where, si, s, "signal", "dialect", "pattern", "max_matches"); err != nil {
				return compiledRule{}, err
			}
			if s.Pattern == "" {
				return compiledRule{}, rejectf("%s regex signal has no pattern", where)
			}
			if len(s.Pattern) > caps.MaxPatternBytes {
				return compiledRule{}, rejectf("%s pattern is %d bytes, over the %d-byte cap", where, len(s.Pattern), caps.MaxPatternBytes)
			}
			re, err := regexp.Compile(s.Pattern)
			if err != nil {
				return compiledRule{}, rejectf("%s pattern does not compile in the linear dialect: %v", where, err)
			}
			// RE2 accepts a few constructs that are not linear-time hazards but are also not
			// useful here; the important direction is the one that cannot happen: a pattern
			// with a lookaround or backreference is a compile error above, because RE2 has no
			// such syntax. This check records the invariant rather than relying on review.
			if strings.Contains(s.Pattern, `\C`) {
				return compiledRule{}, rejectf("%s pattern uses \\C, which is not supported on every target", where)
			}
			cr.pattern = re
			if s.MaxMatches != 0 {
				if s.MaxMatches < 1 || s.MaxMatches > caps.MaxMatchesPerRule {
					return compiledRule{}, rejectf("%s max_matches %d is outside 1..%d", where, s.MaxMatches, caps.MaxMatchesPerRule)
				}
				cr.maxMatches = s.MaxMatches
			}
		case SignalValidator:
			if !regexSeen {
				return compiledRule{}, rejectf("%s has a validator signal at index %d before its regex signal", where, si)
			}
			if err := onlyFields(where, si, s, "signal", "validator"); err != nil {
				return compiledRule{}, err
			}
			if s.Validator == "" {
				return compiledRule{}, rejectf("%s validator signal names no validator", where)
			}
			if _, ok := validators.Lookup(s.Validator); !ok {
				return compiledRule{}, rejectf("%s names validator %q, which is not in the binary's closed validator set %v",
					where, s.Validator, validators.Names())
			}
			cr.validators = append(cr.validators, s.Validator)
		case SignalContext:
			if !regexSeen {
				return compiledRule{}, rejectf("%s has a context signal at index %d before its regex signal", where, si)
			}
			if err := onlyFields(where, si, s, "signal", "predicate", "value", "window"); err != nil {
				return compiledRule{}, err
			}
			switch s.Predicate {
			case PredicateNotPrecededBy, PredicateNotFollowedBy, PredicatePrecededBy, PredicateFollowedBy:
			default:
				return compiledRule{}, rejectf("%s uses context predicate %q outside the closed set {not_preceded_by, not_followed_by, preceded_by, followed_by}", where, s.Predicate)
			}
			if s.Value == "" {
				return compiledRule{}, rejectf("%s context signal has no value", where)
			}
			if len(s.Value) > caps.MaxValueBytes {
				return compiledRule{}, rejectf("%s context value is %d bytes, over the %d-byte cap", where, len(s.Value), caps.MaxValueBytes)
			}
			w := s.Window
			if w == 0 {
				w = 24
			}
			if w < 0 || w > caps.MaxContextWindow {
				return compiledRule{}, rejectf("%s context window %d is outside 0..%d", where, s.Window, caps.MaxContextWindow)
			}
			cr.contexts = append(cr.contexts, compiledContext{predicate: s.Predicate, value: strings.ToLower(s.Value), window: w})
		default:
			return compiledRule{}, rejectf("%s declares signal kind %q, which is outside the closed operator set {regex, validator, context}", where, s.Signal)
		}
	}
	if cr.pattern == nil {
		return compiledRule{}, rejectf("%s has no regex signal", where)
	}
	if cr.Family == "" {
		cr.Family = cr.Class
	}
	if len(cr.Family) > caps.MaxClassBytes {
		return compiledRule{}, rejectf("%s family is %d bytes, over the %d-byte cap", where, len(cr.Family), caps.MaxClassBytes)
	}
	if cr.Family != cr.Class && !classPattern.MatchString(cr.Family) {
		return compiledRule{}, rejectf("%s family %q must match %s", where, cr.Family, classPattern)
	}
	return cr, nil
}

// onlyFields rejects a signal that carries a field belonging to another signal kind. Without
// it, `{"signal":"regex","pattern":"x","validator":"luhn"}` would silently ignore the
// validator, and a rule author's intent would differ from the loaded rule — the class of
// defect a signed-data DSL cannot afford.
func onlyFields(where string, index int, s Signal, allowed ...string) error {
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	present := map[string]bool{}
	if s.Dialect != "" {
		present["dialect"] = true
	}
	if s.Pattern != "" {
		present["pattern"] = true
	}
	if s.MaxMatches != 0 {
		present["max_matches"] = true
	}
	if s.Validator != "" {
		present["validator"] = true
	}
	if s.Predicate != "" {
		present["predicate"] = true
	}
	if s.Value != "" {
		present["value"] = true
	}
	if s.Window != 0 {
		present["window"] = true
	}
	for k := range present {
		if !set[k] {
			return rejectf("%s signal %d is kind %q but carries field %q, which belongs to another signal kind", where, index, s.Signal, k)
		}
	}
	return nil
}
