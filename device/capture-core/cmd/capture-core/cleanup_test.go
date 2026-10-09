package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
	"github.com/shadow-ai-capture/device/capture-core/winproxy"
)

// fakeMachineEnv is the machine environment in memory.
type fakeMachineEnv struct {
	mu   sync.Mutex
	vars map[string]string
}

func (e *fakeMachineEnv) Lookup(name string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.vars[name]
	return v, ok, nil
}

func (e *fakeMachineEnv) Set(name, value string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = value
	return nil
}

func (e *fakeMachineEnv) Unset(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.vars, name)
	return nil
}

// withCleanupSeams points the tool writers, the machine environment and the trust store at the
// test's own copies, and restores the package's seams afterwards.
func withCleanupSeams(t *testing.T, tools string, env toolconfig.MachineEnv, trust trustStore, cf cleanupFacilities) {
	t.Helper()
	prevPlatform, prevCleanup := platform, cleanupPlatform
	t.Cleanup(func() { platform, cleanupPlatform = prevPlatform, prevCleanup })
	platform.claudeCodeSettings = filepath.Join(tools, "ClaudeCode", "managed-settings.json")
	platform.cursorHooks = filepath.Join(tools, "Cursor", "hooks.json")
	platform.codexRequirements = filepath.Join(tools, "OpenAI", "Codex", "requirements.toml")
	platform.codexConfig = filepath.Join(tools, "OpenAI", "Codex", "config.toml")
	platform.machineEnv = env
	platform.trustStore = func(func(string, ...any)) trustStore { return trust }
	cleanupPlatform = cf
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The uninstall cleanup returns each location the agent changed to how the agent found it: a tool's
// managed file that held the customer's keys gets back its exact bytes, a file the agent created is
// deleted, OLLAMA_HOST gets back its value, and a user whose PAC the service could not restore gets
// back their own PAC. The device root leaves the trust store and its key is deleted. A step that
// fails is reported and the others still run, and the exit code is 0.
func TestUninstallCleanupRestoresTheMachine(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	dir, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	env := &fakeMachineEnv{vars: map[string]string{"OLLAMA_HOST": "0.0.0.0:11434"}}
	trust := &fakeTrustStore{}
	const sid = "S-1-5-21-1000"
	user := &fakeUserSettings{values: map[string]string{"AutoConfigURL": "http://corp.example/proxy.pac"}}
	var hiveLoaded sync.Mutex // held while the user's hive is unloaded
	openUser := func(s string) (winproxy.Registry, error) {
		if s != sid {
			return nil, errors.New("no such user")
		}
		if !hiveLoaded.TryLock() {
			return nil, errors.New("the hive is not loaded")
		}
		hiveLoaded.Unlock()
		return user, nil
	}
	var deletedKey string
	var firewallPrefix string
	logFile := filepath.Join(t.TempDir(), uninstallLogName)
	withCleanupSeams(t, tools, env, trust, cleanupFacilities{
		openUserSettings: openUser,
		deleteDeviceKey:  func(caDir string) error { deletedKey = caDir; return nil },
		removeFirewallRules: func(prefix string) (int, error) {
			firewallPrefix = prefix
			return 0, errors.New("the firewall service is not running")
		},
		logPath: func() string { return logFile },
	})

	// What the customer had before the agent was installed.
	claudeOriginal := "{\n\t\"model\": \"customer-model\",\n\t\"env\": {\"CUSTOMER_VAR\": \"1\", \"OTEL_METRICS_EXPORTER\": \"prometheus\"}\n}\n"
	writeTestFile(t, platform.claudeCodeSettings, claudeOriginal)
	codexOriginal := "# the customer's Codex defaults\nmodel = \"o3\"\n\n[otel]\nlog_user_prompt = false\n"
	writeTestFile(t, platform.codexConfig, codexOriginal)

	// The agent at work: tool configuration, Ollama moved, the device root trusted, the PAC applied.
	const token = "0123456789abcdef0123456789abcdef"
	installed := func() bool { return true }
	desired := toolconfig.Desired{
		OTel: true, HTTPListen: "127.0.0.1:47318", Token: token, LogPrompts: true,
		Hooks: true, HookCommand: `C:\Program Files\ShadowAICapture\bin\capture-core.exe`, ManagedOnly: true,
	}
	if err := toolconfig.NewClaudeCodeWriter(dir, platform.claudeCodeSettings, installed).Apply(desired); err != nil {
		t.Fatal(err)
	}
	if err := toolconfig.NewCursorWriter(dir, platform.cursorHooks, installed).Apply(desired); err != nil {
		t.Fatal(err)
	}
	codex := toolconfig.NewCodexFiles(
		toolconfig.NewCodexWriter(dir, platform.codexRequirements, installed),
		toolconfig.NewCodexConfigWriter(dir, platform.codexConfig, nil),
	)
	if err := codex.Apply(desired); err != nil {
		t.Fatal(err)
	}
	if err := toolconfig.NewOllama(dir, env).Relocate(21434); err != nil {
		t.Fatal(err)
	}
	ca, err := tlsproxy.NewCA("DESKTOP-01", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir.Path(state.DeviceCADir, "ca.pem"), string(ca.PEM()))
	if err := trust.Install(ctx, ca.DER()); err != nil {
		t.Fatal(err)
	}
	pac := winproxy.New(winproxy.Config{
		Bundles: func() *policy.Bundle {
			return &policy.Bundle{Version: "1", Interception: policy.Interception{Enabled: true, PacListen: "127.0.0.1:0"}}
		},
		Users:        func(context.Context) ([]string, error) { return []string{sid}, nil },
		OpenSettings: openUser,
		FetchPAC: func(context.Context, string) ([]byte, error) {
			return []byte(`function FindProxyForURL(url, host) { return "DIRECT"; }`), nil
		},
		RecordFile: dir.Path(winproxy.StateFile),
	})
	if err := pac.Start(ctx); err != nil {
		t.Fatal(err)
	}
	applied := user.autoConfigURL()
	for path, want := range map[string]string{platform.claudeCodeSettings: token, platform.cursorHooks: "--hook cursor", platform.codexRequirements: "--hook codex", platform.codexConfig: token} {
		if !strings.Contains(readTestFile(t, path), want) {
			t.Fatalf("%s does not hold the agent's keys", path)
		}
	}
	if env.vars["OLLAMA_HOST"] != "127.0.0.1:21434" || !strings.Contains(applied, "/proxy.pac?u="+sid) {
		t.Fatalf("OLLAMA_HOST %q, AutoConfigURL %q: the agent did not apply its changes", env.vars["OLLAMA_HOST"], applied)
	}

	// The user's hive is unloaded when the service stops, so the service cannot restore their PAC.
	hiveLoaded.Lock()
	if err := pac.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	hiveLoaded.Unlock()
	if got := user.autoConfigURL(); got != applied {
		t.Fatalf("AutoConfigURL = %q after the stop; the test needs the PAC left in place", got)
	}

	var stdout bytes.Buffer
	code := runUninstallCleanup([]string{"--state-dir", stateDir}, &stdout)
	if code != 0 {
		t.Fatalf("exit code %d; the cleanup never fails an uninstall", code)
	}

	if got := readTestFile(t, platform.claudeCodeSettings); got != claudeOriginal {
		t.Errorf("Claude Code's managed settings after cleanup:\n%s\nwant the original:\n%s", got, claudeOriginal)
	}
	if got := readTestFile(t, platform.codexConfig); got != codexOriginal {
		t.Errorf("Codex's config.toml after cleanup:\n%s\nwant the original:\n%s", got, codexOriginal)
	}
	for _, created := range []string{platform.cursorHooks, platform.codexRequirements} {
		if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s, which the agent created, is still there: %v", created, err)
		}
	}
	if got, ok := env.vars["OLLAMA_HOST"]; !ok || got != "0.0.0.0:11434" {
		t.Errorf("OLLAMA_HOST = %q (set %v); want the original 0.0.0.0:11434", got, ok)
	}
	if got := user.autoConfigURL(); got != "http://corp.example/proxy.pac" {
		t.Errorf("AutoConfigURL = %q; want the user's own PAC", got)
	}
	if trust.installed != nil || trust.removes != 1 {
		t.Errorf("trust store: root still installed %v, %d removals; want the root removed", trust.installed != nil, trust.removes)
	}
	if deletedKey != dir.Path(state.DeviceCADir) {
		t.Errorf("device key deleted for %q; want %q", deletedKey, dir.Path(state.DeviceCADir))
	}
	if firewallPrefix != "ShadowAICapture QUIC " {
		t.Errorf("firewall rules removed by prefix %q", firewallPrefix)
	}
	for _, f := range []string{"toolconfig/claude_code/original", "toolconfig/cursor/original", "toolconfig/codex/original", "toolconfig/codex/config/original", "toolconfig/ollama/original", winproxy.StateFile} {
		if _, err := os.Stat(dir.Path(filepath.FromSlash(f))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived the restore: %v", f, err)
		}
	}

	log := readTestFile(t, logFile)
	if log != stdout.String() {
		t.Errorf("the log and the output differ:\nlog:\n%s\noutput:\n%s", log, stdout.String())
	}
	for _, want := range []string{
		"tool_config_claude_code: ok", "tool_config_codex: ok", "tool_config_copilot: ok", "tool_config_cursor: ok",
		"ollama_host: ok", "desktop_proxy_pac: ok, 1 user(s) restored", "trust_root: ok", "device_root_key: ok",
		"quic_firewall_rules: failed: the firewall service is not running", "cli_shim_environment: ok",
		"uninstall cleanup: done, 1 step(s) failed",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, token) {
		t.Error("the log carries the OTLP token")
	}
}

// Without a state directory the steps that need one are reported failed, the others still run, and
// the exit code is 0.
func TestUninstallCleanupWithoutConfiguration(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), uninstallLogName)
	withCleanupSeams(t, t.TempDir(), &fakeMachineEnv{vars: map[string]string{}}, &fakeTrustStore{}, cleanupFacilities{
		deleteDeviceKey:     func(string) error { return nil },
		removeFirewallRules: func(string) (int, error) { return 0, nil },
		logPath:             func() string { return logFile },
	})
	var stdout bytes.Buffer
	if code := runUninstallCleanup([]string{"--config-file", filepath.Join(t.TempDir(), "missing.env")}, &stdout); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"configuration: config file", "tool_config_claude_code: failed: state: no state directory configured", "quic_firewall_rules: ok, 0 rule(s) removed", "cli_shim_environment: ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output has no %q:\n%s", want, out)
		}
	}
}
