// Package anthropic parses requests to the Anthropic Messages API (POST
// api.anthropic.com/v1/messages), including its beta form.
//
// The text read is the request's turn: the trailing user messages, after any trailing assistant
// message that prefills the reply. Earlier user messages were sent, and recorded, with the
// requests that first carried them. Within the turn it reads text blocks, the text of tool_result
// and mcp_tool_result blocks, plain-text and content documents, and search results.
package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const (
	host = "api.anthropic.com"
	path = "/v1/messages"
	name = "anthropic"
)

// Parser is the Messages API parser.
type Parser struct{}

// Match implements parsers.Parser.
func (Parser) Match(h, p string) bool { return h == host && p == path }

// Version implements parsers.Parser: the date of the API reference the known shapes come from.
func (Parser) Version() string { return "documented-2026-10-08" }

type message struct {
	Role    *string         `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type    string          `json:"type"`
	Text    *string         `json:"text"`
	Content json.RawMessage `json:"content"`
	Source  json.RawMessage `json:"source"`
}

// blocksWithoutText are the documented content block types that carry no text a person or a
// tool sent: media, model output passed back, and server tool results.
var blocksWithoutText = map[string]bool{
	"image": true, "thinking": true, "redacted_thinking": true, "tool_use": true,
	"server_tool_use": true, "web_search_tool_result": true, "web_fetch_tool_result": true,
	"code_execution_tool_result": true, "bash_code_execution_tool_result": true,
	"text_editor_code_execution_tool_result": true, "tool_search_tool_result": true,
	"container_upload": true,
	// Beta.
	"advisor_tool_result": true, "mcp_tool_use": true, "compaction": true, "tool_addition": true,
	"tool_removal": true, "mcp_tool_listing": true, "fallback": true,
}

// BlockResponse implements parsers.Parser: the API's error shape, with the type it answers a
// request it does not permit with.
func (Parser) BlockResponse(message, link string) (int, string, []byte) {
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
func (Parser) Parse(body []byte, _ string) (parsers.Result, error) {
	var req struct {
		Messages *[]message `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "the body does not decode as a Messages request")
	}
	if req.Messages == nil || len(*req.Messages) == 0 {
		return parsers.Result{}, parsers.UnknownShape(name, "no messages")
	}
	msgs := *req.Messages
	for i, m := range msgs {
		if m.Role == nil || len(m.Content) == 0 {
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("message %d has no role or content", i))
		}
		switch *m.Role {
		case "user", "assistant", "system":
		default:
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("message %d has an undocumented role", i))
		}
	}

	end := len(msgs)
	for end > 0 && *msgs[end-1].Role == "assistant" {
		end--
	}
	start := end
	for start > 0 && *msgs[start-1].Role == "user" {
		start--
	}
	if start == end {
		return parsers.Result{}, parsers.UnknownShape(name, "no user message ends the conversation")
	}

	var texts []string
	for i := start; i < end; i++ {
		got, err := contentText(msgs[i].Content, fmt.Sprintf("message %d", i))
		if err != nil {
			return parsers.Result{}, err
		}
		texts = append(texts, got...)
	}
	if len(texts) == 0 {
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Text: strings.Join(texts, "\n"), Shape: "messages"}, nil
}

// contentText reads a message's content: a string, or an array of content blocks.
func contentText(raw json.RawMessage, at string) ([]string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, parsers.UnknownShape(name, at+": content is neither a string nor an array of blocks")
	}
	var texts []string
	for j, b := range blocks {
		at := fmt.Sprintf("%s block %d", at, j)
		switch {
		case b.Type == "text":
			if b.Text == nil {
				return nil, parsers.UnknownShape(name, at+": a text block has no text")
			}
			texts = append(texts, *b.Text)
		case b.Type == "tool_result" || b.Type == "mcp_tool_result":
			got, err := toolResultText(b.Content, at)
			if err != nil {
				return nil, err
			}
			texts = append(texts, got...)
		case b.Type == "document":
			got, err := documentText(b.Source, at)
			if err != nil {
				return nil, err
			}
			texts = append(texts, got...)
		case b.Type == "search_result":
			got, err := textBlocks(b.Content, at)
			if err != nil {
				return nil, err
			}
			texts = append(texts, got...)
		case blocksWithoutText[b.Type]:
		default:
			return nil, parsers.UnknownShape(name, at+" has an undocumented type")
		}
	}
	return texts, nil
}

// toolResultText reads a tool result's content: absent, a string, or an array of blocks.
func toolResultText(raw json.RawMessage, at string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, parsers.UnknownShape(name, at+": tool result content is neither a string nor an array of blocks")
	}
	var texts []string
	for k, b := range blocks {
		at := fmt.Sprintf("%s item %d", at, k)
		switch b.Type {
		case "text":
			if b.Text == nil {
				return nil, parsers.UnknownShape(name, at+": a text block has no text")
			}
			texts = append(texts, *b.Text)
		case "document":
			got, err := documentText(b.Source, at)
			if err != nil {
				return nil, err
			}
			texts = append(texts, got...)
		case "search_result":
			got, err := textBlocks(b.Content, at)
			if err != nil {
				return nil, err
			}
			texts = append(texts, got...)
		case "image", "tool_reference", "browser_state":
		default:
			return nil, parsers.UnknownShape(name, at+" has an undocumented type")
		}
	}
	return texts, nil
}

// documentText reads a document's source. A PDF (base64, URL or uploaded file) carries no text
// the device reads.
func documentText(raw json.RawMessage, at string) ([]string, error) {
	var src struct {
		Type    string          `json:"type"`
		Data    *string         `json:"data"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &src); err != nil || len(raw) == 0 {
		return nil, parsers.UnknownShape(name, at+": a document has no source")
	}
	switch src.Type {
	case "text":
		if src.Data == nil {
			return nil, parsers.UnknownShape(name, at+": a text document has no data")
		}
		return []string{*src.Data}, nil
	case "content":
		var s string
		if json.Unmarshal(src.Content, &s) == nil {
			return []string{s}, nil
		}
		var blocks []block
		if err := json.Unmarshal(src.Content, &blocks); err != nil {
			return nil, parsers.UnknownShape(name, at+": a content document has no content")
		}
		var texts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text == nil {
					return nil, parsers.UnknownShape(name, at+": a text block has no text")
				}
				texts = append(texts, *b.Text)
			case "image":
			default:
				return nil, parsers.UnknownShape(name, at+": a content document holds an undocumented type")
			}
		}
		return texts, nil
	case "base64", "url", "file":
		return nil, nil
	default:
		return nil, parsers.UnknownShape(name, at+": a document source has an undocumented type")
	}
}

// textBlocks reads an array of text blocks (a search result's content).
func textBlocks(raw json.RawMessage, at string) ([]string, error) {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, parsers.UnknownShape(name, at+": content is not an array of text blocks")
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" || b.Text == nil {
			return nil, parsers.UnknownShape(name, at+": content holds something other than text blocks")
		}
		texts = append(texts, *b.Text)
	}
	return texts, nil
}
