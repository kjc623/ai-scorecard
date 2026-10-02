package rules

import (
	"strings"
	"time"
)

// Candidate is one rule's matched, context-filtered candidate before the validators stage.
//
// The rules stage deliberately stops here. §9.2 makes validators a *stage* — "validators turn a
// pattern match into a defensible verdict" — so a candidate carries the validator names the
// rule declared (`PendingValidators`) rather than a verdict, and the pipeline's validators
// stage confirms or drops it. That split is what gives the two stages separate measured
// budgets (§9.4) and what makes "the validator did not run" distinguishable from "the pattern
// did not match".
type Candidate struct {
	RuleID            string
	Class             string
	Family            string
	Score             float64
	Start             int // byte offset of the first surviving match in the normalised text
	End               int
	Matches           int
	PendingValidators []string
	Evidence          []string
}

// MatchText returns the first surviving match, which is the text a validator is asked about.
func (c Candidate) MatchText(text string) string {
	if c.Start < 0 || c.End > len(text) || c.Start > c.End {
		return ""
	}
	return text[c.Start:c.End]
}

// Outcome is what one evaluation of the rule set produced.
type Outcome struct {
	Candidates []Candidate

	// FamiliesRun names the families that were fully evaluated.
	FamiliesRun []string

	// Truncated is true when the deadline expired mid-evaluation. §9.4's exhaustion behaviour
	// for the rules stage is "stop after the current rule family, emit labels found, mark
	// degraded"; the candidates already produced are returned and the caller must degrade.
	Truncated bool

	// StoppedInFamily is the family the deadline expired inside, when Truncated is true.
	StoppedInFamily string

	RulesEvaluated int
	Duration       time.Duration
}

// Evaluate runs the rule set over normalised text under a wall-clock deadline.
//
// The deadline is the host's, never the rule's (§9.4: "the host enforces it, not the stages: the
// pipeline runs under a deadline and returns whatever the stages produced when it expires, and
// a stage cannot extend it"). The check happens between rules and between match batches, so the
// worst case past the deadline is one bounded regexp scan over already-capped input.
func (s *Set) Evaluate(text string, deadline time.Time, now func() time.Time) Outcome {
	if now == nil {
		now = time.Now
	}
	start := now()
	out := Outcome{}
	expired := func() bool {
		return !deadline.IsZero() && !now().Before(deadline)
	}

	for _, family := range s.families {
		if expired() {
			out.Truncated = true
			out.StoppedInFamily = family
			out.Duration = now().Sub(start)
			return out
		}
		complete := true
		for _, idx := range s.byFamily[family] {
			if expired() {
				out.Truncated = true
				out.StoppedInFamily = family
				complete = false
				break
			}
			out.RulesEvaluated++
			out.Candidates = append(out.Candidates, s.rules[idx].evaluate(text, now, deadline)...)
		}
		if complete {
			out.FamiliesRun = append(out.FamiliesRun, family)
		} else {
			// The family the deadline landed in is not claimed as run: a family whose rules
			// were only partially evaluated must not look complete to the caller.
			out.Duration = now().Sub(start)
			return out
		}
	}
	out.Duration = now().Sub(start)
	return out
}

// evaluate applies one rule: pattern scan, context filters, and candidate construction.
func (r *compiledRule) evaluate(text string, now func() time.Time, deadline time.Time) []Candidate {
	if r.pattern == nil {
		return nil
	}
	matches := r.pattern.FindAllStringIndex(text, r.maxMatches)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Candidate, 0, len(matches))
	for i, m := range matches {
		// Cheap deadline check inside a large match set: the scan itself is bounded by
		// max_matches and by the host's input cap, and this keeps a pathological text from
		// holding the interactive path for the full match budget.
		if i%8 == 7 && !deadline.IsZero() && !now().Before(deadline) {
			break
		}
		if !r.contextHolds(text, m[0], m[1]) {
			continue
		}
		out = append(out, Candidate{
			RuleID:            r.RuleID,
			Class:             r.Class,
			Family:            r.Family,
			Score:             r.Score,
			Start:             m[0],
			End:               m[1],
			Matches:           len(matches),
			PendingValidators: append([]string(nil), r.validators...),
			Evidence:          r.evidence(),
		})
	}
	return out
}

// contextHolds applies every context predicate to one match. Context is *filtering*, not
// scoring: §9.5's example uses `not_preceded_by "test"` to keep documentation examples out of a
// payment-card rule, and a filter that only lowered a score would still emit the label.
func (r *compiledRule) contextHolds(text string, start, end int) bool {
	for _, c := range r.contexts {
		before := windowBefore(text, start, c.window)
		after := windowAfter(text, end, c.window)
		switch c.predicate {
		case PredicateNotPrecededBy:
			if strings.Contains(before, c.value) {
				return false
			}
		case PredicatePrecededBy:
			if !strings.Contains(before, c.value) {
				return false
			}
		case PredicateNotFollowedBy:
			if strings.Contains(after, c.value) {
				return false
			}
		case PredicateFollowedBy:
			if !strings.Contains(after, c.value) {
				return false
			}
		}
	}
	return true
}

// windowBefore returns up to n bytes before the match, lowercased, cut at a UTF-8 boundary so a
// multi-byte character cannot be split into a value that would then match by accident.
func windowBefore(text string, start, n int) string {
	if start <= 0 {
		return ""
	}
	lo := start - n
	if lo < 0 {
		lo = 0
	}
	for lo < start && !utf8Start(text[lo]) {
		lo++
	}
	return strings.ToLower(text[lo:start])
}

// windowAfter returns up to n bytes after the match, lowercased and boundary-aligned.
func windowAfter(text string, end, n int) string {
	if end >= len(text) {
		return ""
	}
	hi := end + n
	if hi > len(text) {
		hi = len(text)
	}
	for hi > end && hi < len(text) && !utf8Start(text[hi]) {
		hi--
	}
	return strings.ToLower(text[end:hi])
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func (r *compiledRule) evidence() []string {
	ev := make([]string, 0, 1+len(r.contexts)+len(r.validators))
	ev = append(ev, "regex:"+r.pattern.String())
	for _, c := range r.contexts {
		ev = append(ev, "context:"+string(c.predicate)+"("+c.value+")")
	}
	for _, v := range r.validators {
		ev = append(ev, "validator:"+v)
	}
	return ev
}
