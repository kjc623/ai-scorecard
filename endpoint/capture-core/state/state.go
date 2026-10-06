// Package state is the agent's state directory: the spool and its key, the device credential,
// held M3 content and its key, the cached policy bundle, the per-device interception CA and the
// latest health snapshot. Only the service may read it: on Windows the directory carries a
// protected DACL (inheritance disabled) granting SYSTEM, Administrators and the service account,
// and elsewhere it is mode 0700, owned by root when the service runs as root. Key files are
// written with the same protection rather than relying on inheritance alone.
package state

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The fixed layout under the state directory.
const (
	SpoolDir       = "spool"
	SpoolKeyFile   = "spool.key"
	CredentialFile = "credential.sealed"
	ContentDir     = "content"
	ContentKeyFile = "content.key"
	PolicyDir      = "policy"
	DeviceCADir    = "device-ca"
	HealthFile     = "health.json"
	LogFile        = "capture-core.log"
)

// KeySize is the length of every symmetric key the agent keeps: 32 bytes, AES-256.
const KeySize = 32

// ErrNotProtected reports a file another account may read, write or own.
var ErrNotProtected = errors.New("readable, writable or owned by an account other than SYSTEM, Administrators or this service")

// Dir is an opened, protected state directory.
type Dir struct{ root string }

// Open creates the state directory when it does not exist and protects it.
func Open(path string) (Dir, error) {
	if path == "" {
		return Dir{}, errors.New("state: no state directory configured")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Dir{}, err
	}
	if err := MkdirProtected(abs); err != nil {
		return Dir{}, fmt.Errorf("state: %s: %w", abs, err)
	}
	return Dir{root: abs}, nil
}

// Root is the absolute path of the state directory.
func (d Dir) Root() string { return d.root }

// Path joins elem onto the state directory.
func (d Dir) Path(elem ...string) string {
	return filepath.Join(append([]string{d.root}, elem...)...)
}

// Key returns the key kept in the named file, creating it when it does not exist.
func (d Dir) Key(name string) ([]byte, error) { return ReadOrCreateKey(d.Path(name)) }

// MkdirProtected creates path (and its parents) when absent and gives it the protected access
// control. A symbolic link in place of the directory is refused.
func MkdirProtected(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return protectDir(path)
}

// WriteFile replaces path with data through a protected temporary file in the same directory, so
// the file is never readable by anyone else, even briefly, and a crash leaves the old file or the
// new one.
func WriteFile(path string, data []byte) error {
	tmp, err := tempName(path)
	if err != nil {
		return err
	}
	f, err := createProtected(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CheckFile reports whether path is protected: only SYSTEM, Administrators or this service may
// own it or have access to it (on Windows), or it is not readable or writable by group or others
// (elsewhere).
func CheckFile(path string) error { return checkFile(path) }

// ReadOrCreateKey returns the KeySize-byte key in path, creating it with random bytes when the
// file does not exist. Creation is exclusive, so two processes cannot end up with two keys. A
// key file that exists but is not protected is re-written with protection; a file of the wrong
// length cannot have encrypted anything (a key is complete before its first use) and is replaced.
func ReadOrCreateKey(path string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		key, err := os.ReadFile(path)
		switch {
		case err == nil && len(key) == KeySize:
			if checkFile(path) != nil {
				if err := WriteFile(path, key); err != nil {
					return nil, fmt.Errorf("state: protecting %s: %w", path, err)
				}
			}
			return key, nil
		case err == nil:
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("state: %s holds %d bytes, want %d: %w", path, len(key), KeySize, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("state: reading %s: %w", path, err)
		}
		key = make([]byte, KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := createProtected(path)
		if errors.Is(err, fs.ErrExist) {
			continue // created concurrently; read what the other writer stored
		}
		if err != nil {
			return nil, fmt.Errorf("state: creating %s: %w", path, err)
		}
		_, werr := f.Write(key)
		if serr := f.Sync(); werr == nil {
			werr = serr
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, fmt.Errorf("state: writing %s: %w", path, werr)
		}
		return key, nil
	}
	return nil, fmt.Errorf("state: %s could not be read or created", path)
}

func tempName(path string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%x.tmp", filepath.Base(path), b)), nil
}
