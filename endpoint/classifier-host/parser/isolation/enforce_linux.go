//go:build linux && !js

package isolation

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The Linux enforcer samples the child's resident set from /proc. There is no job object, so the
// parent's kill is the enforcement: crossing the residency cap terminates the child's process
// group. RLIMIT_AS is deliberately not used to bound the child: the Go runtime reserves a large
// virtual address space at startup, so an address-space limit would kill a healthy parser on
// arrival, and a limit that kills legitimate documents is not a limit — it is an outage.
type linuxEnforcer struct {
	pid  int
	path string
}

func newEnforcer(pid int, memCap, residencyCap int64) (enforcer, error) {
	e := &linuxEnforcer{pid: pid, path: fmt.Sprintf("/proc/%d/statm", pid)}
	if _, err := os.Stat(e.path); err != nil {
		return e, fmt.Errorf("residency sampling unavailable (%v)", err)
	}
	return e, nil
}

func (e *linuxEnforcer) sample() (int64, bool) {
	b, err := os.ReadFile(e.path)
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
	return pages * int64(os.Getpagesize()), true
}

func (e *linuxEnforcer) peak() int64 {
	n, _ := e.sample()
	return n
}

// kill terminates the child's whole process group, so a parser that forked cannot outlive it.
func (e *linuxEnforcer) kill() error {
	return syscall.Kill(-e.pid, syscall.SIGKILL)
}

func (e *linuxEnforcer) close() {}
