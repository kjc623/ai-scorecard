// Package gemini parses requests to the Gemini API's generateContent and streamGenerateContent
// methods on generativelanguage.googleapis.com, v1 and v1beta.
//
// The text read is the request's turn: the trailing user contents (role `user`, or unset), after
// any trailing `model` content. Earlier contents were sent, and recorded, with the requests that
// first carried them. Within the turn it reads text parts and the string values of function
// responses. Field names are accepted in both of the API's JSON spellings (`inlineData` and
// `inline_data`).
package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const (
	host    = "generativelanguage.googleapis.com"
	name    = "gemini"
	shape   = "generate_content"
	version = "documented-2026-10-08"
)

// methodPath matches the generateContent and streamGenerateContent methods of a model, a tuned
// model or a dynamic model.
var methodPath = regexp.MustCompile(`^/(v1|v1beta)/(models|tunedModels|dynamic)/[^/:]+:(generateContent|streamGenerateContent)$`)

// Parser is the generateContent parser.
type Parser struct{}

// Match implements parsers.Parser.
func (Parser) Match(h, p string) bool { return h == host && methodPath.MatchString(p) }

// Version implements parsers.Parser: the date of the API reference the known shapes come from.
func (Parser) Version() string { return version }

// BlockResponse implements parsers.Parser: the error shape of Google's JSON APIs, with the HTTP
// status as the code and the name of the google.rpc.Code that maps to 403.
func (Parser) BlockResponse(message, link string) (int, string, []byte) {
	type status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	body, _ := json.Marshal(struct {
		Error status `json:"error"`
	}{status{http.StatusForbidden, parsers.BlockText(message, link), "PERMISSION_DENIED"}})
	return http.StatusForbidden, "application/json", body
}

type content struct {
	Role  *string                       `json:"role"`
	Parts *[]map[string]json.RawMessage `json:"parts"`
}

// partsWithoutText are the documented Part data fields, besides text and function responses, in
// both spellings.
var partsWithoutText = []string{
	"inlineData", "inline_data", "fileData", "file_data", "functionCall", "function_call",
	"executableCode", "executable_code", "codeExecutionResult", "code_execution_result",
}

// Parse implements parsers.Parser.
func (Parser) Parse(body []byte, _ string) (parsers.Result, error) {
	var req struct {
		Contents *[]content `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "the body does not decode as a GenerateContentRequest")
	}
	if req.Contents == nil || len(*req.Contents) == 0 {
		return parsers.Result{}, parsers.UnknownShape(name, "no contents")
	}
	contents := *req.Contents
	roles := make([]string, len(contents))
	for i, c := range contents {
		if c.Parts == nil {
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("content %d has no parts", i))
		}
		if c.Role != nil {
			roles[i] = *c.Role
		}
		switch roles[i] {
		case "", "user", "model":
		default:
			return parsers.Result{}, parsers.UnknownShape(name, fmt.Sprintf("content %d has an undocumented role", i))
		}
	}

	end := len(contents)
	for end > 0 && roles[end-1] == "model" {
		end--
	}
	start := end
	for start > 0 && roles[start-1] != "model" {
		start--
	}
	if start == end {
		return parsers.Result{}, parsers.UnknownShape(name, "no user content ends the conversation")
	}

	var texts []string
	for i := start; i < end; i++ {
		for j, p := range *contents[i].Parts {
			got, err := partText(p, fmt.Sprintf("content %d part %d", i, j))
			if err != nil {
				return parsers.Result{}, err
			}
			texts = append(texts, got...)
		}
	}
	if len(texts) == 0 {
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Text: strings.Join(texts, "\n"), Shape: shape}, nil
}

// partText reads one part by the data field it carries.
func partText(p map[string]json.RawMessage, at string) ([]string, error) {
	if raw, ok := p["text"]; ok {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, parsers.UnknownShape(name, at+": text is not a string")
		}
		var thought bool
		if raw, ok := p["thought"]; ok && json.Unmarshal(raw, &thought) == nil && thought {
			return nil, nil
		}
		return []string{text}, nil
	}
	if raw, ok := field(p, "functionResponse", "function_response"); ok {
		var fr map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fr); err != nil {
			return nil, parsers.UnknownShape(name, at+": a function response is not an object")
		}
		resp, ok := fr["response"]
		if !ok {
			return nil, parsers.UnknownShape(name, at+": a function response has no response")
		}
		texts, err := stringValues(resp)
		if err != nil {
			return nil, parsers.UnknownShape(name, at+": a function response's response is not JSON")
		}
		return texts, nil
	}
	for _, k := range partsWithoutText {
		if _, ok := p[k]; ok {
			return nil, nil
		}
	}
	return nil, parsers.UnknownShape(name, at+" has no documented data field")
}

func field(p map[string]json.RawMessage, camel, snake string) (json.RawMessage, bool) {
	if raw, ok := p[camel]; ok {
		return raw, true
	}
	raw, ok := p[snake]
	return raw, ok
}

// stringValues returns the string values in a JSON value, in document order, without the
// object keys: what a function returned, as text.
func stringValues(raw json.RawMessage) ([]string, error) {
	type frame struct{ object, expectKey bool }
	var stack []frame
	var out []string
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				stack = append(stack, frame{object: v == '{', expectKey: v == '{'})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				stack[n-1].expectKey = false
				continue
			}
			out = append(out, v)
			valueDone()
		default:
			valueDone()
		}
	}
}
