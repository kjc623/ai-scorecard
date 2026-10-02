//go:build windows

package spool

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// LockFileEx: a real OS lock held by the file handle, released by the kernel when the
// process dies. Called through the standard library's lazy DLL loader rather than a
// dependency, because golang.org/x/sys/windows is not fetchable on this offline host.
//
// The syscall package has no typed wrapper for LockFileEx, so this file is the one place in
// the package that touches unsafe. The call is synchronous (LOCKFILE_FAIL_IMMEDIATELY), so
// the OVERLAPPED structure does not outlive the call, and it is kept alive explicitly.
const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001

	// lockProbeOffset is the start of the locked byte range. It is deliberately not 0: the
	// holder's pid is written at offset 0, and a mandatory range lock would otherwise stop
	// the next process from reading the very diagnostic that explains its failure.
	lockProbeOffset = 1024

	// errorLockViolation is ERROR_LOCK_VIOLATION (33), what LockFileEx returns when the
	// range is already exclusively locked.
	errorLockViolation = syscall.Errno(33)
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
)

func lockExclusive(f *os.File) error {
	ol := new(syscall.Overlapped)
	ol.Offset = lockProbeOffset
	r1, _, err := procLockFileEx.Call(
		uintptr(f.Fd()),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1, // one byte
		0,
		uintptr(unsafe.Pointer(ol)),
	)
	runtime.KeepAlive(ol)
	runtime.KeepAlive(f)
	if r1 == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno == errorLockViolation {
			return errLockHeld
		}
		if err == nil || err == syscall.Errno(0) {
			return errLockHeld
		}
		return err
	}
	return nil
}

// unlockExclusive is a no-op on Windows: closing the handle releases the lock, and doing so
// explicitly would need UnlockFileEx for no additional safety.
func unlockExclusive(f *os.File) error { return nil }
