// Package testhook implements the parser child's CLASSIFIER_HOST_TESTHOOK_* behaviour.
//
// It exists so the §10 isolation tests can drive a child that is hostile in the ways the
// parent-enforced limits must survive: one that allocates past its cap, hangs, exits non-zero, or
// writes something that is not a result frame. Both the production child
// (cmd/classifier-host's `parse-child`) and the isolation test's helper process call FromEnv, so
// the tests exercise the same code path the shipped child runs.
//
// Nothing in production sets these variables, they are read only by the child, and every one of
// them can only make the child *less* well behaved — none of them widens what the child can reach,
// which is the property §10 cares about. They are documented in README.md rather than hidden.
package testhook

import (
	"os"
	"time"
)

// EnvPrefix is the namespace of the test hooks.
const EnvPrefix = "CLASSIFIER_HOST_TESTHOOK_"

// AllocEnv makes the child allocate and touch megabytes until it is killed.
const AllocEnv = EnvPrefix + "ALLOC_MB"

// SleepEnv makes the child hang before parsing.
const SleepEnv = EnvPrefix + "SLEEP_MS"

// ExitEnv makes the child exit with this status instead of parsing.
const ExitEnv = EnvPrefix + "EXIT"

// GarbageEnv makes the child write non-frame bytes to stdout.
const GarbageEnv = EnvPrefix + "GARBAGE"

// FromEnv returns the hook for the current environment, or nil when no hook is set.
func FromEnv() func() { return fromEnv(os.Getenv, os.Stdout, os.Exit) }

// fromEnv is the testable core: it takes the environment, the output stream and the exit function
// so the behaviour can be unit-tested without exiting the test binary.
func fromEnv(getenv func(string) string, stdout *os.File, exit func(int)) func() {
	allocMB := envInt(getenv(AllocEnv))
	sleepMS := envInt(getenv(SleepEnv))
	exitCode := envInt(getenv(ExitEnv))
	garbage := getenv(GarbageEnv) != ""
	if allocMB == 0 && sleepMS == 0 && exitCode == 0 && !garbage {
		return nil
	}
	return func() {
		if garbage {
			_, _ = stdout.WriteString("this is not a result frame")
			exit(0)
			return
		}
		if exitCode != 0 {
			exit(exitCode)
			return
		}
		if sleepMS > 0 {
			time.Sleep(time.Duration(sleepMS) * time.Millisecond)
		}
		if allocMB > 0 {
			// Touch every page: a job object's memory limit and a residency sample both count
			// committed, touched pages rather than reserved address space.
			const chunk = 4 << 20
			var held [][]byte
			for i := 0; i < allocMB/4; i++ {
				b := make([]byte, chunk)
				for j := 0; j < len(b); j += 4096 {
					b[j] = byte(i)
				}
				held = append(held, b)
			}
			_ = held
		}
	}
}

func envInt(v string) int {
	if v == "" {
		return 0
	}
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0
		}
		n = n*10 + int(v[i]-'0')
		if n > 1<<30 {
			return 1 << 30
		}
	}
	return n
}
