// Package toolconfig writes the agent's settings into AI tools' machine-wide, admin-managed
// configuration, so a tool exports its telemetry to the agent, or runs the agent's hook, and a user
// cannot switch that off.
//
// A Writer owns a few keys in one tool's managed file and touches nothing else in it. Before its
// first write it backs the file up once (or records that there was none) under
// toolconfig/<tool>/original in the state directory; Remove takes the agent's keys out again and
// puts back any value the agent replaced. Each tool has one core.Provider, switched by its
// endpoint.tools entry in the signed bundle.
//
// The managed files carry the OTLP bearer token, so nothing here logs a file's content.
package toolconfig

import (
	"github.com/shadow-ai-capture/device/capture-core/policy"
)

// Desired is what the agent wants a tool configured with.
type Desired struct {
	// HTTPListen is the OTLP receiver's OTLP/HTTP address, host:port.
	HTTPListen string
	// Token is the OTLP receiver's bearer token.
	Token string
	// LogPrompts switches the tool's prompt logging on: the resolved mode for the tool is m1 or
	// higher, so the text is needed to classify it on the device.
	LogPrompts bool
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
