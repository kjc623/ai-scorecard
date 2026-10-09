//go:build !windows

package toolconfig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// writeManaged writes data beside path and renames it over path. The new file keeps the old one's
// mode and owner without group or other write; a new file is 0644.
func writeManaged(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	uid, gid := -1, -1
	st, err := os.Stat(path)
	switch {
	case err == nil:
		mode = st.Mode().Perm() &^ 0o022
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if uid >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(tmp, uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}
