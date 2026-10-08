//go:build windows

package toolconfig

import (
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/inventory"
)

// codexSupported: Windows has system requirements and config locations.
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

// codexConfigPath is %ProgramData%\OpenAI\Codex\config.toml, the system config layer the Codex CLI
// reads on Windows. It is Codex's lowest layer: a user's own config.toml overrides it key by key.
func codexConfigPath() string {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return ""
	}
	return filepath.Join(pd, "OpenAI", "Codex", "config.toml")
}

// codexUserConfigs is each user profile's .codex\config.toml, the user layer Codex reads while the
// user's CODEX_HOME is unset.
func codexUserConfigs() []string {
	profiles, _ := inventory.SystemCLIHost().Profiles()
	paths := make([]string, 0, len(profiles))
	for _, p := range profiles {
		paths = append(paths, filepath.Join(p.Dir, ".codex", "config.toml"))
	}
	return paths
}
