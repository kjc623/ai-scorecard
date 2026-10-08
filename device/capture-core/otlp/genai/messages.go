package genai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
)

// errMalformedMessages: a messages attribute is a string that is not JSON. The error never quotes
// the value, which is prompt text.
var errMalformedMessages = errors.New("genai: a messages attribute is not valid JSON")

// userMessage is the text parts of one user message. It is the observation's content reader: the
// parts are joined only when the pipeline's mode permits reading them.
type userMessage []string

// Read implements core.ContentReader.
func (m userMessage) Read(context.Context) ([]byte, error) {
	return []byte(strings.Join(m, "\n")), nil
}

// size is the length of what Read returns, known without joining the parts.
func (m userMessage) size() int64 {
	var n int64
	for _, p := range m {
		n += int64(len(p))
	}
	if len(m) > 1 {
		n += int64(len(m) - 1)
	}
	return n
}

// extractText is the route's extraction: the content is already the user's text.
func extractText(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errors.New("genai: no message text to canonicalise")
	}
	return dedup.Decode(payload), nil, nil
}

// attrMessage is the latest user message in gen_ai.input.messages, else in the older gen_ai.prompt.
// Either is structured or a JSON string. found is false when the attribute is absent or its last
// message is not the user's: the user's message was then reported with an earlier request.
func attrMessage(attrs []*commonpb.KeyValue) (msg userMessage, found bool, err error) {
	for _, key := range []string{attrInputMessages, attrPrompt} {
		v := attr(attrs, key)
		if v == nil {
			continue
		}
		var msgs any
		if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
			if err := json.Unmarshal([]byte(s.StringValue), &msgs); err != nil {
				return nil, false, errMalformedMessages
			}
		} else {
			msgs = plain(v)
		}
		msg, found = lastUserMessage(msgs)
		return msg, found, nil
	}
	return nil, false, nil
}

// lastUserMessage is the text of the last message when it is the user's. A message is the semconv
// form {role, parts: [{type: "text", content}]} or the older OpenAI form {role, content}, where
// content is a string or a list of {type: "text", text}.
func lastUserMessage(msgs any) (userMessage, bool) {
	list, ok := msgs.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	m, ok := list[len(list)-1].(map[string]any)
	if !ok || m["role"] != "user" {
		return nil, false
	}
	var msg userMessage
	if parts, ok := m["parts"].([]any); ok {
		msg = textParts(parts, "content")
	} else {
		switch c := m["content"].(type) {
		case string:
			if c != "" {
				msg = userMessage{c}
			}
		case []any:
			msg = textParts(c, "text")
		}
	}
	return msg, len(msg) > 0
}

func textParts(parts []any, field string) userMessage {
	var msg userMessage
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok || part["type"] != "text" {
			continue
		}
		if s, ok := part[field].(string); ok && s != "" {
			msg = append(msg, s)
		}
	}
	return msg
}

// bodyMessage is the content of a gen_ai.user.message event's body, {content: "..."}.
func bodyMessage(body *commonpb.AnyValue) (userMessage, bool) {
	b, ok := plain(body).(map[string]any)
	if !ok {
		return nil, false
	}
	if s, ok := b["content"].(string); ok && s != "" {
		return userMessage{s}, true
	}
	return nil, false
}

// plain converts an OTLP value to the shapes encoding/json decodes into.
func plain(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return float64(x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, plain(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = plain(kv.GetValue())
		}
		return out
	}
	return nil
}
