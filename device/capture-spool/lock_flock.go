//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package spool

import (
	"errors"
	"os"
	"syscall"
)

// flock(2): an advisory lock attached to the open file description, released by the kernel
// when the process dies. It is the POSIX counterpart of the Windows LockFileEx path in
// lock_windows.go, and carries the same property the spool needs: no stale lock to detect,
// because a dead holder cannot hold one.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return errLockHeld
	default:
		return err
	}
}

func unlockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
