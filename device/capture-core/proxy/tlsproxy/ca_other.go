//go:build !windows

package tlsproxy

import "path/filepath"

// platformKeyStore keeps the device root's key as a file in the protected state directory, beside
// its certificate.
func platformKeyStore(dir string) (caKeyStore, error) {
	return fileKeyStore{path: filepath.Join(dir, deviceCAKeyFile)}, nil
}
