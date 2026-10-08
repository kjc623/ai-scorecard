//go:build !windows

package toolconfig

// codexSupported: the macOS and Linux system location (/etc/codex/requirements.toml) is not
// written yet, so the provider reports tool_version_unsupported here.
const codexSupported = false

func codexRequirementsPath() string { return "" }
