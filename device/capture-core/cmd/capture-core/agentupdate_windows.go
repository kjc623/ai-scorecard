//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/protocol"
)

// platformAgentUpdate is the platform the release statement must name, and how its package is
// installed: Windows Installer, detached from the service, which the MSI's major upgrade stops,
// replaces and starts again. The package's own tenant file beside it satisfies the MSI's check.
func platformAgentUpdate() (string, agentInstaller) {
	if runtime.GOARCH != "amd64" {
		return "", nil
	}
	return protocol.AgentPlatformWindowsAMD64, func(path, logPath string) error {
		msiexec := filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe")
		cmd := exec.Command(msiexec, "/i", path, "/qn", "/norestart", "/l*v", logPath)
		cmd.Dir = filepath.Dir(path)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}
