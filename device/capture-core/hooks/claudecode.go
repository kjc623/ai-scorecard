package hooks

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/shadow-ai-capture/device/protocol"
)

// ClaudeCodeTool is Claude Code's tool key, the one its managed hook command names.
const ClaudeCodeTool = "claude_code"

// The Claude Code hook events the agent declares.
const (
	ClaudeCodeUserPromptSubmit = "UserPromptSubmit"
	ClaudeCodePreToolUse       = "PreToolUse"
)

// claudeCode reads Claude Code's command-hook input and answers in its JSON output, always with
// exit code 0: a parsed JSON object decides, and empty output lets the action proceed.
type claudeCode struct{}

// claudeCodeInput is the part of a hook's stdin the agent reads.
type claudeCodeInput struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Prompt    *string         `json:"prompt"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

func (claudeCode) Parse(event string, stdin []byte) (protocol.HookEvaluate, error) {
	var in claudeCodeInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return protocol.HookEvaluate{}, errors.New("claude code hook: the input is not a hook event")
	}
	ev := protocol.HookEvaluate{Tool: ClaudeCodeTool, Event: event, SessionID: in.SessionID, Cwd: in.Cwd}
	switch event {
	case ClaudeCodeUserPromptSubmit:
		if in.Prompt == nil {
			return protocol.HookEvaluate{}, errors.New("claude code hook: UserPromptSubmit carries no prompt")
		}
		ev.PromptText = *in.Prompt
	case ClaudeCodePreToolUse:
		if in.ToolName == "" || len(in.ToolInput) == 0 {
			return protocol.HookEvaluate{}, errors.New("claude code hook: PreToolUse carries no tool call")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, in.ToolInput); err != nil {
			return protocol.HookEvaluate{}, errors.New("claude code hook: the tool input is not JSON")
		}
		ev.ToolName = in.ToolName
		ev.PromptText = compact.String()
	default:
		return protocol.HookEvaluate{}, errors.New("claude code hook: the event is not one the agent declares")
	}
	ev.PromptBytes = int64(len(ev.PromptText))
	return ev, nil
}

// claudeCodeOutput is Claude Code's hook JSON output. SystemMessage is shown to the user; a
// UserPromptSubmit block is Decision "block" with Reason shown to the user; a PreToolUse deny is
// in HookSpecificOutput, whose reason Claude reads.
type claudeCodeOutput struct {
	Decision           string                    `json:"decision,omitempty"`
	Reason             string                    `json:"reason,omitempty"`
	SystemMessage      string                    `json:"systemMessage,omitempty"`
	HookSpecificOutput *claudeCodeSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type claudeCodeSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// SuppressOriginalPrompt keeps a blocked prompt's text out of the block message Claude Code
	// shows and writes to the session transcript.
	SuppressOriginalPrompt bool `json:"suppressOriginalPrompt,omitempty"`
}

func (a claudeCode) Render(event string, d protocol.HookDecision) ([]byte, int) {
	text := userText(d)
	var out claudeCodeOutput
	switch {
	case d.Action == protocol.HookWarn:
		out.SystemMessage = text
	case d.Action == protocol.HookBlock && event == ClaudeCodeUserPromptSubmit:
		out.Decision = "block"
		out.Reason = text
		out.HookSpecificOutput = &claudeCodeSpecificOutput{HookEventName: event, SuppressOriginalPrompt: true}
	case d.Action == protocol.HookBlock && event == ClaudeCodePreToolUse:
		out.SystemMessage = text
		out.HookSpecificOutput = &claudeCodeSpecificOutput{HookEventName: event, PermissionDecision: "deny", PermissionDecisionReason: text}
	default:
		return a.Allow(event)
	}
	if out.SystemMessage == "" && out.Decision == "" && out.HookSpecificOutput == nil {
		return a.Allow(event)
	}
	b, err := marshalJSON(out)
	if err != nil {
		return a.Allow(event)
	}
	return b, 0
}

// Allow is empty output with exit code 0: the prompt or tool call proceeds as if no hook ran.
func (claudeCode) Allow(string) ([]byte, int) { return nil, 0 }

// CanEnforce is true: Claude Code stops the prompt or the tool call a block or deny answers.
func (claudeCode) CanEnforce(string) bool { return true }
