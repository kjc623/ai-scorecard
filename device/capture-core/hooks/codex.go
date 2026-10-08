package hooks

import (
	"encoding/json"
	"errors"

	"github.com/shadow-ai-capture/device/protocol"
)

// CodexTool is the Codex CLI's tool key, the one its managed hook command names.
const CodexTool = "codex"

// CodexUserPromptSubmit is the Codex hook event the agent declares: it runs before a submitted
// prompt reaches the model.
const CodexUserPromptSubmit = "UserPromptSubmit"

// codex reads Codex's command-hook input and answers in its JSON output, always with exit code 0.
// Empty output lets the prompt proceed; plain text would be added to the model's context.
type codex struct{}

// codexInput is the part of a hook's stdin the agent reads.
type codexInput struct {
	SessionID string  `json:"session_id"`
	Cwd       string  `json:"cwd"`
	Prompt    *string `json:"prompt"`
}

func (codex) Parse(event string, stdin []byte) (protocol.HookEvaluate, error) {
	if event != CodexUserPromptSubmit {
		return protocol.HookEvaluate{}, errors.New("codex hook: the event is not one the agent declares")
	}
	var in codexInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return protocol.HookEvaluate{}, errors.New("codex hook: the input is not a hook event")
	}
	if in.Prompt == nil {
		return protocol.HookEvaluate{}, errors.New("codex hook: UserPromptSubmit carries no prompt")
	}
	return protocol.HookEvaluate{
		Tool:        CodexTool,
		Event:       event,
		SessionID:   in.SessionID,
		Cwd:         in.Cwd,
		PromptText:  *in.Prompt,
		PromptBytes: int64(len(*in.Prompt)),
	}, nil
}

// codexOutput is Codex's UserPromptSubmit output. Decision "block" stops the turn and Codex shows
// Reason to the user; SystemMessage is shown as a warning.
type codexOutput struct {
	Decision      string `json:"decision,omitempty"`
	Reason        string `json:"reason,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
}

func (a codex) Render(event string, d protocol.HookDecision) ([]byte, int) {
	if event != CodexUserPromptSubmit {
		return a.Allow(event)
	}
	var out codexOutput
	switch d.Action {
	case protocol.HookBlock:
		// Codex ignores a block whose reason is empty, so a rule without a message is named by its id.
		out.Decision, out.Reason = "block", userText(d)
		if out.Reason == "" {
			out.Reason = d.RuleID
		}
	case protocol.HookWarn:
		out.SystemMessage = userText(d)
	}
	if out.Reason == "" && out.SystemMessage == "" {
		return a.Allow(event)
	}
	b, err := marshalJSON(out)
	if err != nil {
		return a.Allow(event)
	}
	return b, 0
}

// Allow is empty output with exit code 0: the prompt proceeds as if no hook ran.
func (codex) Allow(string) ([]byte, int) { return nil, 0 }

// CanEnforce is true for UserPromptSubmit: Codex stops the turn a block answers.
func (codex) CanEnforce(event string) bool { return event == CodexUserPromptSubmit }
