package toolconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// The agent's two hook commands for testExe.
const (
	promptCommand = `"` + testExe + `" --hook cursor beforeSubmitPrompt`
	mcpCommand    = `"` + testExe + `" --hook cursor beforeMCPExecution`
)

// cursorDesired is what the provider asks of Cursor's writer: the hooks, running testExe.
var cursorDesired = Desired{Hooks: true, HookCommand: testExe}

// cursorConfig is a provider configuration whose running executable is testExe.
var cursorConfig = Config{Executable: func() (string, error) { return testExe, nil }}

// customerHooks is an enterprise hooks file a customer already manages: a prompt hook of its own,
// a hook on an event the agent does not use, and a key the agent does not know.
const customerHooks = "{\r\n  \"version\": 1,\r\n  \"hooks\": {\r\n    \"beforeSubmitPrompt\": [\r\n      { \"command\": \"C:\\\\Tools\\\\audit.exe prompt\", \"timeout\": 5 }\r\n    ],\r\n    \"stop\": [ { \"command\": \"C:\\\\Tools\\\\audit.exe stop\" } ]\r\n  },\r\n  \"x-owner\": \"it-security\"\r\n}\r\n"

// newCursorTestWriter is a Cursor writer over a hooks file and a state directory in the test's
// temporary directory, with Cursor installed.
func newCursorTestWriter(t *testing.T) (*Cursor, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewCursorWriter(dir, filepath.Join(root, "Cursor", "hooks.json"), func() bool { return true })
	return w, w.Path()
}

// cursorDoc is the hooks file as Cursor reads it.
type cursorDoc struct {
	Version int                                     `json:"version"`
	Hooks   map[string][]map[string]json.RawMessage `json:"hooks"`
}

func readCursorDoc(t *testing.T, path string) cursorDoc {
	t.Helper()
	var d cursorDoc
	if err := json.Unmarshal(readFile(t, path), &d); err != nil {
		t.Fatalf("the hooks file is not JSON: %v", err)
	}
	return d
}

// commands lists an event's hook commands in order.
func commands(t *testing.T, d cursorDoc, event string) []string {
	t.Helper()
	var out []string
	for _, e := range d.Hooks[event] {
		var c string
		if err := json.Unmarshal(e["command"], &c); err != nil {
			t.Fatalf("a %s entry has no command: %v", event, err)
		}
		out = append(out, c)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A machine with no enterprise hooks file gets one holding the schema version and the agent's two
// entries, each running the installed executable with the tool and the event.
func TestCursorWritesTheHookEntries(t *testing.T) {
	w, path := newCursorTestWriter(t)
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	d := readCursorDoc(t, path)
	if d.Version != 1 {
		t.Fatalf("version = %d, want 1", d.Version)
	}
	if got := commands(t, d, "beforeSubmitPrompt"); !equal(got, []string{promptCommand}) {
		t.Fatalf("beforeSubmitPrompt = %q", got)
	}
	if got := commands(t, d, "beforeMCPExecution"); !equal(got, []string{mcpCommand}) {
		t.Fatalf("beforeMCPExecution = %q", got)
	}
	if ok, err := w.Holds(cursorDesired); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
}

// With a customer's hooks present, the agent's entries go after theirs, every customer entry, key
// and value stays, and a second apply changes nothing. Remove gives back the customer's file byte
// for byte.
func TestCursorKeepsTheCustomersHooks(t *testing.T) {
	w, path := newCursorTestWriter(t)
	files := &countingFiles{}
	w.files = files
	writeFile(t, path, []byte(customerHooks))
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	d := readCursorDoc(t, path)
	if got := commands(t, d, "beforeSubmitPrompt"); !equal(got, []string{`C:\Tools\audit.exe prompt`, promptCommand}) {
		t.Fatalf("beforeSubmitPrompt = %q", got)
	}
	if string(d.Hooks["beforeSubmitPrompt"][0]["timeout"]) != "5" {
		t.Fatalf("the customer's entry lost its timeout: %v", d.Hooks["beforeSubmitPrompt"][0])
	}
	if got := commands(t, d, "stop"); !equal(got, []string{`C:\Tools\audit.exe stop`}) {
		t.Fatalf("stop = %q", got)
	}
	if got := commands(t, d, "beforeMCPExecution"); !equal(got, []string{mcpCommand}) {
		t.Fatalf("beforeMCPExecution = %q", got)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, path), &doc); err != nil || string(doc["x-owner"]) != `"it-security"` {
		t.Fatalf("the customer's key is gone: %s (%v)", doc["x-owner"], err)
	}

	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	if files.writes != 1 {
		t.Fatalf("%d writes, want 1: a file that holds the entries is not rewritten", files.writes)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerHooks {
		t.Fatalf("after Remove the file is\n%q\nwant\n%q", got, customerHooks)
	}
	if _, ok, _ := w.backup.load(); ok {
		t.Fatal("the backup is kept after a complete Remove")
	}
}

// The backup is the file as it was before the agent's first write, not after a later one.
func TestCursorBackupIsTakenOnce(t *testing.T) {
	w, path := newCursorTestWriter(t)
	writeFile(t, path, []byte(customerHooks))
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte(`{"version":1,"hooks":{}}`))
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	orig, ok, err := w.backup.load()
	if err != nil || !ok || !orig.Present || string(orig.Content) != customerHooks {
		t.Fatalf("backup = %+v, %v, %v; want the customer's file", orig, ok, err)
	}
}

// A file the agent created is deleted again by Remove.
func TestCursorRemoveOfAFileThatWasAbsent(t *testing.T) {
	w, path := newCursorTestWriter(t)
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the hooks file the agent created is still there: %v", err)
	}
}

// A hook the customer added while the agent's entries were in stays after Remove, with the version
// it needs; only the agent's entries go.
func TestCursorRemoveKeepsOtherChanges(t *testing.T) {
	w, path := newCursorTestWriter(t)
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	d := readCursorDoc(t, path)
	edited, err := json.Marshal(map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"beforeSubmitPrompt": []map[string]string{{"command": promptCommand}, {"command": `C:\Tools\audit.exe prompt`}},
			"beforeMCPExecution": d.Hooks["beforeMCPExecution"],
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, edited)
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	d = readCursorDoc(t, path)
	if d.Version != 1 {
		t.Fatalf("version = %d, want 1 beside the customer's hook", d.Version)
	}
	if got := commands(t, d, "beforeSubmitPrompt"); !equal(got, []string{`C:\Tools\audit.exe prompt`}) {
		t.Fatalf("beforeSubmitPrompt = %q", got)
	}
	if _, ok := d.Hooks["beforeMCPExecution"]; ok {
		t.Fatalf("the agent's beforeMCPExecution array is left: %v", d.Hooks)
	}
}

// A file Cursor could not read as hooks is left as it is, and the write fails.
func TestCursorLeavesAFileThatIsNotHooks(t *testing.T) {
	for name, content := range map[string]string{
		"not an object":       `["version", 1]`,
		"hooks not an object": `{"version":1,"hooks":[]}`,
		"event not an array":  `{"version":1,"hooks":{"beforeSubmitPrompt":{"command":"x"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			w, path := newCursorTestWriter(t)
			writeFile(t, path, []byte(content))
			if err := w.Apply(cursorDesired); err == nil {
				t.Fatal("Apply accepted the file")
			}
			if got := string(readFile(t, path)); got != content {
				t.Fatalf("the file was changed to %q", got)
			}
		})
	}
}

// Remove without a backup has nothing to restore and changes nothing.
func TestCursorRemoveWithoutBackupChangesNothing(t *testing.T) {
	w, path := newCursorTestWriter(t)
	writeFile(t, path, []byte(customerHooks))
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerHooks {
		t.Fatalf("the file was changed to %q", got)
	}
}

// cursorHooksBundle switches the hook relay and Cursor's hooks on.
func cursorHooksBundle() policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: protocol.ModeM1,
		Endpoint: policy.EndpointPolicy{
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{"cursor": {Hooks: true}},
		},
	}
}

// supportedCursor is Cursor on a platform the agent writes its hooks on.
var supportedCursor = func() tool { t := cursorTool; t.supported = true; return t }()

// tool_config_cursor follows endpoint.hooks.enabled and endpoint.tools.cursor.hooks, not the OTel
// switches.
func TestCursorProviderNameAndSwitch(t *testing.T) {
	p := NewCursor(&Cursor{}, Config{})
	if p.Name() != protocol.CollectorToolConfigCursor {
		t.Fatalf("Name = %s", p.Name())
	}
	b := cursorHooksBundle()
	if !p.Enabled(&b) {
		t.Fatal("not enabled with the hook relay and Cursor's hooks on")
	}
	if p.Enabled(nil) {
		t.Fatal("enabled with no bundle in force")
	}
	off := cursorHooksBundle()
	off.Endpoint.Tools["cursor"] = policy.EndpointTool{OTel: true}
	off.Endpoint.OTel.Enabled = true
	if p.Enabled(&off) {
		t.Fatal("enabled with Cursor's hooks off")
	}
	off = cursorHooksBundle()
	off.Endpoint.Hooks.Enabled = false
	if p.Enabled(&off) {
		t.Fatal("enabled with the hook relay off: a tool switch takes effect only while its collector is on")
	}
}

// Under the registry, switching Cursor's hooks on writes the entries beside the customer's and
// reports healthy; switching them off restores the customer's file, without a restart.
func TestCursorProviderFollowsThePolicyToggle(t *testing.T) {
	w, path := newCursorTestWriter(t)
	writeFile(t, path, []byte(customerHooks))
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newProvider(supportedCursor, w, cursorConfig)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })

	reg.ApplyPolicy(cursorHooksBundle())
	if got := commands(t, readCursorDoc(t, path), "beforeSubmitPrompt"); !equal(got, []string{`C:\Tools\audit.exe prompt`, promptCommand}) {
		t.Fatalf("beforeSubmitPrompt = %q after the switch went on", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCursor); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s, want healthy", h.State, h.Detail)
	}
	off := cursorHooksBundle()
	off.Endpoint.Tools["cursor"] = policy.EndpointTool{}
	reg.ApplyPolicy(off)
	if got := string(readFile(t, path)); got != customerHooks {
		t.Fatalf("after the switch went off the file is %q, want the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCursor); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// The row's states: absent/tool_not_installed without Cursor, degraded/config_write_failed when
// the write fails or the file loses the entries, healthy while the file holds them.
func TestCursorProviderHealth(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		w, path := newCursorTestWriter(t)
		w.installed = func() bool { return false }
		p := newProvider(supportedCursor, w, cursorConfig)
		_ = p.ApplyPolicy(cursorHooksBundle())
		_ = p.Start(context.Background())
		if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolNotInstalled {
			t.Fatalf("health = %s/%s, want absent/tool_not_installed", h.State, h.Detail)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("the hooks file was written for a tool that is not installed")
		}
	})
	t.Run("write fails", func(t *testing.T) {
		w, _ := newCursorTestWriter(t)
		w.files = failingFiles{}
		p := newProvider(supportedCursor, w, cursorConfig)
		_ = p.ApplyPolicy(cursorHooksBundle())
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start failed on a write failure: %v", err)
		}
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed || h.Counters[protocol.CounterErrors] != 1 {
			t.Fatalf("health = %s/%s errors %d, want degraded/config_write_failed and 1", h.State, h.Detail, h.Counters[protocol.CounterErrors])
		}
	})
	t.Run("entries removed", func(t *testing.T) {
		w, path := newCursorTestWriter(t)
		p := newProvider(supportedCursor, w, cursorConfig)
		_ = p.ApplyPolicy(cursorHooksBundle())
		_ = p.Start(context.Background())
		t.Cleanup(func() { _ = p.Stop(context.Background()) })
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
		}
		writeFile(t, path, []byte(`{"version":1,"hooks":{}}`))
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
			t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
		}
	})
	t.Run("unsupported platform", func(t *testing.T) {
		w, _ := newCursorTestWriter(t)
		unsupported := cursorTool
		unsupported.supported = false
		p := newProvider(unsupported, w, cursorConfig)
		_ = p.ApplyPolicy(cursorHooksBundle())
		_ = p.Start(context.Background())
		if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolVersionUnsupported {
			t.Fatalf("health = %s/%s, want absent/tool_version_unsupported", h.State, h.Detail)
		}
	})
}

// An entry the agent wrote from an earlier install path is replaced in place, after the customer's
// entry it followed, and Remove takes out the current one.
func TestCursorReplacesAnEarlierInstallsEntry(t *testing.T) {
	w, path := newCursorTestWriter(t)
	old := `{"version":1,"hooks":{"beforeSubmitPrompt":[{"command":"\"D:\\Old\\capture-core.exe\" --hook cursor beforeSubmitPrompt"},{"command":"C:\\Tools\\audit.exe prompt"}]}}`
	writeFile(t, path, []byte(old))
	if err := w.Apply(cursorDesired); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, readCursorDoc(t, path), "beforeSubmitPrompt"); !equal(got, []string{promptCommand, `C:\Tools\audit.exe prompt`}) {
		t.Fatalf("beforeSubmitPrompt = %q", got)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, readCursorDoc(t, path), "beforeSubmitPrompt"); !equal(got, []string{`C:\Tools\audit.exe prompt`}) {
		t.Fatalf("beforeSubmitPrompt = %q after Remove", got)
	}
}

// Cursor has hooks only: its desired state is the hook command, whatever the OTel switches and
// managed-only hooks say.
func TestCursorDesiredIsHooksOnly(t *testing.T) {
	p := newProvider(supportedCursor, &Cursor{}, Config{
		Token:      func() string { return "token" },
		Executable: func() (string, error) { return testExe, nil },
	})
	b := cursorHooksBundle()
	b.Endpoint.OTel = policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318"}
	b.Endpoint.Tools["cursor"] = policy.EndpointTool{OTel: true, Hooks: true}
	b.Endpoint.Hooks.ManagedOnly = true
	if got := p.desired(&b); got != cursorDesired {
		t.Fatalf("desired = %+v, want %+v", got, cursorDesired)
	}
}
