// Package toolconfig writes the agent's settings into AI tools' machine-wide, admin-managed
// configuration, so a tool exports its telemetry to the agent and runs the agent's prompt hooks,
// and a user cannot switch either off.
//
// A Writer owns a few keys in one tool's managed file, or a few registry values for a tool
// configured through the registry, and touches nothing else. Before its first write it backs the
// file (or each value) up once, or records that there was none, under toolconfig/<tool>/original in
// the state directory; Remove takes the agent's keys out again and puts back any value the agent
// replaced. Each tool has one core.Provider, switched by its endpoint.tools entry in the signed
// bundle: on while either of the tool's OTel and hooks switches is in effect.
//
// The managed files and values carry the OTLP bearer token, so nothing here logs their content.
package toolconfig

import (
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/policy"
)

// Desired is what the agent wants a tool configured with. The OTel fields apply only with OTel, and
// the hook fields only with Hooks.
type Desired struct {
	// OTel points the tool's telemetry at the OTLP receiver.
	OTel bool
	// HTTPListen is the OTLP receiver's OTLP/HTTP address, host:port.
	HTTPListen string
	// Token is the OTLP receiver's bearer token.
	Token string
	// LogPrompts switches the tool's prompt logging on: the resolved mode for the tool is m1 or
	// higher, so the text is needed to classify it on the device.
	LogPrompts bool
	// LogCLIPrompts is LogPrompts for the tool's CLI, for a tool whose CLI is a catalog app of its
	// own with its own collection mode.
	LogCLIPrompts bool

	// Hooks declares the agent's prompt hooks, which run HookCommand.
	Hooks bool
	// HookCommand is the installed capture-core executable.
	HookCommand string
	// ManagedOnly lets only the hooks an administrator declares run.
	ManagedOnly bool
}

// isCaptureCore reports whether path names a capture-core executable, from wherever the agent is or
// was installed.
func isCaptureCore(path string) bool {
	base := strings.ToLower(path[strings.LastIndexAny(path, `/\`)+1:])
	return strings.TrimSuffix(base, ".exe") == "capture-core"
}

// Writer is one tool's managed configuration.
type Writer interface {
	// Installed reports whether the tool is installed on the device.
	Installed() bool
	// Apply merges the agent's keys for d into the managed file, backing the file up first.
	Apply(d Desired) error
	// Holds reports whether the managed file on disk carries the agent's keys for d.
	Holds(d Desired) (bool, error)
	// Remove takes the agent's keys out of the managed file and restores what they replaced.
	Remove() error
	// Path is the managed file.
	Path() string
}

// partialWriter is a Writer for a tool with more than one installable part, some of which may have
// no machine-wide configuration the agent can write.
type partialWriter interface {
	// Unenforced reports whether, at the last Installed, an installed part had none.
	Unenforced() bool
}

// toolByApp maps a catalog app to the endpoint.tools key its native collectors are switched by.
var toolByApp = map[string]string{
	"claude_code":        "claude_code",
	"claude_code_vscode": "claude_code",
	"codex":              "codex",
	"copilot_cli":        "copilot",
	"github_copilot":     "copilot",
	"cursor":             "cursor",
}

// ToolForApp returns the endpoint.tools key for a catalog app key.
func ToolForApp(appKey string) (string, bool) {
	t, ok := toolByApp[appKey]
	return t, ok
}

// NativelyCovered reports whether the bundle switches on a native collector (OTel or hooks, with
// the collector itself enabled) for the tool a catalog app belongs to. Such an app's traffic is not
// decrypted, so one prompt is not recorded twice.
func NativelyCovered(b *policy.Bundle, appKey string) bool {
	tool, ok := ToolForApp(appKey)
	if b == nil || !ok {
		return false
	}
	t := b.Endpoint.Tools[tool]
	return (t.OTel && b.Endpoint.OTel.Enabled) || (t.Hooks && b.Endpoint.Hooks.Enabled)
}
