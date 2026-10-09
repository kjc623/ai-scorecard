package anthropic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const canary = "sac canary anthropic-api AKIAIOSFODNN7EXAMPLE"

func fixtures(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = b
	}
	return out
}

func TestEveryFixtureParsesToItsCanary(t *testing.T) {
	for path, body := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			res, err := Parser{}.Parse(body, "application/json")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := dedup.CanonicalText(res.Text); got != canary {
				t.Fatalf("text = %q, want the canary", got)
			}
			if res.Shape != "messages" {
				t.Fatalf("shape = %q", res.Shape)
			}
		})
	}
}

// Renaming a field the parser keys on turns a fixture into a shape it does not know.
func TestAlteredFixturesAreUnknownShapes(t *testing.T) {
	for path, body := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			altered := strings.Replace(string(body), `"messages"`, `"msgs"`, 1)
			if _, err := (Parser{}).Parse([]byte(altered), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
				t.Fatalf("err = %v, want ErrUnknownShape", err)
			}
		})
	}
	cases := []struct{ fixture, old, new string }{
		{"messages-string.json", `"content": "sac`, `"contents": "sac`},
		{"messages-prefill.json", `"role": "user"`, `"author": "user"`},
		{"messages-multi-turn.json", `"type": "text", "text": "sac`, `"type": "txt", "text": "sac`},
		{"messages-tool-result.json", `"type": "tool_result"`, `"type": "tool_output"`},
		{"messages-tool-result-blocks.json", `"content": [
            {"type": "text"`, `"content": [
            {"type": "markdown"`},
		{"messages-document.json", `"source": {"type": "text"`, `"source": {"type": "plain"`},
		{"messages-search-result.json", `"source": {"type": "content", "content"`, `"source": {"type": "content", "body"`},
		{"messages-beta-mcp-tool-result.json", `"role": "assistant"`, `"role": "agent"`},
	}
	for _, c := range cases {
		t.Run(c.fixture+"/"+c.new, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "documented", c.fixture))
			if err != nil {
				t.Fatal(err)
			}
			// A checkout with CRLF line endings must still match the multi-line cases.
			text := strings.ReplaceAll(string(body), "\r\n", "\n")
			altered := strings.Replace(text, c.old, c.new, 1)
			if altered == text {
				t.Fatalf("%q is not in the fixture", c.old)
			}
			if _, err := (Parser{}).Parse([]byte(altered), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
				t.Fatalf("err = %v, want ErrUnknownShape", err)
			}
		})
	}
}

func TestTurnWithoutTextIsNoText(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`
	if _, err := (Parser{}).Parse([]byte(body), "application/json"); !errors.Is(err, parsers.ErrNoText) {
		t.Fatalf("err = %v, want ErrNoText", err)
	}
}

func TestOnlyTheTurnIsRead(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"earlier"},{"role":"assistant","content":"reply"},{"role":"user","content":"first"},{"role":"user","content":[{"type":"text","text":"second"}]}]}`
	res, err := Parser{}.Parse([]byte(body), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "first\nsecond" {
		t.Fatalf("text = %q, want the trailing user messages only", res.Text)
	}
}

// An unknown-shape error names structure, never a value from the body.
func TestUnknownShapeErrorCarriesNoBodyText(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"secret-type-AKIAIOSFODNN7EXAMPLE","text":"AKIAIOSFODNN7EXAMPLE"}]}]}`
	_, err := Parser{}.Parse([]byte(body), "application/json")
	if !errors.Is(err, parsers.ErrUnknownShape) {
		t.Fatalf("err = %v, want ErrUnknownShape", err)
	}
	if strings.Contains(err.Error(), "AKIA") {
		t.Fatalf("the error carries body text: %v", err)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		host, path string
		want       bool
	}{
		{"api.anthropic.com", "/v1/messages", true},
		{"api.anthropic.com", "/v1/messages/count_tokens", false},
		{"api.anthropic.com", "/v1/messages/batches", false},
		{"claude.ai", "/v1/messages", false},
	}
	for _, c := range cases {
		if got := (Parser{}).Match(c.host, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}

// A blocked request is answered in the Messages API's error shape, as a permission_error.
func TestBlockResponseIsTheAPIErrorShape(t *testing.T) {
	status, ctype, body := Parser{}.BlockResponse("Remove the credential and try again.", "https://intranet.example/ai")
	if status != 403 || ctype != "application/json" {
		t.Fatalf("status, Content-Type = %d, %q", status, ctype)
	}
	want := `{"type":"error","error":{"type":"permission_error","message":"Remove the credential and try again. https://intranet.example/ai"}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant   %s", body, want)
	}
}
