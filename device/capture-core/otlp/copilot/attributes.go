package copilot

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Attribute keys the normalizer reads.
const (
	attrServiceName = "service.name"

	attrOperation     = "gen_ai.operation.name"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrToolName      = "gen_ai.tool.name"
	attrErrorType     = "error.type"

	attrInputMessages = "gen_ai.input.messages"
	attrUserRequest   = "copilot_chat.user_request"
)

// mapped is every attribute key the normalizer reads, with what it feeds. Attributes are read
// through this table only, so a key missing from it is never read. Every other key Copilot sends
// is dropped; the package's tests hold the list of those and why.
var mapped = map[string]string{
	attrServiceName:   "selects this normalizer and the app fingerprint (resource attribute)",
	attrOperation:     "selects the span's mapping",
	attrRequestModel:  "model",
	attrResponseModel: "model when the request names none",
	attrInputTokens:   "input_tokens",
	attrOutputTokens:  "output_tokens",
	attrToolName:      "tool_name",
	attrErrorType:     "outcome error",
	attrInputMessages: "the root agent span's latest user message: the prompt's content reader, classified on the device at m1 and above",
	attrUserRequest:   "the prompt's content reader when the root agent span carries no messages (VS Code)",
}

// maxNameLen is the envelope's limit on model and tool_name.
const maxNameLen = 128

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

// str is a string attribute; empty when absent or not a string.
func (a attrs) str(key string) string {
	return a[key].GetStringValue()
}

// name is a string attribute cut to the envelope's limit on a name.
func (a attrs) name(key string) string {
	s := a.str(key)
	if utf8.RuneCountInString(s) <= maxNameLen {
		return s
	}
	return string([]rune(s)[:maxNameLen])
}

// count is a non-negative integer attribute. Copilot sends integers, but a whole double or a
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

// kv is one mapped attribute as a key-value pair, for readers that take a list.
func (a attrs) kv(key string) []*commonpb.KeyValue {
	v, ok := a[key]
	if !ok {
		return nil
	}
	return []*commonpb.KeyValue{{Key: key, Value: v}}
}
