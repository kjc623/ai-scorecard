//go:build !windows

package inventory

import "github.com/shadow-ai-capture/device/capture-core/policy"

// CLIInstalled reports whether the CLI scanner finds appKey: never on macOS and Linux, which have no
// CLI scanner yet.
func CLIInstalled(*policy.Bundle, string) bool { return false }
