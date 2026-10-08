// Package copilot turns GitHub Copilot's OpenTelemetry spans, from the VS Code extension and from
// the Copilot CLI, into observations. Both follow the GenAI semantic conventions: a chat span is a
// model request, an execute_tool span a tool call, and a root invoke_agent span, which wraps the
// work for one message the person sent, carries the prompt. Copilot's log events repeat what its
// spans carry and are not converted. The person is always the sending process's owner; Copilot's
// own identity attributes are never used.
//
// Spans carry prompt text, so nothing here logs an attribute value.
package copilot

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
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/otlp/genai"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// The service.name values Copilot exports under: the VS Code extension's, and the Copilot
// runtime's, which the CLI sends.
const (
	ServiceVSCode = "copilot-chat"
	ServiceCLI    = "github-copilot"
)

// The catalog fingerprints of the two.
const (
	FingerprintVSCode = "app:github_copilot"
	FingerprintCLI    = "app:copilot_cli"
)

// The gen_ai.operation.name values the normalizer converts.
const (
	opChat        = "chat"
	opExecuteTool = "execute_tool"
	opInvokeAgent = "invoke_agent"
)

// Pipeline is the part of core.Pipeline the normalizer uses.
type Pipeline interface {
	Identity() (core.Identity, bool)
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
	Record(ctx context.Context, f core.Fact) error
}

// Config is the normalizer's configuration.
type Config struct {
	Pipeline Pipeline

	// Counters is the otel_receiver collector's counter set; the outcome of each record is counted
	// on its health row.
	Counters *core.CounterSet

	// Bundles returns the bundle in force, for the enforcement rules. nil means no rules: a prompt
	// then records `logged` under the default rule.
	Bundles func() *policy.Bundle

	Clock func() time.Time
}

// Normalizer is Copilot's otlp.Normalizer.
type Normalizer struct {
	cfg Config
}

var _ otlp.Normalizer = (*Normalizer)(nil)

// New returns the normalizer.
func New(cfg Config) *Normalizer {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Counters == nil {
		cfg.Counters = core.NewCounterSet(cfg.Clock())
	}
	return &Normalizer{cfg: cfg}
}

// Name implements otlp.Normalizer.
func (*Normalizer) Name() string { return "copilot" }

// Accepts implements otlp.Normalizer.
func (*Normalizer) Accepts(serviceName string) bool {
	return serviceName == ServiceVSCode || serviceName == ServiceCLI
}

// Logs implements otlp.Normalizer. VS Code's log events repeat its spans (an inference event per
// chat span, a tool event per execute_tool span) or record editor actions with no envelope kind,
// so none is converted.
func (n *Normalizer) Logs(_ context.Context, _ otlp.Sender, rl *logspb.ResourceLogs) {
	for _, sl := range rl.GetScopeLogs() {
		n.cfg.Counters.Incr(protocol.CounterSkippedNotGenerative, uint64(len(sl.GetLogRecords())))
	}
}

// Spans implements otlp.Normalizer.
func (n *Normalizer) Spans(ctx context.Context, from otlp.Sender, rs *tracepb.ResourceSpans) {
	fp := FingerprintVSCode
	if readAttrs(rs.GetResource().GetAttributes()).str(attrServiceName) == ServiceCLI {
		fp = FingerprintCLI
	}
	for _, ss := range rs.GetScopeSpans() {
		for _, s := range ss.GetSpans() {
			if !n.span(ctx, from, fp, s) {
				n.cfg.Counters.Add(protocol.CounterSkippedNotGenerative)
			}
		}
	}
}

// span records one span, reporting whether it yielded an event.
func (n *Normalizer) span(ctx context.Context, from otlp.Sender, fp string, s *tracepb.Span) bool {
	a := readAttrs(s.GetAttributes())
	switch a.str(attrOperation) {
	case opChat:
		model := a.name(attrRequestModel)
		if model == "" {
			model = a.name(attrResponseModel)
		}
		n.activity(ctx, from, fp, s, a, core.FactFields{
			ActivityType: protocol.ActivityTypeModelRequest,
			Model:        model,
			InputTokens:  a.count(attrInputTokens),
			OutputTokens: a.count(attrOutputTokens),
		})
	case opExecuteTool:
		n.activity(ctx, from, fp, s, a, core.FactFields{
			ActivityType: protocol.ActivityTypeToolCall,
			ToolName:     a.name(attrToolName),
		})
	case opInvokeAgent:
		// A subagent's agent span is a child of the tool call that started it, and its input is
		// what the model wrote, not what the person typed. Its usage totals repeat its chat spans'.
		if len(s.GetParentSpanId()) != 0 {
			return false
		}
		n.prompt(ctx, from, fp, n.startedAt(s), a)
	default:
		return false
	}
	return true
}

// activity records an agent_activity fact for a span.
func (n *Normalizer) activity(ctx context.Context, from otlp.Sender, fp string, s *tracepb.Span, a attrs, f core.FactFields) {
	if start, end := s.GetStartTimeUnixNano(), s.GetEndTimeUnixNano(); start > 0 && end >= start {
		ms := int64((end - start) / uint64(time.Millisecond))
		f.DurationMS = &ms
	}
	f.Outcome = protocol.ActivityOutcomeSuccess
	if s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR || a.str(attrErrorType) != "" {
		f.Outcome = protocol.ActivityOutcomeError
	}
	id, _ := n.cfg.Pipeline.Identity()
	n.count(n.cfg.Pipeline.Record(ctx, core.Fact{
		Kind:            protocol.KindAgentActivity,
		Route:           protocol.RouteToolOTel,
		ToolFingerprint: fp,
		Person:          from.Person,
		OccurredAt:      n.startedAt(s),
		DedupKey:        activityKey(id, fp, f.ActivityType, spanEventID(s), f.DurationMS),
		FactFields:      f,
	}))
}

// prompt hands the person's message to the pipeline, which applies the mode before reading it.
// With content capture off the span carries no text, and the prompt is still recorded.
func (n *Normalizer) prompt(ctx context.Context, from otlp.Sender, fp string, at time.Time, a attrs) {
	content, size := n.promptText(a)
	out, err := n.cfg.Pipeline.Process(ctx, core.Observation{
		Route:           protocol.RouteToolOTel,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: fp,
		MediaType:       "text/plain",
		OccurredAt:      at,
		SizeBytes:       size,
		Enforce:         n.enforce(fp),
		Content:         content,
		Extract:         genai.ExtractText,
		Person:          from.Person,
	})
	switch {
	case out.Reason == core.ReasonIdentityUnresolved:
		n.cfg.Counters.Add(protocol.CounterErrors)
	case err != nil:
		n.cfg.Counters.Add(protocol.CounterDropped)
	case out.Emitted:
		n.cfg.Counters.Add(protocol.CounterEmitted)
	}
}

// promptText is the latest user message in gen_ai.input.messages, else copilot_chat.user_request,
// behind a reader. A messages value that is not JSON counts an error and is not read.
func (n *Normalizer) promptText(a attrs) (core.ContentReader, int64) {
	if kv := a.kv(attrInputMessages); kv != nil {
		msg, size, found, err := genai.LatestUserMessage(kv)
		if err != nil {
			n.cfg.Counters.Add(protocol.CounterErrors)
		}
		if found {
			return msg, size
		}
	}
	s := a.str(attrUserRequest)
	return text(s), int64(len(s))
}

// text is prompt text behind a reader; empty means the span carries none.
type text string

func (t text) Read(context.Context) ([]byte, error) {
	if t == "" {
		return nil, nil
	}
	return []byte(t), nil
}

// enforce is a prompt's enforcement hook. Route tool.otel reports a prompt after Copilot has sent
// it, so it can never block or warn: the matching rule is recorded as logged.
func (n *Normalizer) enforce(fp string) func(labels []string, known bool) protocol.Decision {
	if n.cfg.Bundles == nil {
		return func([]string, bool) protocol.Decision {
			return enforce.RecordedAction(enforce.Decision{RuleID: enforce.DefaultRuleID, Action: policy.RuleAllow}, false)
		}
	}
	return enforce.Hook(n.cfg.Bundles, protocol.RouteToolOTel, fp, false)
}

func (n *Normalizer) count(err error) {
	switch {
	case errors.Is(err, core.ErrIdentityUnresolved):
		n.cfg.Counters.Add(protocol.CounterErrors)
	case err != nil:
		n.cfg.Counters.Add(protocol.CounterDropped)
	default:
		n.cfg.Counters.Add(protocol.CounterEmitted)
	}
}

// startedAt is the span's start, else now.
func (n *Normalizer) startedAt(s *tracepb.Span) time.Time {
	if ns := s.GetStartTimeUnixNano(); ns > 0 && ns <= uint64(1<<63-1) {
		return time.Unix(0, int64(ns)).UTC()
	}
	return n.cfg.Clock().UTC()
}

// activityKey is sha256(tenant|device|tool_fingerprint|activity_type|event id|duration_ms).
func activityKey(id core.Identity, fp string, t protocol.ActivityType, eventID string, durationMS *int64) string {
	d := ""
	if durationMS != nil {
		d = strconv.FormatInt(*durationMS, 10)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{id.TenantID, id.DeviceID, fp, string(t), eventID, d}, "|")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// spanEventID is the span's trace and span ids, the tool's own id for it, or its start time when
// it has none.
func spanEventID(s *tracepb.Span) string {
	if len(s.GetSpanId()) == 0 {
		return strconv.FormatUint(s.GetStartTimeUnixNano(), 10)
	}
	return hex.EncodeToString(s.GetTraceId()) + hex.EncodeToString(s.GetSpanId())
}
