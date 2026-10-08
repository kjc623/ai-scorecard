package gemini

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

const canary = "sac canary gemini-api AKIAIOSFODNN7EXAMPLE"

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
			if res.Shape != shape {
				t.Fatalf("shape = %q", res.Shape)
			}
		})
	}
}

// Renaming a field the parser keys on turns a fixture into a shape it does not know.
func TestAlteredFixturesAreUnknownShapes(t *testing.T) {
	for path, body := range fixtures(t) {
		t.Run(path, func(t *testing.T) {
			altered := strings.Replace(string(body), `"contents"`, `"messages"`, 1)
			if _, err := (Parser{}).Parse([]byte(altered), "application/json"); !errors.Is(err, parsers.ErrUnknownShape) {
				t.Fatalf("err = %v, want ErrUnknownShape", err)
			}
		})
	}
	cases := []struct{ fixture, old, new string }{
		{"generate-content-text.json", `"parts"`, `"part"`},
		{"generate-content-multi-turn.json", `{"text": "sac`, `{"txt": "sac`},
		{"generate-content-multi-turn.json", `"role": "model"`, `"role": "assistant"`},
		{"generate-content-function-response.json", `"response": {"content"`, `"result": {"content"`},
		{"generate-content-snake-case.json", `"function_response"`, `"tool_response"`},
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

func TestTurnWithoutTextIsNoText(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]}]}`
	if _, err := (Parser{}).Parse([]byte(body), "application/json"); !errors.Is(err, parsers.ErrNoText) {
		t.Fatalf("err = %v, want ErrNoText", err)
	}
}

func TestStringValuesKeepDocumentOrderWithoutKeys(t *testing.T) {
	got, err := stringValues([]byte(`{"b":"one","a":{"k":["two",3,{"x":"three"}],"n":null},"z":"four"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two", "three", "four"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("values = %q, want %q", got, want)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1beta/models/gemini-flash-latest:generateContent", true},
		{"/v1beta/models/gemini-flash-latest:streamGenerateContent", true},
		{"/v1/models/gemini-flash-latest:generateContent", true},
		{"/v1beta/tunedModels/my-model:generateContent", true},
		{"/v1beta/models/gemini-flash-latest:countTokens", false},
		{"/v1beta/models/gemini-flash-latest:embedContent", false},
		{"/upload/v1beta/files", false},
	}
	for _, c := range cases {
		if got := (Parser{}).Match(host, c.path); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if (Parser{}).Match("aiplatform.googleapis.com", "/v1beta/models/m:generateContent") {
		t.Error("matched another host")
	}
}

// A blocked request is answered in the error shape of Google's JSON APIs, as PERMISSION_DENIED.
func TestBlockResponseIsTheAPIErrorShape(t *testing.T) {
	status, ctype, body := Parser{}.BlockResponse("Remove the credential and try again.", "")
	if status != 403 || ctype != "application/json" {
		t.Fatalf("status, Content-Type = %d, %q", status, ctype)
	}
	want := `{"error":{"code":403,"message":"Remove the credential and try again.","status":"PERMISSION_DENIED"}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant   %s", body, want)
	}
}
