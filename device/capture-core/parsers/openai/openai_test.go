package openai

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const canary = "sac canary openai-api AKIAIOSFODNN7EXAMPLE"

// fixture is one request body and what its file name says about it: chat-* files are Chat
// Completions bodies, responses-* files Responses bodies.
type fixture struct {
	body               []byte
	path, shape, field string
}

func fixtures(t *testing.T) map[string]fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	out := map[string]fixture{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		switch base := filepath.Base(p); {
		case strings.HasPrefix(base, "chat-"):
			out[p] = fixture{b, chatPath, shapeChat, `"messages"`}
		case strings.HasPrefix(base, "responses-"):
			out[p] = fixture{b, responsesPath, shapeResponses, `"input"`}
		default:
			t.Fatalf("%s is named for neither API", p)
		}
	}
	return out
}

func TestEveryFixtureParsesToItsCanary(t *testing.T) {
	for path, f := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			if !(Parser{}).Match(host, f.path) {
				t.Fatalf("the parser does not match %s", f.path)
			}
			res, err := Parser{}.Parse(f.body, "application/json")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := dedup.CanonicalText(res.Text); got != canary {
				t.Fatalf("text = %q, want the canary", got)
			}
			if res.Shape != f.shape {
				t.Fatalf("shape = %q, want %q", res.Shape, f.shape)
			}
		})
	}
}

// Renaming a field the parser keys on turns a fixture into a shape it does not know.
func TestAlteredFixturesAreUnknownShapes(t *testing.T) {
	for path, f := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			altered := strings.Replace(string(f.body), f.field, `"renamed"`, 1)
			if _, err := (Parser{}).Parse([]byte(altered), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
				t.Fatalf("err = %v, want ErrUnknownShape", err)
			}
		})
	}
	cases := []struct{ fixture, old, new string }{
		{"chat-string.json", `"role": "user"`, `"role": "human"`},
		{"chat-parts.json", `{"type": "text", "text": "sac`, `{"type": "input_text", "text": "sac`},
		{"chat-tool.json", `"tool_call_id": "call_abc123", "content"`, `"tool_call_id": "call_abc123", "result"`},
		{"chat-parallel-tools.json", `"content": [{"type": "text", "text"`, `"content": [{"type": "text", "value"`},
		{"responses-messages.json", `{"type": "input_text", "text": "sac`, `{"type": "text", "text": "sac`},
		{"responses-function-output.json", `"type": "function_call_output"`, `"type": "function_result"`},
		{"responses-previous-response.json", `"call_id": "call_2",
      "output"`, `"call_id": "call_2",
      "result"`},
	}
	for _, c := range cases {
		t.Run(c.fixture+"/"+c.new, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "documented", c.fixture))
			if err != nil {
				t.Fatal(err)
			}
			altered := strings.Replace(string(body), c.old, c.new, 1)
			if altered == string(body) {
				t.Fatalf("%q is not in the fixture", c.old)
			}
			if _, err := (Parser{}).Parse([]byte(altered), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
				t.Fatalf("err = %v, want ErrUnknownShape", err)
			}
		})
	}
}

func TestOnlyTheTurnIsRead(t *testing.T) {
	cases := map[string]string{
		"chat":      `{"messages":[{"role":"user","content":"earlier"},{"role":"assistant","content":"reply"},{"role":"user","content":"first"},{"role":"user","content":"second"}]}`,
		"responses": `{"input":[{"role":"user","content":"earlier"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"computer_call_output","call_id":"x","output":{}},{"role":"user","content":"first"},{"type":"function_call_output","call_id":"c","output":"second"}]}`,
	}
	for name, body := range cases {
		res, err := Parser{}.Parse([]byte(body), "application/json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Text != "first\nsecond" {
			t.Fatalf("%s: text = %q, want the trailing input only", name, res.Text)
		}
	}
}

func TestTurnWithoutTextIsNoText(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	if _, err := (Parser{}).Parse([]byte(body), "application/json"); !errors.Is(err, parsers.ErrNoText) {
		t.Fatalf("err = %v, want ErrNoText", err)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		host, path string
		want       bool
	}{
		{"api.openai.com", "/v1/chat/completions", true},
		{"api.openai.com", "/v1/responses", true},
		{"api.openai.com", "/v1/responses/resp_123/cancel", false},
		{"api.openai.com", "/v1/embeddings", false},
		{"chatgpt.com", "/v1/responses", false},
	}
	for _, c := range cases {
		if got := (Parser{}).Match(c.host, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}
