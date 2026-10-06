package vault

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What the search index holds for a stored content object.
//
// A content object is the bytes the device captured for one request: the prompt text where the
// device could identify what the person typed, the request body as observed where it could not,
// or a JSON object with a "prompt" string and an "attachments" list whose entries carry a "name".
//
// Search is for finding what a person typed, so the index holds the typed text (prompt_body) and
// the attachment names (attachment_name). A client wraps typed text in context of its own (Claude
// Code prepends <system-reminder> blocks), resends the whole conversation on every turn, and makes
// requests nobody typed (telemetry). In a request body, the latest user message that carries text
// is this turn's input; injected blocks are removed; a body with no user text yields nothing.

const (
	// maxIndexedRunes is the longest body ingest.search_text admits.
	maxIndexedRunes = 65_536
	// maxAttachmentNames bounds the index rows one object can add.
	maxAttachmentNames = 256
)

var injectedBlock = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// indexUnits returns the typed prompt text and the attachment names of a content object.
func indexUnits(content []byte) (prompt string, names []string) {
	text := strings.TrimSpace(string(content))
	if strings.HasPrefix(text, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &obj) == nil {
			names = attachmentNames(obj["attachments"])
			var p string
			if raw, ok := obj["prompt"]; ok && json.Unmarshal(raw, &p) == nil {
				return indexable(typedText(p)), names
			}
			return indexable(typedFromBody(obj)), names
		}
	}
	return indexable(typedText(text)), nil
}

// typedText returns what the person typed in captured text: the text itself, or for a request body
// the latest user message that carries text.
func typedText(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &obj) == nil {
			return typedFromBody(obj)
		}
	}
	return stripInjected(text)
}

func typedFromBody(body map[string]json.RawMessage) string {
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if raw, ok := body["messages"]; !ok || json.Unmarshal(raw, &messages) != nil {
		return ""
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
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func stripInjected(text string) string {
	return strings.TrimSpace(injectedBlock.ReplaceAllString(text, ""))
}

func attachmentNames(raw json.RawMessage) []string {
	var list []struct {
		Name string `json:"name"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var names []string
	for _, a := range list {
		if name := indexable(a.Name); name != "" && len(names) < maxAttachmentNames {
			names = append(names, name)
		}
	}
	return names
}

// indexable makes text storable as a search body: valid UTF-8 without NUL, trimmed, and no longer
// than the column admits.
func indexable(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", " "))
	if utf8.RuneCountInString(s) <= maxIndexedRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxIndexedRunes {
			return s[:i]
		}
		n++
	}
	return s
}
