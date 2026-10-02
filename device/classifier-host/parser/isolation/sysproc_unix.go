//go:build !windows && !js

package isolation

import (
	"os/exec"
	"syscall"
)

// A child gets its own process group so the parent can kill the group, not just the process: a
// parser that forked must not outlive the document it was given (§10's Lifetime row).
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
