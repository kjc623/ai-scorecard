//go:build !windows

package inventory

import (
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// Scanners is the platform's scan pass: none yet on macOS and Linux, so the provider reports
// absent with tool_version_unsupported.
func Scanners(func(hostinfo.User) core.Person) []Scanner { return nil }
