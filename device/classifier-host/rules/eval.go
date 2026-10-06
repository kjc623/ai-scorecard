package rules

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Candidate is one rule match that survived the rule's context predicates. Its validators have
// not run yet: they are the pipeline's next stage.
type Candidate struct {
	RuleID     string
	Class      string
	Score      float64
	Start, End int // byte offsets of the match in the evaluated text
	Validators []string
}

// MatchText returns the matched span.
func (c Candidate) MatchText(text string) string {
	if c.Start < 0 || c.End > len(text) || c.Start > c.End {
		return ""
	}
	return text[c.Start:c.End]
}

// Outcome is the result of one evaluation.
type Outcome struct {
	Candidates []Candidate
	// Truncated is set when the deadline expired before every rule ran; StoppedAt names the
	// first rule that did not run to completion.
	Truncated bool
	StoppedAt string
}

// Evaluate runs every rule over text in declaration order. The deadline is checked between
// rules and every few matches, so the overrun past it is at most one bounded scan.
func (s *Set) Evaluate(text string, deadline time.Time, now func() time.Time) Outcome {
	var out Outcome
	expired := func() bool { return !deadline.IsZero() && !now().Before(deadline) }
	for i := range s.rules {
		r := &s.rules[i]
		if expired() {
			out.Truncated, out.StoppedAt = true, r.id
			return out
		}
		cands, complete := r.evaluate(text, expired)
		out.Candidates = append(out.Candidates, cands...)
		if !complete {
			out.Truncated, out.StoppedAt = true, r.id
			return out
		}
	}
	return out
}

func (r *compiledRule) evaluate(text string, expired func() bool) ([]Candidate, bool) {
	matches := r.pattern.FindAllStringIndex(text, r.maxMatches)
	var out []Candidate
	for i, m := range matches {
		if i%8 == 7 && expired() {
			return out, false
		}
		if !r.contextHolds(text, m[0], m[1]) {
			continue
		}
		out = append(out, Candidate{
			RuleID: r.id, Class: r.class, Score: r.score,
			Start: m[0], End: m[1], Validators: r.validators,
		})
	}
	return out, true
}

// contextHolds applies every context predicate to one match. A predicate filters the match out;
// it never adjusts the score.
func (r *compiledRule) contextHolds(text string, start, end int) bool {
	for _, c := range r.contexts {
		var window string
		switch c.predicate {
		case NotPrecededBy, PrecededBy:
			window = windowBefore(text, start, c.window)
		default:
			window = windowAfter(text, end, c.window)
		}
		found := strings.Contains(window, c.value)
		if found != (c.predicate == PrecededBy || c.predicate == FollowedBy) {
			return false
		}
	}
	return true
}

// windowBefore returns up to n bytes before start, lowercased and cut at a character boundary.
func windowBefore(text string, start, n int) string {
	lo := max(start-n, 0)
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	return strings.ToLower(text[lo:start])
}

// windowAfter returns up to n bytes after end, lowercased and cut at a character boundary.
func windowAfter(text string, end, n int) string {
	hi := min(end+n, len(text))
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return strings.ToLower(text[end:hi])
}
