//go:build windows

package spool

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockProbeOffset is the start of the locked byte range. It is not 0: the holder's pid is
// written at offset 0, and a mandatory range lock there would stop the next process from reading
// the diagnostic that explains its failure.
const lockProbeOffset = 1024

// lockExclusive takes a LockFileEx lock on one byte, which the kernel releases when the holding
// handle closes or the process dies.
func lockExclusive(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockProbeOffset}
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockHeld
	}
	return err
}

// unlockExclusive is a no-op on Windows: closing the handle releases the lock.
func unlockExclusive(*os.File) error { return nil }
