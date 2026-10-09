package toolconfig

import (
	"errors"
	"io/fs"
	"os"
)

// managedFiles reads and replaces a tool's managed file. Tests replace it to make a write fail.
type managedFiles interface {
	// read returns the file's content, or false when there is no file.
	read(path string) ([]byte, bool, error)
	// write replaces the file atomically. The new file keeps the old one's access control unless
	// that lets users write it, and a new file gets access control users can read but not write.
	write(path string, data []byte) error
	// remove deletes the file; a missing file is not an error.
	remove(path string) error
}

// systemFiles is the managed file on this machine's file system.
type systemFiles struct{}

func (systemFiles) read(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (systemFiles) write(path string, data []byte) error { return writeManaged(path, data) }

func (systemFiles) remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeTemp writes data to a new file in dir and returns its name. The caller renames it over the
// managed file or removes it.
func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".managed-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
