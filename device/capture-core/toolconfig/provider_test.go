package toolconfig

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// otelBundle switches Claude Code's OTel export on, at a tenant default mode.
func otelBundle(mode protocol.CollectionMode) policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: mode,
		Endpoint: policy.EndpointPolicy{
			OTel:  policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
			Tools: map[string]policy.EndpointTool{"claude_code": {OTel: true, Hooks: true}},
		},
	}
}

// supportedTool is Claude Code on a platform the agent writes its configuration on.
var supportedTool = func() tool { t := claudeCodeTool; t.supported = true; return t }()

func newTestProvider(t *testing.T, w Writer) *Provider {
	t.Helper()
	return newProvider(supportedTool, w, Config{
		Token:      func() string { return testToken },
		Executable: func() (string, error) { return testExe, nil },
	})
}

// failingFiles reads like the file system and refuses every write.
type failingFiles struct{ systemFiles }

func (failingFiles) write(string, []byte) error { return errors.New("access is denied") }

// countingFiles is the file system, counting writes.
type countingFiles struct {
	systemFiles
	writes int
}

func (f *countingFiles) write(path string, data []byte) error {
	f.writes++
	return f.systemFiles.write(path, data)
}

func TestProviderNameAndSwitch(t *testing.T) {
	p := NewClaudeCode(&ClaudeCode{}, Config{})
	if p.Name() != protocol.CollectorToolConfigClaudeCode {
		t.Fatalf("Name = %s", p.Name())
	}
	b := otelBundle(protocol.ModeM1)
	if !p.Enabled(&b) {
		t.Fatal("not enabled with the receiver and Claude Code's OTel on")
	}
	if p.Enabled(nil) {
		t.Fatal("enabled with no bundle in force")
	}
	off := otelBundle(protocol.ModeM1)
	off.Endpoint.Tools["claude_code"] = policy.EndpointTool{Hooks: true}
	if p.Enabled(&off) {
		t.Fatal("enabled with Claude Code's OTel off")
	}
	off = otelBundle(protocol.ModeM1)
	off.Endpoint.OTel.Enabled = false
	if p.Enabled(&off) {
		t.Fatal("enabled with the receiver off: a tool switch takes effect only while its collector is on")
	}
	hooksOnly := hooksBundle(false)
	if !p.Enabled(&hooksOnly) {
		t.Fatal("not enabled with the hooks and Claude Code's hooks on, OTel off")
	}
	hooksOnly.Endpoint.Hooks.Enabled = false
	if p.Enabled(&hooksOnly) {
		t.Fatal("enabled with the hook collector off")
	}
}

// hooksBundle switches Claude Code's hooks on and its OTel export off.
func hooksBundle(managedOnly bool) policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: protocol.ModeM1,
		Endpoint: policy.EndpointPolicy{
			OTel:  policy.EndpointOTel{HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
			Hooks: policy.EndpointHooks{Enabled: true, ManagedOnly: managedOnly},
			Tools: map[string]policy.EndpointTool{"claude_code": {OTel: true, Hooks: true}},
		},
	}
}

// Under the registry, the hooks and managed-only follow each bundle without a restart, beside OTel
// and on their own; with both switches off the file is the customer's again.
func TestProviderFollowsTheHookSwitches(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerHooksFile))
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newTestProvider(t, w)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })
	healthy := func(when string) {
		t.Helper()
		if h, _ := reg.HealthFor(protocol.CollectorToolConfigClaudeCode); h.State != protocol.StateHealthy {
			t.Fatalf("%s: row = %s/%s, want healthy", when, h.State, h.Detail)
		}
	}

	reg.ApplyPolicy(hooksBundle(false))
	m := readManaged(t, path)
	checkGroups(t, m, "UserPromptSubmit", customerPromptHook, agentGroup("UserPromptSubmit", testExe))
	checkGroups(t, m, "PreToolUse", customerToolHook, agentGroup("PreToolUse", testExe))
	if m.Env != nil || m.ManagedOnly != nil {
		t.Fatalf("hooks only wrote env %v and allowManagedHooksOnly %v", m.Env, m.ManagedOnly)
	}
	healthy("hooks on")

	reg.ApplyPolicy(hooksBundle(true))
	if m := readManaged(t, path); m.ManagedOnly == nil || !*m.ManagedOnly {
		t.Fatal("managed-only on did not set allowManagedHooksOnly")
	}
	healthy("managed-only on")
	reg.ApplyPolicy(hooksBundle(false))
	if m := readManaged(t, path); m.ManagedOnly != nil {
		t.Fatal("managed-only off left allowManagedHooksOnly")
	}

	both := hooksBundle(false)
	both.Endpoint.OTel.Enabled = true
	reg.ApplyPolicy(both)
	if m := readManaged(t, path); m.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" || len(m.Hooks["PreToolUse"]) != 2 {
		t.Fatalf("OTel and hooks on: env %v, hooks %v", m.Env, m.Hooks)
	}
	healthy("both on")

	otelOnly := both
	otelOnly.Endpoint.Hooks.Enabled = false
	reg.ApplyPolicy(otelOnly)
	m = readManaged(t, path)
	checkGroups(t, m, "UserPromptSubmit", customerPromptHook)
	checkGroups(t, m, "PreToolUse", customerToolHook)
	if m.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatalf("hooks off took OTel's env with it: %v", m.Env)
	}
	healthy("OTel only")

	otelOnly.Endpoint.OTel.Enabled = false
	reg.ApplyPolicy(otelOnly)
	if got := string(readFile(t, path)); got != customerHooksFile {
		t.Fatalf("with both off the file is\n%s\nwant the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigClaudeCode); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// Hooks that cannot be written, because the agent's path is unknown or the file cannot be
// replaced, leave the row degraded with config_write_failed; hooks taken out of the file are not
// reported healthy.
func TestProviderHookWriteFailureIsDegraded(t *testing.T) {
	w, path := newTestWriter(t)
	p := newProvider(supportedTool, w, Config{Executable: func() (string, error) { return "", errors.New("no path") }})
	_ = p.ApplyPolicy(hooksBundle(false))
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("without the executable: health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
	_ = p.Stop(context.Background())

	w.files = failingFiles{}
	p = newTestProvider(t, w)
	_ = p.ApplyPolicy(hooksBundle(false))
	_ = p.Start(context.Background())
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("on a failed write: health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
	_ = p.Stop(context.Background())

	w.files = systemFiles{}
	p = newTestProvider(t, w)
	_ = p.ApplyPolicy(hooksBundle(true))
	_ = p.Start(context.Background())
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
	}
	writeFile(t, path, []byte(`{"hooks":{}}`))
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("with the hooks taken out: health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
}

// Start applies what the bundle asks for, the file then holds it and the row is healthy; Stop
// removes it.
func TestProviderStartAppliesAndStopRemoves(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	p := newTestProvider(t, w)
	if err := p.ApplyPolicy(otelBundle(protocol.ModeM1)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := w.backup.load(); ok {
		t.Fatal("a bundle applied before Start wrote the file")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s, want healthy", h.State, h.Detail)
	}
	if env := envOf(t, path); env["OTEL_LOG_USER_PROMPTS"] != "1" || env["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Bearer "+testToken {
		t.Fatalf("env = %v", env)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerFile {
		t.Fatalf("after Stop the file is %q, want the customer's", got)
	}
	if h := p.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after Stop = %s", h.State)
	}
}

// Prompt logging follows the resolved mode for app:claude_code: on at m1 and above, off at m0, and
// a new bundle that changes the mode flips it.
func TestProviderModeChangeFlipsPromptLogging(t *testing.T) {
	w, path := newTestWriter(t)
	p := newTestProvider(t, w)
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM0))
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if got := envOf(t, path)["OTEL_LOG_USER_PROMPTS"]; got != "0" {
		t.Fatalf("at m0 OTEL_LOG_USER_PROMPTS = %q, want 0", got)
	}
	if err := p.ApplyPolicy(otelBundle(protocol.ModeM2)); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, path)["OTEL_LOG_USER_PROMPTS"]; got != "1" {
		t.Fatalf("at m2 OTEL_LOG_USER_PROMPTS = %q, want 1", got)
	}
	// The tool's own entry narrows the tenant default.
	b := otelBundle(protocol.ModeM3)
	b.ToolModes = map[string]protocol.CollectionMode{ClaudeCodeFingerprint: protocol.ModeM0}
	if err := p.ApplyPolicy(b); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, path)["OTEL_LOG_USER_PROMPTS"]; got != "0" {
		t.Fatalf("with app:claude_code at m0 OTEL_LOG_USER_PROMPTS = %q, want 0", got)
	}
	if got := envOf(t, path)["OTEL_LOG_ASSISTANT_RESPONSES"]; got != "0" {
		t.Fatalf("OTEL_LOG_ASSISTANT_RESPONSES = %q, want 0 at every mode", got)
	}
}

// A new receiver address re-applies; an unchanged bundle writes nothing.
func TestProviderReappliesOnlyOnChange(t *testing.T) {
	w, path := newTestWriter(t)
	files := &countingFiles{}
	w.files = files
	p := newTestProvider(t, w)
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM1))
	_ = p.Start(context.Background())
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if files.writes != 1 {
		t.Fatalf("writes = %d after Start, want 1", files.writes)
	}
	if err := p.ApplyPolicy(otelBundle(protocol.ModeM1)); err != nil {
		t.Fatal(err)
	}
	if files.writes != 1 {
		t.Fatal("an unchanged bundle rewrote the file")
	}
	b := otelBundle(protocol.ModeM1)
	b.Endpoint.OTel.HTTPListen = "127.0.0.1:50318"
	if err := p.ApplyPolicy(b); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, path)["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://127.0.0.1:50318" {
		t.Fatalf("endpoint = %q after the address changed", got)
	}
}

// Without Claude Code installed nothing is written and the row is absent with tool_not_installed.
func TestProviderNotInstalledIsAbsent(t *testing.T) {
	w, path := newTestWriter(t)
	w.installed = func() bool { return false }
	p := newTestProvider(t, w)
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM1))
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolNotInstalled {
		t.Fatalf("health = %s/%s, want absent/tool_not_installed", h.State, h.Detail)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the managed file was written for a tool that is not installed")
	}
	// Installed later, the next bundle configures it.
	w.installed = func() bool { return true }
	if err := p.ApplyPolicy(otelBundle(protocol.ModeM1)); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s after installing, want healthy", h.State, h.Detail)
	}
}

// A failed write leaves the provider running, degraded with config_write_failed, and counted.
func TestProviderWriteFailureIsDegraded(t *testing.T) {
	w, _ := newTestWriter(t)
	w.files = failingFiles{}
	p := newTestProvider(t, w)
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM1))
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start failed on a write failure: %v", err)
	}
	h := p.Health()
	if h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
	if h.Counters[protocol.CounterErrors] != 1 {
		t.Fatalf("errors = %d, want 1", h.Counters[protocol.CounterErrors])
	}
	// The next bundle tries again even though it asks for the same thing.
	if err := p.ApplyPolicy(otelBundle(protocol.ModeM1)); err == nil {
		t.Fatal("the retry did not try to write")
	}
}

// A file that no longer holds the agent's keys is not reported healthy.
func TestProviderHealthReadsTheFile(t *testing.T) {
	w, path := newTestWriter(t)
	p := newTestProvider(t, w)
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM1))
	_ = p.Start(context.Background())
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	writeFile(t, path, []byte(`{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}`))
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailConfigWriteFailed {
		t.Fatalf("health = %s/%s, want degraded/config_write_failed", h.State, h.Detail)
	}
}

// Where the agent has no managed location for the tool, the row is absent with
// tool_version_unsupported and nothing is written or removed.
func TestProviderUnsupportedPlatform(t *testing.T) {
	w, path := newTestWriter(t)
	unsupported := supportedTool
	unsupported.supported = false
	p := newProvider(unsupported, w, Config{})
	_ = p.ApplyPolicy(otelBundle(protocol.ModeM1))
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailToolVersionUnsupported {
		t.Fatalf("health = %s/%s, want absent/tool_version_unsupported", h.State, h.Detail)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the managed file was written on an unsupported platform")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Under the registry, a bundle switching the tool on starts the provider and writes the file, and
// one switching it off stops it and restores the file.
func TestProviderFollowsThePolicyToggle(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newTestProvider(t, w)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })

	reg.ApplyPolicy(otelBundle(protocol.ModeM1))
	if env := envOf(t, path); env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatalf("env = %v after the switch went on", env)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigClaudeCode); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s, want healthy", h.State, h.Detail)
	}
	off := otelBundle(protocol.ModeM1)
	off.Endpoint.Tools["claude_code"] = policy.EndpointTool{}
	reg.ApplyPolicy(off)
	if got := string(readFile(t, path)); got != customerFile {
		t.Fatalf("after the switch went off the file is %q, want the customer's", got)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigClaudeCode); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// The app-to-tool table, and whether a bundle covers an app with a native collector.
func TestNativeCoverage(t *testing.T) {
	for app, want := range map[string]string{
		"claude_code": "claude_code", "claude_code_vscode": "claude_code", "codex": "codex",
		"copilot_cli": "copilot", "github_copilot": "copilot", "cursor": "cursor",
	} {
		if got, ok := ToolForApp(app); !ok || got != want {
			t.Errorf("ToolForApp(%s) = %q, %v; want %s", app, got, ok, want)
		}
	}
	if _, ok := ToolForApp("chatgpt_desktop"); ok {
		t.Error("an app without native collectors maps to a tool")
	}

	b := otelBundle(protocol.ModeM1)
	b.Endpoint.Tools["cursor"] = policy.EndpointTool{Hooks: true}
	b.Endpoint.Tools["codex"] = policy.EndpointTool{OTel: false}
	cases := []struct {
		app   string
		hooks bool
		want  bool
	}{
		{"claude_code", false, true},
		{"claude_code_vscode", false, true},
		{"cursor", false, false}, // hooks switched on for the tool, but the hook collector is off
		{"cursor", true, true},
		{"codex", true, false},
		{"chatgpt_desktop", true, false},
	}
	for _, c := range cases {
		b.Endpoint.Hooks.Enabled = c.hooks
		if got := NativelyCovered(&b, c.app); got != c.want {
			t.Errorf("NativelyCovered(%s, hooks collector %v) = %v, want %v", c.app, c.hooks, got, c.want)
		}
	}
	if NativelyCovered(nil, "claude_code") {
		t.Error("covered with no bundle in force")
	}
}
