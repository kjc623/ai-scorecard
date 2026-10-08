package targets

import (
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
	"github.com/shadow-ai-capture/device/capture-core/parsers/anthropic"
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
		// Claude Desktop's and ChatGPT Desktop's backends have no parser yet.
		{"claude.ai", "/api/organizations/o/chat_conversations/c/completion", parsers.Generic{}},
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
