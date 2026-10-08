//go:build windows

package toolconfig

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// claudeCodeSupported: Windows has a machine-wide managed settings location.
const claudeCodeSupported = true

// claudeCodeManagedPath is %ProgramFiles%\ClaudeCode\managed-settings.json, the file Claude Code
// reads its managed settings from on Windows.
func claudeCodeManagedPath() string {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return ""
	}
	return filepath.Join(pf, "ClaudeCode", "managed-settings.json")
}

// claudeCodeInstalled looks for Claude Code in every user profile and in the machine-wide WinGet
// folder.
func claudeCodeInstalled() bool {
	profiles, err := windows.KnownFolderPath(windows.FOLDERID_UserProfiles, 0)
	if err != nil {
		return false
	}
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		pf = ""
	}
	return claudeCodeInstalledUnder(profiles, pf)
}

// claudeCodeInstalledUnder reports whether a profile under profilesDir, or programFiles, holds one
// of Claude Code's install locations:
//   - the native installer's %USERPROFILE%\.local\bin\claude.exe;
//   - a global npm install, in npm's default prefix %AppData%\npm;
//   - a WinGet package folder, for the user or for the machine.
func claudeCodeInstalledUnder(profilesDir, programFiles string) bool {
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	matches := func(pattern string) bool {
		m, err := filepath.Glob(pattern)
		return err == nil && len(m) > 0
	}
	if programFiles != "" && matches(filepath.Join(programFiles, "WinGet", "Packages", "Anthropic.ClaudeCode_*")) {
		return true
	}
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		home := filepath.Join(profilesDir, e.Name())
		if exists(filepath.Join(home, ".local", "bin", "claude.exe")) ||
			exists(filepath.Join(home, "AppData", "Roaming", "npm", "node_modules", "@anthropic-ai", "claude-code")) ||
			matches(filepath.Join(home, "AppData", "Local", "Microsoft", "WinGet", "Packages", "Anthropic.ClaudeCode_*")) {
			return true
		}
	}
	return false
}
