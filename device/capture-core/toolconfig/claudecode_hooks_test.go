package toolconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// testExe is where the tests pretend the agent is installed.
const testExe = `C:\Program Files\Shadow AI Capture\bin\capture-core.exe`

func hooksDesired(managedOnly bool) Desired {
	return Desired{Hooks: true, HookCommand: testExe, ManagedOnly: managedOnly}
}

// customerHooksFile holds an unrelated key and the customer's own hooks on both of the agent's
// events.
const customerHooksFile = `{
  "model": "claude-sonnet",
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "C:\\corp\\audit.exe prompt"}]}],
    "PreToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "C:\\corp\\audit.exe write", "timeout": 5}]}]
  }
}
`

const (
	customerPromptHook = `{"hooks":[{"type":"command","command":"C:\\corp\\audit.exe prompt"}]}`
	customerToolHook   = `{"matcher":"Write","hooks":[{"type":"command","command":"C:\\corp\\audit.exe write","timeout":5}]}`
)

// agentGroup is the matcher group the agent declares for event, as Claude Code's hooks reference
// documents a command hook in exec form.
func agentGroup(event, command string) string {
	matcher := ""
	if event == "PreToolUse" {
		matcher = `"matcher":"^(Bash|PowerShell|WebFetch|mcp__.*)$",`
	}
	cmd, _ := json.Marshal(command)
	return `{` + matcher + `"hooks":[{"type":"command","command":` + string(cmd) + `,"args":["--hook","claude_code","` + event + `"],"timeout":1}]}`
}

// managedFile is the part of the managed settings the hook tests read.
type managedFile struct {
	Model       string                       `json:"model"`
	Env         map[string]string            `json:"env"`
	Hooks       map[string][]json.RawMessage `json:"hooks"`
	ManagedOnly *bool                        `json:"allowManagedHooksOnly"`
}

func readManaged(t *testing.T, path string) managedFile {
	t.Helper()
	var m managedFile
	if err := json.Unmarshal(bytes.TrimPrefix(readFile(t, path), utf8BOM), &m); err != nil {
		t.Fatalf("the managed file is not JSON: %v", err)
	}
	return m
}

// checkGroups fails unless the event's matcher groups are want, in order.
func checkGroups(t *testing.T, m managedFile, event string, want ...string) {
	t.Helper()
	got := m.Hooks[event]
	if len(got) != len(want) {
		t.Fatalf("%s holds %d groups, want %d: %s", event, len(got), len(want), got)
	}
	for i := range want {
		if !equalJSON(got[i], json.RawMessage(want[i])) {
			t.Fatalf("%s[%d] = %s, want %s", event, i, got[i], want[i])
		}
	}
}

// The agent's hooks join the customer's on both events; the unrelated key and the customer's hooks
// survive, allowManagedHooksOnly appears only with managed-only, and Remove restores the file.
func TestClaudeCodeHooksMergeBesideTheCustomers(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerHooksFile))

	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	m := readManaged(t, path)
	if m.Model != "claude-sonnet" {
		t.Fatalf("model = %q, want the customer's", m.Model)
	}
	checkGroups(t, m, "UserPromptSubmit", customerPromptHook, agentGroup("UserPromptSubmit", testExe))
	checkGroups(t, m, "PreToolUse", customerToolHook, agentGroup("PreToolUse", testExe))
	if m.ManagedOnly != nil {
		t.Fatal("allowManagedHooksOnly is set without managed-only")
	}
	if m.Env != nil {
		t.Fatalf("env = %v with only the hooks on", m.Env)
	}
	if ok, err := w.Holds(hooksDesired(false)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
	if ok, _ := w.Holds(hooksDesired(true)); ok {
		t.Fatal("Holds says managed-only is on while the file does not set it")
	}

	if err := w.Apply(hooksDesired(true)); err != nil {
		t.Fatal(err)
	}
	m = readManaged(t, path)
	if m.ManagedOnly == nil || !*m.ManagedOnly {
		t.Fatal("allowManagedHooksOnly is not true with managed-only")
	}
	checkGroups(t, m, "UserPromptSubmit", customerPromptHook, agentGroup("UserPromptSubmit", testExe))
	checkGroups(t, m, "PreToolUse", customerToolHook, agentGroup("PreToolUse", testExe))
	if ok, err := w.Holds(hooksDesired(true)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v with managed-only", ok, err)
	}

	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	if m := readManaged(t, path); m.ManagedOnly != nil {
		t.Fatal("allowManagedHooksOnly stayed after managed-only went off")
	}

	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerHooksFile {
		t.Fatalf("after Remove the file is\n%s\nwant the customer's", got)
	}
	if _, ok, _ := w.backup.load(); ok {
		t.Fatal("the backup is still kept after the file was restored")
	}
}

// Without a file, the agent creates one holding only its hooks, and Remove deletes it.
func TestClaudeCodeHooksIntoNoFile(t *testing.T) {
	w, path := newTestWriter(t)
	if err := w.Apply(hooksDesired(true)); err != nil {
		t.Fatal(err)
	}
	m := readManaged(t, path)
	checkGroups(t, m, "UserPromptSubmit", agentGroup("UserPromptSubmit", testExe))
	checkGroups(t, m, "PreToolUse", agentGroup("PreToolUse", testExe))
	if m.ManagedOnly == nil || !*m.ManagedOnly {
		t.Fatal("allowManagedHooksOnly is not set")
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the managed file is still there (%v)", err)
	}
}

// OTel and hooks switch independently: each set of keys comes and goes on its own, and what the
// other leaves stays.
func TestClaudeCodeOTelAndHooksSwitchIndependently(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	both := testDesired(true)
	both.Hooks, both.HookCommand = true, testExe

	if err := w.Apply(both); err != nil {
		t.Fatal(err)
	}
	m := readManaged(t, path)
	if m.Env["OTEL_LOG_USER_PROMPTS"] != "1" || m.Env["HTTPS_PROXY"] != "http://proxy.corp.example:8080" {
		t.Fatalf("env = %v", m.Env)
	}
	checkGroups(t, m, "PreToolUse", agentGroup("PreToolUse", testExe))

	// OTel only: the hooks go, and the file has no hooks object again.
	if err := w.Apply(testDesired(true)); err != nil {
		t.Fatal(err)
	}
	m = readManaged(t, path)
	if m.Hooks != nil {
		t.Fatalf("hooks = %v with only OTel on", m.Hooks)
	}
	if m.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatalf("env = %v, want the agent's telemetry", m.Env)
	}

	// Hooks only: the telemetry variables go back to the customer's.
	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	m = readManaged(t, path)
	want := map[string]string{"HTTPS_PROXY": "http://proxy.corp.example:8080", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.corp.example"}
	if len(m.Env) != len(want) || m.Env["HTTPS_PROXY"] != want["HTTPS_PROXY"] || m.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != want["OTEL_EXPORTER_OTLP_ENDPOINT"] {
		t.Fatalf("env = %v, want the customer's %v", m.Env, want)
	}
	checkGroups(t, m, "UserPromptSubmit", agentGroup("UserPromptSubmit", testExe))

	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerFile {
		t.Fatalf("after Remove the file is %q, want the customer's", got)
	}
}

// The customer's own allowManagedHooksOnly is the agent's to set while managed-only is on, and is
// back as it was once it is off.
func TestClaudeCodeKeepsTheCustomersManagedOnly(t *testing.T) {
	const original = `{"allowManagedHooksOnly": false, "model": "claude-opus"}`
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(original))
	if err := w.Apply(hooksDesired(true)); err != nil {
		t.Fatal(err)
	}
	if m := readManaged(t, path); m.ManagedOnly == nil || !*m.ManagedOnly {
		t.Fatal("allowManagedHooksOnly is not true with managed-only")
	}
	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	if m := readManaged(t, path); m.ManagedOnly == nil || *m.ManagedOnly {
		t.Fatal("the customer's allowManagedHooksOnly false is not back")
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != original {
		t.Fatalf("after Remove the file is %q", got)
	}
}

// An agent installed elsewhere replaces its earlier entries in place; a hook that only resembles the
// agent's is the customer's and stays.
func TestClaudeCodeHooksReplaceTheAgentsOwnEntries(t *testing.T) {
	const lookalike = `{"hooks":[{"type":"command","command":"C:\\corp\\wrapper.exe","args":["--hook","claude_code","PreToolUse"]}]}`
	w, path := newTestWriter(t)
	old := `D:\Old\bin\capture-core.exe`
	writeFile(t, path, []byte(`{"hooks":{"PreToolUse":[`+agentGroup("PreToolUse", old)+`,`+lookalike+`,`+agentGroup("PreToolUse", old)+`]}}`))
	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	m := readManaged(t, path)
	checkGroups(t, m, "PreToolUse", agentGroup("PreToolUse", testExe), lookalike)
	checkGroups(t, m, "UserPromptSubmit", agentGroup("UserPromptSubmit", testExe))

	files := &countingFiles{}
	w.files = files
	if err := w.Apply(hooksDesired(false)); err != nil {
		t.Fatal(err)
	}
	if files.writes != 0 {
		t.Fatal("a file already holding the agent's hooks was rewritten")
	}
}

// The hooks are not declared without the agent's own path.
func TestClaudeCodeHooksNeedTheExecutable(t *testing.T) {
	w, path := newTestWriter(t)
	if err := w.Apply(Desired{Hooks: true}); err == nil {
		t.Fatal("Apply declared hooks without a command")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the managed file was written")
	}
}
