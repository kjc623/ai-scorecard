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

// codexFixtures holds Codex's hook input. The device phase adds a folder captured from the
// installed version beside the documented one.
const codexFixtures = "testdata/codex/documented"

const codexSession = "0199c3a1-7b2e-7c40-9d1f-3e5a6b7c8d90"

func codexAdapter(t *testing.T) hooks.Adapter {
	t.Helper()
	a, ok := hooks.Lookup(hooks.CodexTool)
	if !ok {
		t.Fatal("no adapter is registered for codex")
	}
	return a
}

func codexFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(codexFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// UserPromptSubmit hands over the prompt as typed, the session and the working directory, from the
// main agent and from a subagent alike.
func TestCodexParsesTheDocumentedInput(t *testing.T) {
	a := codexAdapter(t)
	for _, c := range []struct{ file, prompt string }{
		{"user-prompt-submit.json", `Deploy with key AKIAIOSFODNN7EXAMPLE to the "staging" bucket <now>`},
		{"user-prompt-submit-subagent.json", `List the files that read AWS_SECRET_ACCESS_KEY`},
	} {
		t.Run(c.file, func(t *testing.T) {
			ev, err := a.Parse("UserPromptSubmit", codexFixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			want := protocol.HookEvaluate{
				Tool: "codex", Event: "UserPromptSubmit", SessionID: codexSession, Cwd: `C:\work\app`,
				PromptText: c.prompt, PromptBytes: int64(len(c.prompt)),
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

// Input the adapter cannot use is refused without quoting it, so the hook fails open.
func TestCodexRefusesUnusableInputWithoutQuotingIt(t *testing.T) {
	a := codexAdapter(t)
	for _, c := range []struct{ name, event, in string }{
		{"not JSON", "UserPromptSubmit", `AKIAIOSFODNN7EXAMPLE`},
		{"no prompt", "UserPromptSubmit", `{"session_id":"s-1","cwd":"C:\\work"}`},
		{"undeclared event", "PreToolUse", `{"session_id":"s-1","tool_name":"shell","tool_input":{"command":"echo AKIAIOSFODNN7EXAMPLE"}}`},
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

// Render answers with exit code 0: a block stops the turn with the rule's message and its link on
// its own line as the reason, or the rule's id when it has no message, since Codex ignores a block
// without a reason; a warn goes ahead with the message as a system message; allow, and any event the
// agent does not declare, is empty output.
func TestCodexRendersEachDecision(t *testing.T) {
	a := codexAdapter(t)
	block := protocol.HookDecision{Action: protocol.HookBlock, Message: "Remove the credential & try again.", Link: "https://intranet.example/ai", RuleID: "block_credentials"}
	bare := protocol.HookDecision{Action: protocol.HookBlock, RuleID: "block_unsanctioned"}
	warn := protocol.HookDecision{Action: protocol.HookWarn, Message: "This tool is not approved.", RuleID: "warn_unsanctioned"}
	allow := protocol.HookDecision{Action: protocol.HookAllow, RuleID: "policy.default"}
	for _, c := range []struct {
		name, event string
		d           protocol.HookDecision
		want        string
	}{
		{"block", "UserPromptSubmit", block, `{"decision":"block","reason":"Remove the credential & try again.\nhttps://intranet.example/ai"}`},
		{"block without a message", "UserPromptSubmit", bare, `{"decision":"block","reason":"block_unsanctioned"}`},
		{"warn", "UserPromptSubmit", warn, `{"systemMessage":"This tool is not approved."}`},
		{"warn without a message", "UserPromptSubmit", protocol.HookDecision{Action: protocol.HookWarn, RuleID: "w"}, ``},
		{"allow", "UserPromptSubmit", allow, ``},
		{"undeclared event", "PreToolUse", block, ``},
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
	if out, code := a.Allow("UserPromptSubmit"); len(out) != 0 || code != 0 {
		t.Fatalf("Allow gives %q/%d, want empty output and 0", out, code)
	}
}

// Codex stops the turn a UserPromptSubmit block asks it to stop; the agent declares no other event.
func TestCodexCanEnforceThePromptEvent(t *testing.T) {
	a := codexAdapter(t)
	for event, want := range map[string]bool{"UserPromptSubmit": true, "PreToolUse": false} {
		if got := a.CanEnforce(event); got != want {
			t.Errorf("CanEnforce(%s) = %v, want %v", event, got, want)
		}
	}
}

// Codex's terminal UI shows a block's reason under "Blocked by hook" and a systemMessage as a hook
// warning, as plain text with each line of its own. The device phase replaces these files with
// what Codex is seen to show.
func TestCodexRendersACoachingMessageWithALink(t *testing.T) {
	a := codexAdapter(t)
	for _, c := range []struct {
		action protocol.HookAction
		golden string
		shown  string
	}{
		{protocol.HookWarn, "user-prompt-submit-warn.json", "systemMessage"},
		{protocol.HookBlock, "user-prompt-submit-block.json", "reason"},
	} {
		t.Run(c.golden, func(t *testing.T) {
			renderGolden(t, a, "UserPromptSubmit", coachingDecision(t, c.action), filepath.Join("testdata/codex/render", c.golden), c.shown)
		})
	}
}

// The documented prompt, parsed by the adapter and decided by the relay under a block rule on
// credential with Codex's hooks on, renders as a blocked turn with the rule's message, and is
// recorded on tool.hook as blocked for app:codex.
func TestCodexPromptWithACredentialIsBlockedThroughTheRelay(t *testing.T) {
	a := codexAdapter(t)
	b := testBundle(protocol.ModeM1, blockCredentials)
	b.Endpoint.Tools = map[string]policy.EndpointTool{"codex": {Hooks: true}}
	sink := &memSink{}
	pipe := newPipeline(t, sink, b)
	r := newRelay(t, pipe, &stubClassifier{})

	ev, err := a.Parse("UserPromptSubmit", codexFixture(t, "user-prompt-submit.json"))
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
	if want := `{"decision":"block","reason":"Remove the credential and try again.\nhttps://intranet.example/ai"}` + "\n"; string(out) != want || code != 0 {
		t.Fatalf("rendered %q/%d, want %q/0", out, code, want)
	}
	waitServed(t, served)

	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("%d records, want 1", len(entries))
	}
	env := decodeEnvelope(t, entries[0])
	if env.Source != string(protocol.RouteToolHook) || env.ToolFingerprint != "app:codex" || env.Decision == nil ||
		env.Decision.Action != protocol.ActionBlocked || env.Decision.RuleID != "block_credentials" {
		t.Fatalf("recorded %+v (decision %+v)", env, env.Decision)
	}
}
