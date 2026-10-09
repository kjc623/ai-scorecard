//go:build !windows

package component

import (
	"os"
	"syscall"
)

// sysProcAttr puts the child in a process group of its own, so a kill reaches what it started
// too, and has the kernel kill it when the service dies where the platform can.
func sysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(attr)
	return attr
}

// processGroup is the child's process group.
type processGroup struct{ pid int }

func contain(p *os.Process) (containment, error) { return processGroup{pid: p.Pid}, nil }

func (g processGroup) kill() error { return syscall.Kill(-g.pid, syscall.SIGKILL) }

func (processGroup) release() {}
