package hooks_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// claudeCodeFixtures holds Claude Code's hook input. The device phase adds a folder captured from
// the installed version beside the documented one.
const claudeCodeFixtures = "testdata/claude-code/documented"

func claudeCodeAdapter(t *testing.T) hooks.Adapter {
	t.Helper()
	a, ok := hooks.Lookup(hooks.ClaudeCodeTool)
	if !ok {
		t.Fatal("no adapter is registered for claude_code")
	}
	return a
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(claudeCodeFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const fixtureSession = "8f2c1e4a-5b6d-4c7e-9f80-1a2b3c4d5e6f"

// UserPromptSubmit hands over the prompt as typed; PreToolUse the tool's input as compact JSON,
// with the tool's name. Both carry the session and the working directory.
func TestClaudeCodeParsesTheDocumentedInput(t *testing.T) {
	a := claudeCodeAdapter(t)
	for _, c := range []struct {
		file, event, toolName, prompt string
	}{
		{"user-prompt-submit.json", "UserPromptSubmit", "", `Deploy with key AKIAIOSFODNN7EXAMPLE to the "staging" bucket <now>`},
		{"pre-tool-use-bash.json", "PreToolUse", "Bash", `{"command":"aws s3 ls --profile ci","description":"List the buckets","timeout":120000,"run_in_background":false}`},
		{"pre-tool-use-webfetch.json", "PreToolUse", "WebFetch", `{"url":"https://example.com/api","prompt":"Extract the API endpoints"}`},
		{"pre-tool-use-mcp.json", "PreToolUse", "mcp__memory__create_entities", `{"entities":[{"name":"deploy","entityType":"note","observations":["key AKIAIOSFODNN7EXAMPLE"]}]}`},
	} {
		t.Run(c.file, func(t *testing.T) {
			ev, err := a.Parse(c.event, fixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			want := protocol.HookEvaluate{
				Tool: "claude_code", Event: c.event, SessionID: fixtureSession, Cwd: `C:\work\app`,
				ToolName: c.toolName, PromptText: c.prompt, PromptBytes: int64(len(c.prompt)),
			}
			if ev != want {
				t.Fatalf("Parse =\n%+v\nwant\n%+v", ev, want)
			}
			if err := ev.Validate(); err != nil {
				t.Fatalf("the parsed input is not a valid hook_evaluate: %v", err)
			}
		})
	}
}

// Input the agent cannot read is an error that never quotes the input; hook mode then fails open.
func TestClaudeCodeRefusesUnreadableInput(t *testing.T) {
	a := claudeCodeAdapter(t)
	const secret = "AKIAIOSFODNN7EXAMPLE"
	for name, c := range map[string]struct{ event, input string }{
		"not JSON":             {"UserPromptSubmit", "prompt " + secret},
		"no prompt":            {"UserPromptSubmit", `{"session_id":"s","cwd":"c"}`},
		"no tool input":        {"PreToolUse", `{"session_id":"s","tool_name":"Bash"}`},
		"no tool name":         {"PreToolUse", `{"session_id":"s","tool_input":{"command":"` + secret + `"}}`},
		"event not declared":   {"Stop", `{"session_id":"s","prompt":"` + secret + `"}`},
		"prompt not a string":  {"UserPromptSubmit", `{"session_id":"s","prompt":["` + secret + `"]}`},
		"tool input truncated": {"PreToolUse", `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"` + secret},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := a.Parse(c.event, []byte(c.input))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the error quotes the input: %v", err)
			}
		})
	}
}

var credentialRule = protocol.HookDecision{
	Action: protocol.HookBlock, RuleID: "block_credentials",
	Message: "Remove the credential & try again.", Link: "https://intranet.example/ai?a=1&b=2",
}

// Each decision renders as the JSON output Claude Code documents, always with exit code 0.
func TestClaudeCodeRender(t *testing.T) {
	a := claudeCodeAdapter(t)
	warn := credentialRule
	warn.Action = protocol.HookWarn
	const text = "Remove the credential & try again.\nhttps://intranet.example/ai?a=1&b=2"
	for name, c := range map[string]struct {
		event string
		d     protocol.HookDecision
		want  string
	}{
		"block a prompt": {"UserPromptSubmit", credentialRule,
			`{"decision":"block","reason":` + quote(text) + `,"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","suppressOriginalPrompt":true}}`},
		"deny a tool call": {"PreToolUse", credentialRule,
			`{"systemMessage":` + quote(text) + `,"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":` + quote(text) + `}}`},
		"warn on a prompt":    {"UserPromptSubmit", warn, `{"systemMessage":` + quote(text) + `}`},
		"warn on a tool call": {"PreToolUse", warn, `{"systemMessage":` + quote(text) + `}`},
		"block without a link": {"UserPromptSubmit", protocol.HookDecision{Action: protocol.HookBlock, Message: "No."},
			`{"decision":"block","reason":"No.","hookSpecificOutput":{"hookEventName":"UserPromptSubmit","suppressOriginalPrompt":true}}`},
		"warn with only a link": {"PreToolUse", protocol.HookDecision{Action: protocol.HookWarn, Link: "https://intranet.example/ai"},
			`{"systemMessage":"https://intranet.example/ai"}`},
		"allow":                {"UserPromptSubmit", protocol.HookDecision{Action: protocol.HookAllow, RuleID: "policy.default"}, ``},
		"warn with no text":    {"UserPromptSubmit", protocol.HookDecision{Action: protocol.HookWarn}, ``},
		"block on other event": {"Stop", credentialRule, ``},
	} {
		t.Run(name, func(t *testing.T) {
			out, code := a.Render(c.event, c.d)
			if code != 0 {
				t.Fatalf("exit code %d, want 0", code)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != c.want {
				t.Fatalf("Render =\n%s\nwant\n%s", got, c.want)
			}
			if len(out) > 0 && !json.Valid(out) {
				t.Fatal("the output is not JSON")
			}
		})
	}
	for _, event := range []string{"UserPromptSubmit", "PreToolUse"} {
		if out, code := a.Allow(event); len(out) != 0 || code != 0 {
			t.Fatalf("Allow(%s) = %q, %d; want no output and exit code 0", event, out, code)
		}
	}
}

// coachingMessage is as long as a rule's message may be, with characters a tool might escape or
// mangle: an ampersand, a hash and curly quotes.
const coachingMessage = "Customer data must not go to AI tools we have not approved. Use Contoso Assistant instead: " +
	"it keeps prompts in our tenant & retains nothing outside it. Paste the same request there, or ask the security " +
	"team in #ai-help if it cannot help. See “Approved AI tools” at the link below."

const coachingLink = "https://intranet.example/ai/approved-tools?from=hook&rule=redirect_customer_data"

// coachingDecision is a rule's warn or block with the longest message and a link.
func coachingDecision(t *testing.T, action protocol.HookAction) protocol.HookDecision {
	t.Helper()
	if n := utf8.RuneCountInString(coachingMessage); n != policy.MaxRuleMessage {
		t.Fatalf("the message has %d characters, want %d", n, policy.MaxRuleMessage)
	}
	d := protocol.HookDecision{Action: action, RuleID: "redirect_customer_data", Message: coachingMessage, Link: coachingLink}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	return d
}

// renderGolden renders d for event and compares the output with the golden file. Each field in
// shown, a dotted path to what the tool's documentation says it shows the user, must carry the
// whole message with the link on a line of its own.
func renderGolden(t *testing.T, a hooks.Adapter, event string, d protocol.HookDecision, golden string, shown ...string) {
	t.Helper()
	out, code := a.Render(event, d)
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may end the golden file with CRLF.
	if got, w := strings.TrimSuffix(string(out), "\n"), strings.TrimRight(string(want), "\r\n"); got != w {
		t.Fatalf("Render =\n%s\nwant (%s)\n%s", got, golden, w)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the output is not a JSON object: %v", err)
	}
	for _, path := range shown {
		var v any = doc
		for _, key := range strings.Split(path, ".") {
			obj, _ := v.(map[string]any)
			v = obj[key]
		}
		if v != coachingMessage+"\n"+coachingLink {
			t.Errorf("%s is %q, want the whole message with the link on its own line", path, v)
		}
	}
}

// The documentation says Claude Code shows systemMessage to the user, and a blocked prompt's
// reason; a tool call's deny reason goes to Claude, so the deny also carries a systemMessage. The
// device phase replaces these files with what Claude Code is seen to show.
func TestClaudeCodeRendersACoachingMessageWithALink(t *testing.T) {
	a := claudeCodeAdapter(t)
	for _, c := range []struct {
		event  string
		action protocol.HookAction
		golden string
		shown  []string
	}{
		{"UserPromptSubmit", protocol.HookWarn, "user-prompt-submit-warn.json", []string{"systemMessage"}},
		{"UserPromptSubmit", protocol.HookBlock, "user-prompt-submit-block.json", []string{"reason"}},
		{"PreToolUse", protocol.HookWarn, "pre-tool-use-warn.json", []string{"systemMessage"}},
		{"PreToolUse", protocol.HookBlock, "pre-tool-use-block.json", []string{"systemMessage", "hookSpecificOutput.permissionDecisionReason"}},
	} {
		t.Run(c.golden, func(t *testing.T) {
			renderGolden(t, a, c.event, coachingDecision(t, c.action), filepath.Join("testdata/claude-code/render", c.golden), c.shown...)
		})
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	// The adapter leaves HTML characters unescaped.
	return strings.NewReplacer(`\u0026`, "&", `\u003c`, "<", `\u003e`, ">").Replace(string(b))
}

// The documented prompt, through the relay with a credential rule in force, is blocked with the
// rule's message and recorded on route tool.hook.
func TestClaudeCodePromptIsBlockedThroughTheRelay(t *testing.T) {
	a := claudeCodeAdapter(t)
	sink := &memSink{}
	b := testBundle(protocol.ModeM1, blockCredentials)
	r := newRelay(t, newPipeline(t, sink, b), &stubClassifier{})

	for _, c := range []struct{ file, event string }{
		{"user-prompt-submit.json", "UserPromptSubmit"},
		{"pre-tool-use-mcp.json", "PreToolUse"},
	} {
		ev, err := a.Parse(c.event, fixture(t, c.file))
		if err != nil {
			t.Fatal(err)
		}
		ev.CapPrompt()
		body, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		answer, served := ask(t, r, protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "1", Body: body})
		d := decision(t, answer)
		waitServed(t, served)
		if d.Action != protocol.HookBlock || d.RuleID != blockCredentials.RuleID {
			t.Fatalf("%s: decision %+v, want the credential rule's block", c.file, d)
		}
		out, _ := a.Render(c.event, d)
		if !strings.Contains(string(out), blockCredentials.Message+`\n`+blockCredentials.Link) {
			t.Fatalf("%s: the output %s does not carry the rule's message and link", c.file, out)
		}
	}

	recorded := len(sink.all())
	if recorded != 2 {
		t.Fatalf("%d records, want 2", recorded)
	}
	for _, e := range sink.all() {
		env := decodeEnvelope(t, e)
		if env.Source != string(protocol.RouteToolHook) || env.ToolFingerprint != "app:claude_code" || env.Decision == nil || env.Decision.Action != "blocked" {
			t.Fatalf("envelope %+v, want a blocked app:claude_code prompt on tool.hook", env)
		}
	}
	// The same prompt with the tool's hooks off is allowed and not recorded.
	off := testBundle(protocol.ModeM1, blockCredentials)
	off.Endpoint.Tools["claude_code"] = policy.EndpointTool{}
	r = newRelay(t, newPipeline(t, sink, off), &stubClassifier{})
	ev, _ := a.Parse("UserPromptSubmit", fixture(t, "user-prompt-submit.json"))
	body, _ := json.Marshal(ev)
	answer, served := ask(t, r, protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "1", Body: body})
	waitServed(t, served)
	if d := decision(t, answer); d.Action != protocol.HookAllow {
		t.Fatalf("with Claude Code's hooks off the decision is %+v", d)
	}
	if out, _ := a.Render("UserPromptSubmit", decision(t, answer)); len(out) != 0 {
		t.Fatalf("an allow rendered %q", out)
	}
	if len(sink.all()) != recorded {
		t.Fatal("a prompt was recorded with Claude Code's hooks off")
	}
}
