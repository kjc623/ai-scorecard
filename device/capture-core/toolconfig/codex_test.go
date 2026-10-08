package toolconfig

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// codexPromptCommand is the agent's hook command for testExe.
const codexPromptCommand = `cmd /c "` + testExe + `" --hook codex UserPromptSubmit`

// codexDesired is what the provider asks of the Codex writer: the hooks, running testExe.
var codexDesired = Desired{Hooks: true, HookCommand: testExe}

// codexConfig is a provider configuration whose running executable is testExe.
var codexConfig = Config{Executable: func() (string, error) { return testExe, nil }}

// customerRequirements is a requirements file a customer already manages: a comment, a constraint
// of its own, a feature it pins, managed-only hooks explicitly off, a prompt hook in an inline
// array and a tool hook as an array of tables.
const customerRequirements = "# Managed by IT security.\r\n" +
	"allowed_approval_policies = [\"on-request\", \"untrusted\"]\r\n" +
	"allow_managed_hooks_only = false\r\n" +
	"\r\n" +
	"[features]\r\n" +
	"web_search = false\r\n" +
	"\r\n" +
	"[hooks]\r\n" +
	"UserPromptSubmit = [{ hooks = [{ type = \"command\", command = 'C:\\Tools\\audit.exe prompt', timeout = 5 }] }]\r\n" +
	"\r\n" +
	"[[hooks.PreToolUse]]\r\n" +
	"matcher = \"shell\"\r\n" +
	"[[hooks.PreToolUse.hooks]]\r\n" +
	"type = \"command\"\r\n" +
	"command = 'C:\\Tools\\audit.exe tool'\r\n"

// customerPromptGroup is the customer's UserPromptSubmit group as the writer reads it.
var customerPromptGroup = map[string]any{"hooks": []any{map[string]any{
	"type": "command", "command": `C:\Tools\audit.exe prompt`, "timeout": int64(5),
}}}

// newCodexTestWriter is a Codex writer over a requirements file and a state directory in the test's
// temporary directory, with the Codex CLI installed.
func newCodexTestWriter(t *testing.T) (*Codex, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewCodexWriter(dir, filepath.Join(root, "OpenAI", "Codex", "requirements.toml"), func() bool { return true })
	return w, w.Path()
}

// readCodexDoc is the requirements file as a TOML reader sees it, arrays of tables and inline
// arrays alike.
func readCodexDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if _, err := toml.Decode(string(readFile(t, path)), &doc); err != nil {
		t.Fatalf("the requirements file is not TOML: %v", err)
	}
	return normalizeTOML(doc).(map[string]any)
}

// promptGroups is the file's hooks.UserPromptSubmit.
func promptGroups(doc map[string]any) []any {
	h, _ := doc["hooks"].(map[string]any)
	l, _ := h["UserPromptSubmit"].([]any)
	return l
}

// The agent's group runs the installed executable through a nested cmd, which both PowerShell and
// cmd hand a quoted path followed by arguments to.
func TestCodexHookCommand(t *testing.T) {
	if got := codexCommand(testExe); got != codexPromptCommand {
		t.Fatalf("command = %q, want %q", got, codexPromptCommand)
	}
	for cmd, want := range map[string]bool{
		codexPromptCommand: true,
		`cmd /c "D:\Old\capture-core.exe" --hook codex UserPromptSubmit`: true,
		`cmd /c "D:\Old\capture-core" --hook codex UserPromptSubmit`:     true,
		`cmd /c "C:\Tools\audit.exe" --hook codex UserPromptSubmit`:      false,
		`cmd /c "D:\Old\capture-core.exe" --hook codex PreToolUse`:       false,
		`"D:\Old\capture-core.exe" --hook codex UserPromptSubmit`:        false,
	} {
		if got := isCodexCommand(cmd); got != want {
			t.Errorf("isCodexCommand(%s) = %v, want %v", cmd, got, want)
		}
	}
}

// A machine with no requirements file gets one declaring the agent's UserPromptSubmit hook, with its
// timeout, and the hooks feature pinned on so a user cannot switch hooks off.
func TestCodexWritesTheHook(t *testing.T) {
	w, path := newCodexTestWriter(t)
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	doc := readCodexDoc(t, path)
	want := map[string]any{
		"features": map[string]any{"hooks": true},
		"hooks": map[string]any{"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": codexPromptCommand, "timeout": int64(5),
		}}}}},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("requirements = %#v\nwant %#v", doc, want)
	}
	if ok, err := w.Holds(codexDesired); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
}

// With a customer's requirements present, the agent's group goes after the customer's, every
// customer key, group and value stays, and a second apply changes nothing. Remove gives back the
// customer's file byte for byte, comment included.
func TestCodexKeepsTheCustomersRequirements(t *testing.T) {
	w, path := newCodexTestWriter(t)
	files := &countingFiles{}
	w.files = files
	writeFile(t, path, []byte(customerRequirements))
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	doc := readCodexDoc(t, path)
	if got := promptGroups(doc); !reflect.DeepEqual(got, []any{customerPromptGroup, codexGroup(testExe)}) {
		t.Fatalf("UserPromptSubmit = %#v", got)
	}
	h := doc["hooks"].(map[string]any)
	if tool, _ := h["PreToolUse"].([]any); len(tool) != 1 || tool[0].(map[string]any)["matcher"] != "shell" {
		t.Fatalf("the customer's PreToolUse hook is gone: %#v", h["PreToolUse"])
	}
	if f := doc["features"].(map[string]any); f["web_search"] != false || f["hooks"] != true {
		t.Fatalf("features = %#v", f)
	}
	if doc["allow_managed_hooks_only"] != false || !reflect.DeepEqual(doc["allowed_approval_policies"], []any{"on-request", "untrusted"}) {
		t.Fatalf("the customer's keys changed: %#v", doc)
	}

	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	if files.writes != 1 {
		t.Fatalf("%d writes, want 1: a file that holds the hook is not rewritten", files.writes)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerRequirements {
		t.Fatalf("after Remove the file is\n%q\nwant\n%q", got, customerRequirements)
	}
	if _, ok, _ := w.backup.load(); ok {
		t.Fatal("the backup is kept after a complete Remove")
	}
}

// Managed-only hooks set allow_managed_hooks_only; switching them off gives the key the customer's
// value back, and pinning the hooks feature over a customer's false is undone by Remove.
func TestCodexManagedOnlyAndTheFeaturePin(t *testing.T) {
	w, path := newCodexTestWriter(t)
	original := "allow_managed_hooks_only = false\n\n[features]\nhooks = false\n"
	writeFile(t, path, []byte(original))
	managed := codexDesired
	managed.ManagedOnly = true
	if err := w.Apply(managed); err != nil {
		t.Fatal(err)
	}
	doc := readCodexDoc(t, path)
	if doc["allow_managed_hooks_only"] != true || doc["features"].(map[string]any)["hooks"] != true {
		t.Fatalf("requirements = %#v, want managed-only hooks and the hooks feature on", doc)
	}
	if ok, err := w.Holds(managed); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	if doc := readCodexDoc(t, path); doc["allow_managed_hooks_only"] != false {
		t.Fatalf("allow_managed_hooks_only = %v with managed-only hooks off, want the customer's false", doc["allow_managed_hooks_only"])
	}
	if ok, _ := w.Holds(managed); ok {
		t.Fatal("Holds reports managed-only hooks the file no longer sets")
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != original {
		t.Fatalf("after Remove the file is %q, want %q", got, original)
	}
}

// The backup is the file as it was before the agent's first write, not after a later one.
func TestCodexBackupIsTakenOnce(t *testing.T) {
	w, path := newCodexTestWriter(t)
	writeFile(t, path, []byte(customerRequirements))
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("[hooks]\n"))
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	orig, ok, err := w.backup.load()
	if err != nil || !ok || !orig.Present || string(orig.Content) != customerRequirements {
		t.Fatalf("backup = %+v, %v, %v; want the customer's file", orig, ok, err)
	}
}

// A file the agent created is deleted again by Remove.
func TestCodexRemoveOfAFileThatWasAbsent(t *testing.T) {
	w, path := newCodexTestWriter(t)
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the requirements file the agent created is still there: %v", err)
	}
}

// A requirement the customer added while the agent's hook was in stays after Remove; only the
// agent's group and keys go.
func TestCodexRemoveKeepsOtherChanges(t *testing.T) {
	w, path := newCodexTestWriter(t)
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	edited := "allowed_sandbox_modes = [\"read-only\"]\n" + string(readFile(t, path))
	writeFile(t, path, []byte(edited))
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	doc := readCodexDoc(t, path)
	if want := map[string]any{"allowed_sandbox_modes": []any{"read-only"}}; !reflect.DeepEqual(doc, want) {
		t.Fatalf("after Remove the requirements are %#v, want %#v", doc, want)
	}
}

// A file Codex could not read as requirements the agent merges into is left as it is, and the write
// fails.
func TestCodexLeavesAFileThatIsNotRequirements(t *testing.T) {
	for name, content := range map[string]string{
		"not TOML":                           "hooks = [",
		"hooks not a table":                  "hooks = 1\n",
		"features not a table":               "features = [\"hooks\"]\n",
		"UserPromptSubmit not an array":      "[hooks.UserPromptSubmit]\ncommand = \"x\"\n",
		"UserPromptSubmit holds a non-table": "[hooks]\nUserPromptSubmit = [\"x\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			w, path := newCodexTestWriter(t)
			writeFile(t, path, []byte(content))
			if err := w.Apply(codexDesired); err == nil {
				t.Fatal("Apply accepted the file")
			}
			if got := string(readFile(t, path)); got != content {
				t.Fatalf("the file was changed to %q", got)
			}
		})
	}
}

// Remove without a backup has nothing to restore and changes nothing.
func TestCodexRemoveWithoutBackupChangesNothing(t *testing.T) {
	w, path := newCodexTestWriter(t)
	writeFile(t, path, []byte(customerRequirements))
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerRequirements {
		t.Fatalf("the file was changed to %q", got)
	}
}

// A group the agent wrote from an earlier install path is replaced in place, before the customer's
// group it preceded, and Remove takes out the current one.
func TestCodexReplacesAnEarlierInstallsGroup(t *testing.T) {
	w, path := newCodexTestWriter(t)
	old := "[[hooks.UserPromptSubmit]]\n[[hooks.UserPromptSubmit.hooks]]\ntype = \"command\"\ncommand = 'cmd /c \"D:\\Old\\capture-core.exe\" --hook codex UserPromptSubmit'\n\n" +
		"[[hooks.UserPromptSubmit]]\n[[hooks.UserPromptSubmit.hooks]]\ntype = \"command\"\ncommand = 'C:\\Tools\\audit.exe prompt'\ntimeout = 5\n"
	writeFile(t, path, []byte(old))
	if err := w.Apply(codexDesired); err != nil {
		t.Fatal(err)
	}
	if got := promptGroups(readCodexDoc(t, path)); !reflect.DeepEqual(got, []any{codexGroup(testExe), customerPromptGroup}) {
		t.Fatalf("UserPromptSubmit = %#v", got)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := promptGroups(readCodexDoc(t, path)); !reflect.DeepEqual(got, []any{customerPromptGroup}) {
		t.Fatalf("UserPromptSubmit = %#v after Remove", got)
	}
}

// codexHooksBundle switches the hook relay and the Codex CLI's hooks on.
func codexHooksBundle() policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: protocol.ModeM1,
		Endpoint: policy.EndpointPolicy{
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{"codex": {Hooks: true}},
		},
	}
}

// supportedCodex is the Codex CLI on a platform the agent writes its hooks on.
var supportedCodex = func() tool { t := codexTool; t.supported = true; return t }()

// tool_config_codex follows endpoint.hooks.enabled and endpoint.tools.codex.hooks, not the OTel
// switches.
func TestCodexProviderNameAndSwitch(t *testing.T) {
	p := NewCodex(&Codex{}, Config{})
	if p.Name() != protocol.CollectorToolConfigCodex {
		t.Fatalf("Name = %s", p.Name())
	}
	b := codexHooksBundle()
	if !p.Enabled(&b) {
		t.Fatal("not enabled with the hook relay and Codex's hooks on")
	}
	if p.Enabled(nil) {
		t.Fatal("enabled with no bundle in force")
	}
	off := codexHooksBundle()
	off.Endpoint.Tools["codex"] = policy.EndpointTool{OTel: true}
	off.Endpoint.OTel.Enabled = true
	if p.Enabled(&off) {
		t.Fatal("enabled with Codex's hooks off")
	}
	off = codexHooksBundle()
	off.Endpoint.Hooks.Enabled = false
	if p.Enabled(&off) {
		t.Fatal("enabled with the hook relay off: a tool switch takes effect only while its collector is on")
	}
}

// The desired state is the hook command and managed-only hooks, whatever the OTel switches say.
func TestCodexDesiredIsHooksAndManagedOnly(t *testing.T) {
	p := newProvider(supportedCodex, &Codex{}, Config{
		Token:      func() string { return "token" },
		Executable: func() (string, error) { return testExe, nil },
	})
	b := codexHooksBundle()
	b.Endpoint.OTel = policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318"}
	b.Endpoint.Tools["codex"] = policy.EndpointTool{OTel: true, Hooks: true}
	if got := p.desired(&b); got != codexDesired {
		t.Fatalf("desired = %+v, want %+v", got, codexDesired)
	}
	b.Endpoint.Hooks.ManagedOnly = true
	want := codexDesired
	want.ManagedOnly = true
	if got := p.desired(&b); got != want {
		t.Fatalf("desired = %+v, want %+v", got, want)
	}
}

// Under the registry, switching Codex's hooks on writes the hook beside the customer's and reports
// healthy; switching them off restores the customer's file, without a restart.
func TestCodexProviderFollowsThePolicyToggle(t *testing.T) {
	w, path := newCodexTestWriter(t)
	writeFile(t, path, []byte(customerRequirements))
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newProvider(supportedCodex, w, codexConfig)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })

	reg.ApplyPolicy(codexHooksBundle())
	if got := promptGroups(readCodexDoc(t, path)); !reflect.DeepEqual(got, []any{customerPromptGroup, codexGroup(testExe)}) {
		t.Fatalf("UserPromptSubmit = %#v after the switch went on", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s, want healthy", h.State, h.Detail)
	}
	off := codexHooksBundle()
	off.Endpoint.Tools["codex"] = policy.EndpointTool{}
	reg.ApplyPolicy(off)
	if got := string(readFile(t, path)); got != customerRequirements {
		t.Fatalf("after the switch went off the file is %q, want the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// The row's states: absent/tool_not_installed without the Codex CLI, degraded/config_write_failed
// when the write fails or the file loses the hook, healthy while the file holds it, and
// absent/tool_version_unsupported where the agent does not write it.
func TestCodexProviderHealth(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		w, path := newCodexTestWriter(t)
		w.installed = func() bool { return false }
		p := newProvider(supportedCodex, w, codexConfig)
		_ = p.ApplyPolicy(codexHooksBundle())
		_ = p.Start(context.Background())
		if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolNotInstalled {
			t.Fatalf("health = %s/%s, want absent/tool_not_installed", h.State, h.Detail)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("the requirements file was written for a tool that is not installed")
		}
	})
	t.Run("write fails", func(t *testing.T) {
		w, _ := newCodexTestWriter(t)
		w.files = failingFiles{}
		p := newProvider(supportedCodex, w, codexConfig)
		_ = p.ApplyPolicy(codexHooksBundle())
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start failed on a write failure: %v", err)
		}
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed || h.Counters[protocol.CounterErrors] != 1 {
			t.Fatalf("health = %s/%s errors %d, want degraded/config_write_failed and 1", h.State, h.Detail, h.Counters[protocol.CounterErrors])
		}
	})
	t.Run("hook removed", func(t *testing.T) {
		w, path := newCodexTestWriter(t)
		p := newProvider(supportedCodex, w, codexConfig)
		_ = p.ApplyPolicy(codexHooksBundle())
		_ = p.Start(context.Background())
		t.Cleanup(func() { _ = p.Stop(context.Background()) })
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
		}
		writeFile(t, path, []byte("[features]\nhooks = true\n"))
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
			t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
		}
	})
	t.Run("unsupported platform", func(t *testing.T) {
		w, _ := newCodexTestWriter(t)
		unsupported := codexTool
		unsupported.supported = false
		p := newProvider(unsupported, w, codexConfig)
		_ = p.ApplyPolicy(codexHooksBundle())
		_ = p.Start(context.Background())
		if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolVersionUnsupported {
			t.Fatalf("health = %s/%s, want absent/tool_version_unsupported", h.State, h.Detail)
		}
	})
}
