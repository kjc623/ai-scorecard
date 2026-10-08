//go:build windows

package toolconfig

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// cursorSupported: Windows has an enterprise hooks location.
const cursorSupported = true

// cursorHooksPath is %ProgramData%\Cursor\hooks.json, the enterprise hooks file Cursor reads on
// Windows.
func cursorHooksPath() string {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return ""
	}
	return filepath.Join(pd, "Cursor", "hooks.json")
}
