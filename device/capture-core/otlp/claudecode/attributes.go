package claudecode

import (
	"math"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Attribute keys the normalizer reads.
const (
	attrServiceName    = "service.name"
	attrEventName      = "event.name"
	attrEventTimestamp = "event.timestamp"
	attrEventSequence  = "event.sequence"
	attrSessionID      = "session.id"

	attrPromptLength = "prompt_length"
	attrPrompt       = "prompt"
	attrPromptText   = "prompt_text"

	attrToolName     = "tool_name"
	attrSuccess      = "success"
	attrDecisionType = "decision_type"
	attrDecision     = "decision"

	attrModel        = "model"
	attrInputTokens  = "input_tokens"
	attrOutputTokens = "output_tokens"
	attrDurationMS   = "duration_ms"
)

// mapped is every attribute key the normalizer reads, with what it feeds. A record's attributes
// are read through this table only, so a key missing from it is never read. Every other key the
// tool sends is dropped; the package's tests hold the list of those and why.
var mapped = map[string]string{
	attrServiceName:    "selects this normalizer (resource attribute)",
	attrEventName:      "selects the event's mapping",
	attrEventTimestamp: "the tool's event id in agent_activity dedup_key; occurred_at when the record has no time",
	attrEventSequence:  "the tool's event id in agent_activity dedup_key",
	attrSessionID:      "the prompt's client id",
	attrPromptLength:   "prompt size_bytes",
	attrPrompt:         "the prompt's content reader, classified on the device at m1 and above",
	attrPromptText:     "the prompt's content reader when prompt carries no text",
	attrToolName:       "tool_name",
	attrSuccess:        "tool_call outcome success or error",
	attrDecisionType:   "tool_call outcome denied on reject",
	attrDecision:       "tool_call outcome denied on reject",
	attrModel:          "model",
	attrInputTokens:    "input_tokens",
	attrOutputTokens:   "output_tokens",
	attrDurationMS:     "duration_ms",
}

// redacted is the value Claude Code sends in place of prompt text while prompt logging is off.
const redacted = "<REDACTED>"

// attrs is a record's mapped attributes.
type attrs map[string]*commonpb.AnyValue

func readAttrs(kvs []*commonpb.KeyValue) attrs {
	a := make(attrs, len(mapped))
	for _, kv := range kvs {
		if _, ok := mapped[kv.GetKey()]; ok {
			a[kv.GetKey()] = kv.GetValue()
		}
	}
	return a
}

// str is a scalar attribute as text; empty when absent or not a scalar.
func (a attrs) str(key string) string {
	v, ok := a[key]
	if !ok {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	}
	return ""
}

// count is a non-negative integer attribute. Claude Code sends integers, but a whole double or a
// decimal string is read the same way.
func (a attrs) count(key string) *int64 {
	v, ok := a[key]
	if !ok {
		return nil
	}
	var n int64
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		n = x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		if x.DoubleValue != math.Trunc(x.DoubleValue) || x.DoubleValue > math.MaxInt64 || x.DoubleValue < 0 {
			return nil
		}
		n = int64(x.DoubleValue)
	case *commonpb.AnyValue_StringValue:
		p, err := strconv.ParseInt(strings.TrimSpace(x.StringValue), 10, 64)
		if err != nil {
			return nil
		}
		n = p
	default:
		return nil
	}
	if n < 0 {
		return nil
	}
	return &n
}

// flag is a Boolean attribute sent as a Boolean or as "true" or "false".
func (a attrs) flag(key string) (value, ok bool) {
	v, present := a[key]
	if !present {
		return false, false
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue, true
	case *commonpb.AnyValue_StringValue:
		b, err := strconv.ParseBool(strings.TrimSpace(x.StringValue))
		return b, err == nil
	}
	return false, false
}

// promptText is the prompt the record carries, empty when prompt logging is off.
func (a attrs) promptText() string {
	for _, key := range []string{attrPrompt, attrPromptText} {
		if s := a.str(key); s != "" && s != redacted {
			return s
		}
	}
	return ""
}

// eventTime is event.timestamp, zero when absent or unreadable.
func (a attrs) eventTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, a.str(attrEventTimestamp))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
