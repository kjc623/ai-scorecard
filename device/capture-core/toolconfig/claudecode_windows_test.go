//go:build windows

package toolconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The managed settings file is in %ProgramFiles%\ClaudeCode.
func TestClaudeCodeManagedPath(t *testing.T) {
	want := filepath.Join(os.Getenv("ProgramFiles"), "ClaudeCode", "managed-settings.json")
	if got := claudeCodeManagedPath(); !strings.EqualFold(got, want) {
		t.Fatalf("managed path = %q, want %q", got, want)
	}
}

// On Windows the provider writes Claude Code's configuration.
func TestClaudeCodeIsSupportedOnWindows(t *testing.T) {
	if !claudeCodeTool.supported {
		t.Fatal("Claude Code's configuration is not written on Windows")
	}
}
