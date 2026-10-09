//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// firewallRuleRemover is nil: the agent adds firewall rules on Windows only.
func firewallRuleRemover() func(string) (int, error) { return nil }

// uninstallLogPath is the uninstall log in the system's temporary directory.
func uninstallLogPath() string { return filepath.Join(os.TempDir(), uninstallLogName) }
