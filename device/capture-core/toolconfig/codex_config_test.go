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

// codexOTelDesired is what the provider asks of Codex's config with OTel on.
func codexOTelDesired(logPrompts bool) Desired {
	return Desired{OTel: true, HTTPListen: "127.0.0.1:47318", Token: testToken, LogPrompts: logPrompts}
}

// codexOTelConfig is a provider configuration with the test token and executable.
var codexOTelConfig = Config{
	Token:      func() string { return testToken },
	Executable: func() (string, error) { return testExe, nil },
}

// customerConfig is a system config a customer already manages: settings and tables the agent does
// not own, an [otel] table with a key of theirs and an exporter of their own, and comments.
const customerConfig = "# Managed by IT.\r\n" +
	"model = \"gpt-5.5\"\r\n" +
	"approval_policy = \"on-request\"\r\n" +
	"\r\n" +
	"[otel]\r\n" +
	"environment = \"prod\"\r\n" +
	"exporter = { otlp-grpc = { endpoint = \"https://collector.example:4317\" } }\r\n" +
	"\r\n" +
	"[mcp_servers.docs]\r\n" +
	"command = \"docs-server\"\r\n" +
	"args = [\"--port\", \"7000\"]\r\n" +
	"\r\n" +
	"[sandbox_workspace_write]\r\n" +
	"network_access = false\r\n"

// newCodexConfigTestWriter is a config writer over a file and a state directory in the test's
// temporary directory, with the users' config files those in users.
func newCodexConfigTestWriter(t *testing.T, users ...string) (*CodexConfig, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewCodexConfigWriter(dir, filepath.Join(root, "OpenAI", "Codex", "config.toml"), func() []string { return users })
	return w, w.Path()
}

// newCodexFilesTestWriter is both of Codex's files in one folder, with Codex installed, and the
// users' config files those in users. It returns the requirements and config paths.
func newCodexFilesTestWriter(t *testing.T, users ...string) (*CodexFiles, string, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "OpenAI", "Codex")
	req := NewCodexWriter(dir, filepath.Join(folder, "requirements.toml"), func() bool { return true })
	cfg := NewCodexConfigWriter(dir, filepath.Join(folder, "config.toml"), func() []string { return users })
	return NewCodexFiles(req, cfg), req.Path(), cfg.Path()
}

// readCodexConfig decodes the config file as TOML.
func readCodexConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("the config is not TOML: %v", err)
	}
	return doc
}

// codexOTel is the decoded config's [otel] table.
func codexOTel(t *testing.T, path string) map[string]any {
	t.Helper()
	otel, ok := readCodexConfig(t, path)["otel"].(map[string]any)
	if !ok {
		t.Fatal("the config has no [otel] table")
	}
	return otel
}

// The agent's log exporter is OTLP/HTTP with protobuf bodies to the receiver's logs path, with the
// bearer token, and prompt logging as asked; nothing else is in a file the agent created.
func TestCodexConfigWritesTheOTelKeys(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	if err := w.Apply(codexOTelDesired(true)); err != nil {
		t.Fatal(err)
	}
	if doc := readCodexConfig(t, path); len(doc) != 1 {
		t.Fatalf("config = %v, want only [otel]", doc)
	}
	otel := codexOTel(t, path)
	if len(otel) != 2 || otel["log_user_prompt"] != true {
		t.Fatalf("[otel] = %v, want exporter and log_user_prompt = true", otel)
	}
	http, _ := otel["exporter"].(map[string]any)["otlp-http"].(map[string]any)
	if http["endpoint"] != "http://127.0.0.1:47318/v1/logs" || http["protocol"] != "binary" {
		t.Fatalf("otlp-http = %v", http)
	}
	if h, _ := http["headers"].(map[string]any); len(h) != 1 || h["Authorization"] != "Bearer "+testToken {
		t.Fatalf("headers = %v", http["headers"])
	}
	if ok, err := w.Holds(codexOTelDesired(true)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
	if ok, _ := w.Holds(codexOTelDesired(false)); ok {
		t.Fatal("Holds says prompt logging is off while the file switches it on")
	}
}

// In a customer's file the agent sets its two [otel] keys and keeps every other setting, table and
// [otel] key; a second apply writes nothing; Remove gives back the customer's file byte for byte,
// comments included.
func TestCodexConfigMergesBesideUnrelatedTables(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	files := &countingFiles{}
	w.files = files
	writeFile(t, path, []byte(customerConfig))
	if err := w.Apply(codexOTelDesired(false)); err != nil {
		t.Fatal(err)
	}
	doc := readCodexConfig(t, path)
	if doc["model"] != "gpt-5.5" || doc["approval_policy"] != "on-request" {
		t.Fatalf("the customer's settings changed: %v", doc)
	}
	docs, _ := doc["mcp_servers"].(map[string]any)["docs"].(map[string]any)
	if docs["command"] != "docs-server" || !reflect.DeepEqual(docs["args"], []any{"--port", "7000"}) {
		t.Fatalf("the customer's MCP server changed: %v", docs)
	}
	if sw, _ := doc["sandbox_workspace_write"].(map[string]any); sw["network_access"] != false {
		t.Fatalf("the customer's sandbox table changed: %v", doc["sandbox_workspace_write"])
	}
	otel := codexOTel(t, path)
	if otel["environment"] != "prod" || otel["log_user_prompt"] != false {
		t.Fatalf("[otel] = %v", otel)
	}
	if _, ok := otel["exporter"].(map[string]any)["otlp-grpc"]; ok {
		t.Fatalf("the customer's exporter is still in force: %v", otel["exporter"])
	}

	if err := w.Apply(codexOTelDesired(false)); err != nil {
		t.Fatal(err)
	}
	if files.writes != 1 {
		t.Fatalf("%d writes, want 1: a file that holds the keys is not rewritten", files.writes)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerConfig {
		t.Fatalf("after Remove the file is\n%q\nwant\n%q", got, customerConfig)
	}
	if _, ok, _ := w.backup.load(); ok {
		t.Fatal("the backup is kept after a complete Remove")
	}
}

// The backup is the file as it was before the agent's first write, not after a later one.
func TestCodexConfigBackupIsTakenOnce(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	writeFile(t, path, []byte(customerConfig))
	if err := w.Apply(codexOTelDesired(true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(codexOTelDesired(false)); err != nil {
		t.Fatal(err)
	}
	orig, ok, err := w.backup.load()
	if err != nil || !ok || !orig.Present || string(orig.Content) != customerConfig {
		t.Fatalf("backup = %+v, %v, %v; want the customer's file", orig, ok, err)
	}
}

// A file the agent created is deleted again by Remove.
func TestCodexConfigRemoveOfAFileThatWasAbsent(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	if err := w.Apply(codexOTelDesired(true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the config the agent created is still there: %v", err)
	}
}

// A setting the customer added while the agent's keys were in stays after Remove, and an [otel]
// table the agent added goes.
func TestCodexConfigRemoveKeepsOtherChanges(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	writeFile(t, path, []byte("model = \"gpt-5.5\"\n"))
	if err := w.Apply(codexOTelDesired(true)); err != nil {
		t.Fatal(err)
	}
	doc := readCodexConfig(t, path)
	doc["model_reasoning_effort"] = "high"
	out, err := toml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, out)
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	doc = readCodexConfig(t, path)
	if len(doc) != 2 || doc["model"] != "gpt-5.5" || doc["model_reasoning_effort"] != "high" {
		t.Fatalf("after Remove the config is %v", doc)
	}
}

// The customer's own exporter and prompt setting come back when the agent's keys go.
func TestCodexConfigRemoveRestoresTheCustomersOTelKeys(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	writeFile(t, path, []byte("[otel]\nlog_user_prompt = true\nexporter = \"none\"\n"))
	if err := w.Apply(codexOTelDesired(false)); err != nil {
		t.Fatal(err)
	}
	// A key the customer adds meanwhile keeps the file from going back byte for byte.
	otel := codexOTel(t, path)
	otel["environment"] = "staging"
	out, err := toml.Marshal(map[string]any{"otel": otel})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, out)
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	otel = codexOTel(t, path)
	if otel["exporter"] != "none" || otel["log_user_prompt"] != true || otel["environment"] != "staging" {
		t.Fatalf("[otel] after Remove = %v", otel)
	}
}

// A file Codex could not read is left as it is, and the write fails.
func TestCodexConfigLeavesAFileThatIsNotItsConfig(t *testing.T) {
	for name, content := range map[string]string{
		"not TOML":            "model = \n",
		"otel is not a table": "otel = \"on\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			w, path := newCodexConfigTestWriter(t)
			writeFile(t, path, []byte(content))
			if err := w.Apply(codexOTelDesired(true)); err == nil {
				t.Fatal("Apply accepted the file")
			}
			if got := string(readFile(t, path)); got != content {
				t.Fatalf("the file was changed to %q", got)
			}
		})
	}
}

// Remove without a backup has nothing to restore and changes nothing.
func TestCodexConfigRemoveWithoutBackupChangesNothing(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	writeFile(t, path, []byte(customerConfig))
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerConfig {
		t.Fatalf("the file was changed to %q", got)
	}
}

// A file saved with a byte-order mark keeps it.
func TestCodexConfigKeepsAByteOrderMark(t *testing.T) {
	w, path := newCodexConfigTestWriter(t)
	writeFile(t, path, append(append([]byte(nil), utf8BOM...), "model = \"gpt-5.5\"\n"...))
	if err := w.Apply(codexOTelDesired(true)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); string(got[:3]) != string(utf8BOM) {
		t.Fatalf("the byte-order mark is gone: %q", got[:8])
	}
	if ok, err := w.Holds(codexOTelDesired(true)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v", ok, err)
	}
}

// codexOTelBundle switches the receiver and Codex's OTel export on, at a tenant default mode.
func codexOTelBundle(mode protocol.CollectionMode) policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: mode,
		Endpoint: policy.EndpointPolicy{
			OTel:  policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
			Tools: map[string]policy.EndpointTool{"codex": {OTel: true}},
		},
	}
}

// Prompt logging follows the resolved mode for app:codex: on at m1 and higher, off at m0, and the
// tool's own mode narrows the tenant default. With the hooks off, the requirements file is not
// written.
func TestCodexModeSwitchesPromptLogging(t *testing.T) {
	w, reqPath, path := newCodexFilesTestWriter(t)
	p := newProvider(supportedCodex, w, codexOTelConfig)
	_ = p.ApplyPolicy(codexOTelBundle(protocol.ModeM0))
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if got := codexOTel(t, path)["log_user_prompt"]; got != false {
		t.Fatalf("at m0 log_user_prompt = %v, want false", got)
	}
	for _, m := range []protocol.CollectionMode{protocol.ModeM1, protocol.ModeM2, protocol.ModeM3} {
		if err := p.ApplyPolicy(codexOTelBundle(m)); err != nil {
			t.Fatal(err)
		}
		if got := codexOTel(t, path)["log_user_prompt"]; got != true {
			t.Fatalf("at %s log_user_prompt = %v, want true", m, got)
		}
	}
	b := codexOTelBundle(protocol.ModeM3)
	b.ToolModes = map[string]protocol.CollectionMode{CodexFingerprint: protocol.ModeM0}
	if err := p.ApplyPolicy(b); err != nil {
		t.Fatal(err)
	}
	if got := codexOTel(t, path)["log_user_prompt"]; got != false {
		t.Fatalf("with app:codex at m0 log_user_prompt = %v, want false", got)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatal("the requirements file was written with Codex's hooks off")
	}
}

// Under the registry, one provider runs both files: the OTel switch writes the config beside the
// customer's keys, the hooks switch the requirements, and switching both off restores both
// customer files, without a restart.
func TestCodexFilesFollowBothSwitches(t *testing.T) {
	w, reqPath, path := newCodexFilesTestWriter(t)
	writeFile(t, path, []byte(customerConfig))
	writeFile(t, reqPath, []byte(customerRequirements))
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newProvider(supportedCodex, w, codexOTelConfig)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })

	reg.ApplyPolicy(codexOTelBundle(protocol.ModeM1))
	if got := codexOTel(t, path)["log_user_prompt"]; got != true {
		t.Fatalf("log_user_prompt = %v after the OTel switch went on", got)
	}
	if got := string(readFile(t, reqPath)); got != customerRequirements {
		t.Fatal("the requirements file changed with only the OTel switch on")
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s with OTel on, want healthy", h.State, h.Detail)
	}

	both := codexOTelBundle(protocol.ModeM1)
	both.Endpoint.Hooks.Enabled = true
	both.Endpoint.Tools["codex"] = policy.EndpointTool{OTel: true, Hooks: true}
	reg.ApplyPolicy(both)
	if got := promptGroups(readCodexDoc(t, reqPath)); !reflect.DeepEqual(got, []any{customerPromptGroup, codexGroup(testExe)}) {
		t.Fatalf("UserPromptSubmit = %#v with both switches on", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s with both on, want healthy", h.State, h.Detail)
	}

	hooksOnly := both
	hooksOnly.Endpoint.Tools = map[string]policy.EndpointTool{"codex": {Hooks: true}}
	reg.ApplyPolicy(hooksOnly)
	if got := string(readFile(t, path)); got != customerConfig {
		t.Fatalf("with the OTel switch off the config is %q, want the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s with hooks only, want healthy", h.State, h.Detail)
	}

	off := both
	off.Endpoint.Tools = map[string]policy.EndpointTool{"codex": {}}
	reg.ApplyPolicy(off)
	if got := string(readFile(t, reqPath)); got != customerRequirements {
		t.Fatalf("after both switches went off the requirements file is %q, want the customer's", got)
	}
	if got := string(readFile(t, path)); got != customerConfig {
		t.Fatalf("after both switches went off the config is %q, want the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCodex); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// With OTel on, the row is degraded/config_write_failed when the config write fails or the config
// loses the keys.
func TestCodexConfigProviderHealth(t *testing.T) {
	t.Run("write fails", func(t *testing.T) {
		w, _, _ := newCodexFilesTestWriter(t)
		w.config.files = failingFiles{}
		p := newProvider(supportedCodex, w, codexOTelConfig)
		_ = p.ApplyPolicy(codexOTelBundle(protocol.ModeM1))
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start failed on a write failure: %v", err)
		}
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed || h.Counters[protocol.CounterErrors] != 1 {
			t.Fatalf("health = %s/%s errors %d, want degraded/config_write_failed and 1", h.State, h.Detail, h.Counters[protocol.CounterErrors])
		}
	})
	t.Run("keys removed", func(t *testing.T) {
		w, _, path := newCodexFilesTestWriter(t)
		p := newProvider(supportedCodex, w, codexOTelConfig)
		_ = p.ApplyPolicy(codexOTelBundle(protocol.ModeM1))
		_ = p.Start(context.Background())
		t.Cleanup(func() { _ = p.Stop(context.Background()) })
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
		}
		writeFile(t, path, []byte("[otel]\nexporter = \"none\"\n"))
		if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
			t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
		}
	})
}

// A user's config.toml whose [otel] disables or redirects the log export, laid over the system
// config key by key as Codex does, makes the row degraded with config_tampered; one that leaves the
// export alone does not. The users' files are never changed.
func TestCodexUserOverrideIsTampering(t *testing.T) {
	for _, c := range []struct {
		fixture  string
		tampered bool
	}{
		{"user-exporter-none.toml", true},
		{"user-exporter-redirected.toml", true},
		{"user-endpoint-only.toml", true},
		{"user-headers-only.toml", true},
		{"user-otel-not-a-table.toml", true},
		{"user-prompt-logging-only.toml", false},
		{"user-same-exporter.toml", false},
		{"user-no-otel.toml", false},
		{"user-not-toml.toml", false},
		{"missing.toml", false},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			fixture := filepath.Join("testdata", "codex", c.fixture)
			before, _ := os.ReadFile(fixture)
			other := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, other, []byte("model = \"gpt-5.5\"\n"))
			w, _, _ := newCodexFilesTestWriter(t, other, fixture)
			p := newProvider(supportedCodex, w, codexOTelConfig)
			_ = p.ApplyPolicy(codexOTelBundle(protocol.ModeM1))
			_ = p.Start(context.Background())
			t.Cleanup(func() { _ = p.Stop(context.Background()) })
			state, want := protocol.StateHealthy, protocol.DetailNone
			if c.tampered {
				state, want = protocol.StateDegraded, protocol.DetailConfigTampered
			}
			if h := p.Health(); h.State != state || h.Detail != want {
				t.Fatalf("health = %s/%s, want %s/%s", h.State, h.Detail, state, want)
			}
			if after, _ := os.ReadFile(fixture); string(after) != string(before) {
				t.Fatal("the user's config was changed")
			}
		})
	}
}

// With the export off, nothing a user sets is tampering: the hooks in requirements.toml are not
// the user's to override.
func TestCodexOverrideNeedsTheExport(t *testing.T) {
	w, _, _ := newCodexFilesTestWriter(t, filepath.Join("testdata", "codex", "user-exporter-none.toml"))
	if w.Overridden(Desired{Hooks: true, HookCommand: testExe}) {
		t.Fatal("overridden with only the hooks on")
	}
	if !w.Overridden(codexOTelDesired(true)) {
		t.Fatal("not overridden with the export on")
	}
}
