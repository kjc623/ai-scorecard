// Package codex turns Codex's OpenTelemetry log events into observations: each prompt goes through
// the pipeline's mode gate and classifier, and model requests and tool calls become agent_activity
// records. The person is always the sending process's owner; Codex's own user.* attributes are
// never used for identity.
//
// Records carry prompt text, so nothing here logs an attribute value.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Name is the normalizer's name.
const Name = "codex"

// ToolFingerprint is Codex's catalog fingerprint.
const ToolFingerprint = "app:codex"

// serviceNames are the service.name values Codex exports under: the interactive CLI, codex exec,
// and the app server that Codex Desktop and the IDE extensions run.
var serviceNames = map[string]bool{
	"codex_cli_rs":     true,
	"codex_exec":       true,
	"codex-app-server": true,
}

// The events the normalizer converts, by event.name.
const (
	eventUserPrompt       = "codex.user_prompt"
	eventToolDecision     = "codex.tool_decision"
	eventToolResult       = "codex.tool_result"
	eventAPIRequest       = "codex.api_request"
	eventWebsocketRequest = "codex.websocket_request"
	eventSSE              = "codex.sse_event"
)

// responseCompleted is the codex.sse_event kind that ends a model response, over HTTP streaming
// and websocket alike.
const responseCompleted = "response.completed"

// deniedDecisions are the tool_decision values under which the call does not run.
var deniedDecisions = map[string]bool{
	"denied":                          true,
	"denied_with_network_policy_deny": true,
	"abort":                           true,
	"timed_out":                       true,
}

// maxDeniedCalls bounds the denied calls remembered until their tool_result arrives.
const maxDeniedCalls = 1024

// Pipeline is the part of core.Pipeline the normalizer uses.
type Pipeline interface {
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
	Record(ctx context.Context, f core.Fact) error
	Identity() (core.Identity, bool)
}

// Config is the normalizer's configuration.
type Config struct {
	Pipeline Pipeline
	Log      core.Logger
	// Bundles returns the bundle in force, for the enforcement rules. nil means no rules.
	Bundles func() *policy.Bundle
}

// Normalizer is Codex's otlp.Normalizer.
type Normalizer struct {
	cfg    Config
	denied deniedCalls
}

var _ otlp.Normalizer = (*Normalizer)(nil)

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns the normalizer.
func New(cfg Config) *Normalizer {
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	return &Normalizer{cfg: cfg}
}

// Name implements otlp.Normalizer.
func (n *Normalizer) Name() string { return Name }

// Accepts implements otlp.Normalizer.
func (n *Normalizer) Accepts(serviceName string) bool { return serviceNames[serviceName] }

// Spans implements otlp.Normalizer. Codex's spans are not converted: its log events carry every
// record this normalizer emits.
func (n *Normalizer) Spans(context.Context, otlp.Sender, *tracepb.ResourceSpans) {}

// Logs implements otlp.Normalizer.
func (n *Normalizer) Logs(ctx context.Context, from otlp.Sender, rl *logspb.ResourceLogs) {
	for _, sl := range rl.GetScopeLogs() {
		for _, lr := range sl.GetLogRecords() {
			n.convert(ctx, from, lr)
		}
	}
}

func (n *Normalizer) convert(ctx context.Context, from otlp.Sender, lr *logspb.LogRecord) {
	a := readAttrs(lr.GetAttributes())
	name := a.str(attrEventName)
	at := occurredAt(lr, a)
	var err error
	switch name {
	case eventUserPrompt:
		err = n.prompt(ctx, from, a, at)
	case eventToolDecision:
		// An approved call is recorded by its tool_result. A call that does not run still has a
		// tool_result, a failure, so its decision is the record and that result is skipped.
		if !deniedDecisions[a.str(attrDecision)] {
			return
		}
		n.denied.add(callKey(a))
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeToolCall,
			ToolName:     a.str(attrToolName),
			Outcome:      protocol.ActivityOutcomeDenied,
		})
	case eventToolResult:
		if n.denied.take(callKey(a)) {
			return
		}
		outcome := protocol.ActivityOutcomeSuccess
		if ok, known := a.flag(attrSuccess); known && !ok {
			outcome = protocol.ActivityOutcomeError
		}
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeToolCall,
			ToolName:     a.str(attrToolName),
			DurationMS:   a.count(attrDurationMS),
			Outcome:      outcome,
		})
	case eventAPIRequest, eventWebsocketRequest:
		// A request that succeeds is recorded by its completed response, which carries the tokens.
		if !requestFailed(name, a) {
			return
		}
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        a.str(attrModel),
			DurationMS:   a.count(attrDurationMS),
			Outcome:      protocol.ActivityOutcomeError,
		})
	case eventSSE:
		if a.str(attrEventKind) != responseCompleted {
			return
		}
		f := core.FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        a.str(attrModel),
			Outcome:      protocol.ActivityOutcomeSuccess,
		}
		// A stream that fails after the request was accepted ends with this kind and an error.
		if a.has(attrErrorMessage) {
			f.Outcome = protocol.ActivityOutcomeError
		} else {
			f.InputTokens = a.count(attrInputTokens)
			f.OutputTokens = a.count(attrOutputTokens)
		}
		err = n.activity(ctx, from, a, at, f)
	default:
		return
	}
	// Before enrolment every record is refused; the pipeline counts those.
	if err != nil && !errors.Is(err, core.ErrIdentityUnresolved) {
		n.cfg.Log.Printf("otlp: codex %s event not recorded: %v", name, err)
	}
}

// requestFailed reports whether an api_request or websocket_request failed, as Codex itself
// decides: an HTTP request succeeds on a 2xx status without an error, a websocket request without
// an error.
func requestFailed(name string, a attrs) bool {
	if a.has(attrErrorMessage) {
		return true
	}
	if name == eventWebsocketRequest {
		ok, known := a.flag(attrSuccess)
		return known && !ok
	}
	status := a.count(attrStatusCode)
	return status == nil || *status < 200 || *status > 299
}

// prompt hands a user_prompt to the pipeline. The text stays behind the reader, so the resolved
// mode decides whether it is ever read.
func (n *Normalizer) prompt(ctx context.Context, from otlp.Sender, a attrs, at time.Time) error {
	var size int64
	if s := a.count(attrPromptLength); s != nil {
		size = *s
	}
	_, err := n.cfg.Pipeline.Process(ctx, core.Observation{
		Route:           protocol.RouteToolOTel,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: ToolFingerprint,
		MediaType:       "text/plain",
		OccurredAt:      at,
		SizeBytes:       size,
		Enforce:         n.enforce(),
		Content:         promptReader(a.promptText()),
		Extract:         core.ExtractorFunc(extractPrompt),
		Person:          from.Person,
		ClientID:        a.str(attrConversationID),
	})
	return err
}

// enforce is the prompt's enforcement hook. Route tool.otel reports a prompt after Codex has sent
// it, so it can never block or warn: the matching rule is recorded as logged.
func (n *Normalizer) enforce() func(labels []string, known bool) protocol.Decision {
	if n.cfg.Bundles == nil {
		return func([]string, bool) protocol.Decision {
			return enforce.RecordedAction(enforce.Decision{RuleID: enforce.DefaultRuleID, Action: policy.RuleAllow}, false)
		}
	}
	return enforce.Hook(n.cfg.Bundles, protocol.RouteToolOTel, ToolFingerprint, false)
}

// activity records an agent_activity fact.
func (n *Normalizer) activity(ctx context.Context, from otlp.Sender, a attrs, at time.Time, f core.FactFields) error {
	id, _ := n.cfg.Pipeline.Identity()
	return n.cfg.Pipeline.Record(ctx, core.Fact{
		Kind:            protocol.KindAgentActivity,
		Route:           protocol.RouteToolOTel,
		ToolFingerprint: ToolFingerprint,
		Person:          from.Person,
		OccurredAt:      at,
		DedupKey:        activityKey(id, f.ActivityType, eventID(a, at), f.DurationMS),
		FactFields:      f,
	})
}

// eventID is the tool's own identity for an event. Codex sends no event sequence: the event, its
// conversation and its millisecond timestamp identify it, with the call id or attempt that tell
// apart a turn's tool calls and retries.
func eventID(a attrs, at time.Time) string {
	ts := a.str(attrEventTimestamp)
	if ts == "" {
		ts = at.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{a.str(attrEventName), a.str(attrConversationID), ts, a.str(attrCallID), a.str(attrAttempt)}, "/")
}

// activityKey is sha256(tenant|device|tool_fingerprint|activity_type|event id|duration_ms).
func activityKey(id core.Identity, t protocol.ActivityType, event string, durationMS *int64) string {
	d := ""
	if durationMS != nil {
		d = strconv.FormatInt(*durationMS, 10)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{id.TenantID, id.DeviceID, ToolFingerprint, string(t), event, d}, "|")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// callKey names a tool call across its decision and result; empty without a call id.
func callKey(a attrs) string {
	call := a.str(attrCallID)
	if call == "" {
		return ""
	}
	return a.str(attrConversationID) + "/" + call
}

// deniedCalls remembers the calls whose denial was recorded, oldest first out, until their
// tool_result arrives.
type deniedCalls struct {
	mu    sync.Mutex
	set   map[string]bool
	order []string
}

func (d *deniedCalls) add(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.set == nil {
		d.set = make(map[string]bool)
	}
	if d.set[key] {
		return
	}
	if len(d.order) == maxDeniedCalls {
		delete(d.set, d.order[0])
		d.order = d.order[1:]
	}
	d.set[key] = true
	d.order = append(d.order, key)
}

// take reports whether key was denied, and forgets it.
func (d *deniedCalls) take(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.set[key] {
		return false
	}
	delete(d.set, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	return true
}

// occurredAt is the record's time: its own, else when it was observed, else event.timestamp.
// Codex's log bridge sets only the observed time.
func occurredAt(lr *logspb.LogRecord, a attrs) time.Time {
	for _, ns := range []uint64{lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano()} {
		if ns > 0 && ns <= uint64(1<<63-1) {
			return time.Unix(0, int64(ns)).UTC()
		}
	}
	if t := a.eventTime(); !t.IsZero() {
		return t
	}
	return time.Now().UTC()
}

// promptReader returns the prompt's text, or nothing when the record carries none.
type promptReader string

func (p promptReader) Read(context.Context) ([]byte, error) {
	if p == "" {
		return nil, nil
	}
	return []byte(p), nil
}

// errNoPromptText: prompt logging is off in Codex, so the event has a length and no text.
var errNoPromptText = errors.New("codex: the event carries no prompt text")

// extractPrompt takes the prompt text as the authored text: the attribute holds only the text the
// person entered. With no text the record is degraded rather than digesting nothing.
func extractPrompt(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errNoPromptText
	}
	return string(payload), nil, nil
}
