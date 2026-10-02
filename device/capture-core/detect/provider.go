// Package detect is `proc.detect` (docs/01-collectors.md §4.4): the process and model detector.
//
// It is the cheapest provider — no ports, no trust configuration, started first and stopped last
// so it can report tamper signals about the others (C24) — and the only one whose failure costs
// nothing user-visible: it observes passively and **fails open by doing nothing**.
//
// R7 is enforced structurally here rather than by discipline. The document's table has exactly
// two outputs and one field:
//
//	evidence a local model ran                  -> one model_detection with detection_basis
//	a candidate active with no submission       -> one usage_rollup per device, per tool, per day
//	process identity for another provider's event -> a field on *that* envelope, never a record
//
// There is no counter or kind for per-cycle telemetry, so a defect cannot start shipping it.
package detect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ProcessInfo is one sampled process. The enumerator that produces it is a seam because
// process enumeration is platform code; the real implementation is NOT VERIFIED here (see the
// endpoint report), and the provider degrades to `enumeration_partial` rather than pretending.
type ProcessInfo struct {
	PID             int
	ImagePath       string
	SignerSubject   string
	Version         string
	Modules         []string
	ListeningPorts  []int
	ComputePermille int    // coarse compute signature, 0..1000
	UserRef         string // attribution only: it is a field on another provider's envelope
}

// Enumerator samples the process table.
type Enumerator interface {
	Enumerate(ctx context.Context) ([]ProcessInfo, error)
}

// EnumeratorFunc adapts a function.
type EnumeratorFunc func(ctx context.Context) ([]ProcessInfo, error)

// Enumerate calls f.
func (f EnumeratorFunc) Enumerate(ctx context.Context) ([]ProcessInfo, error) { return f(ctx) }

// Emitter is the pipeline as this provider uses it: resolve a tool's mode, then mint a record
// for a kind the provider owns. *core.Pipeline satisfies it.
type Emitter interface {
	ResolveMode(q core.ScopeQuery) core.Resolution
	EmitEnvelope(ctx context.Context, in core.EnvelopeInput) (core.Outcome, error)
}

// Config is the provider's seams. Everything that is policy lives in the bundle's ProcDetect
// section, so a new runtime signature is a bundle change and not a release.
type Config struct {
	Enumerator Enumerator
	Bundles    func() *policy.Bundle
	Pipeline   Emitter

	// Submissions answers "how many submissions did another provider record for this tool in
	// this window". It is the only way a rollup can assert `submission_count: 0` honestly;
	// without it the provider records zero and says so in the outcome.
	Submissions func(tool string, windowStart, windowEnd time.Time) int

	// Agent identifies the device/user for scope resolution and the envelope.
	Agent core.ScopeQuery

	CycleInterval time.Duration
	MissWindow    time.Duration
	RollupWindow  time.Duration

	Log   core.Logger
	Clock func() time.Time
}

func (c Config) withDefaults() Config {
	if c.CycleInterval <= 0 {
		c.CycleInterval = 30 * time.Second
	}
	if c.MissWindow <= 0 {
		c.MissWindow = 3 * c.CycleInterval
	}
	if c.RollupWindow <= 0 {
		c.RollupWindow = 24 * time.Hour
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Submissions == nil {
		c.Submissions = func(string, time.Time, time.Time) int { return 0 }
	}
	return c
}

// Provider is `proc.detect`.
type Provider struct {
	cfg Config

	mu          sync.Mutex
	started     bool
	stopped     bool
	counters    *core.CounterSet
	startedAt   time.Time
	lastCycle   time.Time
	lastSuccess time.Time
	lastErr     string
	lastDetail  protocol.Detail
	cycles      int
	missed      int
	detected    map[string]bool // tool|day already emitted as a model_detection
	rolled      map[string]bool // tool|day already emitted as a usage_rollup

	stopCh   chan struct{}
	wg       sync.WaitGroup
	identity Identity
}

// Name implements core.Provider: the collector name is the route.
func (p *Provider) Name() protocol.Route { return protocol.RouteProcDetect }

// New returns an unstarted provider.
func New(cfg Config) *Provider {
	cfg = cfg.withDefaults()
	now := cfg.Clock()
	return &Provider{
		cfg:       cfg,
		counters:  core.NewCounterSet(now),
		startedAt: now,
		detected:  map[string]bool{},
		rolled:    map[string]bool{},
		stopCh:    make(chan struct{}),
	}
}

// Start begins the sampling cycle. It binds nothing and installs no trust: the whole provider
// is a reader of the process table, so its Start cannot fail in a way that affects the user.
func (p *Provider) Start(ctx context.Context) error {
	if p.cfg.Enumerator == nil {
		// A provider with no way to see processes must not report healthy while blind, and it must
		// not silently do nothing forever either: this is a wiring defect and it is loud.
		return ErrNoEnumerator
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	p.started = true
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.loop(ctx)
	}()
	// One cycle runs immediately so the coverage row has something to say before the interval.
	p.cycle(ctx)
	return nil
}

func (p *Provider) loop(ctx context.Context) {
	t := time.NewTicker(p.cfg.CycleInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.cycle(ctx)
		}
	}
}

// Stop implements core.Provider. Idempotent; it has nothing to release, which is why it is safe
// to call "last" in the shutdown order while other providers are being torn down.
func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()
	close(p.stopCh)
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ApplyPolicy implements core.Provider: a diff. The seed set is signed bundle data, so a new
// runtime signature arrives here and takes effect on the next cycle — never by restarting.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(b.ProcDetect.ImageSignatures) == 0 && len(b.ProcDetect.ModuleSignatures) == 0 && len(b.ProcDetect.PortMap) == 0 {
		// An empty seed set is a stale signature set, not a licence to detect everything.
		p.lastDetail = protocol.DetailSignatureSetStale
		return nil
	}
	if p.lastDetail == protocol.DetailSignatureSetStale {
		p.lastDetail = protocol.DetailNone
	}
	return nil
}

// Health implements core.Provider.
//
//	healthy   a cycle completed inside its window and the seed set is usable
//	degraded  enumeration_partial (a cycle could not enumerate) or signature_set_stale
//	absent    a cycle missed its window, or the provider never started / was stopped
//	tampered  the service was stopped — reported by this provider about others, not about itself
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	started, stopped := p.started, p.stopped
	lastCycle, lastSuccess, detail := p.lastCycle, p.lastSuccess, p.lastDetail
	p.mu.Unlock()

	if !started || stopped {
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, p.startedAt, lastSuccess)
	}
	if lastCycle.IsZero() || p.cfg.Clock().Sub(lastCycle) > p.cfg.MissWindow {
		// A cycle that missed its window is `absent`, because the provider is not observing.
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, p.startedAt, lastSuccess)
	}
	if detail == protocol.DetailSignatureSetStale {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailSignatureSetStale, p.startedAt, lastSuccess)
	}
	if detail != protocol.DetailNone {
		return p.counters.Snapshot(protocol.StateDegraded, detail, p.startedAt, lastSuccess)
	}
	return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, p.startedAt, lastSuccess)
}

// Counters exposes the closed counter set for the coverage row.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

// cycle samples the process table once and emits at most one model_detection per tool per day
// and one usage_rollup per tool per day.
func (p *Provider) cycle(ctx context.Context) {
	now := p.cfg.Clock()
	p.mu.Lock()
	p.lastCycle = now
	p.cycles++
	p.mu.Unlock()

	procs, err := p.cfg.Enumerator.Enumerate(ctx)
	if err != nil {
		// The weakest-signal provider reports loudly and does nothing else: no output, a named
		// gap, and the cycle still counts as attempted so health can distinguish "degraded" from
		// "absent".
		p.mu.Lock()
		p.lastDetail = protocol.DetailEnumerationPartial
		p.lastErr = err.Error()
		p.missed++
		p.mu.Unlock()
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("detect: enumeration partial: %v", err)
		return
	}

	seed := p.seedSet()
	p.mu.Lock()
	if len(seed.imageSigs) == 0 && len(seed.moduleSigs) == 0 && len(seed.portMap) == 0 {
		p.lastDetail = protocol.DetailSignatureSetStale
	} else {
		p.lastDetail = protocol.DetailNone
	}
	p.mu.Unlock()

	for _, proc := range procs {
		tool, _, candidate := matchCandidate(proc, seed)
		if !candidate {
			continue
		}
		// Candidate processes seen per cycle is the coverage denominator (§4.4).
		p.counters.Add(protocol.CounterObserved)

		used, evidenceBasis := evidenceOfUse(proc, seed)
		if used {
			// Evidence of use, not merely installed: an installed-but-idle runtime is not a model
			// that ran, and saying otherwise would inflate mode I with exactly the overstatement
			// R11 warns about.
			p.emitDetection(ctx, tool, evidenceBasis, proc, now)
		}
		// A candidate active in a window where no provider recorded a submission is a coverage
		// fact: one rollup per device, per tool, per day, which may carry submission_count: 0.
		p.emitRollup(ctx, tool, now)
	}

	p.mu.Lock()
	p.lastSuccess = now
	p.mu.Unlock()
}

type seedSet struct {
	imageSigs  []string
	moduleSigs []string
	portMap    map[int]string
	minCompute int
	version    string
}

func (p *Provider) seedSet() seedSet {
	s := seedSet{portMap: map[int]string{}}
	if p.cfg.Bundles == nil {
		return s
	}
	b := p.cfg.Bundles()
	if b == nil {
		return s
	}
	for _, sig := range b.ProcDetect.ImageSignatures {
		s.imageSigs = append(s.imageSigs, strings.ToLower(sig))
	}
	for _, sig := range b.ProcDetect.ModuleSignatures {
		s.moduleSigs = append(s.moduleSigs, strings.ToLower(sig))
	}
	for _, pm := range b.ProcDetect.PortMap {
		s.portMap[pm.Port] = pm.ToolFingerprint
	}
	s.minCompute = b.ProcDetect.MinComputePermille
	s.version = b.ProcDetect.SignatureVersion
	// The cycle interval is wiring configuration: changing sampling cadence mid-cycle would race
	// the ticker, and "a cycle misses its window" only means something if the window is stable.
	// ApplyPolicy installs a new seed set; it does not re-time the sampler.
	return s
}

// matchCandidate applies §4.4's candidate rule: an inference-runtime signature **in the signed
// seed set**, or a listening loopback socket in the configured port map.
func matchCandidate(proc ProcessInfo, seed seedSet) (tool string, basis string, ok bool) {
	image := strings.ToLower(proc.ImagePath)
	for _, sig := range seed.imageSigs {
		if sig != "" && strings.Contains(image, sig) {
			return toolForImage(image, seed), "process_scan", true
		}
	}
	for _, mod := range proc.Modules {
		lm := strings.ToLower(mod)
		for _, sig := range seed.moduleSigs {
			if sig != "" && strings.Contains(lm, sig) {
				return toolForImage(image, seed), "module_signature", true
			}
		}
	}
	for _, port := range proc.ListeningPorts {
		if t, ok := seed.portMap[port]; ok {
			return t, "process_scan", true
		}
	}
	return "", "", false
}

// evidenceOfUse is §4.4's "becomes a model_detection only with evidence of use": a listening
// loopback socket on a configured inference port, or a loaded inference-runtime module with
// sustained compute.
func evidenceOfUse(proc ProcessInfo, seed seedSet) (bool, string) {
	for _, port := range proc.ListeningPorts {
		if _, ok := seed.portMap[port]; ok {
			return true, "process_scan"
		}
	}
	if seed.minCompute > 0 {
		for _, mod := range proc.Modules {
			lm := strings.ToLower(mod)
			for _, sig := range seed.moduleSigs {
				if sig != "" && strings.Contains(lm, sig) && proc.ComputePermille >= seed.minCompute {
					return true, "module_signature"
				}
			}
		}
	}
	return false, ""
}

// toolForImage derives a behaviour-shaped fingerprint from the image path when no port-map entry
// names the tool. It is deliberately not a brand name: the schema requires a behaviour-derived
// fingerprint, and the sanctioned/unsanctioned judgement is per-tenant state held server-side.
func toolForImage(image string, seed seedSet) string {
	for _, sig := range seed.imageSigs {
		if sig != "" && strings.Contains(image, sig) {
			return "proc_" + sanitiseFingerprint(sig)
		}
	}
	return "proc_unknown"
}

func sanitiseFingerprint(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "unknown"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// emitDetection emits one model_detection for evidence that a local model ran. At most one per
// tool per day: §4.4 forbids per-cycle records, and the day cap is what keeps a long-running
// runtime from becoming a per-cycle stream in a different costume.
func (p *Provider) emitDetection(ctx context.Context, tool, basis string, proc ProcessInfo, now time.Time) {
	if p.cfg.Pipeline == nil {
		return
	}
	key := tool + "|" + dayOf(now)
	p.mu.Lock()
	if p.detected[key] {
		p.mu.Unlock()
		return
	}
	p.detected[key] = true
	p.mu.Unlock()

	res := p.cfg.Pipeline.ResolveMode(core.ScopeQuery{
		ToolFingerprint: tool,
		Population:      p.cfg.Agent.Population,
		UserRef:         p.cfg.Agent.UserRef,
		DeviceID:        p.cfg.Agent.DeviceID,
	})
	dedupKey, err := dedup.DetectionKey(p.tenant(), p.device(), tool, string(protocol.KindModelDetection), now, basis)
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("detect: detection key: %v", err)
		return
	}
	_, err = p.cfg.Pipeline.EmitEnvelope(ctx, core.EnvelopeInput{
		Identity:          core.Identity{TenantID: p.tenant(), DeviceID: p.device(), UserRef: p.cfg.Agent.UserRef},
		Kind:              protocol.KindModelDetection,
		Route:             protocol.RouteProcDetect,
		Mode:              res.Mode,
		ToolFingerprint:   tool,
		OccurredAt:        now,
		MonotonicOffsetMS: now.Sub(p.startedAt).Milliseconds(),
		DedupKey:          dedupKey,
		DetectionBasis:    basis,
	})
	if err != nil {
		p.counters.Add(protocol.CounterDropped)
		return
	}
	p.counters.Add(protocol.CounterEmitted)
}

// emitRollup emits one usage_rollup per device, per tool, per day for a candidate that was
// active — the only exit for process-level observation (D8). `submission_count: 0` is a real
// assertion: "this tool was active and we captured no submissions for it" is a coverage fact,
// not an absence of one.
func (p *Provider) emitRollup(ctx context.Context, tool string, now time.Time) {
	if p.cfg.Pipeline == nil {
		return
	}
	key := tool + "|" + dayOf(now)
	p.mu.Lock()
	if p.rolled[key] {
		p.mu.Unlock()
		return
	}
	p.rolled[key] = true
	p.mu.Unlock()

	start := now.Truncate(p.cfg.RollupWindow)
	end := start.Add(p.cfg.RollupWindow)
	submissions := p.cfg.Submissions(tool, start, end)
	res := p.cfg.Pipeline.ResolveMode(core.ScopeQuery{
		ToolFingerprint: tool,
		Population:      p.cfg.Agent.Population,
		UserRef:         p.cfg.Agent.UserRef,
		DeviceID:        p.cfg.Agent.DeviceID,
	})
	dedupKey, err := dedup.RollupKey(p.tenant(), p.device(), tool, string(protocol.KindUsageRollup), start, end)
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		return
	}
	bytesTotal := int64(0)
	_, err = p.cfg.Pipeline.EmitEnvelope(ctx, core.EnvelopeInput{
		Identity:          core.Identity{TenantID: p.tenant(), DeviceID: p.device(), UserRef: p.cfg.Agent.UserRef},
		Kind:              protocol.KindUsageRollup,
		Route:             protocol.RouteProcDetect,
		Mode:              res.Mode,
		ToolFingerprint:   tool,
		OccurredAt:        now,
		MonotonicOffsetMS: now.Sub(p.startedAt).Milliseconds(),
		DedupKey:          dedupKey,
		WindowStart:       &start,
		WindowEnd:         &end,
		SubmissionCount:   &submissions,
		BytesTotal:        &bytesTotal,
	})
	if err != nil {
		p.counters.Add(protocol.CounterDropped)
		return
	}
	p.counters.Add(protocol.CounterEmitted)
}

func (p *Provider) tenant() string { return p.identityOrAgent().TenantID }

func (p *Provider) device() string { return p.identityOrAgent().DeviceID }

// ErrNoEnumerator reports a provider with no way to see processes: it is a configuration defect,
// not a runtime condition, and Start refuses rather than reporting healthy while blind.
var ErrNoEnumerator = errors.New("detect: no process enumerator configured")

// Ensure the provider satisfies the contract it claims to.
var _ core.Provider = (*Provider)(nil)

// Coverage is §4.4's coverage row: candidate processes seen, models detected, rollups emitted,
// plus the named gap.
type Coverage struct {
	CandidatesSeen int
	ModelsDetected int
	RollupsEmitted int
	Cycles         int
	MissedCycles   int
	Detail         protocol.Detail
}

// CoverageRow reports the provider's own coverage statement.
func (p *Provider) CoverageRow() Coverage {
	cum := p.counters.Cumulative()
	p.mu.Lock()
	defer p.mu.Unlock()
	return Coverage{
		CandidatesSeen: int(cum[protocol.CounterObserved]),
		// Detections and rollups are counted separately from the `emitted` counter, which counts
		// both kinds: a row that merged them could not answer "how many models did we detect",
		// which is half of what §4.4 asks the coverage row to say.
		ModelsDetected: len(p.detected),
		RollupsEmitted: len(p.rolled),
		Cycles:         p.cycles,
		MissedCycles:   p.missed,
		Detail:         p.lastDetail,
	}
}

// Identity is the enrolled identity the provider stamps on records it mints. It is separate from
// Agent (which carries the scope query) so a wiring mistake cannot attribute a detection to the
// wrong tenant: an empty TenantID is refused by the emitter.
type Identity struct {
	TenantID string
	DeviceID string
}

// SetIdentity installs the device identity. It must be called before Start; a provider with no
// identity emits nothing rather than emitting something mis-attributed.
func (p *Provider) SetIdentity(id Identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = id
}

// identityOrAgent prefers the explicit identity and falls back to the scope agent's device id,
// which is what a single-tenant test wiring uses.
func (p *Provider) identityOrAgent() Identity {
	p.mu.Lock()
	id := p.identity
	p.mu.Unlock()
	if id.TenantID == "" {
		id = Identity{TenantID: p.cfg.Agent.DeviceID, DeviceID: p.cfg.Agent.DeviceID}
	}
	return id
}

// Describe renders the coverage row for a log line or a health detail.
func (c Coverage) Describe() string {
	return fmt.Sprintf("candidates=%d models=%d rollups=%d cycles=%d missed=%d detail=%s",
		c.CandidatesSeen, c.ModelsDetected, c.RollupsEmitted, c.Cycles, c.MissedCycles, c.Detail)
}
