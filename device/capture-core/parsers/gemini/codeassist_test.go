package gemini

import (
	"errors"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

// Each fixture's GenerateContentRequest, wrapped as Gemini CLI wraps it for Code Assist, reads as
// the API's.
func TestCodeAssistReadsTheWrappedRequest(t *testing.T) {
	for path, body := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			wrapped := `{"model":"gemini-2.5-pro","project":"sac-placeholder-project","user_prompt_id":"p-1","request":` +
				string(body) + `}`
			res, err := CodeAssist{}.Parse([]byte(wrapped), "application/json")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := dedup.CanonicalText(res.Text); got != canary {
				t.Fatalf("text = %q, want the canary", got)
			}
		})
	}
}

func TestCodeAssistUnknownShapes(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":           `contents`,
		"no request":         `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"x"}]}]}`,
		"a null request":     `{"request":null}`,
		"no contents inside": `{"request":{"messages":[]}}`,
	} {
		if _, err := (CodeAssist{}).Parse([]byte(body), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
			t.Errorf("%s: err = %v, want ErrUnknownShape", name, err)
		}
	}
}

func TestCodeAssistMatch(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1internal:generateContent":         true,
		"/v1internal:streamGenerateContent":   true,
		"/v1internal:countTokens":             false,
		"/v1internal:loadCodeAssist":          false,
		"/v1beta/models/m:generateContent":    false,
		"/v1internal/x:streamGenerateContent": false,
	} {
		if got := (CodeAssist{}).Match(codeAssistHost, path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
	if (CodeAssist{}).Match(host, "/v1internal:generateContent") {
		t.Error("matched the API's host")
	}
}
