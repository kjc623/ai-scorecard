//go:build !windows

package tlsproxy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// platformKeyStore keeps the device root's key as a file in the protected state directory, beside
// its certificate.
func platformKeyStore(dir string) (caKeyStore, error) {
	return fileKeyStore{path: filepath.Join(dir, deviceCAKeyFile)}, nil
}

// deletePlatformKey deletes the key file beside the certificate.
func deletePlatformKey(dir string) error {
	if err := os.Remove(filepath.Join(dir, deviceCAKeyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
