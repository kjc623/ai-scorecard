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

// Claude Code is found at each documented install location, for any user profile or for the
// machine, and not where none exists.
func TestClaudeCodeInstalledUnder(t *testing.T) {
	cases := map[string][]string{
		"native installer": {"profiles", "alice", ".local", "bin", "claude.exe"},
		"npm global":       {"profiles", "bob", "AppData", "Roaming", "npm", "node_modules", "@anthropic-ai", "claude-code", "package.json"},
		"winget for user":  {"profiles", "carol", "AppData", "Local", "Microsoft", "WinGet", "Packages", "Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe", "claude.exe"},
		"winget machine":   {"pf", "WinGet", "Packages", "Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe", "claude.exe"},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			profiles, pf := filepath.Join(root, "profiles"), filepath.Join(root, "pf")
			for _, d := range []string{filepath.Join(profiles, "Public"), pf} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if claudeCodeInstalledUnder(profiles, pf) {
				t.Fatal("installed with no install location present")
			}
			file := filepath.Join(append([]string{root}, parts...)...)
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if !claudeCodeInstalledUnder(profiles, pf) {
				t.Fatalf("not installed with %s present", file)
			}
		})
	}
}

// On Windows the provider writes Claude Code's configuration.
func TestClaudeCodeIsSupportedOnWindows(t *testing.T) {
	if !claudeCodeTool.supported {
		t.Fatal("Claude Code's configuration is not written on Windows")
	}
}
