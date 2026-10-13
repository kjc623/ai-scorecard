// Package claudeai reads the prompts claude.ai's web app sends, which Claude Desktop loads as its
// window: (RPC) the PerformAction call of its Connect RPC conversation service, whose protobuf
// message carries the typed text, and (Completion) the completion request of its REST API, whose
// JSON carries it in `prompt`. claude.ai documents neither, so the shapes are the ones captured from
// the web app, as the browser extension reads them.
//
// The app sends its prompts to claude.ai on these two paths alone. Its other requests there
// (telemetry, timeline streams, viewing reports, and the PerformAction calls of other actions) are
// no prompts, and the proxy records none of them.
package claudeai

import (
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const (
	host = "claude.ai"
	name = "claude.ai"
)

var (
	// performAction is the RPC each conversation action is sent by; a message is one of them.
	performAction = regexp.MustCompile(`^/claudeai-rpc/[^/]+\.ConversationService/PerformAction$`)
	// completion is the REST method a message is sent by, under an organization and a conversation.
	completion = regexp.MustCompile(`^/api/organizations/[^/]+/chat_conversations/[^/]+/completion$`)
)

// The PerformAction request holds a message action in field 2, and the message its typed text in
// field 3. Other actions leave field 2 out.
const (
	actionField protowire.Number = 2
	textField   protowire.Number = 3
)

// RPC reads PerformAction calls.
type RPC struct{}

// Match implements parsers.Parser.
func (RPC) Match(h, p string) bool { return h == host && performAction.MatchString(p) }

// OwnsHost implements parsers.HostOwner.
func (RPC) OwnsHost(h string) bool { return h == host }

// Version implements parsers.Parser: the date the web app's request was captured.
func (RPC) Version() string { return "captured-2026-10-12" }

// BlockResponse implements parsers.Parser: a Connect error, which a Connect client raises with its
// message whatever codec the call used.
func (RPC) BlockResponse(message, link string) (int, string, []byte) {
	body, _ := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{"permission_denied", parsers.BlockText(message, link)})
	return http.StatusForbidden, "application/json", body
}

// Parse implements parsers.Parser. The body is a unary call's protobuf message.
func (RPC) Parse(body []byte, mediaType string) (parsers.Result, error) {
	if mt, _, err := mime.ParseMediaType(mediaType); err != nil || mt != "application/proto" {
		return parsers.Result{}, parsers.UnknownShape(name, "the call is not a unary protobuf call")
	}
	action, err := field(body, actionField)
	switch {
	case err != nil:
		return parsers.Result{}, parsers.UnknownShape(name, "the body does not decode as a protobuf message")
	case action == nil:
		return parsers.Result{}, parsers.ErrNotPrompt
	}
	text, err := field(action, textField)
	switch {
	case err != nil:
		return parsers.Result{}, parsers.UnknownShape(name, "the action does not decode as a protobuf message")
	case !utf8.Valid(text):
		return parsers.Result{}, parsers.UnknownShape(name, "the message's text is not UTF-8")
	case strings.TrimSpace(string(text)) == "":
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Text: string(text), Shape: "perform_action"}, nil
}

// field is the value of the first length-delimited field num of a protobuf message, nil when the
// message has none.
func field(msg []byte, num protowire.Number) ([]byte, error) {
	for len(msg) > 0 {
		n, typ, l := protowire.ConsumeTag(msg)
		if l < 0 {
			return nil, protowire.ParseError(l)
		}
		msg = msg[l:]
		if n == num && typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(msg)
			if l < 0 {
				return nil, protowire.ParseError(l)
			}
			return v, nil
		}
		l = protowire.ConsumeFieldValue(n, typ, msg)
		if l < 0 {
			return nil, protowire.ParseError(l)
		}
		msg = msg[l:]
	}
	return nil, nil
}

// Completion reads completion requests.
type Completion struct{}

// Match implements parsers.Parser.
func (Completion) Match(h, p string) bool { return h == host && completion.MatchString(p) }

// OwnsHost implements parsers.HostOwner.
func (Completion) OwnsHost(h string) bool { return h == host }

// Version implements parsers.Parser: the date the web app's request was read.
func (Completion) Version() string { return "documented-2026-10-11" }

// BlockResponse implements parsers.Parser: the error shape of Anthropic's APIs.
func (Completion) BlockResponse(message, link string) (int, string, []byte) {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	body, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Error detail `json:"error"`
	}{"error", detail{"permission_error", parsers.BlockText(message, link)}})
	return http.StatusForbidden, "application/json", body
}

// Parse implements parsers.Parser.
func (Completion) Parse(body []byte, _ string) (parsers.Result, error) {
	var req struct {
		Prompt *string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "the body does not decode as a completion request")
	}
	switch {
	case req.Prompt == nil:
		return parsers.Result{}, parsers.UnknownShape(name, "no prompt")
	case strings.TrimSpace(*req.Prompt) == "":
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Text: *req.Prompt, Shape: "completion"}, nil
}
