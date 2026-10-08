//go:build !windows

package toolconfig

// codexSupported: the macOS and Linux system locations (/etc/codex/requirements.toml and
// config.toml) are not written yet, so the provider reports tool_version_unsupported here.
const codexSupported = false

func codexRequirementsPath() string { return "" }

func codexConfigPath() string { return "" }

func codexUserConfigs() []string { return nil }
