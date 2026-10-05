//go:build !windows

package main

import (
	"fmt"
	"os"
)

// writeProtectedFile writes the key readable by its owner only (the agent's account, root for the
// service), the POSIX form of the Windows DACL.
func writeProtectedFile(path string, data []byte) error {
	return writeFileAtomic(path, data, 0o600)
}

// checkProtectedFile refuses a key file any other account may read or write.
func checkProtectedFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w (mode %o)", errNotProtected, st.Mode().Perm())
	}
	return nil
}
