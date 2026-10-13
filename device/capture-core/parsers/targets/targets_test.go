package targets

import (
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
	"github.com/shadow-ai-capture/device/capture-core/parsers/anthropic"
	"github.com/shadow-ai-capture/device/capture-core/parsers/claudeai"
	"github.com/shadow-ai-capture/device/capture-core/parsers/gemini"
	"github.com/shadow-ai-capture/device/capture-core/parsers/openai"
)

func TestRegistryChoosesByHostAndPath(t *testing.T) {
	cases := []struct {
		host, path string
		want       parsers.Parser
	}{
		{"api.anthropic.com", "/v1/messages", anthropic.Parser{}},
		{"api.anthropic.com:443", "/v1/messages", anthropic.Parser{}},
		{"api.openai.com", "/v1/chat/completions", openai.Parser{}},
		{"api.openai.com", "/v1/responses", openai.Parser{}},
		{"generativelanguage.googleapis.com", "/v1beta/models/gemini-flash-latest:streamGenerateContent", gemini.Parser{}},
		{"generativelanguage.googleapis.com", "/upload/v1beta/files", gemini.Upload{}},
		{"cloudcode-pa.googleapis.com", "/v1internal:streamGenerateContent", gemini.CodeAssist{}},
		{"cloudcode-pa.googleapis.com", "/v1internal:loadCodeAssist", parsers.Generic{}},
		{"claude.ai", "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction", claudeai.RPC{}},
		{"claude.ai", "/api/organizations/o/chat_conversations/c/completion", claudeai.Completion{}},
		// ChatGPT Desktop's backend has no parser yet.
		{"chatgpt.com", "/backend-api/conversation", parsers.Generic{}},
		{"api.anthropic.com", "/v1/messages/count_tokens", parsers.Generic{}},
		{"127.0.0.1:11434", "/api/chat", parsers.Generic{}},
	}
	for _, c := range cases {
		if got := Registry().Lookup(c.host, c.path); got != c.want {
			t.Errorf("Lookup(%q, %q) = %T, want %T", c.host, c.path, got, c.want)
		}
	}
}

// A request to claude.ai on any path but its two prompt methods carries no prompt; a host no parser
// owns may carry one on any path.
func TestRegistryKnowsWhichRequestsCarryNoPrompt(t *testing.T) {
	for _, c := range []struct {
		host, path string
		want       bool
	}{
		{"claude.ai", "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction", true},
		{"claude.ai:443", "/api/organizations/o/chat_conversations/c/completion", true},
		{"claude.ai", "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/StreamTimeline", false},
		{"Claude.AI", "/api/v2/rum", false},
		{"chatgpt.com", "/backend-api/sentinel/chat-requirements", true},
		{"api.openai.com", "/v1/files", true},
		{"127.0.0.1:11434", "/api/chat", true},
	} {
		if got := Registry().MayCarryPrompt(c.host, c.path); got != c.want {
			t.Errorf("MayCarryPrompt(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}

	rpc := "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction"
	other := []byte{0x0a, 0x01, 'x', 0x7a, 0x01, 'y'} // fields 1 and 15: an action that is no message
	if !Registry().NotPrompt("claude.ai", rpc, other, "application/proto") {
		t.Error("another PerformAction action is read as a prompt")
	}
	msg := []byte{0x12, 0x04, 0x1a, 0x02, 'h', 'i'} // field 2 holding field 3, "hi"
	if Registry().NotPrompt("claude.ai", rpc, msg, "application/proto") {
		t.Error("a message is read as no prompt")
	}
	if Registry().NotPrompt("api.openai.com", "/v1/chat/completions", []byte(`{}`), "application/json") {
		t.Error("an unknown shape is read as no prompt")
	}
}
