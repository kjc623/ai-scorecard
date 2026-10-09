package hooks_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// cursorFixtures holds Cursor's hook input. The device phase adds a folder captured from the
// installed version beside the documented one.
const cursorFixtures = "testdata/cursor/documented"

const cursorConversation = "5d3c9f0e-7a41-4b2e-9c6d-1f8e2a3b4c5d"

func cursorAdapter(t *testing.T) hooks.Adapter {
	t.Helper()
	a, ok := hooks.Lookup(hooks.CursorTool)
	if !ok {
		t.Fatal("no adapter is registered for cursor")
	}
	return a
}

func cursorFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cursorFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// beforeSubmitPrompt hands over the prompt as typed; beforeMCPExecution the call's arguments as
// compact JSON, whether Cursor sends them as a JSON string or an object, with the tool's name. Both
// carry the conversation as the session.
func TestCursorParsesTheDocumentedInput(t *testing.T) {
	a := cursorAdapter(t)
	for _, c := range []struct {
		file, event, toolName, prompt string
	}{
		{"before-submit-prompt.json", "beforeSubmitPrompt", "", `Deploy with key AKIAIOSFODNN7EXAMPLE to the "staging" bucket <now>`},
		{"before-mcp-execution.json", "beforeMCPExecution", "create_entities", `{"entities":[{"name":"deploy","entityType":"note","observations":["key AKIAIOSFODNN7EXAMPLE"]}]}`},
		{"before-mcp-execution-object.json", "beforeMCPExecution", "fetch", `{"url":"https://example.com/api","max_length":5000}`},
	} {
		t.Run(c.file, func(t *testing.T) {
			ev, err := a.Parse(c.event, cursorFixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			want := protocol.HookEvaluate{
				Tool: "cursor", Event: c.event, SessionID: cursorConversation,
				ToolName: c.toolName, PromptText: c.prompt, PromptBytes: int64(len(c.prompt)),
			}
			if ev != want {
				t.Fatalf("parsed\n%+v\nwant\n%+v", ev, want)
			}
			if err := ev.Validate(); err != nil {
				t.Fatalf("the parsed hook_evaluate is refused: %v", err)
			}
		})
	}
}

// An MCP call's arguments in a string that is not JSON are sent as written.
func TestCursorSendsToolInputThatIsNotJSONAsWritten(t *testing.T) {
	in := `{"conversation_id":"c-1","tool_name":"run","tool_input":"key AKIAIOSFODNN7EXAMPLE"}`
	ev, err := cursorAdapter(t).Parse("beforeMCPExecution", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if ev.PromptText != "key AKIAIOSFODNN7EXAMPLE" || ev.ToolName != "run" {
		t.Fatalf("parsed %+v", ev)
	}
}

// Input the adapter cannot use is refused without quoting it, so the hook fails open.
func TestCursorRefusesUnusableInputWithoutQuotingIt(t *testing.T) {
	a := cursorAdapter(t)
	for _, c := range []struct{ name, event, in string }{
		{"not JSON", "beforeSubmitPrompt", `AKIAIOSFODNN7EXAMPLE`},
		{"no conversation", "beforeSubmitPrompt", `{"prompt":"AKIAIOSFODNN7EXAMPLE"}`},
		{"no prompt", "beforeSubmitPrompt", `{"conversation_id":"c-1"}`},
		{"no tool name", "beforeMCPExecution", `{"conversation_id":"c-1","tool_input":"{\"k\":\"AKIAIOSFODNN7EXAMPLE\"}"}`},
		{"no tool input", "beforeMCPExecution", `{"conversation_id":"c-1","tool_name":"run"}`},
		{"undeclared event", "beforeShellExecution", `{"conversation_id":"c-1","command":"echo AKIAIOSFODNN7EXAMPLE"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := a.Parse(c.event, []byte(c.in))
			if err == nil {
				t.Fatal("the input was accepted")
			}
			if strings.Contains(err.Error(), "AKIA") {
				t.Fatalf("the error quotes the input: %v", err)
			}
		})
	}
}

// Render answers each event in its own output, with exit code 0: a block stops the prompt
// (continue false) or the tool call (permission deny) and shows the rule's message with its link
// on its own line; a warn goes ahead and carries the message; allow carries nothing else.
func TestCursorRendersEachDecision(t *testing.T) {
	a := cursorAdapter(t)
	block := protocol.HookDecision{Action: protocol.HookBlock, Message: "Remove the credential & try again.", Link: "https://intranet.example/ai", RuleID: "block_credentials"}
	warn := protocol.HookDecision{Action: protocol.HookWarn, Message: "This tool is not approved.", RuleID: "warn_unsanctioned"}
	allow := protocol.HookDecision{Action: protocol.HookAllow, RuleID: "policy.default"}
	for _, c := range []struct {
		name, event string
		d           protocol.HookDecision
		want        string
	}{
		{"prompt block", "beforeSubmitPrompt", block, `{"continue":false,"user_message":"Remove the credential & try again.\nhttps://intranet.example/ai"}`},
		{"prompt warn", "beforeSubmitPrompt", warn, `{"continue":true,"user_message":"This tool is not approved."}`},
		{"prompt allow", "beforeSubmitPrompt", allow, `{"continue":true}`},
		{"mcp block", "beforeMCPExecution", block, `{"permission":"deny","user_message":"Remove the credential & try again.\nhttps://intranet.example/ai","agent_message":"Remove the credential & try again.\nhttps://intranet.example/ai"}`},
		{"mcp warn", "beforeMCPExecution", warn, `{"permission":"allow","user_message":"This tool is not approved."}`},
		{"mcp allow", "beforeMCPExecution", allow, `{"permission":"allow"}`},
		{"undeclared event", "stop", block, ``},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := a.Render(c.event, c.d)
			if code != 0 {
				t.Fatalf("exit code %d", code)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != c.want {
				t.Fatalf("rendered\n%s\nwant\n%s", got, c.want)
			}
		})
	}
	for _, event := range []string{"beforeSubmitPrompt", "beforeMCPExecution", "stop"} {
		gotOut, gotCode := a.Allow(event)
		wantOut, wantCode := a.Render(event, protocol.HookDecision{Action: protocol.HookAllow})
		if string(gotOut) != string(wantOut) || gotCode != wantCode {
			t.Errorf("%s: Allow gives %q/%d, an allow decision %q/%d", event, gotOut, gotCode, wantOut, wantCode)
		}
	}
}

// Cursor stops the action a block from either declared event asks it to stop.
func TestCursorCanEnforceBothEvents(t *testing.T) {
	e := cursorAdapter(t)
	for event, want := range map[string]bool{"beforeSubmitPrompt": true, "beforeMCPExecution": true, "stop": false} {
		if got := e.CanEnforce(event); got != want {
			t.Errorf("CanEnforce(%s) = %v, want %v", event, got, want)
		}
	}
}

// Cursor shows user_message to the user, by the sources the adapter follows; whether it shows one
// on a prompt or tool call that goes ahead is not documented. The device phase replaces these files
// with what Cursor is seen to show.
func TestCursorRendersACoachingMessageWithALink(t *testing.T) {
	a := cursorAdapter(t)
	for _, c := range []struct {
		event  string
		action protocol.HookAction
		golden string
	}{
		{"beforeSubmitPrompt", protocol.HookWarn, "before-submit-prompt-warn.json"},
		{"beforeSubmitPrompt", protocol.HookBlock, "before-submit-prompt-block.json"},
		{"beforeMCPExecution", protocol.HookWarn, "before-mcp-execution-warn.json"},
		{"beforeMCPExecution", protocol.HookBlock, "before-mcp-execution-block.json"},
	} {
		t.Run(c.golden, func(t *testing.T) {
			renderGolden(t, a, c.event, coachingDecision(t, c.action), filepath.Join("testdata/cursor/render", c.golden), "user_message")
		})
	}
}

// The documented prompt, parsed by the adapter and decided by the relay under a block rule on
// credential with Cursor's hooks on, renders as a stopped prompt with the rule's message, and is
// recorded on tool.hook as blocked for app:cursor.
func TestCursorPromptWithACredentialIsBlockedThroughTheRelay(t *testing.T) {
	a := cursorAdapter(t)
	b := testBundle(protocol.ModeM1, blockCredentials)
	b.Endpoint.Tools = map[string]policy.EndpointTool{"cursor": {Hooks: true}}
	sink := &memSink{}
	pipe := newPipeline(t, sink, b)
	r := newRelay(t, pipe, &stubClassifier{})

	ev, err := a.Parse("beforeSubmitPrompt", cursorFixture(t, "before-submit-prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev.CapPrompt()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	answer, served := ask(t, r, protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "1", Body: body})
	out, code := a.Render(ev.Event, decision(t, answer))
	if want := `{"continue":false,"user_message":"Remove the credential and try again.\nhttps://intranet.example/ai"}` + "\n"; string(out) != want || code != 0 {
		t.Fatalf("rendered %q/%d, want %q/0", out, code, want)
	}
	waitServed(t, served)

	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("%d records, want 1", len(entries))
	}
	env := decodeEnvelope(t, entries[0])
	if env.Source != string(protocol.RouteToolHook) || env.ToolFingerprint != "app:cursor" || env.Decision == nil ||
		env.Decision.Action != protocol.ActionBlocked || env.Decision.RuleID != "block_credentials" {
		t.Fatalf("recorded %+v (decision %+v)", env, env.Decision)
	}
}
