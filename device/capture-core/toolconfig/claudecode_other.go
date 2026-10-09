//go:build !windows

package toolconfig

// claudeCodeSupported: the macOS and Linux managed locations are not written yet, so the provider
// reports tool_version_unsupported here.
const claudeCodeSupported = false

func claudeCodeManagedPath() string { return "" }
