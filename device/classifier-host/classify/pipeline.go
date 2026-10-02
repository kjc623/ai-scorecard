package classify

import (
	"sort"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
	"github.com/shadow-ai-capture/device/classifier-host/validators"
	"github.com/shadow-ai-capture/device/protocol"
)

// Stage names used in protocol.StageResult. They follow §9.2's pipeline diagram, which names a
// Normalise stage that protocol's comment does not list; the protocol field is a free string
// (reported as an open decision).
const (
	StageMode       = "mode"
	StageNormalise  = "normalise"
	StageRules      = "rules"
	StageValidators = "validators"
	StageModel      = "model"
	StageParse      = "parse"
	StageRelease    = "release"
)

// Budget is §9.4's tiered table. The stage shares are the p95 columns; Total is the interactive
// target the sum is measured against (150 ms). The inline warn/block path has its own 300 ms
// target in the extension, which shows up as a larger request budget, not as larger stage
// shares.
type Budget struct {
	Normalise  time.Duration
	Rules      time.Duration
	Validators time.Duration
	Model      time.Duration
	Total      time.Duration
}

// NativeBudget is §9.4's p95 native column: 5 / 20 / 10 / 60, summing to 95 ms against the
// 150 ms interactive target.
func NativeBudget() Budget {
	return Budget{Normalise: 5 * time.Millisecond, Rules: 20 * time.Millisecond,
		Validators: 10 * time.Millisecond, Model: 60 * time.Millisecond, Total: 150 * time.Millisecond}
}

// WASMBudget is §9.4's p95 wasm column: 10 / 35 / 15 / 90.
func WASMBudget() Budget {
	return Budget{Normalise: 10 * time.Millisecond, Rules: 35 * time.Millisecond,
		Validators: 15 * time.Millisecond, Model: 90 * time.Millisecond, Total: 150 * time.Millisecond}
}

// Sum is the sum of the stage shares, which §9.4 compares with the total target.
func (b Budget) Sum() time.Duration {
	return b.Normalise + b.Rules + b.Validators + b.Model
}

// pipeline is one request's state. A fresh pipeline per request keeps the host free of shared
// mutable state on the interactive path.
type pipeline struct {
	rel     *release.Release
	req     protocol.ClassifyRequest
	budget  Budget
	limits  norm.Limits
	now     func() time.Time
	start   time.Time
	overall time.Time
	metrics *Metrics

	stages []protocol.StageResult
	count  map[string]uint64

	// degraded is set by any path that did not complete the work; cause names the first one.
	degraded bool
	cause    protocol.Detail
	causeErr string

	labels          []protocol.Label
	partialLabels   []protocol.Label
	confidence      protocol.Confidence
	excerpt         *protocol.Excerpt
	digest          string
	normalisedText  string
	deferred        bool
	peakParserBytes int64
	parsedOK        bool
}

func newPipeline(rel *release.Release, req protocol.ClassifyRequest, b Budget, lim norm.Limits, now func() time.Time, m *Metrics) *pipeline {
	if now == nil {
		now = time.Now
	}
	if b.Total <= 0 {
		b = NativeBudget()
	}
	start := now()
	total := b.Total
	if want, ok := req.EffectiveBudget(); ok && want > 0 && want < total {
		total = want
	}
	return &pipeline{
		rel: rel, req: req, budget: b, limits: lim, now: now,
		start: start, overall: start.Add(total), metrics: m,
		count: map[string]uint64{},
	}
}

// degrade records the first degradation cause. Later causes do not overwrite it: the first stage
// that did not complete is the cause an operator needs, and a cascade of consequences would
// bury it.
func (p *pipeline) degrade(detail protocol.Detail, err string) {
	if !p.degraded {
		p.degraded = true
		p.cause = detail
		p.causeErr = err
	}
}

// stageDeadline bounds one stage by both its own §9.4 share and the request's overall deadline.
// This is what "the host enforces it, not the stages" (§9.4) means in code.
func (p *pipeline) stageDeadline(share time.Duration) time.Time {
	d := p.now().Add(share)
	if d.After(p.overall) {
		return p.overall
	}
	return d
}

// expired reports whether the request's overall budget has run out.
func (p *pipeline) expired() bool { return !p.now().Before(p.overall) }

// inc bumps a counter.
func (p *pipeline) inc(name string) { p.count[name]++ }

// record appends one stage result and observes its duration in the rolling p95.
//
// §9.2: "every stage emits labels; a stage that does not run is recorded, never silently
// skipped" — so every stage the pipeline considered appears here, ran or not.
func (p *pipeline) record(stage string, ran, failed, truncated bool, detail protocol.Detail, err string, d time.Duration) {
	p.stages = append(p.stages, protocol.StageResult{
		Stage: stage, Ran: ran, Failed: failed, Detail: detail, Err: err, Duration: d, Truncated: truncated,
	})
	if p.metrics != nil {
		p.metrics.Observe(stage, d)
	}
}

// runNormalise executes §9.2's normalise stage over the effective media type: a parsed
// document's extracted text is normalised as text, because the parser has already resolved its
// format.
func (p *pipeline) runNormalise(mediaType string, content []byte) {
	t0 := p.now()
	res, err := norm.Normalise(mediaType, content, p.limits)
	took := p.now().Sub(t0)
	if err != nil {
		detail := protocol.DetailUndecodableContent
		if err == norm.ErrOverCap {
			detail = protocol.DetailContentOverCap
		}
		p.record(StageNormalise, true, true, false, detail, err.Error(), took)
		p.degrade(detail, err.Error())
		p.inc("normalise_failed")
		return
	}
	p.digest = res.Digest
	p.normalisedText = res.Text
	if p.req.ContentDigest != "" && p.req.ContentDigest != res.Digest {
		// The caller's digest and this host's disagree, which means the two normalise
		// differently. That is a dedup-contract defect (§9.2), not a classification failure:
		// the verdict stands, the disagreement is counted, and the host's digest is returned.
		p.inc("digest_mismatch")
	}
	if res.TruncationAffectsRules {
		p.record(StageNormalise, true, false, true, protocol.DetailNormaliseTruncated, res.Pathological, took)
		p.degrade(protocol.DetailNormaliseTruncated,
			"normalisation truncated the payload such that a rule could not see all of it")
		p.inc("normalise_truncated")
		return
	}
	p.record(StageNormalise, true, false, false, protocol.DetailNone, "", took)
}

// runRules executes the rules stage. Candidates are produced only when the stage completed; the
// engine's own deadline check is what makes a slow rules stage stop rather than run on.
func (p *pipeline) runRules(text string) []rules.Candidate {
	t0 := p.now()
	out := p.rel.Rules.Evaluate(text, p.stageDeadline(p.budget.Rules), p.now)
	took := p.now().Sub(t0)
	if out.Truncated {
		p.record(StageRules, true, false, true, protocol.DetailBudgetExhausted,
			"rules stage stopped in family "+out.StoppedInFamily+" at its "+p.budget.Rules.String()+" share", took)
		p.degrade(protocol.DetailBudgetExhausted, "rules stage exhausted its budget in family "+out.StoppedInFamily)
		p.inc("rules_budget_exhausted")
		return out.Candidates
	}
	p.record(StageRules, true, false, false, protocol.DetailNone, "", took)
	return out.Candidates
}

// runValidators executes §9.2's validators stage: a pattern match becomes a defensible verdict
// only when every validator the rule named confirms it. Candidates without validators are
// confirmed by the rules stage and produced here unchanged, so the stage always runs and is
// always recorded.
func (p *pipeline) runValidators(cands []rules.Candidate, text string) []protocol.Label {
	t0 := p.now()
	deadline := p.stageDeadline(p.budget.Validators)
	out := make([]protocol.Label, 0, len(cands))
	exhausted := false
	for _, c := range cands {
		if !p.now().Before(deadline) {
			exhausted = true
			break
		}
		ok := true
		for _, name := range c.PendingValidators {
			v, exists := validators.Lookup(name)
			if !exists {
				// Compile rejects unknown validators, so this cannot happen for a loaded
				// release; it is a defensive branch, and a missing validator must not become a
				// silent pass.
				ok = false
				p.inc("validator_missing")
				break
			}
			if !v.Check(c.MatchText(text)) {
				ok = false
				p.inc("validator_rejected")
				break
			}
		}
		if !ok {
			continue
		}
		out = append(out, protocol.Label{Class: c.Class, Score: c.Score, RuleID: c.RuleID})
	}
	took := p.now().Sub(t0)
	if exhausted {
		p.record(StageValidators, true, false, true, protocol.DetailBudgetExhausted,
			"validators stage stopped at its "+p.budget.Validators.String()+" share", took)
		p.degrade(protocol.DetailBudgetExhausted, "validators stage exhausted its budget")
		p.inc("validators_budget_exhausted")
		return out
	}
	p.record(StageValidators, true, false, false, protocol.DetailNone, "", took)
	return out
}

// runModel executes §9.2's model stage on the classes the rules did not resolve.
//
// §9.4's exhaustion behaviour is explicit: "Skip the model stage, emit rules-only labels with
// confidence: degraded". §9.7 adds the second cause: a model artefact that is missing,
// unloadable or unverified. Both are recorded as a stage that did not run, never as a stage that
// ran and found nothing.
func (p *pipeline) runModel(text string, resolved map[string]bool) []model.Prediction {
	if p.rel == nil || p.rel.Model == nil {
		p.record(StageModel, false, true, false, protocol.DetailModelUnavailable, "no model artefact is loaded", 0)
		p.degrade(protocol.DetailModelUnavailable, "the model artefact was missing, unloadable or failed to verify")
		p.inc("model_unavailable")
		return nil
	}
	t0 := p.now()
	deadline := p.stageDeadline(p.budget.Model)
	preds := p.rel.Model.Score(text, resolved, func() bool { return !p.now().Before(deadline) })
	took := p.now().Sub(t0)
	if !p.now().Before(deadline) {
		p.record(StageModel, true, false, true, protocol.DetailBudgetExhausted,
			"model stage stopped at its "+p.budget.Model.String()+" share", took)
		p.degrade(protocol.DetailBudgetExhausted, "model stage exhausted its budget")
		p.inc("model_budget_exhausted")
		return nil
	}
	p.record(StageModel, true, false, false, protocol.DetailNone, "", took)
	return preds
}

// mergeLabels folds rules/validators labels and model predictions into the response's label set.
//
// The contract allows up to 64 labels, each a (class, score, rule_id) triple. Two rules may
// produce the same class; the label set is keyed by class with the highest score winning, and
// the suppressed count is recorded rather than silently dropped. §9.2 leaves the merge open
// ("Output is a label set", with no keying rule), so this is reported as an open decision.
func mergeLabels(ruleLabels []protocol.Label, preds []model.Prediction) ([]protocol.Label, int) {
	byClass := map[string]protocol.Label{}
	suppressed := 0
	for _, l := range ruleLabels {
		prev, ok := byClass[l.Class]
		if ok {
			suppressed++
			if prev.Score >= l.Score {
				continue
			}
		}
		byClass[l.Class] = l
	}
	for _, p := range preds {
		if prev, ok := byClass[p.Class]; ok {
			suppressed++
			if prev.Score >= p.Score {
				continue
			}
		}
		byClass[p.Class] = protocol.Label{Class: p.Class, Score: p.Score}
	}
	out := make([]protocol.Label, 0, len(byClass))
	for _, l := range byClass {
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Class < out[j].Class
	})
	return out, suppressed
}

// band computes the confidence of a completed classification. §9.7 defines `degraded` exactly
// and leaves the confident bands to the caller; the rule used here is that a deterministic
// checksum-confirmed verdict is high, a model-only verdict is medium or low by score, and a
// completed run that found nothing is high (not degraded, §9.2).
func band(labels []protocol.Label, hadRuleEvidence bool) protocol.Confidence {
	if len(labels) == 0 {
		return protocol.ConfidenceHigh
	}
	if hadRuleEvidence {
		return protocol.ConfidenceHigh
	}
	best := 0.0
	for _, l := range labels {
		if l.Score > best {
			best = l.Score
		}
	}
	if best >= 0.85 {
		return protocol.ConfidenceMedium
	}
	return protocol.ConfidenceLow
}

// buildExcerpt produces the minimised excerpt of §9.2's `emits` policy. It exists only at M2:
// M1 has no excerpt by definition, and the contract forbids `content_excerpt` at M3, where the
// content is held locally instead.
func buildExcerpt(mode protocol.CollectionMode, cands []rules.Candidate, text string, maxChars int) *protocol.Excerpt {
	if mode != protocol.ModeM2 || len(cands) == 0 || maxChars <= 0 {
		return nil
	}
	c := cands[0]
	span := c.MatchText(text)
	if span == "" {
		return nil
	}
	runes := []rune(span)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return &protocol.Excerpt{
		Kind:        protocol.ExcerptMatchSpan,
		Text:        string(runes),
		MatchType:   c.Class,
		OffsetStart: c.Start,
		OffsetEnd:   c.Start + len(span),
	}
}
