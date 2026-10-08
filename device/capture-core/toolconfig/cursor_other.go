//go:build !windows

package toolconfig

// cursorSupported: the macOS and Linux enterprise locations are not written yet, so the provider
// reports tool_version_unsupported here.
const cursorSupported = false

func cursorHooksPath() string { return "" }
