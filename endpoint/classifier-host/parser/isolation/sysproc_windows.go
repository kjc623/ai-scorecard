//go:build windows && !js

package isolation

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps a console from flashing when a document is attached.
const createNoWindow = 0x08000000

func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
