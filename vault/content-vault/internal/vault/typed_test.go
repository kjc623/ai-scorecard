package vault

import "testing"

func TestTypedTextIsWhatThePersonTyped(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"extracted prompt text is kept", "What is the capital of Australia", "What is the capital of Australia"},
		{"injected context is removed", "<system-reminder>\nGenerated with [Claude Code](https://claude.com/claude-code)\n</system-reminder>What is the capital of Australia", "What is the capital of Australia"},
		{
			"a request body yields its latest user message",
			`{"system":"Generated with [Claude Code](https://claude.com/claude-code)","messages":[{"role":"user","content":"an earlier turn"},{"role":"assistant","content":"an answer"},{"role":"user","content":[{"type":"text","text":"<system-reminder>context</system-reminder>"},{"type":"text","text":"What is the capital of Australia"}]}]}`,
			"What is the capital of Australia",
		},
		{
			"a tool result is not typed text, so the turn before it is used",
			`{"messages":[{"role":"user","content":"run the tests"},{"role":"user","content":[{"type":"tool_result","content":"ok"}]}]}`,
			"run the tests",
		},
		{"telemetry has nothing typed", `{"events":[{"event_type":"startup"}]}`, ""},
		{"a body with no user message has nothing typed", `{"messages":[{"role":"assistant","content":"hello"}]}`, ""},
		{"text that only looks like JSON is still a prompt", "{not json", "{not json"},
	}
	for _, c := range cases {
		if got := typedText(c.content); got != c.want {
			t.Errorf("%s: typedText = %q, want %q", c.name, got, c.want)
		}
	}
}
