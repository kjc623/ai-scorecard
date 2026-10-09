// Package hooks is the hook relay: the tools' prompt hooks (`capture-core --hook <tool> <event>`)
// hand the prompt a user is about to send to the service over the native endpoint, and the
// service decides allow, warn or block from the bundle in force, answers, and then records the
// prompt on route tool.hook.
//
// The answer never waits on the record: it is written before the observation reaches the
// pipeline, and a record that cannot be spooled is counted, not answered. Nothing on this path
// makes a network call, and nothing here logs a prompt or an error that could quote one.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ClassifyBudget bounds the classification a decision waits for. The hook's whole process has
// 400 ms; a classification that does not finish in time leaves the labels unknown.
const ClassifyBudget = 30 * time.Millisecond

// writeTimeout bounds writing the answer to a hook that stopped reading.
const writeTimeout = time.Second

// Pipeline is the part of core.Pipeline the relay uses.
type Pipeline interface {
	ResolveMode(q core.ScopeQuery) core.Resolution
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
	Counters(route protocol.Route) *core.CounterSet
}

// Prompts takes the prompts a user submits, which another path may also report.
type Prompts interface {
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
}

// Config is the relay's wiring.
type Config struct {
	Pipeline Pipeline
	// Prompts receives the answered prompts that are not tool calls; nil sends them to Pipeline.
	Prompts Prompts
	// Bundles returns the bundle in force; nil means none.
	Bundles func() *policy.Bundle
	// Classifier labels the prompt for the decision; nil leaves the labels unknown.
	Classifier core.Classifier
	// Person names the person a hook's account is attributed to. nil attributes the prompt to the
	// pipeline's identity.
	Person func(hostinfo.User) core.Person
	Log    core.Logger
	Clock  func() time.Time
}

// Relay is the hook_relay collector.
type Relay struct {
	cfg      Config
	counters *core.CounterSet

	mu          sync.Mutex
	running     bool
	startedAt   time.Time
	lastSuccess time.Time
	// served is when each tool's hook last reached the relay with an event it could read.
	served map[string]time.Time
	// recording counts the answered prompts not yet handed back by the pipeline; idle is closed
	// whenever it is zero.
	recording int
	idle      chan struct{}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns a stopped relay. Its counters are the pipeline's for route tool.hook, so what the
// pipeline observes, emits and drops for the route is what the relay's health row reports.
func New(cfg Config) *Relay {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	idle := make(chan struct{})
	close(idle)
	return &Relay{cfg: cfg, counters: cfg.Pipeline.Counters(protocol.RouteToolHook), startedAt: cfg.Clock(), served: map[string]time.Time{}, idle: idle}
}

// LastServed reports when a hook of the tool (its endpoint.tools key) last reached the relay with an
// event it could read, zero when none has since the service started.
func (r *Relay) LastServed(tool string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.served[tool]
}

// Name implements core.Provider.
func (r *Relay) Name() protocol.Collector { return protocol.CollectorHookRelay }

// Enabled implements core.Toggled: the relay decides only while the bundle in force switches the
// hooks on.
func (r *Relay) Enabled(b *policy.Bundle) bool { return b != nil && b.Endpoint.Hooks.Enabled }

// Start puts the relay in the path: from now on a hook is answered from the bundle's rules.
func (r *Relay) Start(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		r.running = true
		r.startedAt = r.cfg.Clock()
	}
	return nil
}

// Stop takes the relay out of the path, so a hook is answered allow at once, and waits until the
// prompts already answered are recorded or ctx ends.
func (r *Relay) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.running = false
	idle := r.idle
	r.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
	}
	return nil
}

// ApplyPolicy changes nothing: each decision reads the bundle in force.
func (r *Relay) ApplyPolicy(policy.Bundle) error { return nil }

// Health is healthy while the relay is in the path.
func (r *Relay) Health() core.Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return r.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, r.startedAt, r.lastSuccess)
	}
	return r.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, r.startedAt, r.lastSuccess)
}

// beginRecord reports whether the relay is in the path and, when it is, counts a prompt that will
// be recorded; endRecord must follow.
func (r *Relay) beginRecord() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false
	}
	if r.recording == 0 {
		r.idle = make(chan struct{})
	}
	r.recording++
	return true
}

func (r *Relay) endRecord() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording--
	if r.recording == 0 {
		close(r.idle)
	}
}

// Serve answers a connection whose first frame is hook_evaluate, then records the prompt. A frame
// it cannot read is refused; the hook then fails open.
func (r *Relay) Serve(conn net.Conn, peer hostinfo.User, first protocol.NativeMessage) {
	received := r.cfg.Clock()
	ev, reason, err := decodeEvaluate(first)
	if err != nil {
		r.counters.Add(protocol.CounterErrors)
		r.answer(conn, refusal(reason, err))
		return
	}
	r.mu.Lock()
	r.served[ev.Tool] = received
	r.mu.Unlock()
	answer, rec := r.decide(ev, peer, received)
	if !r.answer(conn, decisionFrame(first.ID, answer)) {
		r.counters.Add(protocol.CounterErrors)
	} else {
		r.mu.Lock()
		r.lastSuccess = r.cfg.Clock()
		r.mu.Unlock()
	}
	if rec != nil {
		r.record(*rec, ev.Tool, ev.ToolName == "")
	}
}

// answer writes one frame and reports whether it was written.
func (r *Relay) answer(conn net.Conn, frame []byte) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return localipc.WriteFrame(conn, frame) == nil
}

// decide answers one prompt and returns the observation to record, nil when nothing is recorded.
// An observation returned is counted by beginRecord, and record ends the count.
func (r *Relay) decide(ev protocol.HookEvaluate, peer hostinfo.User, received time.Time) (protocol.HookDecision, *core.Observation) {
	allow := protocol.HookDecision{Action: protocol.HookAllow}
	var b *policy.Bundle
	if r.cfg.Bundles != nil {
		b = r.cfg.Bundles()
	}
	// A relay out of the path, and a tool whose hooks the tenant switched off, record nothing.
	if b == nil || !b.Endpoint.Hooks.Enabled || !b.Endpoint.Tools[ev.Tool].Hooks || !r.beginRecord() {
		return allow, nil
	}

	fp := "app:" + ev.Tool
	q := core.ScopeQuery{ToolFingerprint: fp}
	var person *core.Person
	if r.cfg.Person != nil {
		p := r.cfg.Person(peer)
		person = &p
		q.UserRef = p.UserRef
	}
	res := r.cfg.Pipeline.ResolveMode(q)

	// At m0 nothing is classified, so a rule that lists labels cannot match.
	var labels []string
	known := false
	if res.ReadsContent() && !ev.OverCap && ev.PromptText != "" {
		labels, known = r.classify(res.Mode, ev.PromptText)
	}
	d := enforce.Evaluate(b, enforce.Input{Route: protocol.RouteToolHook, ToolFingerprint: fp, Labels: labels, LabelsKnown: known})

	recorded := enforce.RecordedAction(d, canEnforce(ev.Tool, ev.Event))
	obs := &core.Observation{
		Route:           protocol.RouteToolHook,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: fp,
		MediaType:       "text/plain",
		OccurredAt:      received,
		SizeBytes:       ev.PromptBytes,
		Enforce:         func([]string, bool) protocol.Decision { return recorded },
		Content:         promptReader(ev.PromptText),
		Extract:         core.ExtractorFunc(extractPrompt),
		OverCap:         ev.OverCap,
		Person:          person,
		ClientID:        ev.SessionID,
	}
	return protocol.HookDecision{Action: hookAction(d.Action), Message: d.Message, Link: d.Link, RuleID: d.RuleID}, obs
}

// canEnforce reports whether the tool stops the action when the event's hook blocks it. A tool with
// no adapter has no hook that could have asked.
func canEnforce(tool, event string) bool {
	a, ok := Lookup(tool)
	return ok && a.CanEnforce(event)
}

// classify labels the prompt within ClassifyBudget. known is false when the classification did not
// complete.
func (r *Relay) classify(mode protocol.CollectionMode, text string) (labels []string, known bool) {
	if r.cfg.Classifier == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), ClassifyBudget)
	defer cancel()
	resp, err := r.cfg.Classifier.Classify(ctx, protocol.ClassifyRequest{
		Content:   []byte(text),
		Mode:      mode,
		MediaType: "text/plain",
		BudgetMS:  ClassifyBudget.Milliseconds(),
		Budget:    ClassifyBudget,
	})
	if err != nil || resp.Validate() != nil || resp.Confidence == protocol.ConfidenceDegraded {
		return nil, false
	}
	for _, l := range resp.Labels {
		if !slices.Contains(labels, l.Class) {
			labels = append(labels, l.Class)
		}
	}
	return labels, true
}

// record hands an answered prompt to the pipeline, a prompt the user submitted through Prompts. The
// pipeline counts a record it could not spool as dropped on the route's counters, which are the
// relay's.
func (r *Relay) record(obs core.Observation, tool string, submitted bool) {
	defer r.endRecord()
	var to Prompts = r.cfg.Pipeline
	if submitted && r.cfg.Prompts != nil {
		to = r.cfg.Prompts
	}
	out, err := to.Process(context.Background(), obs)
	// Before enrolment every record is refused; the pipeline counts those.
	if err != nil && !errors.Is(err, core.ErrIdentityUnresolved) {
		r.cfg.Log.Printf("hooks: a %s prompt was not recorded (%s)", tool, out.Reason)
	}
}

// decodeEvaluate reads a hook_evaluate frame. Its errors never quote the frame.
func decodeEvaluate(msg protocol.NativeMessage) (protocol.HookEvaluate, protocol.RefusalReason, error) {
	if msg.Version != protocol.Version {
		return protocol.HookEvaluate{}, protocol.RefusalVersionMismatch, errors.New("the frame's version is not this agent's")
	}
	if msg.Type != protocol.TypeHookEvaluate {
		return protocol.HookEvaluate{}, protocol.RefusalUnknownType, errors.New("the frame is not a hook_evaluate")
	}
	var ev protocol.HookEvaluate
	if err := json.Unmarshal(msg.Body, &ev); err != nil {
		return protocol.HookEvaluate{}, protocol.RefusalMalformed, errors.New("the body is not a hook_evaluate")
	}
	if err := ev.Validate(); err != nil {
		return protocol.HookEvaluate{}, protocol.RefusalMalformed, err
	}
	return ev, "", nil
}

func hookAction(a policy.RuleAction) protocol.HookAction {
	switch a {
	case policy.RuleBlock:
		return protocol.HookBlock
	case policy.RuleWarn:
		return protocol.HookWarn
	default:
		return protocol.HookAllow
	}
}

func decisionFrame(id string, d protocol.HookDecision) []byte {
	body, _ := json.Marshal(d)
	out, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeHookDecision, Version: protocol.Version, ID: id, Body: body})
	return out
}

func refusal(reason protocol.RefusalReason, err error) []byte {
	body, _ := json.Marshal(protocol.Refusal{Reason: reason, Message: err.Error()})
	out, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeRefusal, Version: protocol.Version, Body: body})
	return out
}

// promptReader is the prompt text behind the pipeline's content gate.
type promptReader string

func (p promptReader) Read(context.Context) ([]byte, error) {
	if p == "" {
		return nil, nil
	}
	return []byte(p), nil
}

var errNoPromptText = errors.New("hooks: the prompt was not sent")

// extractPrompt is the authored text: the hook hands over exactly what the user typed.
func extractPrompt(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errNoPromptText
	}
	return string(payload), nil, nil
}
