//go:build !windows

package tlsproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Where the key is a file beside the certificate, deleting the key deletes the file, and a second
// delete finds nothing to do.
func TestDeleteDeviceKeyRemovesTheKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, deviceCAKeyFile)
	if err := os.WriteFile(keyFile, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteDeviceKey(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key file is still there: %v", err)
	}
	if err := DeleteDeviceKey(dir); err != nil {
		t.Fatalf("deleting again: %v", err)
	}
}
