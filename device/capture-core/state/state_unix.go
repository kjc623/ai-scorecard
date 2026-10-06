//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
)

// protectDir makes the directory 0700 and, when the service runs as root, owned by root.
func protectDir(path string) error {
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 0, 0); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o700)
}

// createProtected creates path exclusively with mode 0600. An existing file is an error that
// satisfies errors.Is(err, fs.ErrExist).
func createProtected(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	// The umask can only narrow the mode; set it explicitly so it is exactly 0600.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w (mode %o)", ErrNotProtected, st.Mode().Perm())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() && sys.Uid != 0 {
		return fmt.Errorf("%w (owner uid %d)", ErrNotProtected, sys.Uid)
	}
	return nil
}
