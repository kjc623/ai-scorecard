package classify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// Verdict is the host's whole answer: the protocol response plus the enforcement half of §9.6.
//
// The classification half is protocol.ClassifyResponse verbatim, because that is the shape the
// contract and capture-core agree on. The enforcement half has no home in device/protocol —
// ClassifyResponse carries no Decision — so it is carried alongside and reported as an open
// decision; the response half still validates on its own.
type Verdict struct {
	Response protocol.ClassifyResponse `json:"response"`

	// Release is the classifier version whose rules produced this verdict.
	Release string `json:"release,omitempty"`
	// ReleaseState is the state §9.6 says enforcement must apply.
	ReleaseState release.State `json:"release_state,omitempty"`

	// Action is what the enforcing decision would do: blocked | warned | logged. In `shadow`
	// and `rolled_back` it is always `logged`.
	Action         string `json:"action"`
	RuleID         string `json:"rule_id,omitempty"`
	ActionClass    string `json:"action_class,omitempty"`
	DecidedLocally bool   `json:"decided_locally"`

	// Shadowed is §9.6's shadow marker: the decision was computed and recorded, not enforced.
	Shadowed bool `json:"shadowed"`
	// EnforcementSuppressed names why enforcement did not act, when it did not.
	EnforcementSuppressed bool   `json:"enforcement_suppressed,omitempty"`
	SuppressionCause      string `json:"suppression_cause,omitempty"`

	// PartialLabels is §9.4's "emit labels found" when a stage exhausted its budget. The
	// protocol response carries no labels when degraded (protocol.ClassifyResponse.Validate),
	// so the rules-only labels §9.4 asks to emit are kept here for the audit path and counted.
	PartialLabels []protocol.Label `json:"partial_labels,omitempty"`

	// Digest is the sha256 of the *normalised* text, the digest §9.2 says is the one in
	// content_digest. It is returned so the caller's own digest can be checked against it.
	Digest string `json:"content_digest,omitempty"`

	// Deferred marks a document whose parse was moved off the interactive path (§10, A13): the
	// synchronous answer is degraded and the labels arrive later as a correction.
	Deferred bool `json:"parse_deferred,omitempty"`

	// Shadow is the co-existing shadow evaluation of §9.6 ("a device can enforce release N
	// while shadow-evaluating N+1 on the same traffic"). It never enforces.
	Shadow *ShadowVerdict `json:"shadow,omitempty"`
}

// ShadowVerdict is an N+1 evaluation running alongside the enforcing release.
type ShadowVerdict struct {
	Release    string              `json:"release"`
	State      release.State       `json:"state"`
	Labels     []protocol.Label    `json:"labels"`
	Confidence protocol.Confidence `json:"confidence"`
	Degraded   bool                `json:"degraded"`
	Action     string              `json:"action"` // always logged
	Evidence   string              `json:"evidence,omitempty"`
}

// Options configures a host.
type Options struct {
	// Store holds the signed releases. A host with no active release degrades every request
	// with `release_load_failed` rather than reporting "no labels found".
	Store *release.Store

	// Parser is the §10 parser child. Nil means this target cannot parse documents (the wasm
	// copy, §9.1); a document then degrades with `parser_failed`.
	Parser docparse.Parser

	// Budget is §9.4's ladder for this target. Zero means NativeBudget().
	Budget Budget

	// Limits bounds normalisation (§9.2).
	Limits norm.Limits

	// Enforcement is the enforcing policy (§8.2). Zero means DefaultEnforcement().
	Enforcement EnforcementPolicy

	// Metrics records the rolling per-stage latency of §9.4. Nil disables recording.
	Metrics *Metrics

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Version is the host binary's own version. It is the classifier_version of last resort
	// when no release is loaded, so a response is always attributable to something.
	Version string

	// InlineParseBytes is §10's inline threshold (A13): a larger document is parsed off the
	// interactive path and the synchronous answer is deferred. Zero means 64 KiB.
	InlineParseBytes int

	// MaxExcerptChars bounds a minimised excerpt (§9.2, contract maxLength). Zero means
	// protocol.MaxExcerptChars.
	MaxExcerptChars int

	// ModelClasses, when non-nil, limits the model to these classes. It exists so a release can
	// carry a broader artefact than a device class needs; nil means every class.
	ModelClasses []string
}

// Host is the classifier host. It is safe for concurrent use: a Store's releases are immutable
// after load, Metrics is locked, and every request builds its own pipeline.
type Host struct {
	opts Options
}

// New builds a host. It fails only for a structurally unusable configuration (no store).
func New(opts Options) (*Host, error) {
	if opts.Store == nil {
		return nil, errNoStore
	}
	if opts.Budget.Total <= 0 {
		opts.Budget = NativeBudget()
	}
	if opts.Limits.MaxInputBytes <= 0 {
		opts.Limits = norm.DefaultLimits()
	}
	if len(opts.Enforcement.Block) == 0 && len(opts.Enforcement.Warn) == 0 {
		opts.Enforcement = DefaultEnforcement()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version == "" {
		opts.Version = "classifier-host-unknown"
	}
	if opts.InlineParseBytes <= 0 {
		opts.InlineParseBytes = 64 << 10
	}
	if opts.MaxExcerptChars <= 0 {
		opts.MaxExcerptChars = protocol.MaxExcerptChars
	}
	return &Host{opts: opts}, nil
}

var errNoStore = errors.New("classify: a host needs a release store")

// Version is the host binary version.
func (h *Host) Version() string { return h.opts.Version }

// Metrics exposes the rolling latency recorder for the health channel.
func (h *Host) Metrics() *Metrics { return h.opts.Metrics }

// Classify produces the verdict for one observation.
//
// It never returns an error: a component on the interactive path may not fail a submission
// (§9.4, C21). Every defect becomes a degraded, attributable verdict — mode violation, missing
// release, a stage over budget, a parser that died — and the response half always satisfies
// protocol.ClassifyResponse.Validate.
func (h *Host) Classify(ctx context.Context, req protocol.ClassifyRequest) Verdict {
	rel := h.opts.Store.Active()
	v := h.classifyWithRelease(ctx, rel, req, true)

	state := h.opts.Store.EffectiveState()
	if rel == nil {
		// No release at all: the state is whatever the store last recorded, but nothing can be
		// enforced from a verdict that does not exist.
		state = release.StateShadow
	}
	v.ReleaseState = state

	// §9.6's coexistence: evaluate release N+1 in shadow while release N is active.
	if shadow := h.opts.Store.Shadow(); shadow != nil && shadow != rel {
		v.Shadow = h.shadowEvaluate(ctx, shadow, req)
	}

	h.applyEnforcement(&v, state)
	return v
}

// ClassifyResponse is the protocol half of Classify, for callers that only need the labels.
func (h *Host) ClassifyResponse(ctx context.Context, req protocol.ClassifyRequest) protocol.ClassifyResponse {
	return h.Classify(ctx, req).Response
}

// classifyWithRelease runs the pipeline for one release.
func (h *Host) classifyWithRelease(ctx context.Context, rel *release.Release, req protocol.ClassifyRequest, observe bool) Verdict {
	m := h.opts.Metrics
	if !observe {
		m = nil
	}
	p := newPipeline(rel, req, h.opts.Budget, h.opts.Limits, h.opts.Now, m)
	v := Verdict{DecidedLocally: true}

	// Stage 0: the mode gate (§11.2). The classifier enforces the mode rather than trusting the
	// caller, so a defect upstream is refused here instead of becoming a stored fact.
	if err := req.Validate(); err != nil {
		p.record(StageMode, true, true, false, protocol.DetailModeViolation, err.Error(), 0)
		p.degrade(protocol.DetailModeViolation, err.Error())
		p.inc("mode_violation")
		return h.assemble(p, v)
	}

	// Stage 0b: the release gate. A caller never selects a release (protocol.ClassifyRequest),
	// and a store that failed its last load is degraded until it succeeds: §9.7's "a release
	// failed to load and rules-only labels came from the retained release".
	switch {
	case rel == nil:
		errText := "no classifier release is loaded"
		if f := h.opts.Store.Failure(); f != nil {
			errText += "; last load failed: " + f.Err
		}
		p.record(StageRelease, false, true, false, protocol.DetailReleaseLoadFailed, errText, 0)
		p.degrade(protocol.DetailReleaseLoadFailed, errText)
		p.inc("release_unavailable")
		return h.assemble(p, v)
	case req.ReleaseID != "" && req.ReleaseID != rel.Version:
		errText := "the caller selected release " + req.ReleaseID + "; policy selects releases, callers do not"
		p.record(StageRelease, true, true, false, protocol.DetailReleaseLoadFailed, errText, 0)
		p.degrade(protocol.DetailReleaseLoadFailed, errText)
		p.inc("release_caller_selected")
		return h.assemble(p, v)
	}
	if f := h.opts.Store.Failure(); f != nil {
		// The retained release still classifies; the answer is degraded because the release the
		// device was told to run is not the release that produced it.
		p.record(StageRelease, true, true, false, protocol.DetailReleaseLoadFailed,
			"release "+f.Version+" failed to load ("+f.Cause+"); labels came from the retained release "+rel.Version, 0)
		p.degrade(protocol.DetailReleaseLoadFailed, "a release failed to load: "+f.Err)
		p.inc("release_load_failed")
		return h.assemble(p, v)
	}

	content := req.Content
	mediaType := req.MediaType
	text := ""
	parsed := false

	// Stage 0c: documents take the parser-child path (§10). The wasm copy cannot parse at all
	// (§9.1), and a document over the inline threshold is deferred off the interactive path
	// (A13) rather than blocking the user.
	if isDocumentMediaType(mediaType) {
		res := h.parseDocument(ctx, p, mediaType, content)
		if res == nil {
			return h.assemble(p, v)
		}
		parsed = true
		content = []byte(res.Text)
		mediaType = "text/plain"
	}

	if !parsed {
		// Stage 0d: an empty body at a mode that permits reading is content the classifier
		// could not process (an over-cap body arrives hashed and sized, not with bytes), which
		// §9.7 lists as a degraded cause, never as "no labels found".
		if len(content) == 0 {
			p.record(StageNormalise, false, true, false, protocol.DetailContentUnprocessable,
				"the request carries no content to classify at mode "+string(req.Mode), 0)
			p.degrade(protocol.DetailContentUnprocessable, "no content was handed over at a mode that reads content")
			p.inc("content_unprocessable")
			return h.assemble(p, v)
		}
	}
	if mediaType == "" && parsed {
		mediaType = "text/plain"
	}
	p.runNormalise(mediaType, content)
	if p.digest == "" {
		return h.assemble(p, v) // normalise refused the body; no text to work with
	}
	text = p.normalisedText

	cands := p.runRules(text)
	ruleLabels := p.runValidators(cands, text)

	resolved := map[string]bool{}
	for _, l := range ruleLabels {
		resolved[l.Class] = true
	}
	preds := p.runModel(text, resolved)

	labels, suppressed := mergeLabels(ruleLabels, preds)
	p.count["labels_suppressed"] += uint64(suppressed)
	p.partialLabels = labels
	if len(labels) == 0 {
		p.inc("empty_result")
	}
	hadRuleEvidence := len(ruleLabels) > 0
	if !p.degraded {
		p.labels = labels
		p.confidence = band(labels, hadRuleEvidence)
		if len(cands) > 0 {
			p.excerpt = buildExcerpt(req.Mode, cands, text, h.opts.MaxExcerptChars)
		}
	}
	_ = ctx
	return h.assemble(p, v)
}

// parseDocument runs the §10 path and records the parse stage. It returns nil when the pipeline
// must stop (no text to classify).
func (h *Host) parseDocument(ctx context.Context, p *pipeline, mediaType string, doc []byte) *docparse.Result {
	parser := h.opts.Parser
	if parser == nil || !parser.Available() {
		errText := "document parsing is unavailable in this target (§9.1); the document cannot be classified inline"
		p.record(StageParse, false, true, false, protocol.DetailParserFailed, errText, 0)
		p.degrade(protocol.DetailParserFailed, errText)
		p.inc("parser_unavailable")
		return nil
	}
	if len(doc) > h.opts.InlineParseBytes {
		// A13: the attachment's labels are produced asynchronously and arrive as a correction.
		// The synchronous answer must not pretend the document was inspected.
		errText := "document of " + strconv.Itoa(len(doc)) + " bytes is over the inline threshold; parsed off the interactive path"
		p.record(StageParse, false, true, false, protocol.DetailParserFailed, errText, 0)
		p.degrade(protocol.DetailParserFailed, errText)
		p.deferred = true
		p.inc("parse_deferred")
		return nil
	}
	if !parser.AllowFormat(mediaType) {
		errText := "the per-format circuit breaker has disabled parsing for " + mediaType
		p.record(StageParse, false, true, false, protocol.DetailParserFailed, errText, 0)
		p.degrade(protocol.DetailParserFailed, errText)
		p.inc("parser_format_circuit_open")
		return nil
	}
	res := parser.Parse(ctx, mediaType, doc)
	parser.RecordFormat(mediaType, res.Cause == docparse.CauseOK)
	stage := protocol.StageResult{
		Stage:     StageParse,
		Ran:       true,
		Failed:    res.Cause != docparse.CauseOK && res.Cause != docparse.CauseNone,
		Detail:    res.Detail,
		Err:       res.Err,
		Duration:  res.Duration,
		Truncated: res.Truncated,
	}
	if stage.Detail == protocol.DetailNone && stage.Failed {
		stage.Detail = protocol.DetailParserFailed
	}
	p.stages = append(p.stages, stage)
	if h.opts.Metrics != nil {
		h.opts.Metrics.Observe(StageParse, res.Duration)
	}
	p.count["parser_"+string(res.Cause)]++
	p.peakParserBytes = res.PeakBytes
	if stage.Failed {
		p.degrade(stage.Detail, res.Err)
		return nil
	}
	if res.Truncated {
		// §10: "An output cap bounds the result size accepted, truncating and recording
		// degraded, so a 500-page document does not become a 500-page label input."
		p.degrade(protocol.DetailParserOutputCap, "the parser's output cap truncated the extracted text")
		return nil
	}
	p.parsedOK = true
	out := res
	return &out
}

// shadowEvaluate runs the N+1 release alongside the active one. It has its own pipeline and
// cannot enforce: §9.6's shadow state records, it does not act.
func (h *Host) shadowEvaluate(ctx context.Context, shadow *release.Release, req protocol.ClassifyRequest) *ShadowVerdict {
	if h.opts.Store.EffectiveState() == release.StateRolledBack {
		// A rolled-back device is not evaluating anything new.
		return nil
	}
	v := h.classifyWithRelease(ctx, shadow, req, false)
	sv := &ShadowVerdict{
		Release:    shadow.Version,
		State:      release.StateShadow,
		Labels:     v.Response.Labels,
		Confidence: v.Response.Confidence,
		Degraded:   v.Response.Confidence == protocol.ConfidenceDegraded,
		Action:     protocol.ActionLogged,
	}
	if sv.Labels == nil {
		sv.Labels = []protocol.Label{}
	}
	return sv
}

// applyEnforcement turns labels into an action, or refuses to.
//
// The three refusals are the point of §9.6 and §9.4:
//   - not `enforcing` (shadow, rolled_back): the decision is recorded as `logged`;
//   - degraded: a classification that did not complete may not block anything;
//   - deferred: nothing was inspected synchronously, so nothing may be enforced.
func (h *Host) applyEnforcement(v *Verdict, state release.State) {
	v.Shadowed = state == release.StateShadow
	v.Action = protocol.ActionLogged
	if v.Response.Confidence == protocol.ConfidenceDegraded {
		v.EnforcementSuppressed = true
		v.SuppressionCause = "degraded"
		return
	}
	if v.Deferred {
		v.EnforcementSuppressed = true
		v.SuppressionCause = "deferred"
		return
	}
	if !state.Enforces() {
		v.EnforcementSuppressed = true
		v.SuppressionCause = string(state)
		if h.opts.Metrics != nil {
			h.opts.Metrics.Observe("enforcement_suppressed."+string(state), 0)
		}
		return
	}
	d := h.opts.Enforcement.Decide(v.Response.Labels)
	v.Action = d.Action
	v.RuleID = d.RuleID
	v.ActionClass = d.Class
}

// assemble finishes a pipeline into a verdict, enforcing the response invariants:
// a degraded response carries no labels and names the stage that did not complete.
func (h *Host) assemble(p *pipeline, v Verdict) Verdict {
	resp := protocol.ClassifyResponse{
		ClassifierVersion: h.classifierVersion(p.rel),
		Stages:            p.stages,
		Counters:          p.count,
	}
	if p.degraded {
		resp.Confidence = protocol.ConfidenceDegraded
		resp.Labels = []protocol.Label{}
		resp.VerdictShadowed = false
		v.PartialLabels = p.partialLabels
	} else {
		resp.Confidence = p.confidence
		if resp.Confidence == "" {
			resp.Confidence = protocol.ConfidenceHigh
		}
		resp.Labels = p.labels
		if resp.Labels == nil {
			resp.Labels = []protocol.Label{}
		}
		resp.Excerpt = p.excerpt
	}
	v.Response = resp
	if p.rel != nil {
		v.Release = p.rel.Version
	}
	v.Digest = p.digest
	v.Deferred = p.deferred
	return v
}

func (h *Host) classifierVersion(rel *release.Release) string {
	if rel != nil && rel.Version != "" {
		return rel.Version
	}
	return h.opts.Version
}

// Handshake answers capture-core's connect-time version handshake (§3.4).
//
// A mismatch is a degraded handshake response, never a crash: the core falls back to rules-only
// with `confidence: degraded` and never fails the submission (C21). It is also §9.6's "version
// handshake failure with the running host", one of the release-retention causes.
func (h *Host) Handshake(req protocol.HandshakeRequest) protocol.HandshakeResponse {
	rel := h.opts.Store.Active()
	ver := h.classifierVersion(rel)
	switch {
	case req.ProtocolVersion != protocol.Version:
		return protocol.HandshakeResponse{
			OK: false, ClassifierVersion: ver, Reason: protocol.DetailVersionMismatch,
		}
	case req.CoreVersion == "":
		return protocol.HandshakeResponse{
			OK: false, ClassifierVersion: ver, Reason: protocol.DetailVersionMismatch,
		}
	case req.ClassifierVersion != "" && rel != nil && req.ClassifierVersion != rel.Version:
		// The core holds a different classifier release than this host: it must not be told
		// this connection serves the release it expects.
		return protocol.HandshakeResponse{
			OK: false, ClassifierVersion: ver, Reason: protocol.DetailVersionMismatch,
		}
	}
	return protocol.HandshakeResponse{OK: true, ClassifierVersion: ver}
}

// Health is what the classifier contributes to the device's health channel: the release it is
// running, its state, its counters and §9.4's rolling per-stage p95.
type Health struct {
	Version     string             `json:"version"`
	Release     string             `json:"release,omitempty"`
	State       release.State      `json:"release_state,omitempty"`
	Shadow      string             `json:"shadow_release,omitempty"`
	Retained    string             `json:"retained_release,omitempty"`
	ModelDigest string             `json:"model_digest,omitempty"`
	Counters    map[string]uint64  `json:"counters,omitempty"`
	StageP95MS  map[string]float64 `json:"stage_p95_ms,omitempty"`
	LastFailure *release.Failure   `json:"last_release_failure,omitempty"`
	Loads       uint64             `json:"release_loads"`
	Rejections  uint64             `json:"release_rejections"`
}

// Health snapshots the host's state for the health channel. It carries no identity: building a
// protocol.HealthReport needs a device id, and this component must not be able to see one.
func (h *Host) Health() Health {
	rel := h.opts.Store.Active()
	out := Health{
		Version:    h.opts.Version,
		State:      h.opts.Store.EffectiveState(),
		Counters:   map[string]uint64{},
		StageP95MS: map[string]float64{},
	}
	if rel != nil {
		out.Release = rel.Version
		out.ModelDigest = rel.ModelDigest
	}
	if s := h.opts.Store.Shadow(); s != nil {
		out.Shadow = s.Version
	}
	if r := h.opts.Store.Retained(); r != nil {
		out.Retained = r.Version
	}
	out.LastFailure = h.opts.Store.Failure()
	out.Loads, out.Rejections = h.opts.Store.Counters()
	for stage, st := range h.opts.Metrics.Snapshot() {
		out.StageP95MS[stage] = float64(st.P95) / float64(time.Millisecond)
	}
	return out
}

// isDocumentMediaType reports whether the bytes are a document the §10 child parses rather than
// text the normalise stage reads. XML and JSON are deliberately *not* here: normalisation
// extracts their text in-process (§9.2), and routing them to a child would put a process spawn
// on the interactive path for a body that needs no parser.
func isDocumentMediaType(mt string) bool {
	m := strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "application/pdf",
		"application/zip",
		"application/x-zip-compressed",
		"application/gzip",
		"application/x-gzip",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/msword",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet":
		return true
	}
	return strings.HasPrefix(m, "application/vnd.openxmlformats-")
}

// RuleIDs lists the active release's rule ids, for the health channel and tests. §9.2 makes
// "why was this flagged" answerable with a rule id.
func (h *Host) RuleIDs() []string {
	rel := h.opts.Store.Active()
	if rel == nil {
		return nil
	}
	ids := rel.Rules.RuleIDs()
	sort.Strings(ids)
	return ids
}
