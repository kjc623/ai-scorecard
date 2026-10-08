// Package enforce evaluates the bundle's enforcement rules for one observation and turns the
// result into the decision the envelope records. It does no I/O.
package enforce

import (
	"slices"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// DefaultRuleID names the decision when no rule matches.
const DefaultRuleID = "policy.default"

// The values a rule's sanction list holds.
const (
	Sanctioned   = "sanctioned"
	Unsanctioned = "unsanctioned"
)

// Input is what a rule can match on.
type Input struct {
	Route           protocol.Route
	ToolFingerprint string
	// Labels are classifier classes. LabelsKnown is false when no classification completed (m0,
	// a degraded classification): the absence of a label then says nothing, so a rule that
	// lists labels does not match.
	Labels      []string
	LabelsKnown bool
}

// Decision is what the first matching rule asks for.
type Decision struct {
	RuleID  string
	Action  policy.RuleAction
	Message string
	Link    string
}

// Evaluate returns the first rule in b that matches in, or the default allow. A nil bundle has no
// rules.
func Evaluate(b *policy.Bundle, in Input) Decision {
	if b != nil {
		for _, r := range b.Rules {
			if matches(b, r.Match, in) {
				return Decision{RuleID: r.RuleID, Action: r.Action, Message: r.Message, Link: r.Link}
			}
		}
	}
	return Decision{RuleID: DefaultRuleID, Action: policy.RuleAllow}
}

// matches applies AND across the non-empty lists and OR inside each.
func matches(b *policy.Bundle, m policy.RuleMatch, in Input) bool {
	if len(m.Labels) > 0 && (!in.LabelsKnown || !slices.ContainsFunc(in.Labels, func(l string) bool { return slices.Contains(m.Labels, l) })) {
		return false
	}
	if len(m.Tools) > 0 && !slices.Contains(m.Tools, in.ToolFingerprint) {
		return false
	}
	// Categories resolve through the bundle's app catalog, which the bundle does not carry yet, so
	// no tool has a known category and a rule that lists categories never matches.
	if len(m.Categories) > 0 {
		return false
	}
	if len(m.Sanction) > 0 {
		state := Unsanctioned
		if slices.Contains(b.SanctionedTools, in.ToolFingerprint) {
			state = Sanctioned
		}
		if !slices.Contains(m.Sanction, state) {
			return false
		}
	}
	if len(m.Routes) > 0 && !slices.Contains(m.Routes, in.Route) {
		return false
	}
	return true
}

// RecordedAction is what the envelope records for d: what the route did, not what the rule asked
// for. A route that cannot stop the prompt records logged and keeps the rule id, so the rule that
// would have acted is visible.
func RecordedAction(d Decision, canEnforce bool) protocol.Decision {
	action := protocol.ActionLogged
	if canEnforce {
		switch d.Action {
		case policy.RuleBlock:
			action = protocol.ActionBlocked
		case policy.RuleWarn:
			action = protocol.ActionWarned
		}
	}
	return protocol.Decision{RuleID: d.RuleID, Action: action, DecidedLocally: true}
}

// Hook returns the pipeline's enforcement hook for one observation on a route. It reads the bundle
// in force when the pipeline calls it, after classification, so a rule change applies to the next
// decision without a restart.
func Hook(bundles func() *policy.Bundle, route protocol.Route, tool string, canEnforce bool) func(labels []string, known bool) protocol.Decision {
	return func(labels []string, known bool) protocol.Decision {
		var b *policy.Bundle
		if bundles != nil {
			b = bundles()
		}
		return RecordedAction(Evaluate(b, Input{Route: route, ToolFingerprint: tool, Labels: labels, LabelsKnown: known}), canEnforce)
	}
}
