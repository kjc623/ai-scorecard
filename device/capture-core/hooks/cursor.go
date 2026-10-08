package hooks

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/shadow-ai-capture/device/protocol"
)

// CursorTool is Cursor's tool key, the one its hook command names.
const CursorTool = "cursor"

// The Cursor hook events the agent declares.
const (
	CursorBeforeSubmitPrompt = "beforeSubmitPrompt"
	CursorBeforeMCPExecution = "beforeMCPExecution"
)

// cursor reads Cursor's hook input and answers in its JSON output, always with exit code 0.
// beforeSubmitPrompt answers with continue, beforeMCPExecution with permission; user_message is
// what Cursor shows the user.
type cursor struct{}

// cursorInput is the part of a hook's stdin the agent reads.
type cursorInput struct {
	ConversationID string          `json:"conversation_id"`
	Prompt         *string         `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

func (cursor) Parse(event string, stdin []byte) (protocol.HookEvaluate, error) {
	var in cursorInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return protocol.HookEvaluate{}, errors.New("cursor hook: the input is not a hook event")
	}
	if in.ConversationID == "" {
		return protocol.HookEvaluate{}, errors.New("cursor hook: the input carries no conversation id")
	}
	ev := protocol.HookEvaluate{Tool: CursorTool, Event: event, SessionID: in.ConversationID}
	switch event {
	case CursorBeforeSubmitPrompt:
		if in.Prompt == nil {
			return protocol.HookEvaluate{}, errors.New("cursor hook: beforeSubmitPrompt carries no prompt")
		}
		ev.PromptText = *in.Prompt
	case CursorBeforeMCPExecution:
		if in.ToolName == "" || len(in.ToolInput) == 0 {
			return protocol.HookEvaluate{}, errors.New("cursor hook: beforeMCPExecution carries no tool call")
		}
		text, err := cursorToolInput(in.ToolInput)
		if err != nil {
			return protocol.HookEvaluate{}, err
		}
		ev.ToolName = in.ToolName
		ev.PromptText = text
	default:
		return protocol.HookEvaluate{}, errors.New("cursor hook: the event is not one the agent declares")
	}
	ev.PromptBytes = int64(len(ev.PromptText))
	return ev, nil
}

// cursorToolInput is an MCP call's arguments as compact JSON. Cursor hands them over as a string
// holding the JSON; an object is taken as it is. A string that is not JSON is sent as written.
func cursorToolInput(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(s)) != nil {
			return s, nil
		}
		return compact.String(), nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", errors.New("cursor hook: the tool input is not JSON")
	}
	return compact.String(), nil
}

// CanEnforce reports whether Cursor stops the action when the event's hook blocks it: a decision
// the tool does not carry out is recorded as logged, not blocked.
func (cursor) CanEnforce(event string) bool {
	switch event {
	case CursorBeforeSubmitPrompt, CursorBeforeMCPExecution:
		return true
	default:
		return false
	}
}

// cursorPromptOutput answers beforeSubmitPrompt: continue false stops the prompt.
type cursorPromptOutput struct {
	Continue    bool   `json:"continue"`
	UserMessage string `json:"user_message,omitempty"`
}

// cursorPermissionOutput answers beforeMCPExecution: permission deny stops the tool call, and
// agent_message tells the agent why.
type cursorPermissionOutput struct {
	Permission   string `json:"permission"`
	UserMessage  string `json:"user_message,omitempty"`
	AgentMessage string `json:"agent_message,omitempty"`
}

func (cursor) Render(event string, d protocol.HookDecision) ([]byte, int) {
	block := d.Action == protocol.HookBlock
	var text string
	if d.Action == protocol.HookBlock || d.Action == protocol.HookWarn {
		text = userText(d)
	}
	var out any
	switch event {
	case CursorBeforeSubmitPrompt:
		out = cursorPromptOutput{Continue: !block, UserMessage: text}
	case CursorBeforeMCPExecution:
		o := cursorPermissionOutput{Permission: "allow", UserMessage: text}
		if block {
			o.Permission, o.AgentMessage = "deny", text
		}
		out = o
	default:
		// No output lets Cursor go ahead as if no hook ran.
		return nil, 0
	}
	b, err := marshalJSON(out)
	if err != nil {
		return nil, 0
	}
	return b, 0
}

// Allow is the event's go-ahead: continue true, or permission allow.
func (a cursor) Allow(event string) ([]byte, int) {
	return a.Render(event, protocol.HookDecision{Action: protocol.HookAllow})
}
