package genai

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

// errMalformedMessages: a messages attribute is a string that is not JSON. The error never quotes
// the value, which is prompt text.
var errMalformedMessages = errors.New("genai: a messages attribute is not valid JSON")

// userMessage is one user message: its text parts and the files its other parts name. It is the
// observation's content reader: the parts are joined only when the pipeline's mode permits reading
// them.
type userMessage struct {
	texts []string
	files []dedup.Attachment
}

// Read implements core.ContentReader.
func (m userMessage) Read(context.Context) ([]byte, error) {
	return []byte(strings.Join(m.texts, "\n")), nil
}

// size is the length of what Read returns, known without joining the parts.
func (m userMessage) size() int64 {
	var n int64
	for _, p := range m.texts {
		n += int64(len(p))
	}
	if len(m.texts) > 1 {
		n += int64(len(m.texts) - 1)
	}
	return n
}

// extract is the message's extraction: its text, with the files it names.
func (m userMessage) extract(payload []byte, contentType string) (string, []dedup.Attachment, error) {
	text, _, err := extractText(payload, contentType)
	return text, m.files, err
}

// extractText is the extraction for content that is already the user's text.
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
				return userMessage{}, false, errMalformedMessages
			}
		} else {
			msgs = plain(v)
		}
		msg, found = lastUserMessage(msgs)
		return msg, found, nil
	}
	return userMessage{}, false, nil
}

// LatestUserMessage applies attrMessage's rule for a tool's own normalizer: the latest user
// message behind a reader, with the length the reader returns, so the text is read only when the
// pipeline's mode permits. The error never quotes the value.
func LatestUserMessage(attrs []*commonpb.KeyValue) (msg core.ContentReader, size int64, found bool, err error) {
	m, found, err := attrMessage(attrs)
	if !found || err != nil {
		return nil, 0, found, err
	}
	return m, m.size(), true, nil
}

// ExtractText is the extraction for a prompt whose content is already the user's text.
var ExtractText core.Extractor = core.ExtractorFunc(extractText)

// lastUserMessage is the last message when it is the user's and has text. A message is the semconv
// form {role, parts: [{type: "text", content}, ...]} or the older OpenAI form {role, content},
// where content is a string or a list of {type: "text", text} and other parts.
func lastUserMessage(msgs any) (userMessage, bool) {
	list, ok := msgs.([]any)
	if !ok || len(list) == 0 {
		return userMessage{}, false
	}
	m, ok := list[len(list)-1].(map[string]any)
	if !ok || m["role"] != "user" {
		return userMessage{}, false
	}
	var msg userMessage
	if parts, ok := m["parts"].([]any); ok {
		msg = messageParts(parts, "content")
	} else {
		switch c := m["content"].(type) {
		case string:
			if c != "" {
				msg.texts = []string{c}
			}
		case []any:
			msg = messageParts(c, "text")
		}
	}
	return msg, len(msg.texts) > 0
}

// messageParts reads a message's text parts, whose text is in field, and names the files its other
// parts attach.
func messageParts(parts []any, field string) userMessage {
	var msg userMessage
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if part["type"] != "text" {
			if name := partFileName(part); name != "" {
				msg.files = parsers.AppendFile(msg.files, dedup.Attachment{Name: name})
			}
			continue
		}
		if s, ok := part[field].(string); ok && s != "" {
			msg.texts = append(msg.texts, s)
		}
	}
	return msg
}

// partFileName is the name a part gives the file it attaches: an OpenAI file part's filename, an
// Anthropic document's title, else the file name the part's URL ends in. A file sent as bytes or by
// id alone, as the semconv blob and file parts are, has none.
func partFileName(part map[string]any) string {
	field := func(m map[string]any, key string) string {
		s, _ := m[key].(string)
		return s
	}
	object := func(key string) map[string]any {
		m, _ := part[key].(map[string]any)
		return m
	}
	switch part["type"] {
	case "file":
		return field(object("file"), "filename")
	case "input_file":
		return cmp.Or(field(part, "filename"), parsers.URLFileName(field(part, "file_url")))
	case "document":
		return cmp.Or(field(part, "title"), parsers.URLFileName(field(object("source"), "url")))
	case "image":
		return parsers.URLFileName(field(object("source"), "url"))
	case "image_url":
		return parsers.URLFileName(field(object("image_url"), "url"))
	case "input_image":
		return parsers.URLFileName(field(part, "image_url"))
	case "uri":
		return parsers.URLFileName(field(part, "uri"))
	}
	return ""
}

// bodyMessage is the content of a gen_ai.user.message event's body, {content: "..."}.
func bodyMessage(body *commonpb.AnyValue) (userMessage, bool) {
	b, ok := plain(body).(map[string]any)
	if !ok {
		return userMessage{}, false
	}
	if s, ok := b["content"].(string); ok && s != "" {
		return userMessage{texts: []string{s}}, true
	}
	return userMessage{}, false
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
