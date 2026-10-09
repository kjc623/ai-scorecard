package parsers

import (
	"encoding/json"
	"net/http"
)

// Generic reads a JSON body of no known target: a top-level `prompt`, else the last user-role
// message of `messages[]`, else `input`. A body it cannot interpret returns ErrNoText so the
// observation degrades instead of guessing which characters the user authored.
type Generic struct{}

// Match implements Parser: the generic parser reads any destination.
func (Generic) Match(string, string) bool { return true }

// Version implements Parser.
func (Generic) Version() string { return "1" }

// BlockResponse implements Parser: plain text, because a destination of no known target has no
// error shape its client is known to display.
func (Generic) BlockResponse(message, link string) (int, string, []byte) {
	return http.StatusForbidden, "text/plain; charset=utf-8", []byte(BlockText(message, link))
}

// Parse implements Parser.
func (Generic) Parse(payload []byte, _ string) (Result, error) {
	if len(payload) == 0 {
		return Result{}, ErrNoText
	}
	var body struct {
		Prompt   string `json:"prompt"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Result{}, ErrNoText
	}
	if body.Prompt != "" {
		return Result{Text: body.Prompt, Shape: "prompt"}, nil
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role != "user" {
			continue
		}
		if text, ok := decodeContent(body.Messages[i].Content); ok {
			return Result{Text: text, Shape: "messages"}, nil
		}
	}
	if len(body.Input) > 0 {
		if text, ok := decodeContent(body.Input); ok {
			return Result{Text: text, Shape: "input"}, nil
		}
	}
	return Result{}, ErrNoText
}

// decodeContent accepts both the string form and the content-part array form, because both are
// in the wild and both address the same authored text.
func decodeContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out string
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" {
				if out != "" {
					out += " "
				}
				out += p.Text
			}
		}
		if out != "" {
			return out, true
		}
	}
	return "", false
}
