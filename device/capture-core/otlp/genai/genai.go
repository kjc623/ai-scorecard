// Package genai is the normalizer for telemetry that follows the OpenTelemetry GenAI semantic
// conventions (gen_ai.* attributes), for senders no tool-specific normalizer accepts. It is
// registered after every other normalizer, so it sees only what they leave.
//
// The app is named from the process that sent the telemetry, never from the service.name it
// declares: a sender can claim any name.
package genai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/protocol"
)

// Attribute keys and values from the GenAI semantic conventions.
const (
	attrOperation     = "gen_ai.operation.name"
	attrSystem        = "gen_ai.system"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrToolName      = "gen_ai.tool.name"
	attrInputMessages = "gen_ai.input.messages"
	attrPrompt        = "gen_ai.prompt"

	opChat            = "chat"
	opTextCompletion  = "text_completion"
	opGenerateContent = "generate_content"
	opExecuteTool     = "execute_tool"

	eventPrompt      = "gen_ai.content.prompt"
	eventUserMessage = "gen_ai.user.message"

	genAIPrefix = "gen_ai."
)

// maxNameLen is the envelope's limit on model and tool_name.
const maxNameLen = 128

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

	// AppByExe names the catalog app whose executable has this base name. nil matches nothing.
	AppByExe func(base string) (appKey string, ok bool)

	// Decide supplies the policy decision a prompt event requires. The default records `logged`
	// under the default rule, so a decision is never absent.
	Decide func(tool string) *protocol.Decision

	Clock func() time.Time
}

// Normalizer turns GenAI spans and log records into agent_activity and prompt events.
type Normalizer struct {
	cfg       Config
	startedAt time.Time
}

// New returns the normalizer.
func New(cfg Config) *Normalizer {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Counters == nil {
		cfg.Counters = core.NewCounterSet(cfg.Clock())
	}
	if cfg.Decide == nil {
		cfg.Decide = func(string) *protocol.Decision {
			return &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
		}
	}
	return &Normalizer{cfg: cfg, startedAt: cfg.Clock()}
}

// Name implements otlp.Normalizer.
func (*Normalizer) Name() string { return "genai" }

// Accepts implements otlp.Normalizer: every service, because it is consulted last.
func (*Normalizer) Accepts(string) bool { return true }

// Spans implements otlp.Normalizer.
func (n *Normalizer) Spans(ctx context.Context, from otlp.Sender, rs *tracepb.ResourceSpans) {
	fp := n.fingerprint(from)
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
	attrs := s.GetAttributes()
	if !generative(attrs) {
		return false
	}
	f, ok := activity(s)
	if !ok {
		return false
	}
	at := n.timeOr(s.GetStartTimeUnixNano())
	id, _ := n.cfg.Pipeline.Identity()
	n.count(n.cfg.Pipeline.Record(ctx, core.Fact{
		Kind:              protocol.KindAgentActivity,
		Route:             protocol.RouteToolOTel,
		ToolFingerprint:   fp,
		Person:            from.Person,
		OccurredAt:        at,
		MonotonicOffsetMS: n.offset(),
		DedupKey:          activityKey(id, fp, f.ActivityType, spanEventID(s), f.DurationMS),
		FactFields:        f,
	}))
	// Only a request to a model sends the user's message; an agent or tool span that carries the
	// same messages would report it twice.
	if f.ActivityType == protocol.ActivityTypeModelRequest {
		msg, found, err := spanMessage(s)
		switch {
		case err != nil:
			n.cfg.Counters.Add(protocol.CounterErrors)
		case found:
			n.prompt(ctx, from, fp, at, msg)
		}
	}
	return true
}

// activity maps a span onto the agent_activity fields, or reports that it is neither a model
// request nor a tool call.
func activity(s *tracepb.Span) (core.FactFields, bool) {
	attrs := s.GetAttributes()
	op := stringAttr(attrs, attrOperation)
	model := stringAttr(attrs, attrRequestModel)
	var f core.FactFields
	switch {
	case op == opChat || op == opTextCompletion || op == opGenerateContent,
		op == "" && stringAttr(attrs, attrSystem) != "" && model != "":
		f.ActivityType = protocol.ActivityTypeModelRequest
		if model == "" {
			model = stringAttr(attrs, attrResponseModel)
		}
		f.Model = clip(model)
		f.InputTokens = countAttr(attrs, attrInputTokens)
		f.OutputTokens = countAttr(attrs, attrOutputTokens)
	case op == opExecuteTool:
		f.ActivityType = protocol.ActivityTypeToolCall
		f.ToolName = clip(stringAttr(attrs, attrToolName))
	default:
		return f, false
	}
	if start, end := s.GetStartTimeUnixNano(), s.GetEndTimeUnixNano(); start > 0 && end >= start {
		ms := int64((end - start) / uint64(time.Millisecond))
		f.DurationMS = &ms
	}
	f.Outcome = protocol.ActivityOutcomeSuccess
	if s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		f.Outcome = protocol.ActivityOutcomeError
	}
	return f, true
}

// spanMessage is the latest user message a model-request span carries: in gen_ai.input.messages,
// or in the older gen_ai.prompt, on the span or on its gen_ai.content.prompt event.
func spanMessage(s *tracepb.Span) (userMessage, bool, error) {
	if msg, found, err := attrMessage(s.GetAttributes()); found || err != nil {
		return msg, found, err
	}
	for _, e := range s.GetEvents() {
		if e.GetName() == eventPrompt {
			if msg, found, err := attrMessage(e.GetAttributes()); found || err != nil {
				return msg, found, err
			}
		}
	}
	return nil, false, nil
}

// Logs implements otlp.Normalizer. A log record yields only a prompt event: the activity it
// belongs to is its span's.
func (n *Normalizer) Logs(ctx context.Context, from otlp.Sender, rl *logspb.ResourceLogs) {
	fp := n.fingerprint(from)
	for _, sl := range rl.GetScopeLogs() {
		recs := sl.GetLogRecords()
		for i, lr := range recs {
			var next *logspb.LogRecord
			if i+1 < len(recs) {
				next = recs[i+1]
			}
			if !n.logRecord(ctx, from, fp, lr, next) {
				n.cfg.Counters.Add(protocol.CounterSkippedNotGenerative)
			}
		}
	}
}

// logRecord records one log record, reporting whether it yielded an event. next is the record that
// follows it in its scope, which says whether a message event is the latest of its request.
func (n *Normalizer) logRecord(ctx context.Context, from otlp.Sender, fp string, lr, next *logspb.LogRecord) bool {
	name := eventName(lr)
	if !strings.HasPrefix(name, genAIPrefix) && !generative(lr.GetAttributes()) {
		return false
	}
	at := n.timeOr(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano())
	msg, found, err := attrMessage(lr.GetAttributes())
	if err != nil {
		// Counted as an error rather than as a record with nothing generative in it.
		n.cfg.Counters.Add(protocol.CounterErrors)
		return true
	}
	if !found && name == eventUserMessage && !superseded(lr, next) {
		msg, found = bodyMessage(lr.GetBody())
	}
	if !found {
		return false
	}
	n.prompt(ctx, from, fp, at, msg)
	return true
}

// superseded reports that a message event is followed by another message of the same request, so
// it is an earlier turn. A request's messages are emitted together, in order, under one span.
func superseded(lr, next *logspb.LogRecord) bool {
	return next != nil && messageEvent(eventName(next)) &&
		string(next.GetTraceId()) == string(lr.GetTraceId()) && string(next.GetSpanId()) == string(lr.GetSpanId())
}

func messageEvent(name string) bool {
	switch name {
	case "gen_ai.system.message", eventUserMessage, "gen_ai.assistant.message", "gen_ai.tool.message":
		return true
	}
	return false
}

// eventName is the record's event name, which older SDKs carry as the event.name attribute.
func eventName(lr *logspb.LogRecord) string {
	if name := lr.GetEventName(); name != "" {
		return name
	}
	return stringAttr(lr.GetAttributes(), "event.name")
}

// prompt hands the user's message to the pipeline, which applies the mode before reading it.
func (n *Normalizer) prompt(ctx context.Context, from otlp.Sender, fp string, at time.Time, msg userMessage) {
	out, err := n.cfg.Pipeline.Process(ctx, core.Observation{
		Route:             protocol.RouteToolOTel,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   fp,
		MediaType:         "text/plain",
		OccurredAt:        at,
		MonotonicOffsetMS: n.offset(),
		SizeBytes:         msg.size(),
		Decision:          n.cfg.Decide(fp),
		Content:           msg,
		Extract:           core.ExtractorFunc(extractText),
		Person:            from.Person,
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

// fingerprint names the sending app: the catalog app of its executable, else a hash of the
// executable's name, else exe:unknown when the sender was not resolved.
func (n *Normalizer) fingerprint(from otlp.Sender) string {
	if !from.Resolved || from.Image == "" {
		return "exe:unknown"
	}
	base := strings.ToLower(from.Image[strings.LastIndexAny(from.Image, `\/`)+1:])
	if n.cfg.AppByExe != nil {
		if key, ok := n.cfg.AppByExe(base); ok {
			return "app:" + key
		}
	}
	sum := sha256.Sum256([]byte(base))
	return "exe:" + hex.EncodeToString(sum[:])[:16]
}

// activityKey is the agent_activity dedup key: sha256 of
// tenant|device|tool_fingerprint|activity_type|event id|duration_ms.
func activityKey(id core.Identity, fp string, t protocol.ActivityType, eventID string, durationMS *int64) string {
	d := ""
	if durationMS != nil {
		d = strconv.FormatInt(*durationMS, 10)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{id.TenantID, id.DeviceID, fp, string(t), eventID, d}, "|")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// spanEventID is the span's own id, or its start time when it has none.
func spanEventID(s *tracepb.Span) string {
	if len(s.GetSpanId()) == 0 {
		return strconv.FormatUint(s.GetStartTimeUnixNano(), 10)
	}
	return hex.EncodeToString(s.GetTraceId()) + hex.EncodeToString(s.GetSpanId())
}

// timeOr is the first non-zero timestamp, else now.
func (n *Normalizer) timeOr(nanos ...uint64) time.Time {
	for _, ns := range nanos {
		if ns > 0 {
			return time.Unix(0, int64(ns))
		}
	}
	return n.cfg.Clock()
}

// offset is milliseconds since the normalizer started, the per-device ordering origin.
func (n *Normalizer) offset() int64 { return n.cfg.Clock().Sub(n.startedAt).Milliseconds() }

func generative(attrs []*commonpb.KeyValue) bool {
	for _, kv := range attrs {
		if strings.HasPrefix(kv.GetKey(), genAIPrefix) {
			return true
		}
	}
	return false
}

func attr(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

func stringAttr(attrs []*commonpb.KeyValue, key string) string {
	return attr(attrs, key).GetStringValue()
}

// countAttr is a non-negative integer attribute, nil when absent.
func countAttr(attrs []*commonpb.KeyValue, key string) *int64 {
	v, ok := attr(attrs, key).GetValue().(*commonpb.AnyValue_IntValue)
	if !ok || v.IntValue < 0 {
		return nil
	}
	n := v.IntValue
	return &n
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxNameLen {
		return s
	}
	return string([]rune(s)[:maxNameLen])
}
