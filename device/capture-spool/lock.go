package spool

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// One writer, one encryption key, one place the bound is enforced (§3.4). The writer lock is
// an *operating-system* lock on a lock file in the spool directory, not a sentinel file:
// the OS releases it when the holding process dies, for any reason, so there is no stale
// lock to detect and no window in which two processes can both believe they hold it.
//
// The lock file stays on disk after release. Deleting it would be the classic race — one
// process removing the name while another has already locked the file behind it, leaving a
// third free to create a new file at the same name and be "exclusive" too.
//
// The lock guards against a second service instance, a test harness, or an operator running
// a CLI against a running service. It is not a security boundary against a local
// administrator, and it is not claimed to be one: the encryption key, not the lock, is what
// protects the contents.
const lockFileName = "writer.lock"

// errLockHeld means the lock is held by someone else.
var errLockHeld = errors.New("spool: writer lock is held")

type writerLock struct {
	f    *os.File
	path string
}

func acquireWriterLock(path string) (*writerLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: opening writer lock %s: %w", path, err)
	}
	if err := lockExclusive(f); err != nil {
		holder := readHolderPID(f)
		f.Close()
		if errors.Is(err, errLockHeld) {
			if holder != "" {
				return nil, fmt.Errorf("%w (held by pid %s)", ErrWriterActive, holder)
			}
			return nil, ErrWriterActive
		}
		return nil, fmt.Errorf("spool: locking %s: %w", path, err)
	}
	// The pid is diagnostic only — the OS lock is the mechanism. It is written at offset 0
	// while the exclusive range starts elsewhere, so the process that is about to be told
	// "someone else holds this" can still read who that is.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		// Not fatal: the lock is held, and the pid is a courtesy.
		_ = err
	}
	return &writerLock{f: f, path: path}, nil
}

// release drops the lock. Closing the handle releases an OS lock, which is why this is
// correct even if the process is being torn down.
func (l *writerLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlockExclusive(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}

// readHolderPID reads the diagnostic pid. Failure is not an error: it only affects the
// wording of ErrWriterActive.
func readHolderPID(f *os.File) string {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if n <= 0 {
		if err == nil {
			return ""
		}
		return ""
	}
	return strings.TrimSpace(string(buf[:n]))
}
