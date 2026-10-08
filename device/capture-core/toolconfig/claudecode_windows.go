//go:build windows

package toolconfig

import (
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
