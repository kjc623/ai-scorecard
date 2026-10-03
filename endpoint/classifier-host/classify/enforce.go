package classify

import (
	"sort"

	"github.com/shadow-ai-capture/device/protocol"
)

// ClassThreshold is one enforcement threshold for one class.
type ClassThreshold struct {
	Class    string  `json:"class"`
	MinScore float64 `json:"min_score"`
}

// EnforcementPolicy is the enforcing decision of §8.2: "Where a false positive is expensive is
// *enforcement* — a `blocked` decision on a legitimate request — so the enforcing decision is a
// separate, higher-threshold policy rule."
//
// This struct is where the signed bundle's enforcement policy lands. The pipeline computes the
// labels; only a release in `enforcing` state is allowed to turn them into an action (§9.6),
// and a degraded classification is never allowed to act at all.
type EnforcementPolicy struct {
	Block []ClassThreshold `json:"block,omitempty"`
	Warn  []ClassThreshold `json:"warn,omitempty"`
}

// DefaultEnforcement is the shipped default. Blocking is deliberately narrow — only
// checksum-confirmed classes at a high score — because §8.2 makes a false `blocked` the
// expensive error, and the discovery value of the product comes from the `logged` path.
func DefaultEnforcement() EnforcementPolicy {
	return EnforcementPolicy{
		Block: []ClassThreshold{
			{Class: "payment_card", MinScore: 0.9},
			{Class: "government_id", MinScore: 0.9},
		},
		Warn: []ClassThreshold{
			{Class: "payment_card", MinScore: 0.5},
			{Class: "government_id", MinScore: 0.5},
			{Class: "customer_pii", MinScore: 0.7},
			{Class: "health", MinScore: 0.7},
			{Class: "source_code", MinScore: 0.7},
			{Class: "legal", MinScore: 0.7},
		},
	}
}

// Decision is the enforcement verdict for one label set.
type Decision struct {
	Action string // blocked | warned | logged
	RuleID string
	Class  string
	Score  float64
}

func severity(action string) int {
	switch action {
	case protocol.ActionBlocked:
		return 2
	case protocol.ActionWarned:
		return 1
	default:
		return 0
	}
}

// Decide picks the most severe action any threshold justifies. Ties are broken by class name so
// the decision is deterministic for the equivalence property, and `logged` is the fallback
// (§8.2: enforcement is the higher bar, not the default).
func (p EnforcementPolicy) Decide(labels []protocol.Label) Decision {
	best := Decision{Action: protocol.ActionLogged}
	consider := func(list []ClassThreshold, action string) {
		for _, th := range list {
			for _, l := range labels {
				if l.Class != th.Class || l.Score < th.MinScore {
					continue
				}
				if severity(action) > severity(best.Action) ||
					(severity(action) == severity(best.Action) && (best.Class == "" || l.Class < best.Class)) {
					best = Decision{Action: action, RuleID: l.RuleID, Class: l.Class, Score: l.Score}
				}
			}
		}
	}
	consider(p.Warn, protocol.ActionWarned)
	consider(p.Block, protocol.ActionBlocked)
	if best.RuleID == "" {
		best.RuleID = best.Class
	}
	return best
}

// Classes lists the classes the policy can act on, sorted, for the health channel and for
// documentation.
func (p EnforcementPolicy) Classes() []string {
	seen := map[string]bool{}
	for _, t := range append(append([]ClassThreshold{}, p.Block...), p.Warn...) {
		seen[t.Class] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
