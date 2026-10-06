package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcessAttributes gives the child its own process group, so killing the group also kills
// anything the child started.
func setProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// procEnforcer samples the child's resident set from /proc. RLIMIT_AS is not used: the Go
// runtime reserves a large address space at start-up, so an address-space limit would kill a
// healthy child.
type procEnforcer struct {
	pid      int
	statm    string
	observed int64
}

func newEnforcer(pid int, _ int64) (enforcer, error) {
	e := &procEnforcer{pid: pid, statm: fmt.Sprintf("/proc/%d/statm", pid)}
	if _, ok := e.sample(); !ok {
		return e, fmt.Errorf("cannot read %s", e.statm)
	}
	return e, nil
}

func (e *procEnforcer) sample() (int64, bool) {
	b, err := os.ReadFile(e.statm)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	n := pages * int64(os.Getpagesize())
	e.observed = max(e.observed, n)
	return n, true
}

func (e *procEnforcer) peak() int64 { return e.observed }

func (e *procEnforcer) kill() error { return syscall.Kill(-e.pid, syscall.SIGKILL) }

func (e *procEnforcer) close() {}
