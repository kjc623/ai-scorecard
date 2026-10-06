package classify

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
	"github.com/shadow-ai-capture/device/classifier-host/validators"
	"github.com/shadow-ai-capture/device/protocol"
)

// Stage names, as reported in protocol.StageResult.
const (
	StageRequest    = "request"
	StageMode       = "mode"
	StageRelease    = "release"
	StageParse      = "parse"
	StageNormalise  = "normalise"
	StageRules      = "rules"
	StageValidators = "validators"
	StageModel      = "model"
)

// Budget is the time the rules, validators and model stages may each take, and the total for one
// classification. Normalisation is bounded by its input limits and document parsing by the
// parser's own timeout.
type Budget struct {
	Rules      time.Duration
	Validators time.Duration
	Model      time.Duration
	Total      time.Duration
}

// DefaultBudget keeps a classification well inside an interactive submission.
func DefaultBudget() Budget {
	return Budget{
		Rules:      20 * time.Millisecond,
		Validators: 10 * time.Millisecond,
		Model:      60 * time.Millisecond,
		Total:      150 * time.Millisecond,
	}
}

// pipeline is one request's state.
type pipeline struct {
	opts *Options
	req  protocol.ClassifyRequest

	deadline time.Time
	stages   []protocol.StageResult
	degraded bool

	labels    []protocol.Label
	fromRules bool
	excerpt   *protocol.Excerpt
}

// run executes the stages in order and stops at the first that cannot complete: a degraded
// response carries no labels, so later stages would be wasted work.
func (p *pipeline) run(ctx context.Context) {
	if err := p.req.Validate(); err != nil {
		p.fail(StageMode, protocol.DetailModeViolation, err.Error())
		return
	}
	if id := p.req.ReleaseID; id != "" && id != p.opts.Release.Version {
		p.fail(StageRelease, protocol.DetailReleaseLoadFailed,
			fmt.Sprintf("the request names release %q; this host runs %q", id, p.opts.Release.Version))
		return
	}
	content, mediaType := p.req.Content, p.req.MediaType
	if isDocument(mediaType) {
		text, ok := p.parse(ctx, mediaType, content)
		if !ok {
			return
		}
		content, mediaType = []byte(text), "text/plain"
	} else if len(content) == 0 {
		p.fail(StageNormalise, protocol.DetailContentUnprocessable,
			"the request carries no content at mode "+string(p.req.Mode))
		return
	}

	total := p.opts.Budget.Total
	if want, ok := p.req.EffectiveBudget(); ok && want < total {
		total = want
	}
	p.deadline = p.opts.Now().Add(total)

	text, ok := p.normalise(mediaType, content)
	if !ok {
		return
	}
	cands, ok := p.rules(text)
	if !ok {
		return
	}
	confirmed, ok := p.validators(cands, text)
	if !ok {
		return
	}
	resolved := map[string]bool{}
	for _, c := range confirmed {
		resolved[c.Class] = true
	}
	preds, ok := p.model(text, resolved)
	if !ok {
		return
	}
	p.labels = merge(confirmed, preds)
	p.fromRules = len(confirmed) > 0
	if p.req.Mode == protocol.ModeM2 && len(confirmed) > 0 {
		p.excerpt = excerpt(confirmed[0], text, p.opts.MaxExcerptChars)
	}
}

// record appends a stage that ran to completion.
func (p *pipeline) record(stage string, took time.Duration) {
	p.stages = append(p.stages, protocol.StageResult{Stage: stage, Ran: true, Duration: took})
}

// fail appends a stage that did not complete and degrades the response.
func (p *pipeline) fail(stage string, detail protocol.Detail, msg string) {
	p.stages = append(p.stages, protocol.StageResult{Stage: stage, Failed: true, Detail: detail, Err: msg})
	p.degraded = true
}

// exhaust appends a stage that ran out of budget and degrades the response.
func (p *pipeline) exhaust(stage string, share, took time.Duration) {
	p.stages = append(p.stages, protocol.StageResult{
		Stage: stage, Ran: true, Truncated: true, Duration: took, Detail: protocol.DetailBudgetExhausted,
		Err: stage + " stage stopped at its " + share.String() + " budget",
	})
	p.degraded = true
}

// stageDeadline is the stage's share from now, or the request's deadline if that comes first.
func (p *pipeline) stageDeadline(share time.Duration) time.Time {
	if d := p.opts.Now().Add(share); d.Before(p.deadline) {
		return d
	}
	return p.deadline
}

func (p *pipeline) parse(ctx context.Context, mediaType string, doc []byte) (string, bool) {
	if p.opts.Parser == nil {
		p.fail(StageParse, protocol.DetailParserFailed, "no document parser is configured")
		return "", false
	}
	res := p.opts.Parser.Parse(ctx, mediaType, doc)
	if res.Cause != isolation.CauseOK {
		p.stages = append(p.stages, protocol.StageResult{
			Stage: StageParse, Ran: true, Failed: true, Duration: res.Duration, Detail: res.Cause.Detail(), Err: res.Err,
		})
		p.degraded = true
		return "", false
	}
	p.record(StageParse, res.Duration)
	return res.Text, true
}

func (p *pipeline) normalise(mediaType string, content []byte) (string, bool) {
	start := p.opts.Now()
	res, err := norm.Normalise(mediaType, content, p.opts.Limits)
	took := p.opts.Now().Sub(start)
	switch {
	case err == norm.ErrOverCap:
		p.fail(StageNormalise, protocol.DetailContentOverCap, err.Error())
		return "", false
	case err != nil:
		p.fail(StageNormalise, protocol.DetailUndecodableContent, err.Error())
		return "", false
	case res.TruncationAffectsRules:
		msg := "normalisation dropped content a rule could have matched"
		if res.Pathological != "" {
			msg += " (" + res.Pathological + ")"
		}
		p.stages = append(p.stages, protocol.StageResult{
			Stage: StageNormalise, Ran: true, Truncated: true, Duration: took, Detail: protocol.DetailNormaliseTruncated, Err: msg,
		})
		p.degraded = true
		return "", false
	}
	p.record(StageNormalise, took)
	return res.Text, true
}

func (p *pipeline) rules(text string) ([]rules.Candidate, bool) {
	start := p.opts.Now()
	out := p.opts.Release.Rules.Evaluate(text, p.stageDeadline(p.opts.Budget.Rules), p.opts.Now)
	took := p.opts.Now().Sub(start)
	if out.Truncated {
		p.exhaust(StageRules, p.opts.Budget.Rules, took)
		return nil, false
	}
	p.record(StageRules, took)
	return out.Candidates, true
}

// validators keeps the candidates whose every validator accepts the matched text.
func (p *pipeline) validators(cands []rules.Candidate, text string) ([]rules.Candidate, bool) {
	start := p.opts.Now()
	deadline := p.stageDeadline(p.opts.Budget.Validators)
	var confirmed []rules.Candidate
	for _, c := range cands {
		if !p.opts.Now().Before(deadline) {
			p.exhaust(StageValidators, p.opts.Budget.Validators, p.opts.Now().Sub(start))
			return nil, false
		}
		if accepted(c, text) {
			confirmed = append(confirmed, c)
		}
	}
	p.record(StageValidators, p.opts.Now().Sub(start))
	return confirmed, true
}

func accepted(c rules.Candidate, text string) bool {
	for _, name := range c.Validators {
		check, ok := validators.Lookup(name)
		if !ok || !check(c.MatchText(text)) {
			return false
		}
	}
	return true
}

// model scores the classes the rules did not resolve.
func (p *pipeline) model(text string, resolved map[string]bool) ([]model.Prediction, bool) {
	start := p.opts.Now()
	deadline := p.stageDeadline(p.opts.Budget.Model)
	expired := func() bool { return !p.opts.Now().Before(deadline) }
	preds := p.opts.Release.Model.Score(text, resolved, expired)
	took := p.opts.Now().Sub(start)
	if expired() {
		p.exhaust(StageModel, p.opts.Budget.Model, took)
		return nil, false
	}
	p.record(StageModel, took)
	return preds, true
}

// merge produces one label per class, keeping the highest score, ordered by score then class.
func merge(confirmed []rules.Candidate, preds []model.Prediction) []protocol.Label {
	best := map[string]protocol.Label{}
	keep := func(l protocol.Label) {
		if prev, ok := best[l.Class]; !ok || l.Score > prev.Score {
			best[l.Class] = l
		}
	}
	for _, c := range confirmed {
		keep(protocol.Label{Class: c.Class, Score: c.Score, RuleID: c.RuleID})
	}
	for _, pr := range preds {
		keep(protocol.Label{Class: pr.Class, Score: pr.Score})
	}
	out := make([]protocol.Label, 0, len(best))
	for _, l := range best {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// excerpt is the M2 excerpt: the first confirmed match, cut to maxChars characters.
func excerpt(c rules.Candidate, text string, maxChars int) *protocol.Excerpt {
	span := []rune(c.MatchText(text))
	if len(span) == 0 {
		return nil
	}
	if len(span) > maxChars {
		span = span[:maxChars]
	}
	s := string(span)
	return &protocol.Excerpt{
		Kind: protocol.ExcerptMatchSpan, Text: s, MatchType: c.Class,
		OffsetStart: c.Start, OffsetEnd: c.Start + len(s),
	}
}

// response assembles the answer. A rules-confirmed label set is high confidence; a model-only
// one is medium at a score of 0.85 or more and low below; a completed run that found nothing is
// high.
func (p *pipeline) response(version string) protocol.ClassifyResponse {
	resp := protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: version,
		Confidence:        protocol.ConfidenceHigh,
		Stages:            p.stages,
	}
	if p.degraded {
		resp.Confidence = protocol.ConfidenceDegraded
		return resp
	}
	if len(p.labels) > 0 {
		resp.Labels = p.labels
		if !p.fromRules {
			resp.Confidence = protocol.ConfidenceLow
			if p.labels[0].Score >= 0.85 {
				resp.Confidence = protocol.ConfidenceMedium
			}
		}
	}
	resp.Excerpt = p.excerpt
	return resp
}
