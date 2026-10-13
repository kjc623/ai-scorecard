package gemini

import (
	"encoding/json"
	"regexp"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

// codeAssistHost is Google's Code Assist backend, which Gemini CLI and the Gemini Code Assist IDE
// extensions call for a person signed in with a Google account.
const codeAssistHost = "cloudcode-pa.googleapis.com"

// codeAssistPath matches its generateContent and streamGenerateContent methods. The model is named
// in the body, not the path.
var codeAssistPath = regexp.MustCompile(`^/v1internal:(generateContent|streamGenerateContent)$`)

// CodeAssist reads Code Assist's generateContent requests, which carry the API's
// GenerateContentRequest in `request`, beside the model, the Cloud project and the prompt's id.
type CodeAssist struct{}

// Match implements parsers.Parser.
func (CodeAssist) Match(h, p string) bool {
	return h == codeAssistHost && codeAssistPath.MatchString(p)
}

// Version implements parsers.Parser: the date Gemini CLI's request types were read.
func (CodeAssist) Version() string { return "documented-2026-10-13" }

// BlockResponse implements parsers.Parser: the error shape of Google's JSON APIs, as for the API.
func (CodeAssist) BlockResponse(message, link string) (int, string, []byte) {
	return Parser{}.BlockResponse(message, link)
}

// Parse implements parsers.Parser: the wrapped request is read as the API's.
func (CodeAssist) Parse(body []byte, mediaType string) (parsers.Result, error) {
	var req struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return parsers.Result{}, parsers.UnknownShape(name, "the body does not decode as a Code Assist request")
	}
	if len(req.Request) == 0 || string(req.Request) == "null" {
		return parsers.Result{}, parsers.UnknownShape(name, "no request")
	}
	return Parser{}.Parse(req.Request, mediaType)
}
