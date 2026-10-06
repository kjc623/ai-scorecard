package isolation

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessAttributes gives the child its own process group, so killing the group also kills
// anything the child started.
func setProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// groupEnforcer kills the child's process group. macOS exposes another process's memory only
// through libproc, and RLIMIT_AS would kill a healthy Go child, so the child is bounded by the
// timeout and by its own decompression and output limits.
type groupEnforcer struct {
	pid int
}

func newEnforcer(pid int, _ int64) (enforcer, error) {
	return &groupEnforcer{pid: pid}, errors.New("macOS offers no memory sampling of another process; the timeout bounds the child")
}

func (e *groupEnforcer) sample() (int64, bool) { return 0, false }

func (e *groupEnforcer) peak() int64 { return 0 }

func (e *groupEnforcer) kill() error { return syscall.Kill(-e.pid, syscall.SIGKILL) }

func (e *groupEnforcer) close() {}
