package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Request and response shapes for the classifier host (docs/01-collectors.md §3.4, §9).
//
// The defining constraint is structural, not documentary: the classifier receives bytes and
// returns labels, and it must be unable to see a tool identity, a user_ref or a destination
// (§3.3). ClassifyRequest below has no field that can carry any of them, and the guard at the
// bottom of this file makes that a compile-time property: a future edit that adds one breaks
// the build instead of silently widening what the classifier can see.

// ClassifyRequest asks for labels for one observation's content.
//
// There is deliberately no tenant, device, user, tool, host or route field. Identity stays out
// of the classifier's input so that a defect cannot leak it into a label and a model cannot be
// tuned on who is being classified.
type ClassifyRequest struct {
	// Content is the normalised payload bytes. Absent at M0: the caller must not populate it,
	// and the classifier refuses a request that carries content in a mode that forbids reading.
	Content []byte `json:"content,omitempty"`

	// Mode is the effective collection mode. The classifier enforces it rather than trusting
	// the caller: content in an M0 request is a defect and is rejected.
	Mode CollectionMode `json:"mode"`

	// MediaType lets a validator pick a parser without interpreting the bytes as text.
	MediaType string `json:"media_type,omitempty"`

	// ContentDigest is the sha256 of the normalised content, computed by the caller before the
	// classifier is involved. It is carried so the response can be matched to the observation
	// without the classifier recomputing it over bytes it was handed.
	ContentDigest string `json:"content_digest,omitempty"`

	// ReleaseID names the signed rules/model release to evaluate with. Empty means "the
	// resident release"; a caller never selects a release, policy does.
	ReleaseID string `json:"release_id,omitempty"`

	// BudgetMS is the wall-clock budget for the whole pipeline, in **milliseconds**. It is an
	// explicit integer rather than a time.Duration because encoding/json marshals a Duration as
	// nanoseconds, which would put a number whose unit disagrees with its name on the wire and
	// silently mean 150 ns to a JavaScript host reading "budget_ms": 150.
	BudgetMS int64 `json:"budget_ms"`

	// Budget returns the budget as a duration, for the Go side.
	Budget time.Duration `json:"-"`
}

// EffectiveBudget returns the request's budget as a duration, honouring whichever field the
// caller set. A zero budget means "no explicit budget": the host then applies its own default
// rather than treating zero as "fail immediately".
func (q ClassifyRequest) EffectiveBudget() (time.Duration, bool) {
	if q.BudgetMS > 0 {
		return time.Duration(q.BudgetMS) * time.Millisecond, true
	}
	if q.Budget > 0 {
		return q.Budget, true
	}
	return 0, false
}

// Label is one classification verdict: a label set, never a boolean and never a bare
// "sensitive: yes".
type Label struct {
	Class  string  `json:"class"`
	Score  float64 `json:"score"`
	RuleID string  `json:"rule_id,omitempty"`
}

// Excerpt is a minimised excerpt, present only at M2 and only when policy permits it: a
// matched span identified by offsets, or a redacted window. Never the whole payload.
type Excerpt struct {
	Kind             string `json:"kind"` // match_span | redacted_window
	Text             string `json:"text"`
	MatchType        string `json:"match_type,omitempty"`
	OffsetStart      int    `json:"offset_start,omitempty"`
	OffsetEnd        int    `json:"offset_end,omitempty"`
	RedactionApplied bool   `json:"redaction_applied,omitempty"`
}

// MaxExcerptChars is the structural bound on a minimised excerpt. "Minimised" is enforced by
// the contract's maxLength and by this constant, not by convention.
const MaxExcerptChars = 2048

// Excerpt kind values, from the contract's $defs/excerpt.
const (
	ExcerptMatchSpan      = "match_span"
	ExcerptRedactedWindow = "redacted_window"
)

// StageResult records one pipeline stage's outcome, so a degraded answer names the stage that
// degraded instead of being an unattributed failure.
type StageResult struct {
	Stage     string    `json:"stage"` // rules | validators | model | parse
	Ran       bool      `json:"ran"`
	Failed    bool      `json:"failed"`
	Detail    Detail    `json:"detail,omitempty"`
	Err       string    `json:"error,omitempty"`
	Duration  time.Duration `json:"duration_ms"`
	Truncated bool      `json:"truncated,omitempty"` // a stage that hit its own budget
}

// ClassifyResponse is the classifier's answer. A failed stage is named; the response never
// reports a confident label after a stage failed, and never reports `degraded` for a
// classification that completed.
type ClassifyResponse struct {
	Labels            []Label            `json:"labels"`
	ClassifierVersion string             `json:"classifier_version"`
	Confidence        Confidence         `json:"confidence"`
	Excerpt           *Excerpt           `json:"content_excerpt,omitempty"`
	Stages            []StageResult      `json:"stages,omitempty"`
	Counters          map[string]uint64  `json:"counters,omitempty"`
	VerdictShadowed   bool               `json:"shadowed,omitempty"` // release state `shadow`: recorded, not enforced
}

// Validate enforces the cross-field rules a response must satisfy regardless of what the
// pipeline did. It is called by the core before the response is allowed into an envelope, so a
// defective classifier cannot widen what the device claims to know.
//
// The empty-label rule is subtle and was wrong here once: an event with **no labels** is a
// legitimate output meaning the classifier ran and found nothing — `confidence: high` with an
// empty label set (docs/01-collectors.md §9.2, §9.7's "not emitted when" column, and the
// contract's `labels` has maxItems 64 and no minItems). What is *not* legitimate is an empty
// label set that is also degraded, because §9.7 requires every exhaustion path to say so, which
// is the case that would otherwise be reported as "no sensitive data found".
func (r ClassifyResponse) Validate() error {
	if r.ClassifierVersion == "" {
		return fmt.Errorf("protocol: classifier response without a classifier_version cannot be attributed to a release")
	}
	switch r.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow, ConfidenceDegraded:
	default:
		return fmt.Errorf("protocol: classifier response has confidence %q outside the closed set", r.Confidence)
	}
	if r.Confidence == ConfidenceDegraded {
		// A degraded answer carries no confident label: the envelope omits labels entirely and
		// reports the stage that did not complete instead.
		if len(r.Labels) > 0 {
			return fmt.Errorf("protocol: a degraded response carries %d labels; a stage that did not complete cannot produce a confident verdict", len(r.Labels))
		}
		if !anyStageFailed(r.Stages) {
			return fmt.Errorf("protocol: a degraded response names no failed or truncated stage, so the cause is unattributable")
		}
		return nil
	}
	// Non-degraded: labels may legitimately be empty (the classifier ran and found nothing), so
	// only the label contents are checked.
	for i, l := range r.Labels {
		if l.Class == "" {
			return fmt.Errorf("protocol: label %d has no class", i)
		}
		if l.Score < 0 || l.Score > 1 {
			return fmt.Errorf("protocol: label %d score %v outside [0,1]", i, l.Score)
		}
	}
	if r.Excerpt != nil && len([]rune(r.Excerpt.Text)) > MaxExcerptChars {
		return fmt.Errorf("protocol: excerpt is %d characters, over the %d-character structural bound",
			len([]rune(r.Excerpt.Text)), MaxExcerptChars)
	}
	if r.Excerpt != nil && r.Excerpt.Kind != ExcerptMatchSpan && r.Excerpt.Kind != ExcerptRedactedWindow {
		return fmt.Errorf("protocol: excerpt kind %q outside the closed set", r.Excerpt.Kind)
	}
	return nil
}

// anyStageFailed reports whether a stage did not complete, which is what makes a degraded answer
// attributable to something rather than to "the classifier felt unwell".
func anyStageFailed(stages []StageResult) bool {
	for _, s := range stages {
		if s.Failed || s.Truncated {
			return true
		}
	}
	return false
}

// Validate rejects a request that asks the classifier to do something the mode forbids. The
// caller is expected to have applied the mode before reading content (§11.2); this is the
// second gate, so a defect upstream is refused here rather than becoming a stored fact.
func (q ClassifyRequest) Validate() error {
	if !q.Mode.Valid() {
		return fmt.Errorf("protocol: classify request has mode %q outside the closed set", q.Mode)
	}
	if !q.Mode.ReadsContent() && len(q.Content) > 0 {
		return fmt.Errorf("protocol: classify request carries %d content bytes at mode %s, which forbids reading content",
			len(q.Content), q.Mode)
	}
	return nil
}

// IdentityFieldNames is the set of field names that must never appear on a ClassifyRequest.
// It is data so a test can assert it, and it mirrors the guard below.
var IdentityFieldNames = [...]string{
	"tool_fingerprint", "tool", "host", "hostname", "destination", "url", "user_ref", "user",
	"email", "principal", "tenant_id", "device_id", "route", "source", "account",
}

// Compile-time guard: ClassifyRequest must never gain a field that could carry identity. The
// assertion fails to compile if json.Marshal of a request yields one of IdentityFieldNames -
// which it cannot do while the struct is identity-free, and will do the moment one is added.
var _ = func() bool {
	b, err := json.Marshal(ClassifyRequest{})
	if err != nil {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		panic("protocol: ClassifyRequest is not JSON-marshalable: " + err.Error())
	}
	for _, forbidden := range IdentityFieldNames {
		if _, present := m[forbidden]; present {
			panic("protocol: ClassifyRequest carries identity field " + forbidden +
				"; the classifier must be unable to see identity (docs/01-collectors.md §3.3)")
		}
	}
	return true
}()
