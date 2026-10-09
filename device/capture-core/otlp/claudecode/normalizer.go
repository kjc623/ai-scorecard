// Package claudecode turns Claude Code's OpenTelemetry log events into observations: each prompt
// goes through the pipeline's mode gate and classifier, and model requests and tool calls become
// agent_activity records. The person is always the sending process's owner; Claude Code's own
// user.* attributes are never used for identity.
//
// Records carry prompt text, so nothing here logs an attribute value.
package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
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

// ServiceName is the service.name Claude Code exports under.
const ServiceName = "claude-code"

// ToolFingerprint is Claude Code's catalog fingerprint.
const ToolFingerprint = "app:claude_code"

// eventPrefix is the namespace of Claude Code's event names. The event.name attribute carries the
// name without it; the record body and the OTLP event name may carry it with it.
const eventPrefix = "claude_code."

// The events the normalizer converts, by event.name.
const (
	eventUserPrompt   = "user_prompt"
	eventToolResult   = "tool_result"
	eventToolDecision = "tool_decision"
	eventAPIRequest   = "api_request"
	eventAPIError     = "api_error"
)

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

// Normalizer is Claude Code's otlp.Normalizer.
type Normalizer struct {
	cfg Config
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
func (n *Normalizer) Name() string { return ServiceName }

// Accepts implements otlp.Normalizer.
func (n *Normalizer) Accepts(serviceName string) bool { return serviceName == ServiceName }

// Spans implements otlp.Normalizer. Claude Code's spans are not converted: its log events carry
// every record this normalizer emits.
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
	name := eventName(lr, a)
	at := occurredAt(lr, a)
	var err error
	switch name {
	case eventUserPrompt:
		err = n.prompt(ctx, from, a, at)
	case eventToolResult:
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeToolCall,
			ToolName:     a.str(attrToolName),
			DurationMS:   a.count(attrDurationMS),
			Outcome:      toolOutcome(a),
		})
	case eventToolDecision:
		// An accepted call is recorded by its tool_result. A rejected call has no tool_result, so
		// its decision is the record.
		if a.str(attrDecision) != "reject" {
			return
		}
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeToolCall,
			ToolName:     a.str(attrToolName),
			Outcome:      protocol.ActivityOutcomeDenied,
		})
	case eventAPIRequest:
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        a.str(attrModel),
			InputTokens:  a.count(attrInputTokens),
			OutputTokens: a.count(attrOutputTokens),
			DurationMS:   a.count(attrDurationMS),
			Outcome:      protocol.ActivityOutcomeSuccess,
		})
	case eventAPIError:
		err = n.activity(ctx, from, a, at, core.FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        a.str(attrModel),
			DurationMS:   a.count(attrDurationMS),
			Outcome:      protocol.ActivityOutcomeError,
		})
	default:
		return
	}
	// Before enrolment every record is refused; the pipeline counts those.
	if err != nil && !errors.Is(err, core.ErrIdentityUnresolved) {
		n.cfg.Log.Printf("otlp: claude code %s event not recorded: %v", name, err)
	}
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
		ClientID:        a.str(attrSessionID),
	})
	return err
}

// enforce is the prompt's enforcement hook. Route tool.otel reports a prompt after Claude Code has
// sent it, so it can never block or warn: the matching rule is recorded as logged.
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

// toolOutcome is a tool_result's outcome. Claude Code sends tool_result only for calls it ran, with
// decision_type accept; a reject is still read as denied.
func toolOutcome(a attrs) protocol.ActivityOutcome {
	if a.str(attrDecisionType) == "reject" {
		return protocol.ActivityOutcomeDenied
	}
	if ok, known := a.flag(attrSuccess); known && !ok {
		return protocol.ActivityOutcomeError
	}
	return protocol.ActivityOutcomeSuccess
}

// eventID is the tool's own identity for an event: its timestamp, and its per-process sequence
// number to tell apart events that share one.
func eventID(a attrs, at time.Time) string {
	ts := a.str(attrEventTimestamp)
	if ts == "" {
		ts = at.UTC().Format(time.RFC3339Nano)
	}
	return ts + "/" + a.str(attrEventSequence)
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

// eventName is the record's event name without the claude_code. prefix.
func eventName(lr *logspb.LogRecord, a attrs) string {
	if name := a.str(attrEventName); name != "" {
		return strings.TrimPrefix(name, eventPrefix)
	}
	if name := lr.GetEventName(); name != "" {
		return strings.TrimPrefix(name, eventPrefix)
	}
	if body := lr.GetBody().GetStringValue(); strings.HasPrefix(body, eventPrefix) {
		return strings.TrimPrefix(body, eventPrefix)
	}
	return ""
}

// occurredAt is the record's time: its own, else when it was observed, else event.timestamp.
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

// errNoPromptText: prompt logging is off in Claude Code, so the event has a length and no text.
var errNoPromptText = errors.New("claudecode: the event carries no prompt text")

// extractPrompt takes the prompt text as the authored text: the attribute holds only what the
// person typed. With no text the record is degraded rather than digesting nothing.
func extractPrompt(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errNoPromptText
	}
	return string(payload), nil, nil
}
