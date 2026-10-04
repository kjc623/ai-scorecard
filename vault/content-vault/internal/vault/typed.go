package vault

import (
	"encoding/json"
	"regexp"
	"strings"
)

// What a device captures for one request is more than what a person typed. A client wraps the
// typed text in context of its own (Claude Code prepends <system-reminder> blocks to the user
// message), resends the whole conversation on every turn, and makes requests no person wrote at
// all (telemetry batches). The search index is for finding what a person typed, so that is what
// is indexed: a search does not match a system prompt, and a snippet is prompt text.

var injectedBlock = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

func stripInjected(text string) string {
	return strings.TrimSpace(injectedBlock.ReplaceAllString(text, ""))
}

// messageText is a message's text: a string, or the text blocks of a block list. Tool results and
// images are not typed text.
func messageText(content json.RawMessage) string {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return plain
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// typedText returns what the person typed in a stored capture, or "" when nobody typed anything.
//
// The content is either the prompt text the device extracted, or the request body as observed
// when it could not extract one. In a body, the latest user message that carries text is this
// turn's input: earlier ones are the conversation being resent.
func typedText(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return stripInjected(trimmed)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &body) != nil {
		return stripInjected(trimmed)
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if raw, ok := body["messages"]; ok {
		_ = json.Unmarshal(raw, &messages)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if typed := stripInjected(messageText(messages[i].Content)); typed != "" {
			return typed
		}
	}
	return ""
}
