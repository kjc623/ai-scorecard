//go:build !windows && !linux && !js

package isolation

import (
	"errors"
	"syscall"
)

// The fallback enforcer covers macOS, where §10 asks for "an address-space limit plus residency
// monitoring". Neither is available here in a form that works for this child:
//
//   - an address-space limit (setrlimit RLIMIT_AS) would kill a healthy Go child, because the Go
//     runtime reserves a large virtual address space at startup — the same reason the Linux path
//     does not set it;
//   - residency monitoring has no portable stdlib source on Darwin (no /proc), and shelling out
//     to `ps` per sample would put a process spawn per 2 ms on the interactive path.
//
// So on this platform the child is bounded by the wall-clock timeout and the hard kill, and the
// absence of a residency monitor is reported as a non-fatal construction error so the caller
// records "memory cap not enforced" rather than believing a cap exists. This is an open decision
// in README.md: the honest implementation is a libproc (`proc_pid_rusage`) call, which needs a
// platform binding this offline build host cannot fetch.
type pollEnforcer struct {
	pid int
}

func newEnforcer(pid int, memCap, residencyCap int64) (enforcer, error) {
	return &pollEnforcer{pid: pid}, errors.New("residency sampling is unavailable on this platform; only the timeout and hard kill bound the parser child")
}

func (e *pollEnforcer) sample() (int64, bool) { return 0, false }

func (e *pollEnforcer) peak() int64 { return 0 }

func (e *pollEnforcer) kill() error {
	return syscall.Kill(-e.pid, syscall.SIGKILL)
}

func (e *pollEnforcer) close() {}
