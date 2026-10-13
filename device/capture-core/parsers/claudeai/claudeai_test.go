package claudeai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

// performActionBody is a message's PerformAction request as claude.ai's web app sent it on the
// test VM, with its session, conversation and message ids, model name and one opaque token
// replaced by placeholders of the same length (device/extension/test/tools/claude-web.json).
const performActionBody = "CnoKGQoVc2Vzc18wMDAwMDAwMDAwMDAwMDAwEAESJDAwMDAwMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMRgDOgwIARABGAEwAUABUAFyBWVuLVVTeiAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABKlAQokMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAyEiQwMDAwMDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDMaGHN1bmRpYWwtY2hlY2stY2xhdWRlLXdlYjoSEhBjbGF1ZGUtbW9kZWwtMC0wQgoKCAgBOAFQAHABYhNBbWVyaWNhL0xvc19BbmdlbGVzagVlbi1VU5ABAQ=="

// message is a protobuf message of length-delimited fields, given as number and value in turn.
func message(fields ...any) []byte {
	var b []byte
	for i := 0; i < len(fields); i += 2 {
		b = protowire.AppendTag(b, protowire.Number(fields[i].(int)), protowire.BytesType)
		b = protowire.AppendBytes(b, fields[i+1].([]byte))
	}
	return b
}

func TestRPCReadsTheCapturedMessage(t *testing.T) {
	body, err := base64.StdEncoding.DecodeString(performActionBody)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RPC{}.Parse(body, "application/proto")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Text != "sundial-check-claude-web" || res.Shape != "perform_action" {
		t.Fatalf("got %+v", res)
	}
}

func TestRPCAnswers(t *testing.T) {
	session := message(1, []byte("sess_0000"))
	withVarint := protowire.AppendVarint(protowire.AppendTag(nil, 7, protowire.VarintType), 3)
	for name, tc := range map[string]struct {
		body      []byte
		mediaType string
		text      string
		err       error
	}{
		"a message":                   {message(1, session, 2, message(1, []byte("id"), 3, []byte("hello"))), "application/proto", "hello", nil},
		"a message after a varint":    {append(withVarint, message(2, message(3, []byte("hello")))...), "application/proto; charset=binary", "hello", nil},
		"another action":              {message(1, session, 15, message(1, []byte("x"))), "application/proto", "", parsers.ErrNotPrompt},
		"an empty call":               {nil, "application/proto", "", parsers.ErrNotPrompt},
		"a message without text":      {message(1, session, 2, message(1, []byte("id"))), "application/proto", "", parsers.ErrNoText},
		"a message of spaces":         {message(2, message(3, []byte(" \n"))), "application/proto", "", parsers.ErrNoText},
		"text that is not UTF-8":      {message(2, message(3, []byte{0xff, 0xfe})), "application/proto", "", parsers.ErrUnknownShape},
		"a body that is not protobuf": {[]byte{0x12, 0x40, 0x01}, "application/proto", "", parsers.ErrUnknownShape},
		"an action that is not one":   {message(2, []byte{0x1a, 0x40}), "application/proto", "", parsers.ErrUnknownShape},
		"a streaming call":            {message(2, message(3, []byte("hello"))), "application/connect+proto", "", parsers.ErrUnknownShape},
		"a JSON call":                 {[]byte(`{"text":"hello"}`), "application/json", "", parsers.ErrUnknownShape},
		"a call with no content type": {message(2, message(3, []byte("hello"))), "", "", parsers.ErrUnknownShape},
	} {
		res, err := RPC{}.Parse(tc.body, tc.mediaType)
		if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) || res.Text != tc.text {
			t.Errorf("%s: got %q, %v; want %q, %v", name, res.Text, err, tc.text, tc.err)
		}
	}
}

func TestCompletionAnswers(t *testing.T) {
	for body, want := range map[string]struct {
		text string
		err  error
	}{
		`{"prompt":"hello","timezone":"UTC","attachments":[],"files":[]}`: {"hello", nil},
		`{"prompt":"","files":["f"]}`:                                     {"", parsers.ErrNoText},
		`{"timezone":"UTC"}`:                                              {"", parsers.ErrUnknownShape},
		`{"prompt":7}`:                                                    {"", parsers.ErrUnknownShape},
		`{"prompt":null}`:                                                 {"", parsers.ErrUnknownShape},
		`prompt=hello`:                                                    {"", parsers.ErrUnknownShape},
	} {
		res, err := Completion{}.Parse([]byte(body), "application/json")
		if !errors.Is(err, want.err) || (want.err == nil && err != nil) || res.Text != want.text {
			t.Errorf("%s: got %q, %v; want %q, %v", body, res.Text, err, want.text, want.err)
		}
	}
}

func TestMatchAndOwnership(t *testing.T) {
	rpc := "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction"
	done := "/api/organizations/6f1c2a3e/chat_conversations/0a1b2c3d/completion"
	for _, tc := range []struct {
		parser   parsers.Parser
		host, p  string
		match    bool
		ownsHost bool
	}{
		{RPC{}, "claude.ai", rpc, true, true},
		{RPC{}, "claude.ai", "/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/StreamTimeline", false, true},
		{RPC{}, "claude.ai", "/claudeai-rpc/x/y.ConversationService/PerformAction", false, true},
		{RPC{}, "api.claude.ai", rpc, false, false},
		{RPC{}, "claude.ai", done, false, true},
		{Completion{}, "claude.ai", done, true, true},
		{Completion{}, "claude.ai", "/api/organizations/o/chat_conversations/c/retry_completion", false, true},
		{Completion{}, "claude.ai", "/api/organizations/o/chat_conversations/c/completion/x", false, true},
		{Completion{}, "a.claude.ai", done, false, false},
	} {
		if got := tc.parser.Match(tc.host, tc.p); got != tc.match {
			t.Errorf("%T.Match(%q, %q) = %v, want %v", tc.parser, tc.host, tc.p, got, tc.match)
		}
		if got := tc.parser.(parsers.HostOwner).OwnsHost(tc.host); got != tc.ownsHost {
			t.Errorf("%T.OwnsHost(%q) = %v, want %v", tc.parser, tc.host, got, tc.ownsHost)
		}
	}
}

func TestBlockResponses(t *testing.T) {
	status, ct, body := RPC{}.BlockResponse("Blocked by policy.", "https://example.invalid/why")
	var connectErr struct{ Code, Message string }
	if err := json.Unmarshal(body, &connectErr); err != nil || status != http.StatusForbidden || ct != "application/json" ||
		connectErr.Code != "permission_denied" || connectErr.Message != "Blocked by policy. https://example.invalid/why" {
		t.Errorf("RPC block = %d %s %s", status, ct, body)
	}
	status, ct, body = Completion{}.BlockResponse("Blocked by policy.", "")
	var apiErr struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(body, &apiErr); err != nil || status != http.StatusForbidden || ct != "application/json" ||
		apiErr.Type != "error" || apiErr.Error.Type != "permission_error" || apiErr.Error.Message != "Blocked by policy." {
		t.Errorf("Completion block = %d %s %s", status, ct, body)
	}
}
