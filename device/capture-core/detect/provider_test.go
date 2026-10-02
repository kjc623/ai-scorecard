package detect

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeEmitter records what the provider minted, through the same EnvelopeInput the real pipeline
// takes, so the record shape is checked by core.BuildEnvelope rather than by this test.
type fakeEmitter struct {
	mu      sync.Mutex
	inputs  []core.EnvelopeInput
	payload [][]byte
	mode    protocol.CollectionMode
	err     error
}

func (e *fakeEmitter) ResolveMode(core.ScopeQuery) core.Resolution {
	return core.Resolution{Mode: e.mode, PolicyVersion: "test"}
}

func (e *fakeEmitter) EmitEnvelope(_ context.Context, in core.EnvelopeInput) (core.Outcome, error) {
	if e.err != nil {
		return core.Outcome{}, e.err
	}
	raw, err := core.BuildEnvelope(in)
	if err != nil {
		return core.Outcome{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, in)
	e.payload = append(e.payload, raw)
	return core.Outcome{Route: in.Route, Mode: in.Mode, Emitted: true, EventID: in.EventID}, nil
}

func (e *fakeEmitter) kinds() []protocol.Kind {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]protocol.Kind, 0, len(e.inputs))
	for _, in := range e.inputs {
		out = append(out, in.Kind)
	}
	return out
}

func (e *fakeEmitter) byKind(k protocol.Kind) []core.EnvelopeInput {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []core.EnvelopeInput
	for _, in := range e.inputs {
		if in.Kind == k {
			out = append(out, in)
		}
	}
	return out
}

func (e *fakeEmitter) rawAt(i int) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.payload[i]
}

// seedBundle is the signed seed set §4.4 requires: signatures and the port map, as bundle data.
func seedBundle() *policy.Bundle {
	return &policy.Bundle{
		Version:       "1",
		EffectiveAt:   time.Unix(1_700_000_000, 0),
		TenantDefault: protocol.ModeM1,
		ProcDetect: policy.ProcDetectPolicy{
			ImageSignatures:      []string{"ollama", "llama-server"},
			ModuleSignatures:     []string{"ggml", "llama.dll"},
			PortMap:              []policy.PortMapEntry{{Port: 11434, ToolFingerprint: "ollama_local"}, {Port: 1234, ToolFingerprint: "lmstudio_local"}},
			MinComputePermille:   200,
			SignatureVersion:     "seed-2026-10-01",
			CycleIntervalSeconds: 1,
		},
	}
}

func fixedEnumerator(procs ...ProcessInfo) EnumeratorFunc {
	return func(context.Context) ([]ProcessInfo, error) { return procs, nil }
}

func newProvider(t *testing.T, enum Enumerator, bundle *policy.Bundle, emit *fakeEmitter) *Provider {
	t.Helper()
	p := New(Config{
		Enumerator: enum,
		Bundles:    func() *policy.Bundle { return bundle },
		Pipeline:   emit,
		Agent:      core.ScopeQuery{DeviceID: "device-1", UserRef: "user-1"},
		Log:        testLogger{t},
		Clock:      func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1"})
	return p
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

// §4.4: evidence a local model ran — an inference-runtime process with a listening local socket —
// produces ONE model_detection with detection_basis from the schema's closed set.
func TestDetect_4_4_ListeningRuntimeProducesOneModelDetection(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{
		PID: 4242, ImagePath: `/usr/local/bin/ollama`, ListeningPorts: []int{11434},
		Modules: []string{"libggml.so"}, ComputePermille: 700,
	})
	p := newProvider(t, enum, seedBundle(), emit)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	detections := emit.byKind(protocol.KindModelDetection)
	if len(detections) != 1 {
		t.Fatalf("model_detection count = %d, want exactly one for one piece of evidence", len(detections))
	}
	in := detections[0]
	if in.DetectionBasis != "process_scan" {
		t.Fatalf("detection_basis = %q, want process_scan (the listening socket is the evidence)", in.DetectionBasis)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(emit.rawAt(0), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, forbidden := range []string{"content_digest", "labels", "classifier_version", "content_excerpt", "attachments", "size_bytes", "policy_decision", "window_start"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("model_detection carries %q, which the schema forbids for this kind", forbidden)
		}
	}
	if got := string(env["direction"]); got != `"none"` {
		t.Errorf("direction = %s, want \"none\"", got)
	}
	row := p.CoverageRow()
	if row.ModelsDetected != 1 || row.CandidatesSeen < 1 {
		t.Fatalf("coverage row = %+v, want one detected model and at least one candidate", row)
	}
}

// §4.4: a loaded inference-runtime module with sustained compute is the other evidence path, and
// it is reported as `module_signature`.
func TestDetect_4_4_LoadedRuntimeModuleWithCompute(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{
		PID: 99, ImagePath: `/opt/tool/helper`, Modules: []string{"llama.dll"}, ComputePermille: 650,
	})
	p := newProvider(t, enum, seedBundle(), emit)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	detections := emit.byKind(protocol.KindModelDetection)
	if len(detections) != 1 || detections[0].DetectionBasis != "module_signature" {
		t.Fatalf("detections = %+v, want one with basis module_signature", detections)
	}
}

// §4.4: "an installed-but-idle runtime is not a model that ran". The image matches the seed set,
// there is no listening port and no sustained compute, so nothing is emitted — this is the guard
// against inflating mode I with exactly the overstatement R11 warns about.
func TestDetect_4_4_InstalledButIdleRuntimeIsNotAModelThatRan(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{PID: 7, ImagePath: `/usr/local/bin/ollama`, ComputePermille: 0})
	p := newProvider(t, enum, seedBundle(), emit)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	if got := emit.byKind(protocol.KindModelDetection); len(got) != 0 {
		t.Fatalf("an idle runtime produced a model_detection: %v", got)
	}
	// A rollup is still expected — the application is installed and running, which is a coverage
	// fact — and it asserts submission_count 0.
	rollups := emit.byKind(protocol.KindUsageRollup)
	if len(rollups) != 1 {
		t.Fatalf("rollups = %d, want one (the candidate is active, we captured nothing)", len(rollups))
	}
	if rollups[0].SubmissionCount == nil || *rollups[0].SubmissionCount != 0 {
		t.Fatalf("submission_count = %v, want an explicit 0", rollups[0].SubmissionCount)
	}
	if row := p.CoverageRow(); row.CandidatesSeen != 1 {
		t.Fatalf("candidate seen = %d, want 1: the coverage row must show the candidate even when nothing was detected", row.CandidatesSeen)
	}
}

// §4.4/D8: a candidate active with no submission is ONE usage_rollup per device, per tool, per
// day, and it may carry submission_count: 0 — a coverage fact, not an absence of one.
func TestDetect_4_4_RollupIsOnePerToolPerDayWithZeroSubmissions(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	now := time.Unix(1_700_000_000, 0)
	cycles := 0
	enum := EnumeratorFunc(func(context.Context) ([]ProcessInfo, error) {
		cycles++
		return []ProcessInfo{{PID: 5, ImagePath: "/usr/local/bin/ollama", ComputePermille: 0}}, nil
	})
	p := New(Config{
		Enumerator: enum,
		Bundles:    func() *policy.Bundle { return seedBundle() },
		Pipeline:   emit,
		Agent:      core.ScopeQuery{DeviceID: "device-1", UserRef: "user-1"},
		Log:        testLogger{t},
		Clock:      func() time.Time { return now },
	})
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1"})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())
	p.cycle(context.Background())
	p.cycle(context.Background())

	rollups := emit.byKind(protocol.KindUsageRollup)
	if len(rollups) != 1 {
		t.Fatalf("rollups = %d, want exactly one per tool per day (three cycles must not be three rows)", len(rollups))
	}
	in := rollups[0]
	if in.SubmissionCount == nil || *in.SubmissionCount != 0 {
		t.Fatalf("submission_count = %v, want an explicit 0", in.SubmissionCount)
	}
	if in.WindowStart == nil || in.WindowEnd == nil || !in.WindowEnd.After(*in.WindowStart) {
		t.Fatalf("rollup window = %v..%v", in.WindowStart, in.WindowEnd)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(emit.rawAt(len(emit.payload)-1), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, ok := env["bytes_total"]; !ok {
		t.Error("rollup has no bytes_total, which the schema requires")
	}
}

// §4.4: it reports loudly. An enumeration failure is `degraded` with detail=enumeration_partial,
// the counters show it, and nothing is emitted — the provider fails open by doing nothing.
func TestDetect_4_4_EnumerationPartialIsDegradedAndEmitsNothing(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := EnumeratorFunc(func(context.Context) ([]ProcessInfo, error) {
		return nil, errors.New("access denied enumerating another user's processes")
	})
	p := newProvider(t, enum, seedBundle(), emit)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	h := p.Health()
	if h.State != protocol.StateDegraded || h.Detail != protocol.DetailEnumerationPartial {
		t.Fatalf("health = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailEnumerationPartial)
	}
	if got := emit.kinds(); len(got) != 0 {
		t.Fatalf("a failed enumeration produced records: %v", got)
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterErrors] == 0 {
		t.Fatal("the enumeration failure was not counted")
	}
}

// §4.4: `absent` when a cycle misses its window. It observes or it does not, and it does not
// claim health while blind.
func TestDetect_4_4_AbsentWhenACycleMissesItsWindow(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{PID: 1, ImagePath: "/usr/local/bin/ollama"})
	now := time.Unix(1_700_000_000, 0)
	p := New(Config{
		Enumerator: enum,
		Bundles:    func() *policy.Bundle { return seedBundle() },
		Pipeline:   emit,
		Agent:      core.ScopeQuery{DeviceID: "device-1", UserRef: "user-1"},
		Log:        testLogger{t},
		Clock:      func() time.Time { return now },
		MissWindow: 30 * time.Second,
	})
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1"})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health right after a cycle = %s/%s, want healthy", h.State, h.Detail)
	}
	now = now.Add(2 * time.Minute) // no cycle completes in the window
	if h := p.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after a missed window = %s, want absent", h.State)
	}
}

// The seed set is signed bundle data: a new signature takes effect through ApplyPolicy, and an
// empty seed set is reported as stale rather than treated as "detect everything".
func TestDetect_4_4_SeedSetIsBundleDataAndCanGoStale(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{PID: 11, ImagePath: "/opt/newruntime/bin/serve", ListeningPorts: []int{9999}})
	empty := seedBundle()
	empty.ProcDetect = policy.ProcDetectPolicy{}
	current := empty
	p := New(Config{
		Enumerator: enum,
		// The provider reads the bundle in force, so the closure follows the store the way the
		// supervisor's wiring does.
		Bundles:  func() *policy.Bundle { return current },
		Pipeline: emit,
		Agent:    core.ScopeQuery{DeviceID: "device-1", UserRef: "user-1"},
		Log:      testLogger{t},
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1"})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailSignatureSetStale {
		t.Fatalf("health with no seed set = %s/%s, want degraded/%s", h.State, h.Detail, protocol.DetailSignatureSetStale)
	}
	if got := emit.kinds(); len(got) != 0 {
		t.Fatalf("an empty seed set produced records: %v", got)
	}

	// A bundle that names the new runtime makes it a candidate on the next cycle.
	updated := seedBundle()
	updated.ProcDetect.PortMap = append(updated.ProcDetect.PortMap, policy.PortMapEntry{Port: 9999, ToolFingerprint: "new_runtime"})
	current = updated
	if err := p.ApplyPolicy(*updated); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	p.cycle(context.Background())
	rollups := emit.byKind(protocol.KindUsageRollup)
	if len(rollups) != 1 || rollups[0].ToolFingerprint != "new_runtime" {
		t.Fatalf("rollups after the bundle update = %+v, want one for new_runtime", rollups)
	}
	if h := p.Health(); h.Detail == protocol.DetailSignatureSetStale {
		t.Fatal("health still reports a stale signature set after a usable bundle was applied")
	}
}

// A provider with no enumerator is a wiring defect and Start says so; it never reports healthy
// while blind.
func TestDetect_StartRefusesWithoutAnEnumerator(t *testing.T) {
	p := New(Config{Bundles: func() *policy.Bundle { return seedBundle() }, Log: testLogger{t}})
	if err := p.Start(context.Background()); !errors.Is(err, ErrNoEnumerator) {
		t.Fatalf("Start error = %v, want ErrNoEnumerator", err)
	}
	if h := p.Health(); h.State == protocol.StateHealthy {
		t.Fatal("a provider that never started reported healthy")
	}
}

// It binds nothing and installs no trust: Start and Stop are cheap and Stop is idempotent, which
// is what makes it safe as the first provider started and the last stopped (§3.5).
func TestDetect_StopIsIdempotentAndHealthNeverHealthyAfterStop(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1}
	enum := fixedEnumerator(ProcessInfo{PID: 2, ImagePath: "/usr/local/bin/ollama", ListeningPorts: []int{11434}})
	p := newProvider(t, enum, seedBundle(), emit)
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if h := p.Health(); h.State == protocol.StateHealthy {
		t.Fatal("Health reported healthy after Stop")
	}
}

// A spool that refuses the record is a named gap, not a panic: the provider keeps observing.
func TestDetect_EmitFailureCountsDroppedAndKeepsObserving(t *testing.T) {
	emit := &fakeEmitter{mode: protocol.ModeM1, err: errors.New("spool full")}
	enum := fixedEnumerator(ProcessInfo{PID: 3, ImagePath: "/usr/local/bin/ollama", ListeningPorts: []int{11434}})
	p := newProvider(t, enum, seedBundle(), emit)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())
	if c := p.Counters().Cumulative(); c[protocol.CounterDropped] == 0 {
		t.Fatalf("counters = %v, want a dropped count for the refused record", c)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s: a spool failure is the spool's coverage gap, not this provider's absence", h.State, h.Detail)
	}
}
