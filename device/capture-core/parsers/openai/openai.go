// Package openai parses requests to the OpenAI Chat Completions API (POST
// api.openai.com/v1/chat/completions) and Responses API (POST api.openai.com/v1/responses).
//
// The text read is the request's turn: the trailing input, after any trailing assistant message.
// In Chat Completions that is the trailing user, tool and function messages; in Responses, the
// trailing user messages and tool call outputs, or `input` itself when it is a string. Earlier
// input was sent, and recorded, with the requests that first carried it.
package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const (
	host           = "api.openai.com"
	chatPath       = "/v1/chat/completions"
	responsesPath  = "/v1/responses"
	name           = "openai"
	shapeChat      = "chat_completions"
	shapeResponses = "responses"
	version        = "documented-2026-10-08"
)

// Parser is the Chat Completions and Responses parser.
type Parser struct{}

// Match implements parsers.Parser.
func (Parser) Match(h, p string) bool { return h == host && (p == chatPath || p == responsesPath) }

// Version implements parsers.Parser: the date of the API reference the known shapes come from.
func (Parser) Version() string { return version }

// BlockResponse implements parsers.Parser: the API's error shape, whose param and code are
// required and null here.
func (Parser) BlockResponse(message, link string) (int, string, []byte) {
	type detail struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	}
	body, _ := json.Marshal(struct {
		Error detail `json:"error"`
	}{detail{Message: parsers.BlockText(message, link), Type: "policy_violation"}})
	return http.StatusForbidden, "application/json", body
}

// Parse implements parsers.Parser. The two APIs' bodies are told apart by their fields, not by
// the path, which the registry has already matched.
func (Parser) Parse(body []byte, _ string) (parsers.Result, error) {
	var req struct {
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "the body is not a JSON object")
	}
	switch {
	case len(req.Messages) > 0 && len(req.Input) == 0:
		return parseChat(req.Messages)
	case len(req.Input) > 0 && len(req.Messages) == 0:
		return parseResponses(req.Input)
	default:
		return parsers.Result{}, parsers.UnknownShape(name, "the body has neither messages nor input")
	}
}

type chatMessage struct {
	Role    *string         `json:"role"`
	Content json.RawMessage `json:"content"`
}

type part struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

func parseChat(raw json.RawMessage) (parsers.Result, error) {
	var msgs []chatMessage
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
		return parsers.Result{}, parsers.UnknownShape(name, "messages is not a non-empty array of messages")
	}
	for i, m := range msgs {
		if m.Role == nil {
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("message %d has no role", i))
		}
		switch *m.Role {
		case "developer", "system", "assistant":
		case "user", "tool", "function":
			if len(m.Content) == 0 {
				return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("message %d has no content", i))
			}
		default:
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("message %d has an undocumented role", i))
		}
	}

	end := len(msgs)
	for end > 0 && *msgs[end-1].Role == "assistant" {
		end--
	}
	start := end
	for start > 0 && isChatInput(*msgs[start-1].Role) {
		start--
	}
	if start == end {
		return parsers.Result{}, parsers.UnknownShape(name, "no user or tool message ends the conversation")
	}

	var texts []string
	for i := start; i < end; i++ {
		at := fmt.Sprintf("message %d", i)
		var got []string
		var err error
		switch *msgs[i].Role {
		case "user":
			got, err = contentText(msgs[i].Content, at, "text", userPartsWithoutText)
		case "tool":
			got, err = contentText(msgs[i].Content, at, "text", nil)
		case "function":
			got, err = nullableString(msgs[i].Content, at)
		}
		if err != nil {
			return parsers.Result{}, err
		}
		texts = append(texts, got...)
	}
	return result(texts, shapeChat)
}

func isChatInput(role string) bool { return role == "user" || role == "tool" || role == "function" }

// userPartsWithoutText are the documented Chat Completions user content parts besides text.
var userPartsWithoutText = map[string]bool{"image_url": true, "input_audio": true, "file": true}

// inputPartsWithoutText are the documented Responses input content parts besides input_text.
var inputPartsWithoutText = map[string]bool{"input_image": true, "input_file": true}

// contentText reads content that is a string or an array of parts: textType parts (`text` in
// Chat Completions, `input_text` in Responses) and the documented parts without text in others.
func contentText(raw json.RawMessage, at, textType string, others map[string]bool) ([]string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, parsers.UnknownShape(name, at+": content is neither a string nor an array of parts")
	}
	var texts []string
	for j, p := range parts {
		switch {
		case p.Type == textType:
			if p.Text == nil {
				return nil, parsers.UnknownShape(name, fmt.Sprintf("%s part %d: a text part has no text", at, j))
			}
			texts = append(texts, *p.Text)
		case others[p.Type]:
		default:
			return nil, parsers.UnknownShape(name, fmt.Sprintf("%s part %d has an undocumented type", at, j))
		}
	}
	return texts, nil
}

func nullableString(raw json.RawMessage, at string) ([]string, error) {
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, parsers.UnknownShape(name, at+": content is not a string")
	}
	if s == nil {
		return nil, nil
	}
	return []string{*s}, nil
}

type item struct {
	Type    *string         `json:"type"`
	Role    *string         `json:"role"`
	ID      *string         `json:"id"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
}

// Responses input item kinds.
const (
	kindUserMessage = iota
	kindToolOutput  // carries text in `output`
	kindInputOther  // input to the model that carries no text the device reads
	kindStop        // ends the turn: model output, instructions, references
)

// inputItemsWithoutText are the documented input item types, besides the text-bearing tool
// outputs, that the client sends as the result of a model's call.
var inputItemsWithoutText = map[string]bool{
	"computer_call_output": true, "local_shell_call_output": true, "shell_call_output": true,
	"apply_patch_call_output": true, "tool_search_output": true, "mcp_approval_response": true,
	"program_output": true,
}

// stopItems are the documented input item types that are not part of a turn's input: model
// output passed back, configuration and references.
var stopItems = map[string]bool{
	"file_search_call": true, "computer_call": true, "web_search_call": true, "function_call": true,
	"tool_search_call": true, "additional_tools": true, "configuration_update": true,
	"reasoning": true, "compaction": true, "image_generation_call": true,
	"code_interpreter_call": true, "local_shell_call": true, "shell_call": true,
	"apply_patch_call": true, "mcp_list_tools": true, "mcp_approval_request": true,
	"mcp_call": true, "custom_tool_call": true, "item_reference": true,
	"compaction_trigger": true, "program": true,
}

func parseResponses(raw json.RawMessage) (parsers.Result, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return result([]string{s}, shapeResponses)
	}
	var items []item
	if err := json.Unmarshal(raw, &items); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "input is neither a string nor an array of items")
	}
	kinds := make([]int, len(items))
	for i, it := range items {
		k, err := itemKind(it, fmt.Sprintf("input item %d", i))
		if err != nil {
			return parsers.Result{}, err
		}
		kinds[i] = k
	}

	end := len(items)
	for end > 0 && kinds[end-1] == kindStop && items[end-1].Role != nil && *items[end-1].Role == "assistant" {
		end--
	}
	start := end
	for start > 0 && kinds[start-1] != kindStop {
		start--
	}
	if start == end {
		return parsers.Result{}, parsers.UnknownShape(name, "no user message or tool output ends the input")
	}

	var texts []string
	for i := start; i < end; i++ {
		at := fmt.Sprintf("input item %d", i)
		var got []string
		var err error
		switch kinds[i] {
		case kindUserMessage:
			got, err = contentText(items[i].Content, at, "input_text", inputPartsWithoutText)
		case kindToolOutput:
			got, err = contentText(items[i].Output, at, "input_text", inputPartsWithoutText)
		}
		if err != nil {
			return parsers.Result{}, err
		}
		texts = append(texts, got...)
	}
	return result(texts, shapeResponses)
}

// itemKind classifies one Responses input item. A message may omit `type`; an item reference
// may omit it too and then has only an `id`.
func itemKind(it item, at string) (int, error) {
	typ := ""
	if it.Type != nil {
		typ = *it.Type
	}
	switch {
	case typ == "message" || (typ == "" && it.Role != nil):
		if it.Role == nil || len(it.Content) == 0 {
			return 0, parsers.UnknownShape(name, at+": a message has no role or content")
		}
		switch *it.Role {
		case "user":
			return kindUserMessage, nil
		case "assistant", "system", "developer":
			return kindStop, nil
		default:
			return 0, parsers.UnknownShape(name, at+": a message has an undocumented role")
		}
	case typ == "" && it.ID != nil:
		return kindStop, nil
	case typ == "function_call_output" || typ == "custom_tool_call_output":
		if len(it.Output) == 0 {
			return 0, parsers.UnknownShape(name, at+": a tool call output has no output")
		}
		return kindToolOutput, nil
	case inputItemsWithoutText[typ]:
		return kindInputOther, nil
	case stopItems[typ]:
		return kindStop, nil
	default:
		return 0, parsers.UnknownShape(name, at+" has an undocumented type")
	}
}

func result(texts []string, shape string) (parsers.Result, error) {
	if len(texts) == 0 {
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Text: strings.Join(texts, "\n"), Shape: shape}, nil
}
