//go:build windows

package toolconfig

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// codexSupported: Windows has a system requirements location.
const codexSupported = true

// codexRequirementsPath is %ProgramData%\OpenAI\Codex\requirements.toml, the system requirements
// file the Codex CLI reads on Windows.
func codexRequirementsPath() string {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return ""
	}
	return filepath.Join(pd, "OpenAI", "Codex", "requirements.toml")
}
